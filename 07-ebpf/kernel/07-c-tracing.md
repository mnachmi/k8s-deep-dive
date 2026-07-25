# eBPF Tracing Attachment Points — kprobe, Tracepoint, fentry/fexit, CO-RE

eBPF programs are not free-standing — they must attach to a hook point in the kernel that calls them when a specific event fires. This document covers the four primary tracing attachment mechanisms: kprobe (dynamic, any kernel symbol), tracepoints (static, stable ABI), and fentry/fexit (BTF-based trampolines). It then explains CO-RE, which makes BPF programs portable across kernel versions, and provides bpftrace and bpftool commands for live observation.

## 1. Source Locations

| File | Key Symbols | URL |
|------|-------------|-----|
| `kernel/trace/bpf_trace.c` | `bpf_perf_event_output`, `kprobe_perf_func`, `trace_call_bpf` | https://elixir.bootlin.com/linux/v6.9/source/kernel/trace/bpf_trace.c |
| `kernel/bpf/trampoline.c` | `bpf_trampoline_get`, `bpf_trampoline_update`, fentry/fexit dispatch | https://elixir.bootlin.com/linux/v6.9/source/kernel/bpf/trampoline.c |
| `kernel/trace/trace_kprobe.c` | `trace_kprobe_create`, `kprobe_register` | https://elixir.bootlin.com/linux/v6.9/source/kernel/trace/trace_kprobe.c |
| `include/linux/tracepoint.h` | `DECLARE_TRACE`, `struct tracepoint`, `__DO_TRACE` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/tracepoint.h |
| `kernel/bpf/btf.c` | `btf_check_func_arg_match`, CO-RE relocation resolution | https://elixir.bootlin.com/linux/v6.9/source/kernel/bpf/btf.c |

## 2. kprobe / kretprobe

kprobes provide dynamic instrumentation of any non-inlined kernel symbol. They work by replacing a target instruction with a software breakpoint at registration time, routing execution through a trampoline, and restoring normal execution afterward.

### Mechanism

1. `register_kprobe(kp)` saves the original bytes of the first instruction of the target function and replaces them with `0xcc` (the x86 `int3` opcode). On architectures that support jump optimization, a 5-byte `jmp` to a kprobe-owned trampoline can be used instead, avoiding the single-step path.
2. When a CPU executes the patched address, it faults into the `int3` handler. The kernel dispatches to `kprobe_int3_handler()` → `kprobe_handler()`, which calls `kp->pre_handler(kp, regs)`.
3. For BPF-backed kprobes, the handler is `kprobe_perf_func()` in `kernel/trace/bpf_trace.c`. It calls `trace_call_bpf(call, regs)`, which runs all BPF programs registered on that perf event, passing `struct pt_regs *ctx` as the program context.

### BPF Program Context

Programs of type `BPF_PROG_TYPE_KPROBE` receive `struct pt_regs *` as their context. Function arguments are accessed with architecture-portable macros:

- `PT_REGS_PARM1(ctx)` through `PT_REGS_PARM5(ctx)` — first five function arguments
- `PT_REGS_RC(ctx)` — return value (kretprobe only)
- `PT_REGS_IP(ctx)` — instruction pointer at the probe site

These macros expand to the appropriate register fields (`rdi`, `rsi`, `rdx`, `rcx`, `r8` on x86-64) and must be used rather than direct struct field access because `pt_regs` layout differs across architectures.

### Performance Cost

The `int3` → fault → handler round-trip adds approximately 100–200 ns per probe hit on x86-64. Jump optimization reduces this when enabled (and the kernel supports it), because a 5-byte `jmp` avoids the trap and single-step overhead. Even with optimization, kprobes remain the most expensive tracing mechanism covered here.

## 3. Tracepoints

Tracepoints are static instrumentation sites compiled into the kernel source with the `DECLARE_TRACE` / `DEFINE_TRACE` macros. They are placed at semantically meaningful locations — filesystem entry, scheduler wakeups, network transmit — and expose a stable, versioned payload format that is part of the kernel ABI.

### `struct tracepoint`

```c
// include/linux/tracepoint.h (simplified, Linux 6.9)
struct tracepoint {
    const char          *name;       // e.g. "net_dev_queue", "sys_enter_read"
    struct static_key   key;         // jump key: NOP when disabled, CALL when enabled
    int (*regfunc)(void);
    void (*unregfunc)(void);
    struct tracepoint_func __rcu *funcs; // NULL-terminated array of (func, data) pairs
};
```

`static_key` is the kernel's jump-patching mechanism. When no probes are attached, the call site is a 5-byte NOP — zero runtime cost. When a probe is registered, `static_key_enable()` patches the NOP to a `call __traceiter_<name>` instruction, after which every execution of that code path calls the registered handlers.

