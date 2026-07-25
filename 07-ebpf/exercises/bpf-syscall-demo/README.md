# bpf-syscall-demo — Raw `bpf(2)` Syscall: Map + Program + Socket Filter

## 1. What It Demonstrates

This exercise uses the `bpf(2)` syscall **directly** — no libbpf, no clang, no
LLVM.  Only `gcc` and the kernel headers (`linux/bpf.h`).

The complete flow in a single C file:

| Step | bpf(2) command | What happens |
|------|----------------|--------------|
| 1 | `BPF_MAP_CREATE` | Create a `BPF_MAP_TYPE_ARRAY` (key=u32, value=u64, 8 entries) |
| 2 | `BPF_PROG_LOAD` | Assemble raw BPF bytecode and load a socket-filter program that increments `map[0]` on every received packet then returns 1 (SK_PASS) |
| 3 | `SO_ATTACH_BPF` | Attach the program to a UDP receive socket via `setsockopt` |
| 4 | send/recv | Send one loopback UDP packet — the kernel runs the BPF program |
| 5 | `BPF_MAP_LOOKUP_ELEM` / `BPF_MAP_UPDATE_ELEM` | Read `map[0]` (kernel-incremented counter) and write `map[1]` from userspace |

No helper libraries are used.  Every `bpf(2)` call goes through
`syscall(SYS_bpf, …)`.

## 2. Build and Run

```
# Build only (no root needed)
make build

# Build and run (root required for BPF_PROG_LOAD + SO_ATTACH_BPF)
make run
# or
sudo ./bpf_syscall_demo
```

**Requires**: `root`, or the process capabilities `CAP_BPF` and
`CAP_NET_ADMIN`.  Without them the program exits immediately with a clear
`EPERM` message and a hint.

## 3. Expected Output

```
=== bpf_syscall_demo: raw bpf(2) — no libbpf ===

[+] BPF_MAP_CREATE  => map_fd=3  (type=ARRAY, key=u32, value=u64, entries=8)
[+] BPF_PROG_LOAD   => prog_fd=4  (14 insns, type=SOCKET_FILTER, name=pkt_count_sk)
[+] SO_ATTACH_BPF   => recv_sock=5  send_sock=6  (loopback UDP port 59876)
[+] UDP trigger      => sent 8 bytes, received 8 bytes ("bpf-demo")
    BPF filter ran on the received packet — map[0] should now be 1

--- Userspace map operations ---
[+] BPF_MAP_UPDATE_ELEM  map[1] = 42  (userspace write)
[+] BPF_MAP_LOOKUP_ELEM  map[0] = 1   (BPF kernel increment)
[+] BPF_MAP_LOOKUP_ELEM  map[1] = 42  (userspace write)

=== Summary ===
  map_fd=3  prog_fd=4  recv_sock=5  send_sock=6
  map[0] counter after 1 packet : 1
  map[1] set from userspace      : 42

Tip: inspect the loaded prog with:
  bpftool prog show
  cat /proc/self/fdinfo/4   (prog tag)
```

Fd numbers may differ.  The counter in `map[0]` reflects exactly how many
packets passed through the filter during the run.

## 4. BPF Bytecode Walk

The program loaded via `BPF_PROG_LOAD` is 14 instructions (112 bytes).  The
instruction-building macros are defined locally — no external headers needed.

```
insn  0       BPF_MOV64_REG(r6, r1)
              Save the socket-buffer context pointer (r1) in the callee-saved
              register r6.  Socket-filter programs receive a pointer to the
              sk_buff in r1 on entry.

insn  1–2     BPF_LD_MAP_FD(r1, map_fd)
              Wide (two-instruction, 128-bit) immediate load.  The verifier
              resolves the fd to a kernel map pointer at load time.
              BPF_PSEUDO_MAP_FD in src_reg signals "this imm is a map fd".

insn  3       BPF_MOV64_REG(r2, r10)
              r10 is the read-only frame pointer.  Copy it so we can offset it.

insn  4       BPF_ALU64_IMM(ADD, r2, -4)
              r2 = address of 4 bytes below the stack top (our key slot).

insn  5       BPF_ST_MEM(W, r10, -4, 0)
              Write u32(0) onto the stack — this is the map key (index 0).

insn  6       BPF_EMIT_CALL(BPF_FUNC_map_lookup_elem)
              Helper call: r0 = map_lookup_elem(r1=map, r2=&key).
              Returns a pointer to the value slot, or NULL if not found.

insn  7       BPF_JMP_IMM(JEQ, r0, 0, 5)
              NULL guard: if the pointer is NULL (should not happen for
              a pre-allocated ARRAY), jump past the increment to insn 13.

insn  8       BPF_MOV64_REG(r1, r0)
              r1 = value pointer (scratch r0 is about to be overwritten).

insn  9       BPF_LDX_MEM(DW, r2, r1, 0)
              r2 = *(u64 *)(r1 + 0)  — load current counter.

insn 10       BPF_ALU64_IMM(ADD, r2, 1)
              r2 += 1

insn 11       BPF_STX_MEM(DW, r1, r2, 0)
              *(u64 *)(r1 + 0) = r2  — write back atomically through the
              pointer we obtained from the helper (this is safe; the verifier
              tracks the pointer type).

insn 12       BPF_MOV64_IMM(r0, 1)
              Return value 1 = SK_PASS (accept the packet).

insn 13       BPF_EXIT_INSN()
              Return to the kernel.
```

