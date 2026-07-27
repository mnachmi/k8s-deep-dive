# The Kernel Entry Path — Kernel Deep Dive

## Source Locations

| File | Link |
|------|------|
| `arch/x86/entry/entry_64.S` | https://elixir.bootlin.com/linux/v6.9/source/arch/x86/entry/entry_64.S |
| `arch/x86/entry/common.c` | https://elixir.bootlin.com/linux/v6.9/source/arch/x86/entry/common.c |
| `kernel/entry/common.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/entry/common.c |

## The Most Dangerous Moment in Computing

Every kernel security vulnerability in history has one thing in common: the attacker found a way to abuse the kernel's privileges from unprivileged code. The x86 processor's ring model is supposed to prevent this — Ring 3 code cannot execute privileged instructions, cannot modify page tables, cannot touch hardware directly. But user processes need to do all of those things indirectly, through the kernel. The moment of transition from Ring 3 to Ring 0 is therefore the most security-critical operation the processor performs.

Get it wrong — even for one instruction — and privilege escalation is possible. The kernel entry path exists to perform this transition correctly, with appropriate security checks, minimal privilege exposure, and correct isolation between contexts.

## A Brief History: int 0x80 to syscall

The original Linux syscall interface on 32-bit x86 used software interrupt 0x80. When a process called `int 0x80`, the processor would consult the Interrupt Descriptor Table (IDT), find the gate for vector 128, perform a full privilege-level switch (pushing CS, EIP, EFLAGS, and the old stack pointer onto the kernel stack), and jump to the registered handler. The old userspace stack pointer was saved automatically by the processor.

This worked, but it was expensive. A software interrupt involves the processor walking the IDT, performing multiple permission checks on the gate descriptor, pushing a hardware-defined interrupt frame, and returning via `iret` — which pops the full frame. On a 500 MHz Pentium II, a syscall via `int 0x80` cost around 200 nanoseconds. At 10,000 syscalls per second (a reasonable rate for a web server), that is 2 milliseconds per second of CPU time spent just on mode switching.

Intel introduced `sysenter`/`sysexit` as a faster alternative in the Pentium II era. Rather than looking up a gate in the IDT, `sysenter` reads the kernel entry address directly from an MSR (`MSR_SYSENTER_EIP`). The mode switch is stripped down to the minimum: no IDT lookup, no permission checks on a gate descriptor. The problem was that AMD did not implement `sysenter` in AMD64 (their 64-bit architecture). Instead, AMD introduced `syscall`/`sysret`, which achieves the same goal using different MSRs. Intel later added `syscall` support as well, and it became the standard for 64-bit x86.

On x86-64 Linux, `int 0x80` still works for 32-bit compatibility but all 64-bit code uses `syscall`. The kernel sets up the entry point once during boot:

```c
// arch/x86/kernel/cpu/common.c
wrmsrl(MSR_LSTAR, (unsigned long)entry_SYSCALL_64);
```

`IA32_LSTAR` (Long System Target Address Register) holds the 64-bit kernel entry point. From this moment on, every `syscall` instruction executed by any 64-bit process on this CPU jumps to `entry_SYSCALL_64`.

## What the Processor Does Before Any Kernel Code Runs

The `syscall` instruction itself is not just a jump. It performs a precisely-defined set of operations in hardware, in this order:

1. **Saves the return address.** The address of the instruction after `syscall` is placed in `rcx`. This is the address the kernel must restore to `rip` before returning to userspace.

2. **Saves the CPU flags.** The current `RFLAGS` value is saved in `r11`.

3. **Clears flag bits.** The processor clears certain flags from `RFLAGS` — most importantly, it clears the Interrupt Flag (IF), which disables hardware interrupts. This ensures the processor is not interrupted in the tiny window before the kernel has switched to the kernel stack.

4. **Loads the code segment.** A new Code Segment (CS) selector is loaded from `IA32_STAR` MSR bits 32–47. This switches to Ring 0.

5. **Loads the instruction pointer.** The new `RIP` is loaded from `IA32_LSTAR` — the address of `entry_SYSCALL_64`.

Notice what `syscall` does *not* do: it does not save the stack pointer. At the moment `entry_SYSCALL_64` begins executing, `rsp` still points to the userspace stack. The first job of the kernel's entry assembly is to switch to a safe kernel stack before doing anything else.

The register state at the moment `entry_SYSCALL_64` runs:

| Register | Contents |
|----------|----------|
| `rax` | syscall number |
| `rdi` | argument 1 |
| `rsi` | argument 2 |
| `rdx` | argument 3 |
| `r10` | argument 4 (note: user ABI uses `rcx` for arg 4 but `syscall` clobbers `rcx`) |
| `r8` | argument 5 |
| `r9` | argument 6 |
| `rcx` | return address (saved by `syscall`) |
| `r11` | saved RFLAGS (saved by `syscall`) |
| `rsp` | **userspace stack pointer — not yet switched** |

## Walking entry_SYSCALL_64

The full assembly is in `arch/x86/entry/entry_64.S`. What follows is an annotated tour of the critical sections. This is not simplified pseudo-code — this is exactly what the CPU executes on every syscall.

### swapgs: Gaining Access to Per-CPU Data

```asm
SYM_CODE_START(entry_SYSCALL_64)
    UNWIND_HINT_ENTRY
    ENDBR          /* indirect branch tracking (CET) */
    swapgs