### `DECLARE_TRACE` Code Generation

`DECLARE_TRACE(name, proto, args)` generates three things:

- A `struct tracepoint __tracepoint_##name` object placed in the `__tracepoints` ELF section, holding the `static_key` and the `funcs` array.
- An inline function `trace_##name(proto)` that checks `static_key_enabled(&__tracepoint_##name.key)` and, if true, calls `__DO_TRACE(name, args)` to iterate the `funcs` array.
- `register_trace_##name()` and `unregister_trace_##name()` accessors that add or remove entries from the `funcs` array and patch the `static_key` accordingly.

### BPF Attachment

BPF programs attach to tracepoints via `perf_event_open()` with `attr.type = PERF_TYPE_TRACEPOINT` and `attr.config` set to the tracepoint ID from `/sys/kernel/tracing/events/<category>/<name>/id`. The program type is `BPF_PROG_TYPE_TRACEPOINT`.

The tracepoint `format` file at `/sys/kernel/tracing/events/<category>/<name>/format` documents the payload fields and their byte offsets. BPF programs access these fields through `args->field_name` when compiled with the tracepoint's generated BTF annotations.

Tracepoints are the preferred mechanism for production tracing because they have zero cost when disabled, provide a stable and documented interface, and their payload is typed via BTF.

## 4. fentry / fexit

fentry and fexit (introduced Linux 5.5) use BPF trampolines instead of kprobes — a fundamentally different mechanism that avoids trap overhead entirely.

### `struct bpf_trampoline`

The kernel creates one `struct bpf_trampoline` per `(function address, prog_type)` pair, allocated in `kernel/bpf/trampoline.c`. The trampoline is a small block of JIT-generated native code that:

1. Saves callee-saved and argument registers according to the x86-64 calling convention.
2. Calls each registered fentry BPF program in sequence, passing the saved argument registers as the BPF context.
3. Calls the original function (for fentry; fexit does this then calls the fexit BPF programs with both arguments and the return value).
4. Restores registers and returns.

### Attachment via `bpf_trampoline_update()`

`bpf_trampoline_update()` in `kernel/bpf/trampoline.c` patches the first instruction(s) of the target function to call the trampoline. This is not an `int3` — it is a direct call or a modified function prologue that jumps to the trampoline code. No trap is generated; the CPU branches directly into the trampoline stub.

### BTF-Based Argument Access

Because the verifier knows the target function's signature from BTF (retrieved from `/sys/kernel/btf/vmlinux`), it can verify argument types at load time and expose them directly to the BPF program as typed parameters. No `PT_REGS_*` macros are needed:

```c
// fentry program attached to tcp_sendmsg:
// The verifier knows tcp_sendmsg's BTF signature:
//   int tcp_sendmsg(struct sock *sk, struct msghdr *msg, size_t size)
SEC("fentry/tcp_sendmsg")
int BPF_PROG(trace_tcp_sendmsg, struct sock *sk, struct msghdr *msg, size_t size)
{
    bpf_printk("tcp_sendmsg: size=%zu\n", size);
    return 0;
}
```

fexit receives all arguments plus the return value, making it the right choice for latency measurement (record entry timestamp in fentry, compute delta in fexit).

### Performance

fentry/fexit add approximately 10–30 ns per hit (fexit slightly more, at 15–35 ns, because it must also capture the return value). This is 3–10x faster than kprobes because there is no `int3` fault, no single-step emulation, and the trampoline is JIT-generated native code with a minimal register save/restore sequence.

## 5. Attachment Comparison

| Type | Probe | Overhead | Arg Access | Requires | When to Use |
|------|-------|----------|------------|----------|-------------|
| kprobe | any kernel symbol (not inlined) | ~100–200 ns | `PT_REGS_PARM1–5(ctx)` | nothing extra | legacy kernels, any kernel function, exploratory tracing |
| kretprobe | return of any symbol | ~200 ns | `PT_REGS_RC(ctx)` | nothing extra | return value capture on older kernels |
| tracepoint | static `DECLARE_TRACE` sites | ~5–10 ns (NOP when off) | typed fields via `args->field` | trace event compiled into kernel | stable API, production safe, minimal overhead |
| fentry | BTF-visible function entry | ~10–30 ns | typed args via BTF, no macros | BTF enabled, func in vmlinux | modern replacement for kprobe; cleaner arg access |
| fexit | BTF-visible function return + args | ~15–35 ns | typed args + return value | BTF, func in vmlinux | latency measurement, return-value tracing on 5.5+ |

Tracepoints should be the first choice for any stable event. fentry/fexit should replace kprobe/kretprobe on kernels 5.5 and later. kprobe remains useful for debugging kernel functions that do not have a tracepoint and for kernels without BTF.

