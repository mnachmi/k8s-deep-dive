# 13-a — ARM64 Syscall Path: `el0_svc`, Exception Levels, and How ARM Differs from x86

## The Problem with x86 Assumptions

Chapter 00 traced the x86 system call path: user process executes `SYSCALL` instruction → CPU loads `MSR_LSTAR` into RIP, `MSR_STAR` into CS and SS, saves user RIP/RSP to kernel-private locations → CPU is now in Ring 0 executing `entry_SYSCALL_64` in `arch/x86/entry/entry_64.S`. The hardware mechanism (SYSCALL instruction + MSRs) is x86-specific. The Linux kernel interface above it (syscall number in RAX, arguments in RDI/RSI/RDX/R10/R8/R9, return value in RAX) is also architecture-specific — the same `read(2)` has syscall number 0 on x86-64 and 63 on ARM64.

When you run Kubernetes on a Raspberry Pi 5, everything in chapters 01-11 works — cgroups, namespaces, eBPF, the OOM killer, CFS, the scheduler — because these are Linux kernel abstractions implemented in architecture-independent C. What changes is the path from userspace to kernelspace, the memory model guarantees, and the hardware performance monitoring interface.

Understanding these differences matters not because you need to write architecture-specific code, but because production incidents have root causes in them. A race condition that never fires on x86 (because x86 enforces TSO memory ordering) fires immediately on ARM64. A bpftrace one-liner that uses x86-specific tracepoints needs the ARM64 equivalent. An `mrs` instruction that reads the cycle counter on ARM64 has no direct x86 analog.

## Source Locations

| File | Key Symbols | URL |
|------|-------------|-----|
| `arch/arm64/kernel/entry.S` | `el0_sync`, `el0_svc`, `el0_svc_handler` | https://elixir.bootlin.com/linux/v6.9/source/arch/arm64/kernel/entry.S |
| `arch/arm64/kernel/syscall.c` | `do_el0_svc()`, `el0_svc_common()`, syscall table dispatch | https://elixir.bootlin.com/linux/v6.9/source/arch/arm64/kernel/syscall.c |
| `arch/arm64/include/asm/esr.h` | `ESR_ELx_EC_SVC64`, exception syndrome register values | https://elixir.bootlin.com/linux/v6.9/source/arch/arm64/include/asm/esr.h |
| `arch/arm64/include/asm/ptrace.h` | `struct pt_regs` for ARM64 | https://elixir.bootlin.com/linux/v6.9/source/arch/arm64/include/asm/ptrace.h |
| `include/uapi/asm-generic/unistd.h` | ARM64 syscall table | https://elixir.bootlin.com/linux/v6.9/source/include/uapi/asm-generic/unistd.h |

## 1. ARM64 Exception Levels: EL0-EL3

ARM64 uses "exception levels" instead of x86 protection rings. The hierarchy:

```
EL3 — Secure Monitor (TrustZone firmware — runs secure OS, key storage)
  ↑ SMC (Secure Monitor Call) instruction
EL2 — Hypervisor (KVM on ARM64 runs here — equivalent of Intel VT-x host mode)
  ↑ HVC (Hypervisor Call) instruction
EL1 — OS Kernel (Linux kernel runs here — equivalent of Ring 0)
  ↑ SVC (Supervisor Call) instruction ← this is the syscall mechanism
EL0 — User process (equivalent of Ring 3)
```

On a non-virtualized system (bare RPi5 running Linux), only EL0 and EL1 are active. The kernel runs at EL1. User processes run at EL0. The `SVC #0` instruction is the system call instruction — it causes an exception that switches to EL1.

On a KVM-virtualized ARM64 system, KVM runs at EL2. The guest Linux kernel runs at EL1 (inside the VM). The `SVC` instruction still causes EL0→EL1 transitions inside the guest. The hypervisor intercepts EL2-level events (like I/O accesses) without interfering with the EL0→EL1 syscall path — making KVM on ARM64 structurally cleaner than Intel VT-x in this respect.

## 2. The `SVC` Instruction: ARM64 Syscall Mechanism

x86 syscall: user code executes `SYSCALL` with syscall number in RAX. CPU loads LSTAR/STAR MSRs, switches to Ring 0, jumps to `entry_SYSCALL_64`.

ARM64 syscall: user code executes `SVC #0` with syscall number in **x8** (not x0). CPU:
1. Saves PC (return address) to `ELR_EL1` (Exception Link Register)
2. Saves PSTATE (processor state) to `SPSR_EL1`
3. Loads `VBAR_EL1 + 0x400` into PC (exception vector table, offset for synchronous EL0 exceptions)
4. Switches to EL1 (kernel mode)
5. Begins executing the exception handler

```c
// ARM64 calling convention for syscalls:
// x0-x7: arguments (up to 8 arguments)
// x8:    syscall number
// x0:    return value

// Example: read(fd=3, buf=0x..., count=4096)
// User code (compiler generates):
// mov x8, #63         // __NR_read = 63 on ARM64
// mov x0, #3          // fd
// mov x1, x_buf_ptr   // buf
// mov x2, #4096       // count
// svc #0              // trap to EL1
```

## 3. `VBAR_EL1`: The Exception Vector Table

`VBAR_EL1` (Vector Base Address Register for EL1) is the ARM64 equivalent of x86's IDT (Interrupt Descriptor Table) pointer. It points to a 2KB-aligned table of 16 exception vectors, each 128 bytes of code:

```
VBAR_EL1 + 0x000 — Synchronous exception from current EL with SP_EL0
VBAR_EL1 + 0x080 — IRQ from current EL with SP_EL0
VBAR_EL1 + 0x100 — FIQ from current EL with SP_EL0
VBAR_EL1 + 0x180 — SError from current EL with SP_EL0
VBAR_EL1 + 0x200 — Synchronous exception from current EL with SP_ELx
...
VBAR_EL1 + 0x400 — Synchronous exception from EL0 (AArch64) ← SVC lands here
VBAR_EL1 + 0x480 — IRQ from EL0 (AArch64)
...
```

In Linux, `VBAR_EL1` points to the `vectors` symbol in `arch/arm64/kernel/entry.S`. The `SVC` instruction from EL0 indexes to offset 0x400 — the `el0_sync` handler.

## 4. `el0_svc` Call Path

```asm
// arch/arm64/kernel/entry.S (simplified)
el0_sync:
    kernel_entry 0           // save all user registers to kernel stack (struct pt_regs)
    mrs x25, esr_el1         // read Exception Syndrome Register
    lsr x24, x25, #ESR_ELx_EC_SHIFT   // extract exception class
    cmp x24, #ESR_ELx_EC_SVC64       // was it SVC #0 (syscall)?
    b.eq el0_svc             // yes → syscall path
    ...                      // no → fault, alignment, breakpoint, etc.

el0_svc:
    mov x0, sp               // pass pt_regs pointer to C
    bl  do_el0_svc           // jump to C handler
```

```c
// arch/arm64/kernel/syscall.c
void do_el0_svc(struct pt_regs *regs)
{
    // x8 contains syscall number (ARM64 convention)
    int syscall = regs->regs[8];

    // Range check against syscall table size
    if (syscall < 0 || syscall >= __NR_syscalls)
        goto bad_syscall;

    // Dispatch through syscall table
    // sys_call_table[syscall] → sys_read, sys_write, sys_open, ...
    syscall_fn_t fn = sys_call_table[syscall];
    regs->regs[0] = fn(regs);   // call with pt_regs; return value → x0
    return;

bad_syscall:
    regs->regs[0] = -ENOSYS;
}
```

**Comparison to x86:** On x86, `entry_SYSCALL_64` saves registers with `pushq` instructions in assembly, then calls `do_syscall_64()` which reads RAX as the syscall number. On ARM64, `kernel_entry` macro saves registers in assembly, then calls `do_el0_svc()` which reads `regs->regs[8]` (x8) as the syscall number. The structure is the same; the register conventions differ.

## 5. Live Observation on RPi5

```bash
# Verify ARM64 architecture
uname -m
# → aarch64

# Check exception level of current process (always EL0 for user processes)
# Only visible via EL1-level MSR reads in kernel code, but:
grep -r "EL0\|EL1\|EL2" /proc/cpuinfo
# ARM64 kernel reports CPU part, architecture, variant

# Syscall number comparison: read(2)
# x86-64: syscall 0
python3 -c "import ctypes; print(ctypes.CDLL(None).syscall(0, 0, '', 0))"
# ARM64: syscall 63
# The ABI is different; strace hides this behind the same read() symbol

# Use strace to see syscall dispatch
strace -e trace=read ls /tmp 2>&1 | head
# → read(3, ...) — same API, different number underneath

# bpftrace: attach to ARM64 syscall entry (equivalent of x86 syscall entry)
bpftrace -e 'tracepoint:raw_syscalls:sys_enter { @[args->id] = count(); }' &
sleep 5
# Shows syscall numbers — different from x86-64 numbers

# Check VBAR_EL1 value (requires kernel module or perf, not user accessible directly)
# From kernel: msr reads VBAR_EL1 at boot, stored in kernel log
dmesg | grep -i "vectors\|VBAR"
```

## 6. Syscall Number Mapping: ARM64 vs x86-64

| Syscall | x86-64 | ARM64 |
|---------|--------|-------|
| `read` | 0 | 63 |
| `write` | 1 | 64 |
| `open` | 2 | (openat=56) |
| `close` | 3 | 57 |
| `mmap` | 9 | 222 |
| `execve` | 59 | 221 |
| `clone` | 56 | 220 |
| `brk` | 12 | 214 |

ARM64 Linux uses the "generic" syscall table defined in `include/uapi/asm-generic/unistd.h` which was designed to be ABI-clean (no legacy compat overhead). x86-64 uses `arch/x86/entry/syscalls/syscall_64.tbl` which preserves historical numbering for binary compatibility.

## 7. Key Kernel References

| Symbol | File | URL |
|--------|------|-----|
| `el0_sync` | `arch/arm64/kernel/entry.S` | https://elixir.bootlin.com/linux/v6.9/source/arch/arm64/kernel/entry.S |
| `do_el0_svc()` | `arch/arm64/kernel/syscall.c` | https://elixir.bootlin.com/linux/v6.9/source/arch/arm64/kernel/syscall.c |
| `VBAR_EL1` | `arch/arm64/kernel/head.S` | https://elixir.bootlin.com/linux/v6.9/source/arch/arm64/kernel/head.S |
| ARM64 syscall table | `include/uapi/asm-generic/unistd.h` | https://elixir.bootlin.com/linux/v6.9/source/include/uapi/asm-generic/unistd.h |
| `ESR_ELx_EC_SVC64` | `arch/arm64/include/asm/esr.h` | https://elixir.bootlin.com/linux/v6.9/source/arch/arm64/include/asm/esr.h |