```

`swapgs` swaps the contents of the `GS` segment base register with the value in the `IA32_KERNEL_GS_BASE` MSR. In userspace, the `GS` register is used for thread-local storage (the C runtime stores the `pthread_self()` pointer there). In the kernel, `GS` must point to the per-CPU data area (`struct cpu_entry_area`) so that per-CPU variables can be accessed by offset.

Before `swapgs`, `GS` points to userspace data — the kernel cannot use it. After `swapgs`, `GS` points to the kernel's per-CPU area. Now the kernel can read per-CPU variables like `current_task` and `cpu_current_top_of_stack` using the `PER_CPU_VAR()` macro.

`swapgs` is also one of the most security-sensitive instructions in the entry path. The [Spectre SWAPGS vulnerability (CVE-2019-1125)](https://access.redhat.com/security/cve/cve-2019-1125) allowed userspace code to speculatively access kernel memory by exploiting the fact that the processor might speculatively execute instructions after `swapgs` without actually performing the swap. The kernel's mitigation involves `LFENCE` instructions and, on vulnerable CPUs, use of `IBRS` to limit speculative execution.

### Switching to the Kernel Stack

```asm
    movq    %rsp, PER_CPU_VAR(cpu_tss_rw + TSS_sp2)
    SWITCH_TO_KERNEL_CR3 scratch_reg=%rsp
    movq    PER_CPU_VAR(cpu_current_top_of_stack), %rsp
```

Three things happen here:

1. **Save the userspace stack pointer.** The current `rsp` (pointing to userspace memory) is saved into `TSS_sp2` — a scratch slot in the per-CPU Task State Segment. It is saved here rather than on the stack because there is no stack yet.

2. **Switch the CR3 register.** `SWITCH_TO_KERNEL_CR3` loads the kernel's page table root into `CR3` if Kernel Page-Table Isolation (KPTI) is enabled. KPTI was the kernel's primary mitigation for the Meltdown vulnerability (CVE-2017-5754), discovered in January 2018. Meltdown allowed userspace code to read arbitrary kernel memory by exploiting speculative execution. KPTI's solution: give userspace a minimal page table that maps only the syscall entry trampoline and nothing else. On every syscall entry, the full kernel page tables are loaded; on every return, they are stripped back. On processors without Meltdown (most post-2018 Intel/AMD), KPTI is either not needed or disabled at runtime.

3. **Load the kernel stack pointer.** The per-CPU variable `cpu_current_top_of_stack` contains the top of the current task's kernel stack. Now `rsp` points to a safe kernel stack and we can push registers.

### Building struct pt_regs

```asm
    pushq   $__USER_DS                              /* ss */
    pushq   PER_CPU_VAR(cpu_tss_rw + TSS_sp2)      /* rsp (userspace) */
    pushq   %r11                                    /* rflags */
    pushq   $__USER_CS                              /* cs */
    pushq   %rcx                                    /* rip (return address) */
    pushq   %rax                                    /* orig_ax = syscall number */
    PUSH_AND_CLEAR_REGS rax=$-ENOSYS