## 6. CO-RE (Compile Once, Run Everywhere)

### The Problem

A BPF program compiled against the headers of one kernel version embeds hardcoded field offsets from that version's data structures. If the kernel is upgraded and a struct field moves, the BPF program reads garbage or faults. Traditional BPF workflows required recompiling programs per kernel, making distribution of pre-compiled BPF objects impractical.

### How CO-RE Solves It

CO-RE is a collaboration between the clang compiler, the `.BTF.ext` ELF section, libbpf, and the kernel's BTF infrastructure:

1. **Compiler**: `clang -g -O2 -target bpf` wraps field accesses in `__builtin_preserve_access_index()`. The compiler emits a CO-RE relocation record in `.BTF.ext` for each such access, recording the BTF type ID, field name, and instruction location — not a raw byte offset.

2. **Loader resolution**: At load time, `libbpf` reads the running kernel's BTF from `/sys/kernel/btf/vmlinux`. For each CO-RE relocation, it looks up the field by name in the running kernel's type graph and finds its actual byte offset in the current kernel's struct layout.

3. **Bytecode patching**: `libbpf` patches the corresponding BPF instruction's immediate operand with the resolved offset before passing the bytecode to the `bpf(2)` syscall.

4. **Missing fields**: If the required field is absent in the running kernel's BTF, libbpf can either reject the program with a clear error or, if the BPF program uses `bpf_core_field_exists()`, conditionally skip the access at runtime.

### `BPF_CORE_READ`

```c
// In BPF C (compiled with clang -g -O2 -target bpf):
#include "vmlinux.h"   // generated once per kernel:
                       // bpftool btf dump file /sys/kernel/btf/vmlinux format c > vmlinux.h

SEC("kprobe/do_sys_openat2")
int trace_open(struct pt_regs *ctx)
{
    struct task_struct *task = (struct task_struct *)bpf_get_current_task();

    // BPF_CORE_READ expands to __builtin_preserve_access_index;
    // clang emits a relocation; libbpf patches the offset at load time.
    pid_t pid = BPF_CORE_READ(task, pid);
    u32 nsproxy_offset = bpf_core_field_exists(task->nsproxy) ? 1 : 0;

    bpf_printk("pid=%d\n", pid);
    return 0;
}
```

`vmlinux.h` is generated from `/sys/kernel/btf/vmlinux` and contains forward declarations of every kernel struct and typedef visible through BTF — roughly 100 000 lines. It replaces kernel headers for BPF development and is regenerated once per kernel version.

### vmlinux BTF

`/sys/kernel/btf/vmlinux` is a binary BTF blob embedded in the kernel image during build (requires `CONFIG_DEBUG_INFO_BTF=y`). It is the ground truth for all CO-RE resolutions on a running system.

## 7. Live Observation

```bash
# List available tracepoint categories:
ls /sys/kernel/tracing/events/ | head -20

# List all syscall tracepoints:
ls /sys/kernel/tracing/events/syscalls/ | head -20

# Inspect tracepoint payload format:
cat /sys/kernel/tracing/events/net/netif_receive_skb/format

# bpftrace: attach to a tracepoint (typed args, zero-cost when off):
bpftrace -e 'tracepoint:syscalls:sys_enter_openat { printf("pid=%d file=%s\n", pid, str(args->filename)); }'

# bpftrace: attach to a kprobe:
bpftrace -e 'kprobe:vfs_read { printf("pid=%d\n", pid); }'

# bpftrace: fentry (BTF-aware, no PT_REGS needed):
bpftrace -e 'fentry:tcp_sendmsg { printf("pid=%d len=%d\n", pid, (int)arg2); }'

# List all loaded BPF programs grouped by type:
bpftool prog list

# Show only kprobe programs:
bpftool prog list | grep kprobe

# Show fentry/fexit programs:
bpftool prog list | grep fentry

# Dump a loaded BPF program's instructions:
bpftool prog dump xlated id <id>

# Show which functions have BPF trampolines attached:
bpftool prog list --json | jq '.[] | select(.type=="fentry") | .attach_to_name'
```

## 8. Key Kernel References

| Symbol | File | URL |
|--------|------|-----|
| `struct tracepoint` | `include/linux/tracepoint.h` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/tracepoint.h |
| `kprobe_perf_func` | `kernel/trace/bpf_trace.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/trace/bpf_trace.c |
| `trace_call_bpf` | `kernel/trace/bpf_trace.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/trace/bpf_trace.c |
| `bpf_trampoline_update` | `kernel/bpf/trampoline.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/bpf/trampoline.c |
| `bpf_check_attach_target` | `kernel/bpf/verifier.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/bpf/verifier.c |