## 5. Kernel Path

`syscall(SYS_bpf, BPF_PROG_LOAD, …)` enters the kernel at `__sys_bpf()`:

https://elixir.bootlin.com/linux/v6.9/source/kernel/bpf/syscall.c

`bpf_prog_load()` allocates a `struct bpf_prog`, copies the instructions,
then calls `bpf_check()` — the verifier:

https://elixir.bootlin.com/linux/v6.9/source/kernel/bpf/verifier.c

The verifier performs type-safety analysis (register state, pointer
dereference bounds, helper argument types) and, on success, `bpf_prog_select_runtime()`
JIT-compiles the bytecode to native x86-64.  The resulting native code is
what actually runs on each received packet.

`BPF_MAP_CREATE` follows a similar path through `map_create()` in syscall.c,
which calls `bpf_map_alloc()` with the appropriate map operations vtable
(`array_map_ops` for `BPF_MAP_TYPE_ARRAY`).

## 6. K8s Connection

Cilium (the eBPF-native CNI used by Google GKE, AWS EKS managed add-on, and
many self-hosted clusters) uses exactly this same `BPF_PROG_LOAD` +
`BPF_MAP_CREATE` pattern — at scale:

- **Per-endpoint programs**: for every Pod added to a node, Cilium generates
  a `BPF_PROG_TYPE_SCHED_CLS` (tc) or `BPF_PROG_TYPE_XDP` program and loads
  it with `BPF_PROG_LOAD`.  The bytecode encodes the Pod's security policy and
  Service load-balancing rules.

- **Pinned maps**: Cilium's global maps (e.g., the per-Service LB table, the
  endpoint identity map) are pinned under `/sys/fs/bpf/tc/globals/` using
  `BPF_OBJ_PIN`.  All programs on a node share these maps by opening the
  pinned path — the same `BPF_MAP_LOOKUP_ELEM` / `BPF_MAP_UPDATE_ELEM`
  syscalls used in this exercise.

- **Tail calls**: Cilium chains multiple BPF programs via
  `BPF_MAP_TYPE_PROG_ARRAY` to keep individual programs within the verifier's
  instruction limit.

In short, the four syscall commands exercised here (`BPF_MAP_CREATE`,
`BPF_PROG_LOAD`, `BPF_MAP_UPDATE_ELEM`, `BPF_MAP_LOOKUP_ELEM`) are the
atomic building blocks of the entire Cilium dataplane.

## 7. Exercises

**(a) Drop all packets**

Change `BPF_MOV64_IMM(BPF_REG_0, 1)` at insn 12 to `BPF_MOV64_IMM(BPF_REG_0, 0)`.
Recompile and run.  The `recv()` call will block indefinitely because the
filter drops every packet (SK_DROP = 0).  This shows that the BPF return
value gates packet delivery to userspace.

**(b) Count into a second slot**

Increase `max_entries` to 16 and add a second counter in `map[1]`.  After the
`map_lookup_elem` helper call, jump to a second block that repeats the
load-increment-store sequence for key=1.  Add a second `BPF_MAP_LOOKUP_ELEM`
call in userspace to read both counters after sending two packets.

**(c) Inspect the loaded program**

After `BPF_PROG_LOAD` returns `prog_fd`, in a second terminal run:

```
sudo bpftool prog show
```

Find the program tagged `pkt_count_sk`.  Then inspect its fdinfo:

```
cat /proc/<pid>/fdinfo/<prog_fd>
```

You will see the `prog_type`, `prog_tag` (an 8-byte hash of the translated
bytecode), and the `memlock` size.  The `prog_tag` is stable across identical
programs and is used by `bpftool` for deduplication.