```

These pushes construct a `struct pt_regs` on the kernel stack. `struct pt_regs` is the kernel's representation of a saved register state — the complete set of CPU registers at the moment the process entered the kernel. Every syscall implementation receives a pointer to this structure as its single argument.

The `orig_ax` field (which holds the syscall number) is stored separately from `ax` because the syscall return value will later overwrite `ax`. When ptrace or audit needs to know which syscall was called after it returns, `orig_ax` is the authoritative record. This is why `strace` can report both the syscall name and its return value.

`PUSH_AND_CLEAR_REGS` pushes the remaining general-purpose registers and then *zeros* them. Zeroing prevents a subtle information-leaking attack: if the kernel's syscall implementation uses a register that was not explicitly initialized, without the clear it would contain whatever the userspace caller happened to have there — leaking that value to any kernel subsystem that reads the register. This defense-in-depth measure was added as part of the broader Spectre/Meltdown hardening in 2018.

### The Work Flags Check

```asm
    movq    PER_CPU_VAR(current_task), %r11
    testl   $_TIF_WORK_SYSCALL_ENTRY, TASK_TI_flags(%r11)
    jnz     .Lsyscall_work_entry
```

This is the "fast path vs slow path" decision. The kernel checks whether the current task has any "work flags" set in its `thread_info`:

- `TIF_SYSCALL_TRACE` — a ptrace tracer is attached
- `TIF_SYSCALL_AUDIT` — audit logging is active
- `TIF_SECCOMP` — a seccomp filter is installed
- `TIF_SYSCALL_TRACEPOINT` — a tracepoint-based observer is active

If none of these flags are set, execution falls through to the fast dispatch path. If any are set, execution jumps to `.Lsyscall_work_entry` which calls the full slow-path handler. The branch prediction is always trained toward the fast path — syscall tracing is the uncommon case.

The fast path then calls `do_syscall_64()` directly.

## do_syscall_64() — The C Dispatcher

After the assembly work is done, control reaches `do_syscall_64()` in `arch/x86/entry/common.c`. This is where the syscall number becomes a function call:

```c
// arch/x86/entry/common.c (Linux 6.9)
__visible noinstr void do_syscall_64(struct pt_regs *regs, int nr)
{
    add_random_kstack_offset();
    nr = syscall_enter_from_user_mode(regs, nr);

    if (likely(nr < NR_syscalls)) {
        nr = array_index_nospec(nr, NR_syscalls);
        regs->ax = sys_call_table[nr](regs);
    }

    syscall_exit_to_user_mode(regs);
}
```

### add_random_kstack_offset()

This call adds a random offset to the kernel stack pointer. It was added in Linux 5.13 as a mitigation against stack-based side-channel attacks. Some Spectre-class vulnerabilities allow an attacker to infer the kernel stack layout by observing cache timing. By randomizing the stack position on every syscall, the layout is different each time, making statistical attacks much harder. The offset is cleaned up on the return path.

### syscall_enter_from_user_mode()

This function (in the architecture-independent `kernel/entry/common.c`) handles the slow path for all the work flags:

```c
// kernel/entry/common.c (Linux 6.9, simplified)
static long syscall_enter_from_user_mode_work(struct pt_regs *regs, long syscall)
{
    u32 work = READ_ONCE(current_thread_info()->syscall_work);

    if (work & SYSCALL_WORK_SECCOMP)
        syscall = __seccomp_filter(syscall, regs, false);

    if (work & SYSCALL_WORK_SYSCALL_AUDIT)
        audit_syscall_entry(syscall, regs->di, regs->si, regs->dx, regs->r10);

    if (work & SYSCALL_WORK_SYSCALL_TRACEPOINT)
        trace_sys_enter(regs, syscall);

    if (work & SYSCALL_WORK_SYSCALL_TRACE)
        ptrace_report_syscall_entry(regs);

    return syscall;
}
```

**The seccomp path** is what container security is built on. When runc creates a container, it calls `prctl(PR_SET_SECCOMP, SECCOMP_MODE_FILTER, ...)` to install a BPF program. From that point, every syscall the container process makes runs through `__seccomp_filter()`. The BPF program receives a `struct seccomp_data` containing the syscall number and arguments, evaluates its rules, and returns one of several actions: `SECCOMP_RET_ALLOW`, `SECCOMP_RET_ERRNO` (return a fake error), `SECCOMP_RET_KILL_THREAD` (kill the thread), or `SECCOMP_RET_TRAP` (send a signal to the process). Critically, this check runs *before* `sys_call_table[nr]()` is ever invoked. A blocked syscall costs only the BPF evaluation, not the syscall itself.

**The audit path** records every syscall for compliance logging. When the Linux audit daemon (`auditd`) is active with rules that match a process, `audit_syscall_entry()` writes a record to the audit ring buffer. This is how STIG-compliant Kubernetes environments log every `clone3`, `mount`, and `pivot_root` call from container processes.

**The ptrace path** is how `strace` and `kubectl exec` work. When a tracer calls `ptrace(PTRACE_SYSCALL)` on a process, `TIF_SYSCALL_TRACE` is set. `ptrace_report_syscall_entry()` sends `SIGTRAP` to the traced process, which causes it to stop. The tracer can then read the stopped process's registers (including the syscall number and arguments) via `/proc/<pid>/mem` or `ptrace(PTRACE_GETREGS)`. When the tracer resumes the process, it eventually calls `syscall_exit_to_user_mode()` which sends another `SIGTRAP` so the tracer can see the return value.

### array_index_nospec()

```c
nr = array_index_nospec(nr, NR_syscalls);
regs->ax = sys_call_table[nr](regs);
```

`array_index_nospec` is the Spectre v1 mitigation. The vulnerability works like this: if the branch predictor predicts that `nr < NR_syscalls` is true, the processor may speculatively execute `sys_call_table[nr](regs)` before the comparison is confirmed. If `nr` was crafted to be out of bounds, the speculative execution accesses memory beyond `sys_call_table[]`. Even though this access is never architecturally committed (the branch prediction was wrong), it leaves traces in the cache that an attacker can measure. `array_index_nospec` uses a bitmask operation that clamps `nr` without a conditional branch, eliminating the speculation opportunity.

## The Return Path

After `sys_call_table[nr](regs)` returns, the return value is in `regs->ax`. Control passes to `syscall_exit_to_user_mode()`:

```c
// kernel/entry/common.c (Linux 6.9, simplified)
void syscall_exit_to_user_mode(struct pt_regs *regs)
{
    syscall_exit_to_user_mode_work(regs);
    __exit_to_user_mode();
}
```

`syscall_exit_to_user_mode_work()` handles the exit-side work:

**Signal delivery.** If `TIF_SIGPENDING` is set, `do_signal()` is called to deliver any pending signals. This is the mechanism behind Unix's signal model: signals appear "asynchronous" to the receiving process, but they are actually delivered synchronously at the kernel/userspace boundary — either on syscall return or on interrupt return. A signal sent to a container process while it is blocked in `epoll_wait()` causes the syscall to return `-EINTR`, and then the signal handler runs on the way back to userspace.

**Scheduler preemption.** If `TIF_NEED_RESCHED` is set, `schedule()` is called. This is what makes Linux preemptive at the syscall boundary: even if a process has a long-running syscall, the kernel can yield the CPU to a higher-priority process when the syscall completes.

**Seccomp return-value override.** `SECCOMP_RET_ERRNO` can modify the return value even after the syscall has completed. This is used by sandboxing tools to make a syscall appear to fail without actually running it.

**Tracepoint firing.** `trace_sys_exit(regs, regs->ax)` fires the architecture-independent `syscalls:sys_exit_<name>` tracepoint. This is what tools like `bpftrace` and `perf` use when you attach to a `syscalls:sys_exit_*` event.

Back in the assembly, after `do_syscall_64()` returns:

```asm
    POP_REGS pop_rdi=0
    /* Restore all registers from pt_regs */
    swapgs
    sysretq
