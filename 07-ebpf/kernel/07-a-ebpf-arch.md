# eBPF Architecture — BPF ISA, `struct bpf_prog`, Verifier, JIT

eBPF is a kernel-resident virtual machine that runs user-supplied programs inside the kernel without recompilation. Programs are expressed in a 64-bit RISC instruction set, verified for safety by an abstract interpreter, JIT-compiled to native machine code, and attached to hook points ranging from socket filters to XDP drivers to kprobes. This document traces the architecture from raw bytes to running x86-64 code.

## 1. Source Locations

| File | Key Symbols | URL |
|------|-------------|-----|
| `kernel/bpf/core.c` | `bpf_prog_alloc`, `bpf_prog_select_runtime`, `___bpf_prog_run` | https://elixir.bootlin.com/linux/v6.9/source/kernel/bpf/core.c |
| `kernel/bpf/verifier.c` | `bpf_check`, `do_check`, `check_mem_access` | https://elixir.bootlin.com/linux/v6.9/source/kernel/bpf/verifier.c |
| `include/linux/bpf.h` | `struct bpf_prog`, `struct bpf_map`, `struct bpf_prog_ops` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/bpf.h |
| `include/uapi/linux/bpf.h` | `bpf_insn`, `bpf_attr`, `BPF_*` constants | https://elixir.bootlin.com/linux/v6.9/source/include/uapi/linux/bpf.h |
| `arch/x86/net/bpf_jit_comp.c` | `bpf_int_jit_compile`, `emit_prologue` | https://elixir.bootlin.com/linux/v6.9/source/arch/x86/net/bpf_jit_comp.c |

## 2. BPF ISA

BPF is a 64-bit RISC ISA with 11 registers (r0–r10):

- `r0` — return value from helper calls and from the program itself
- `r1–r5` — function arguments (clobbered across helper calls)
- `r6–r9` — callee-saved (preserved across helper calls)
- `r10` — read-only frame pointer; `r10 - 8` to `r10 - 512` is the BPF stack

### struct bpf_insn

Each instruction is 8 bytes. The layout is defined in `include/uapi/linux/bpf.h`:

```c
struct bpf_insn {
    __u8   code;       // opcode: class (3 bits) | source (1 bit) | operation (4 bits)
    __u8   dst_reg:4;  // destination register (0-10)
    __u8   src_reg:4;  // source register (0-10)
    __s16  off;        // signed 16-bit offset (memory/branch target)
    __s32  imm;        // signed 32-bit immediate
};
```

Key instruction classes:

| Class | Value | Purpose |
|-------|-------|---------|
| `BPF_LD` | `0x00` | Load (immediate, memory) |
| `BPF_ST` | `0x02` | Store to memory |
| `BPF_ALU` | `0x04` | 32-bit ALU operations |
| `BPF_JMP` | `0x05` | Branches, helper calls, program exit |
| `BPF_JMP32` | `0x06` | 32-bit comparison branches |
| `BPF_ALU64` | `0x07` | 64-bit ALU operations |

### Instruction Encoding Examples

```c
// 64-bit move: r0 = 42
BPF_MOV64_IMM(BPF_REG_0, 42)  // code=0xb7, dst=0, imm=42

// Helper call: call bpf_map_lookup_elem (helper #1)
BPF_EMIT_CALL(BPF_FUNC_map_lookup_elem)  // code=0x85, imm=1

// Exit: return r0
BPF_EXIT_INSN()  // code=0x95
```

### Wide Instructions (16 bytes)

`BPF_LD | BPF_DW | BPF_IMM` loads a 64-bit immediate using two consecutive `bpf_insn` slots. The second slot is a zero-filler (padding). This is the only 16-byte (wide) instruction in the BPF ISA:

