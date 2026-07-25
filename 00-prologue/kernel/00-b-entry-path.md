# 00-b — The Kernel Entry Path: `entry_SYSCALL_64`

> **Source files:**
> - [`arch/x86/entry/entry_64.S`](https://elixir.bootlin.com/linux/v6.9/source/arch/x86/entry/entry_64.S)
> - [`arch/x86/entry/common.c`](https://elixir.bootlin.com/linux/v6.9/source/arch/x86/entry/common.c)

When a userspace program calls a library function like `clone()`, the C library
(glibc or musl) eventually executes the `syscall` instruction. From that moment the
CPU is in kernel mode and the Linux entry path takes over. This document traces every
step from the `syscall` instruction to the C function that implements the syscall,
and back to userspace.

This path matters for Kubernetes because:
- seccomp filters (used by runc/containerd to restrict container syscalls) are applied
  here — understanding the entry path tells you *exactly where* the filter check runs.
- The audit subsystem (used for compliance logging of container activity) hooks here.
- Performance profilers like `perf` use hardware-assisted tracing at this boundary.

---

## 1. What Happens at the `syscall` Instruction

### CPU mode switch

On x86-64, the `syscall` instruction does the following in hardware (no kernel code
involved yet):

1. Saves the return address in `rcx` (the address of the instruction *after* `syscall`).
2. Saves the CPU flags in `r11`.
3. Clears certain flag bits (disables interrupts via `RFLAGS.IF = 0`).
4. Loads the new code segment selector from `IA32_STAR` MSR.
5. Loads the new instruction pointer from `IA32_LSTAR` MSR.

The value in `IA32_LSTAR` is set during kernel boot to the address of `entry_SYSCALL_64`.
You can see this in
[`arch/x86/kernel/cpu/common.c`](https://elixir.bootlin.com/linux/v6.9/source/arch/x86/kernel/cpu/common.c)
at the call to `wrmsrl(MSR_LSTAR, (unsigned long)entry_SYSCALL_64)`.

### Register convention at entry

By the time `entry_SYSCALL_64` runs, the registers carry:

| Register | Contents |
|----------|----------|
| `rax` | syscall number (e.g. 56 for `clone`, 435 for `clone3`) |
| `rdi` | argument 1 |
| `rsi` | argument 2 |
| `rdx` | argument 3 |
| `r10` | argument 4 (note: user-ABI uses `rcx` here, but `syscall` clobbers `rcx`) |
| `r8` | argument 5 |
| `r9` | argument 6 |
| `rcx` | return address (saved by `syscall` instruction) |
| `r11` | saved RFLAGS |
| `rsp` | **still the userspace stack pointer** |

---

## 2. Walking Through `entry_SYSCALL_64`

The full assembly is in
[`arch/x86/entry/entry_64.S`](https://elixir.bootlin.com/linux/v6.9/source/arch/x86/entry/entry_64.S).
Below is an annotated walk-through of the critical sections.

### Step 1: `swapgs` — switch the GS base register

```asm
/* entry_64.S line ~87 (Linux 6.9) */
SYM_CODE_START(entry_SYSCALL_64)
    UNWIND_HINT_ENTRY
    ENDBR
    swapgs
```

`swapgs` swaps the value in the `GS` segment base register with the value stored in
`IA32_KERNEL_GS_BASE` MSR. After `swapgs`, `GS` points to the kernel's per-CPU data
area (`struct cpu_entry_area`). This is how the kernel finds per-CPU variables without
using a stack (which it cannot trust yet, since `rsp` still points to userspace).

This is also the point exploited by Spectre/Meltdown mitigations — `IBRS` may be
set here on vulnerable CPUs.

### Step 2: Switch to the kernel stack

```asm
    /* Save user rsp and load kernel rsp */
    movq    %rsp, PER_CPU_VAR(cpu_tss_rw + TSS_sp2)
    SWITCH_TO_KERNEL_CR3 scratch_reg=%rsp
    movq    PER_CPU_VAR(cpu_current_top_of_stack), %rsp
```

The kernel stack pointer lives in the per-CPU `cpu_current_top_of_stack` variable
(part of `struct tss_struct`). After this, `rsp` points to a kernel stack and it
is safe to push registers.

### Step 3: Save all user registers onto the kernel stack

```asm
    /* Construct struct pt_regs on the kernel stack */
    pushq   $__USER_DS                  /* ss */
    pushq   PER_CPU_VAR(cpu_tss_rw + TSS_sp2)  /* rsp (saved above) */
    pushq   %r11                        /* rflags */
    pushq   $__USER_CS                  /* cs */
    pushq   %rcx                        /* rip (return address) */
    pushq   %rax                        /* orig_ax — syscall number */
    pushq   %rdi
    pushq   %rsi
    pushq   %rdx
    pushq   %rcx       /* saved rcx = return address */
    pushq   $-ENOSYS   /* ax (placeholder, will be overwritten by return value) */
    pushq   %r8
    pushq   %r9
    pushq   %r10
    pushq   %r11
    PUSH_AND_CLEAR_REGS rax=$-ENOSYS
```

These pushes construct a
[`struct pt_regs`](https://elixir.bootlin.com/linux/v6.9/source/arch/x86/include/asm/ptrace.h#L56)
on the kernel stack. Every syscall implementation in C receives a pointer to this
structure as its single argument (because of `CONFIG_ARCH_HAS_SYSCALL_WRAPPER`).

`orig_ax` stores the original syscall number because the syscall return value
overwrites `ax`. ptrace and audit need the original number after the call returns.

### Step 4: Check whether we should trace / are in the fast path

```asm
    /* entry_64.S: check TIF_SYSCALL_WORK flags */
    movq    PER_CPU_VAR(current_task), %r11
    testl   $_TIF_WORK_SYSCALL_ENTRY, TASK_TI_flags(%r11)
    jnz     .Lsyscall_work_entry
```

`_TIF_WORK_SYSCALL_ENTRY` is a bitmask of "work flags" that includes:
- `TIF_SYSCALL_TRACE` — ptrace is attached
- `TIF_SYSCALL_AUDIT` — audit is active
- `TIF_SECCOMP` — a seccomp filter is installed
- `TIF_SYSCALL_TRACEPOINT` — a tracing tracepoint is active

If any of these flags are set, execution jumps to `syscall_work_entry` which calls
`syscall_enter_from_user_mode_work()` before dispatching the syscall.
If none are set, the fast path dispatches directly.

### Step 5: Call `do_syscall_64`

```asm
    /* Fast path: call do_syscall_64 */
    call    do_syscall_64
```

The `pt_regs` pointer is in `rdi` (first argument per the x86-64 C ABI).

---

## 3. `do_syscall_64()` — The C Dispatcher

Source:
[`arch/x86/entry/common.c`](https://elixir.bootlin.com/linux/v6.9/source/arch/x86/entry/common.c)

```c
/* arch/x86/entry/common.c (Linux 6.9, annotations added) */
__visible noinstr void do_syscall_64(struct pt_regs *regs, int nr)
{
    /*
     * syscall_enter_from_user_mode() runs the full "slow path" checks:
     *   - audit logging
     *   - seccomp filter evaluation (BPF program)
     *   - ptrace notification (PTRACE_SYSCALL)
     *   - tracepoint: syscalls:sys_enter_<name>
     */
    add_random_kstack_offset();
    nr = syscall_enter_from_user_mode(regs, nr);

    /*
     * Dispatch: index into sys_call_table.
     * NR_syscalls is the compile-time count.
     * array_index_nospec is the Spectre v1 mitigation.
     */
    if (likely(nr < NR_syscalls)) {
        nr = array_index_nospec(nr, NR_syscalls);
        regs->ax = sys_call_table[nr](regs);
    }

    /*
     * Return path: signal delivery, seccomp return value override,
     * ptrace notification, tracepoint: syscalls:sys_exit_<name>
     */
    syscall_exit_to_user_mode(regs);
}
```

### `syscall_enter_from_user_mode()`

This function (in
[`kernel/entry/common.c`](https://elixir.bootlin.com/linux/v6.9/source/kernel/entry/common.c))
is architecture-independent. It calls `syscall_enter_from_user_mode_work()` which
loops over all set work flags:

```c
/* kernel/entry/common.c (Linux 6.9) */
static long syscall_enter_from_user_mode_work(struct pt_regs *regs, long syscall)
{
    u32 work = READ_ONCE(current_thread_info()->syscall_work);

    if (work & SYSCALL_WORK_SECCOMP)
        syscall = __seccomp_filter(syscall, regs, false);

    if (work & SYSCALL_WORK_SYSCALL_AUDIT)
        audit_syscall_entry(...);

    if (work & SYSCALL_WORK_SYSCALL_TRACEPOINT)
        trace_sys_enter(regs, syscall);

    ...
    return syscall;
}
```

Key observation: `__seccomp_filter()` runs *before* the syscall is dispatched.
If the BPF seccomp program returns `SECCOMP_RET_KILL_THREAD`, the thread is killed
before `sys_call_table[nr]()` is ever called.

### `add_random_kstack_offset()`

This call inserts a random offset into the kernel stack before each syscall — a
mitigation against stack-based side-channel attacks (specifically KASLR bypass via
stack spray). The offset is cleaned up on return.

---

## 4. The Return Path

After `sys_call_table[nr](regs)` returns, the return value is in `regs->ax`.
Control passes to `syscall_exit_to_user_mode()`.

### `syscall_exit_to_user_mode()`

Source:
[`kernel/entry/common.c`](https://elixir.bootlin.com/linux/v6.9/source/kernel/entry/common.c)

```c
/* kernel/entry/common.c (Linux 6.9, simplified) */
void syscall_exit_to_user_mode(struct pt_regs *regs)
{
    /*
     * This is the last chance to deliver signals before returning to
     * userspace. If a signal is pending, handle_signal() is called here.
     * This means signal handlers run between the syscall return and the
     * actual sysret instruction.
     */
    syscall_exit_to_user_mode_work(regs);
    __exit_to_user_mode();
}
```

`syscall_exit_to_user_mode_work()` handles:
- **Signal delivery** — `TIF_SIGPENDING`: calls `do_signal()` to deliver any pending
  signals to the process. This is why signals are "asynchronous" from the application's
  perspective but actually synchronous at the kernel boundary.
- **TIF_NEED_RESCHED** — the scheduler preemption flag; if set, `schedule()` is called
  here so the kernel can switch to a higher-priority task.
- **Seccomp return value override** — `SECCOMP_RET_ERRNO` can override the syscall's
  return value even after it has completed.
- **Exit tracepoint** — `trace_sys_exit(regs, regs->ax)` fires the
  `syscalls:sys_exit_<name>` tracepoint.
- **Ptrace notification** — `PTRACE_SYSCALL` stop fires here on exit.

### Back to assembly: `sysret`

After `syscall_exit_to_user_mode()` returns, `entry_64.S` restores registers from
`struct pt_regs` and executes `sysretq` (or `iretq` for the slow path):

```asm
    /* Restore registers from pt_regs and return to userspace */
    POP_REGS pop_rdi=0
    ...
    swapgs
    sysretq         /* loads rip from rcx, rflags from r11, switches back to CPL3 */
```

`swapgs` restores the userspace GS base. `sysretq` is the fast return path — it is
symmetric to `syscall`: it loads `rip` from `rcx` and `rflags` from `r11`.

---

## 5. Why This Matters for Containers

### seccomp filters intercept here

When runc configures a container, it installs a seccomp BPF program via `prctl(2)` /
`seccomp(2)` before executing the container entrypoint. After that call, the
`TIF_SECCOMP` flag is set in the thread's `thread_info`. From that point:

- Every syscall the container process makes goes through `syscall_enter_from_user_mode_work()`
- `__seccomp_filter()` evaluates the BPF program against the syscall number and arguments
- If the container tries to call `mount()` and the seccomp profile says `SCMP_ACT_ERRNO`
  for `mount`, the kernel returns `-EPERM` without ever calling `sys_mount()`

The BPF program loaded by `seccomp(2)` is evaluated by the kernel's classic BPF
(cBPF) interpreter — not eBPF. The program receives a `struct seccomp_data` as input:

```c
/* include/uapi/linux/seccomp.h */
struct seccomp_data {
    int   nr;          /* syscall number */
    __u32 arch;        /* AUDIT_ARCH_X86_64 */
    __u64 instruction_pointer;  /* rip at time of syscall */
    __u64 args[6];     /* rdi, rsi, rdx, r10, r8, r9 */
};
```

Container runtimes (runc, crun, gVisor) generate a seccomp profile from a JSON
specification (defined in the OCI Runtime Spec) and load it here. The default Docker
and Kubernetes seccomp profiles block ~40 dangerous syscalls including
`reboot`, `kexec_load`, `ptrace`, `acct`, and `modify_ldt`.

### ptrace — how `kubectl exec` and `strace` work

When you run `strace -p <pid>` on a container process, it calls `ptrace(PTRACE_ATTACH)`.
This sets `TIF_SYSCALL_TRACE` on the target thread. From then on, every syscall made
by that thread causes it to stop at `syscall_enter_from_user_mode_work()`, allowing
the tracer (strace) to inspect the registers. The same mechanism is used by
`kubectl exec` to inject a process into a running container.

Note: many production seccomp profiles block `ptrace`, which is why `strace` fails
on containers with strict profiles. If you hit `Operation not permitted` when tracing
a container, check the seccomp profile.

### Audit hooks — container syscall logging

The `audit_syscall_entry()` call in `syscall_enter_from_user_mode_work()` is how the
Linux audit subsystem (used for STIG compliance, PCI-DSS logging, etc.) records that a
process made a syscall. When the container runtime or the kubelet has audit rules
configured (via `auditctl`), every `clone3`, `setns`, or `pivot_root` call is logged
to the audit ring buffer and eventually to `/var/log/audit/audit.log`.

### Tracepoints — bpftrace and perf

The `trace_sys_enter` and `trace_sys_exit` calls insert stable tracepoints at the
syscall boundary. These are the `syscalls:sys_enter_*` and `syscalls:sys_exit_*`
events you see in `perf list` and `bpftrace`. They are architecture-independent hooks
that eBPF programs can attach to, which is the foundation of the observability work in
Chapter 07.

---

## Summary

```
userspace: syscall instruction
    │
    │ hardware: save rip→rcx, rflags→r11, load lstar
    ▼
entry_SYSCALL_64 (assembly)
    ├── swapgs            (switch to kernel GS / per-CPU data)
    ├── switch rsp        (load kernel stack)
    ├── push all regs     (build struct pt_regs)
    └── call do_syscall_64
            │
            ├── syscall_enter_from_user_mode()
            │       ├── seccomp filter (BPF program) ← runc installs this
            │       ├── audit_syscall_entry()
            │       └── tracepoint: sys_enter_<name>
            │
            ├── sys_call_table[nr](regs) ← actual syscall runs here
            │
            └── syscall_exit_to_user_mode()
                    ├── seccomp return-value override
                    ├── signal delivery (do_signal)
                    ├── scheduler preemption check
                    └── tracepoint: sys_exit_<name>
    │
    ├── pop all regs
    ├── swapgs
    └── sysretq           (return to userspace)
```

The key insight for containers: the seccomp filter runs at the `syscall_enter` point,
before the syscall implementation is ever called. This makes it an extremely low-cost
policy enforcement mechanism — a blocked syscall costs only the overhead of the BPF
program evaluation, not the cost of the syscall itself.

**Next:** [k8s/00-k8s-connection.md](../k8s/00-k8s-connection.md) — tracing `kubectl run` all the way to `clone()`.