```

`swapgs` restores the userspace `GS` base. `sysretq` is the symmetric counterpart to `syscall`: it loads `rip` from `rcx` (where the return address was saved), loads `rflags` from `r11`, and switches the processor back to Ring 3. The userspace process resumes exactly where it called `syscall`.

## Why This Matters for Containers

The entry path is not academic machinery — it is the enforcement point for every container security mechanism:

**seccomp filters are applied here, before the syscall runs.** When runc creates a container with a seccomp profile (the default `RuntimeDefault` profile or a custom one), the profile is a BPF program loaded via `prctl(PR_SET_SECCOMP)`. From that point, every syscall the container makes hits `__seccomp_filter()` in `syscall_enter_from_user_mode_work()`. If the profile blocks `mount`, the container cannot call `mount` — not because the filesystem check rejects it, but because `sys_mount` is never called. This is why seccomp is efficient: policy evaluation happens before any kernel state is modified.

**ptrace and `kubectl exec` pass through here.** When you run `kubectl exec -it pod-name -- /bin/sh`, the container runtime calls `ptrace(PTRACE_ATTACH)` on the shell process. This sets `TIF_SYSCALL_TRACE`. Every syscall the shell makes then generates a ptrace stop event. This is also why many production seccomp profiles block `ptrace` — allowing a container process to ptrace another undermines the isolation model.

**Audit logs start here.** If you have `auditd` rules matching container processes, every `clone3`, `setns`, and `pivot_root` call is logged. The audit record includes the syscall number, arguments, process identity (uid, pid, comm), and timestamp. This is the kernel-level mechanism behind cloud providers' audit trail features.

**Spectre/Meltdown hardening is here.** The KPTI page-table switch, the `swapgs` timing protection, the random kernel stack offset, and the `array_index_nospec` call all live in the entry path. Every kernel that needs to be secure against speculative execution attacks must get this path right.

## The Complete Entry Path Diagram

```
userspace: syscall instruction
    │
    │ hardware: rip→rcx, rflags→r11, load LSTAR→rip, switch to Ring 0
    ▼