```c
// BPF_LD_MAP_FD expands to TWO bpf_insn entries (16 bytes total)
// First insn: code=0x18, src=BPF_PSEUDO_MAP_FD, dst=reg, imm=lower_32(map_fd)
// Second insn: code=0x00, src=0, dst=0, off=0, imm=upper_32(map_fd)
BPF_LD_MAP_FD(BPF_REG_1, map_fd)
```

The verifier treats both slots as a single atomic operation and type-annotates the destination register as `PTR_TO_MAP_VALUE` after resolution.

## 3. struct bpf_prog

`struct bpf_prog` is the central kernel representation of a loaded BPF program. Defined in `include/linux/bpf.h` (Linux 6.9):

```c
// include/linux/bpf.h (simplified, Linux 6.9)
struct bpf_prog {
    u16             pages;        // number of pages allocated for this prog
    u16             jited:1,      // true if JIT-compiled
                    jit_requested:1,
                    gpl_compatible:1,
                    cb_access:1,
                    dst_needed:1,
                    blinded:1,
                    is_func:1,
                    kprobe_override:1,
                    has_callchain_buf:1,
                    enforce_expected_attach_type:1,
                    call_get_stack:1,
                    call_get_func_ip:1,
                    tstamp_type_access:1;
    enum bpf_prog_type  type;         // BPF_PROG_TYPE_SOCKET_FILTER, _KPROBE, _TRACEPOINT, ...
    enum bpf_attach_type expected_attach_type;
    u32             len;              // number of bpf_insn instructions
    u32             jited_len;        // JIT output size in bytes
    u8              tag[BPF_TAG_SIZE]; // sha1 of instructions (fingerprint)
    struct bpf_prog_stats __percpu *stats;
    int             *active;
    unsigned int    (*bpf_func)(const void *ctx, const struct bpf_insn *insn);
    struct bpf_prog_aux *aux;         // verifier state, BTF, name, used_maps[]
    union {
        struct sock_fprog_kern  *orig_prog;   // classic BPF origin
        struct bpf_insn         insnsi[];     // eBPF instructions (flexible array)
    };
};
```

### Key Fields

`bpf_func` — function pointer that drives execution. After `bpf_prog_select_runtime()` this points to the JIT-compiled native code on x86-64 (JIT is on by default). On architectures without JIT or when JIT is disabled, it points to `___bpf_prog_run` (the BPF interpreter loop in `kernel/bpf/core.c`).

`aux` — pointer to `struct bpf_prog_aux`, which carries verifier output and metadata:
- `aux->verified_insns` — number of instructions the verifier processed (the complexity cost; can be orders of magnitude larger than `len` due to path explosion)
- `aux->used_maps[]` — array of `struct bpf_map *` pointers for every map the program references; these are pinned alive (refcount held) for the lifetime of the program
- `aux->name[BPF_OBJ_NAME_LEN]` — program name string
- `aux->btf` — pointer to BTF (BPF Type Format) data for CO-RE

`insnsi[]` — flexible array member holding the raw `bpf_insn` instructions. It lives at the end of the allocation returned by `bpf_prog_alloc()`, sized as `len * sizeof(struct bpf_insn)`.

The `jited:1` and `jit_requested:1` bitfields (along with the others) are packed into the same `u16` word following `pages`, demonstrating the kernel's preference for bitfield packing in hot data structures.

## 4. Verifier — bpf_check()

The verifier in `kernel/bpf/verifier.c` performs abstract interpretation of every possible execution path before a program is permitted to run. It is the kernel's safety guarantee: no memory escapes, no unbounded loops (prior to Linux 5.3), no uninitialized reads.

### Abstract Interpretation

The verifier simulates the program without running it. It maintains a `struct bpf_verifier_state` for each point in the execution graph, tracking the type and value range of every register and every byte of the BPF stack frame.

### The Four Phases

**Phase 1 — DAG check (`check_cfg`)**

