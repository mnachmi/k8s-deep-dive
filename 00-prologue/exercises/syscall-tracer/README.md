# syscall-tracer — minimal strace clone using ptrace

## What It Demonstrates

This exercise implements a minimal `strace` clone using `ptrace(PTRACE_SYSCALL)`. It forks a child process, attaches the tracer, and intercepts every system call the child makes — printing the syscall number and name on entry.

This is exactly the mechanism used by:
- `strace` for syscall tracing
- `seccomp` BPF filters for syscall auditing and filtering in containers
- Container runtimes (runc, containerd) to enforce syscall allowlists via `libseccomp`

Key concepts demonstrated:
- `PTRACE_TRACEME` / `PTRACE_SYSCALL` — how a parent process intercepts child syscalls
- `PTRACE_O_TRACESYSGOOD` — distinguishes syscall-stops from signal-stops via the `0x80` bit
- Reading `orig_rax` from `struct user_regs_struct` to get the syscall number on x86_64
- The difference between syscall-entry and syscall-exit stops

## Build and Run

```bash
make run
```

This builds the tracer and runs it against `/bin/ls /tmp`.

**Expected output:**

```
[    0] syscall execve          (nr=59)
[    1] syscall openat          (nr=257)
[    2] syscall read            (nr=0)
...

--- child exited with 0 ---
total syscalls traced: <N>
```

Each line shows the sequential syscall index, name (if known), and raw syscall number.

To build without running:
```bash
make build
```

To clean up:
```bash
make clean
```

## Kernel References

- `kernel/fork.c:copy_process()` — how `ptrace` flags are inherited across fork/exec
- `kernel/ptrace.c:ptrace_stop()` — the kernel-side implementation of ptrace stops, including `SYSCALL_TRAP` stops
- `arch/x86/entry/common.c:do_syscall_64()` — x86_64 syscall entry point where `ptrace` hooks are called before and after each syscall dispatch

The `PTRACE_O_TRACESYSGOOD` option sets bit 7 (`0x80`) of the stop signal, allowing the tracer to distinguish syscall-stops from regular signal-stops — see `ptrace_stop()` in `kernel/ptrace.c`.

## Exercise

Modify the tracer to count syscall frequency and sort by count at exit:

```c
long counts[512] = {0};

/* In the syscall-entry branch, increment: */
if (nr >= 0 && nr < 512) counts[nr]++;

/* At exit, sort and print top calls */
```

Hint: use a `long counts[512]` array indexed by syscall number. At the end, walk the array and print any non-zero entry. For sorting, you can use a simple insertion sort or `qsort` with a custom comparator.

## Kubernetes Connection

When you run a Pod, `runc` calls `prctl(PR_SET_SECCOMP, SECCOMP_MODE_FILTER, ...)` to install a BPF program that runs at the same hook point this tracer uses. The BPF filter is compiled from the Pod's `seccompProfile` by `libseccomp`.

The kernel hook is in `security/seccomp.c:seccomp_run_filters()` — called from the same `do_syscall_64()` path shown above. Every syscall made inside a container passes through this filter before execution.

The default Kubernetes seccomp profile blocks ~50 syscalls (including `unshare`, `setns`, `pivot_root`) — exactly the calls listed in this tracer's name table.