entry_SYSCALL_64 (arch/x86/entry/entry_64.S)
    │
    ├── swapgs           — switch GS: userspace TLS → kernel per-CPU area
    ├── save %rsp        — save userspace stack pointer to TSS_sp2
    ├── SWITCH_TO_KERNEL_CR3  — load full kernel page tables (KPTI)
    ├── load kernel %rsp — switch to task's kernel stack
    ├── push pt_regs     — save all user registers (build struct pt_regs)
    ├── clear regs       — zero registers to prevent info leak
    └── check TIF_WORK_SYSCALL_ENTRY flags
           │
           ├─[no work flags] → call do_syscall_64()
           └─[work flags set] → .Lsyscall_work_entry → do_syscall_64()

do_syscall_64() (arch/x86/entry/common.c)
    │
    ├── add_random_kstack_offset()  — Spectre stack-spray mitigation
    ├── syscall_enter_from_user_mode()
    │       ├── __seccomp_filter()      — evaluate container BPF policy
    │       ├── audit_syscall_entry()   — STIG/PCI compliance logging
    │       ├── trace_sys_enter()       — bpftrace/perf tracepoint
    │       └── ptrace_report_syscall_entry() — strace/kubectl exec stop
    │
    ├── array_index_nospec(nr)      — Spectre v1 bounds mitigation
    └── sys_call_table[nr](regs)   ← THE SYSCALL RUNS HERE
           │
           └── return value in regs->ax

syscall_exit_to_user_mode() (kernel/entry/common.c)
    ├── signal delivery (do_signal)  — deliver pending signals
    ├── schedule()                   — preemption check
    ├── seccomp return-value override
    └── trace_sys_exit()             — bpftrace/perf exit tracepoint

entry_SYSCALL_64 (return)
    ├── POP_REGS              — restore all registers from pt_regs
    ├── swapgs                — restore userspace GS base
    └── sysretq               — rip←rcx, rflags←r11, switch to Ring 3
```

## Live Observation

```bash
# Method 1: bpftrace — trace all syscall entries system-wide
sudo bpftrace -e '
tracepoint:raw_syscalls:sys_enter {
    @[comm] = count();
}
interval:s:5 { print(@); clear(@); }'

# Method 2: trace the seccomp filter evaluation path
sudo bpftrace -e 'kprobe:__seccomp_filter {
    printf("seccomp eval: pid=%d comm=%s syscall=%d\n",
           pid, comm, arg0);
}'

# Method 3: observe KPTI page table switches (if enabled)
# Check if KPTI is active on your system:
grep PTI /sys/devices/system/cpu/vulnerabilities/meltdown
# "Mitigation: PTI" = KPTI active; "Not affected" = hardware not vulnerable

# Method 4: strace with register dump — shows pt_regs contents
sudo strace -p $(pidof containerd) -e trace=clone3 -v 2>&1 | head -50

# Method 5: perf trace — kernel-level syscall tracing
sudo perf trace -p $(pidof containerd) --duration 5

# bpftrace: trace the seccomp path for a specific container process
CPID=$(crictl inspect --output go-template --template '{{.info.pid}}' <container-id>)
sudo bpftrace -e "kprobe:__seccomp_filter /pid == $CPID/ {
    printf(\"seccomp: syscall=%d action=%d\\n\", arg0, retval);
}"