Ensures the instruction graph is a DAG: no unreachable instructions, no back-edges that would create unbounded loops (bounded loops via `BPF_JMP_BACK` with the verifier's loop detector are permitted since Linux 5.3), and total instruction count does not exceed `BPF_COMPLEXITY_LIMIT_INSNS` (1,000,000).

**Phase 2 — Type tracking (`do_check`)**

This is the main pass. Each register carries a `struct bpf_reg_state`:

```c
struct bpf_reg_state {
    enum bpf_reg_type type;   // see table below
    // ... value ranges, map ptr info, etc.
};
```

Register type values:

| Type | Meaning |
|------|---------|
| `NOT_INIT` | Register has not been written; any read is rejected |
| `SCALAR_VALUE` | Unknown integer; safe for arithmetic, not for pointer arithmetic without bounds check |
| `PTR_TO_CTX` | Pointer to the program's context struct (e.g., `struct xdp_md`, `struct __sk_buff`) |
| `PTR_TO_MAP_KEY` | Pointer into a map key buffer |
| `PTR_TO_MAP_VALUE` | Verified pointer into a map value; bounds known |
| `PTR_TO_STACK` | Pointer to the BPF stack frame; offset must be within [-512, 0) |
| `PTR_TO_PACKET` | Pointer into packet data; requires a `data < data_end` range check before dereference |

**Phase 3 — Memory access checks (`check_mem_access`)**

Every load and store instruction is checked: the register must be of a `PTR_TO_*` type, the access must be within the known bounds, and alignment must satisfy the architecture's requirements. For `PTR_TO_PACKET`, the verifier requires that program logic has performed a bounds check (`ctx->data + offset < ctx->data_end`) before it will approve the dereference.

**Phase 4 — Helper call checks (`check_helper_call`)**

Each `BPF_EMIT_CALL` opcode is resolved to a helper function. The verifier looks up `bpf_helper_proto` for that helper, which specifies the required type for each argument (e.g., `ARG_PTR_TO_MAP_KEY`) and the return type (e.g., `RET_PTR_TO_MAP_VALUE_OR_NULL`). After the call, the verifier updates register types accordingly — r0 becomes the return type, r1–r5 are marked `NOT_INIT`.

### Verifier Output

On success, the verifier records `env->prog->aux->verified_insns`: the total number of instructions the abstract interpreter processed across all paths. This number (not `prog->len`) represents the actual complexity cost and is reported by `bpftool prog show`.

## 5. JIT Compilation

After the verifier passes, `bpf_prog_select_runtime()` (in `kernel/bpf/core.c`) selects whether to run via interpreter or JIT. On x86-64 with JIT enabled (the default), it calls `bpf_int_jit_compile()` in `arch/x86/net/bpf_jit_comp.c`.

### x86-64 Register Mapping

The JIT maps BPF registers to x86-64 hardware registers:

| BPF Register | x86-64 Register | Role |
|--------------|-----------------|------|
| r0 | rax | Return value |
| r1 | rdi | Argument 1 |
| r2 | rsi | Argument 2 |
| r3 | rdx | Argument 3 |
| r4 | rcx | Argument 4 |
| r5 | r8 | Argument 5 |
| r6 | rbx | Callee-saved |
| r7 | r13 | Callee-saved |
| r8 | r14 | Callee-saved |
| r9 | r15 | Callee-saved |
| r10 | rbp | Frame pointer (read-only) |

The callee-saved BPF registers (r6–r9) map to callee-saved x86-64 registers (rbx, r13, r14, r15), so the JIT prologue must save and restore them around C helper calls — which is exactly what `emit_prologue()` does.

### JIT Code Generation

1. `emit_prologue()` emits: `push rbp; mov rbp, rsp; sub rsp, <stack_size>` followed by pushes for rbx, r13, r14, r15.
2. For each BPF instruction, the JIT emits one or more x86-64 instructions.
3. Helper calls (`BPF_EMIT_CALL`) become direct `call` instructions to kernel helper functions.
4. `bpf_jit_binary_alloc()` allocates an executable memory region via `module_alloc()` (the vmalloc-backed executable region also used for kernel modules). This is why JIT output appears in the module address range.
5. The JIT output address is stored in `prog->bpf_func`, overwriting the interpreter pointer.

After `bpf_prog_select_runtime()` returns, every invocation of the program — regardless of hook type — calls `prog->bpf_func(ctx, prog->insnsi)`. On JIT paths the `insnsi` argument is unused (the native code is self-contained); on interpreter paths `___bpf_prog_run` uses it as the instruction array to walk.

## 6. Lifecycle

```
bpf(BPF_PROG_LOAD, &attr, sizeof(attr))
  └─ bpf_prog_alloc()            # allocate struct bpf_prog + instructions
       └─ bpf_check()            # verifier (bpf_verifier_env)
            └─ bpf_prog_select_runtime()   # JIT compile
                 └─ fd installed in process file table
                    (bpf_prog_get_fd_by_id / prog lives until all FDs + refs released)

Attachment (e.g., SO_ATTACH_BPF):
  setsockopt(sk, SOL_SOCKET, SO_ATTACH_BPF, &fd, sizeof(fd))
    └─ sk_filter_trim_cap() → bpf_prog_get()   # bumps refcount
         └─ prog runs on every packet to that socket

Cleanup:
  close(fd)          # drops file reference
  detach socket      # drops sk_filter reference
  bpf_prog_free()    # called when refcount hits zero
```

### Reference Counting

A `struct bpf_prog` is reference-counted. References are held by:
- The file descriptor returned by `BPF_PROG_LOAD` (one ref per open fd)
- Each attachment point (socket filter, kprobe, XDP hook, etc.)
- BPF tail call maps that store the program

`bpf_prog_free()` releases the JIT memory (via `module_memfree()`), drops references to all `aux->used_maps[]`, and frees the `bpf_prog_aux`.

## 7. Live Observation

```bash
# List all loaded BPF programs:
bpftool prog list

# Show verifier output for a loaded program (prog id from above):
bpftool prog dump xlated id <id>

# Show JIT-compiled native code:
bpftool prog dump jited id <id>

# Show verifier complexity cost:
bpftool prog show id <id> | grep -E "xlated|jited|tag"

# Trace bpf_check() calls:
bpftrace -e 'kprobe:bpf_check { printf("prog_type=%d len=%d\n", ((struct bpf_prog *)arg0)->type, ((struct bpf_prog *)arg0)->len); }'

# Count BPF programs loaded per second:
bpftrace -e 'kprobe:bpf_prog_alloc { @[comm] = count(); } interval:s:5 { print(@); clear(@); }'
```

### What to Look For

`bpftool prog dump xlated` shows the BPF bytecode after verifier processing (with some fixups). `bpftool prog dump jited` shows the actual x86-64 disassembly, confirming the register mapping in section 5. The `verified_insns` count from `bpftool prog show` is the key signal for verifier complexity: a program with 100 instructions that branches heavily may report `verified_insns` in the tens of thousands.

## 8. Key Kernel References

| Symbol | File | URL |
|--------|------|-----|
| `struct bpf_prog` | `include/linux/bpf.h` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/bpf.h |
| `struct bpf_insn` | `include/uapi/linux/bpf.h` | https://elixir.bootlin.com/linux/v6.9/source/include/uapi/linux/bpf.h |
| `bpf_check` | `kernel/bpf/verifier.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/bpf/verifier.c |
| `bpf_int_jit_compile` | `arch/x86/net/bpf_jit_comp.c` | https://elixir.bootlin.com/linux/v6.9/source/arch/x86/net/bpf_jit_comp.c |
| `bpf_prog_select_runtime` | `kernel/bpf/core.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/bpf/core.c |
| `___bpf_prog_run` | `kernel/bpf/core.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/bpf/core.c |