# Verify seccomp is active on a container process
cat /proc/$CPID/status | grep Seccomp
# Seccomp: 2      → SECCOMP_MODE_FILTER (BPF program active)
# Seccomp: 0      → not active
# Seccomp: 1      → SECCOMP_MODE_STRICT (only read/write/exit/sigreturn)
```

## Key Kernel References

| Symbol | File | Link |
|--------|------|------|
| `entry_SYSCALL_64` | arch/x86/entry/entry_64.S | https://elixir.bootlin.com/linux/v6.9/source/arch/x86/entry/entry_64.S |
| `do_syscall_64()` | arch/x86/entry/common.c | https://elixir.bootlin.com/linux/v6.9/source/arch/x86/entry/common.c |
| `syscall_enter_from_user_mode()` | kernel/entry/common.c | https://elixir.bootlin.com/linux/v6.9/source/kernel/entry/common.c |
| `__seccomp_filter()` | kernel/seccomp.c | https://elixir.bootlin.com/linux/v6.9/source/kernel/seccomp.c |
| `add_random_kstack_offset()` | arch/x86/include/asm/entry-common.h | https://elixir.bootlin.com/linux/v6.9/source/arch/x86/include/asm/entry-common.h |
| `struct pt_regs` | arch/x86/include/asm/ptrace.h | https://elixir.bootlin.com/linux/v6.9/source/arch/x86/include/asm/ptrace.h |
| `struct seccomp_data` | include/uapi/linux/seccomp.h | https://elixir.bootlin.com/linux/v6.9/source/include/uapi/linux/seccomp.h |
| `SWITCH_TO_KERNEL_CR3` | arch/x86/entry/calling.h | https://elixir.bootlin.com/linux/v6.9/source/arch/x86/entry/calling.h |

---

## On ARM64 (Raspberry Pi 5): `SVC #0` → `el0_svc`

Everything above is x86-specific. The mechanism ARM64 uses is architecturally different, though the outcome at the `do_syscall` C layer is identical.

**Instruction:** `SVC #0` (Supervisor Call) instead of `SYSCALL`. The `SVC` instruction causes an exception — not a fast-path MSR-guided jump. The CPU saves the return PC to `ELR_EL1` and the processor state to `SPSR_EL1`, then indexes into the vector table at `VBAR_EL1 + 0x400` (the synchronous EL0 exception slot).

**Exception vector vs IDT:** x86 uses the IDT (Interrupt Descriptor Table) for all exceptions including system calls; `LSTAR` MSR is a special fast path bypassing the IDT for syscalls. ARM64 has `VBAR_EL1` — a single base address for all 16 exception vectors. There is no equivalent of `LSTAR`; the `SVC` instruction always goes through the vector table.

**Register convention:** x86 puts the syscall number in `RAX`. ARM64 puts it in `x8`. Arguments use `x0`–`x7` (8 registers) vs x86's `rdi/rsi/rdx/r10/r8/r9` (6 registers with the `r10` quirk).

**No KPTI on ARM64:** x86 needs Kernel Page Table Isolation (`SWITCH_TO_KERNEL_CR3` / `SWITCH_TO_USER_CR3`) on every syscall entry/exit because Meltdown allows reading kernel memory from user mode via speculative execution. ARM64 separates user and kernel page tables at the hardware level via `TTBR0_EL1` (user) and `TTBR1_EL1` (kernel) — two registers, always pointing to independent page tables. The CPU never speculatively reads kernel addresses while executing at EL0. No KPTI needed, no CR3 switch overhead.

**The ARM64 syscall path:**
```
SVC #0
  → CPU saves PC to ELR_EL1, PSTATE to SPSR_EL1
  → CPU jumps to VBAR_EL1 + 0x400 (el0_sync in arch/arm64/kernel/entry.S)
  → el0_sync: reads ESR_EL1 to classify exception type
  → branch to el0_svc
  → el0_svc: calls do_el0_svc() in arch/arm64/kernel/syscall.c
  → do_el0_svc: reads x8 (syscall number), dispatches sys_call_table[x8]
  → return value written to x0, eret back to EL0
```

**Full ARM64 coverage:** `kernel/13-a-arm64-syscall.md` — el0_svc, VBAR_EL1, EL0-EL3 model, syscall number mapping table.
