# The x86-64 Syscall Table — Kernel Deep Dive

## Source Location

| File | Link |
|------|------|
| `arch/x86/entry/syscalls/syscall_64.tbl` | https://elixir.bootlin.com/linux/v6.9/source/arch/x86/entry/syscalls/syscall_64.tbl |
| `arch/x86/kernel/syscall_64.c` | https://elixir.bootlin.com/linux/v6.9/source/arch/x86/kernel/syscall_64.c |
| `arch/x86/entry/common.c` | https://elixir.bootlin.com/linux/v6.9/source/arch/x86/entry/common.c |

## The Boundary Between Two Worlds

The x86 processor runs in one of several privilege levels, called rings. Ring 0 is the kernel: code executing there can issue any instruction, read any physical memory address, program hardware directly. Ring 3 is userspace: code executing there cannot touch I/O ports, cannot modify page tables, cannot disable interrupts. The processor enforces this distinction in hardware — any attempt to execute a privileged instruction from Ring 3 causes a fault.

This separation is the foundation of every security guarantee Linux provides. But it creates a problem: userspace programs need to ask the kernel to do things. Reading a file requires the kernel to walk a directory, access a block device, copy bytes into a user buffer. Creating a process requires the kernel to allocate a `struct task_struct`, set up a new address space, and wire the new task into the scheduler. These operations cannot happen in Ring 3. They require a controlled, auditable mechanism to cross from userspace into kernel space and back.

That mechanism is the system call. It is not a function call — the C calling convention does not work across privilege levels. It is a hardware-assisted mode switch. The processor saves the current instruction pointer and flags, switches to Ring 0, and jumps to a kernel-specified handler. After the kernel finishes, it restores the saved state and returns to Ring 3 exactly where it left off.

The syscall table is the kernel's directory of everything userspace is allowed to ask for. Each entry maps an integer — the syscall number — to the C function that implements it. There are currently 460+ entries on x86-64. The number in `rax` at the moment of the syscall instruction determines which one runs.

## A Brief History of the User/Kernel Interface

The original x86 (32-bit) interface used `int 0x80` — a software interrupt. The CPU would execute the interrupt handler, which happened to be the kernel's syscall dispatcher. It worked, but it was slow: software interrupts involve pushing a hardware-defined interrupt frame onto the stack, looking up the interrupt descriptor table, performing privilege checks, and eventually executing `iret` on the return path. The overhead was measurable — tens to hundreds of nanoseconds per syscall on a 2000s-era processor.

Intel introduced `sysenter`/`sysexit` in the Pentium II era (1997) and AMD introduced `syscall`/`sysret` for their 64-bit architecture. Both use Model Specific Registers (MSRs) to pre-configure the kernel's handler address, bypassing the interrupt descriptor table entirely. The `syscall` instruction, used on x86-64, is the faster path: it reads the handler address from the `IA32_LSTAR` MSR that the kernel writes during boot, performs the mode switch in about 10 cycles, and does not push a full hardware frame.

The kernel sets this up in `arch/x86/kernel/cpu/common.c`:

```c
// Called once per CPU during boot
wrmsrl(MSR_LSTAR, (unsigned long)entry_SYSCALL_64);
```

From that point on, every `syscall` instruction executed by any process on that CPU will jump to `entry_SYSCALL_64` — the assembly entry point covered in `00-b-entry-path.md`.

## The Syscall Table File

`arch/x86/entry/syscalls/syscall_64.tbl` is a plain text file processed by a Perl script during the kernel build. It has no special syntax — just whitespace-delimited fields that the build tooling transforms into C macros:

```
<number>  <abi>  <name>  <entry_point>  [<compat_entry_point>]
```

A few representative lines:

```
# From arch/x86/entry/syscalls/syscall_64.tbl (Linux 6.9)
0       common  read                    sys_read
1       common  write                   sys_write
57      common  fork                    sys_fork
155     common  pivot_root              sys_pivot_root
165     common  mount                   sys_mount
272     common  unshare                 sys_unshare
308     common  setns                   sys_setns
435     common  clone3                  sys_clone3
```

The `abi` column takes three values: `common` (available to both 64-bit native and 32-bit compatibility paths), `64` (64-bit native only), and `x32` (the x32 ABI — 32-bit pointers in 64-bit mode, used almost nowhere). Most interesting syscalls are `common`.

The build system processes this table with `arch/x86/entry/syscalls/syscalltbl.pl` to generate `arch/x86/include/generated/asm/syscalls_64.h`:

```c
/* Auto-generated — do not edit */
__SYSCALL(0,  sys_read)
__SYSCALL(1,  sys_write)
__SYSCALL(57, sys_fork)
__SYSCALL(272, sys_unshare)
__SYSCALL(308, sys_setns)
__SYSCALL(435, sys_clone3)
```

The `__SYSCALL` macro is not defined in this header — it is defined differently by whoever includes the header. This is a clever build-time trick. When `arch/x86/kernel/syscall_64.c` includes it, `__SYSCALL` expands to an array initializer:

```c
// arch/x86/kernel/syscall_64.c
#define __SYSCALL(nr, sym) [nr] = (sys_call_ptr_t)sym,

const sys_call_ptr_t sys_call_table[] = {
#include <asm/syscalls_64.h>
};
```

The result is a static array with function pointers at each index. `sys_call_table[0]` is `sys_read`. `sys_call_table[435]` is `sys_clone3`. Indexing into this array with `rax` takes one instruction.

This flat-array design is not accidental. A hashtable would require hashing the syscall number and resolving potential collisions — several instructions instead of one. A linked list would require traversal — O(n). The flat array trades memory (460 pointers × 8 bytes ≈ 3.7 KB) for O(1) dispatch. At the rate syscalls happen on a busy system — millions per second — that tradeoff is obviously correct.

## sys_call_table[] — The Dispatch Array

```c
typedef asmlinkage long (*sys_call_ptr_t)(const struct pt_regs *);
```

Every function pointer in `sys_call_table[]` has this signature: it takes one argument (a pointer to the saved register state, `struct pt_regs`), and returns a `long` (the syscall return value). The `asmlinkage` attribute tells the compiler that arguments come from the stack rather than from registers — but since `CONFIG_ARCH_HAS_SYSCALL_WRAPPER` was introduced in Linux 4.17, the actual C implementations receive typed arguments unwrapped from `pt_regs`, not `pt_regs` directly. The wrapper layer handles the translation.

The dispatch itself in `do_syscall_64()`:

```c
// arch/x86/entry/common.c (Linux 6.9)
if (likely(nr < NR_syscalls)) {
    nr = array_index_nospec(nr, NR_syscalls);
    regs->ax = sys_call_table[nr](regs);
}
```

`array_index_nospec` is a Spectre v1 mitigation. The Spectre v1 vulnerability allows an attacker to speculatively read beyond an array's bounds — the CPU's branch predictor may decide `nr < NR_syscalls` is true and execute the array access before the comparison is verified, exposing data via cache timing. `array_index_nospec` uses a bitmask technique that clamps `nr` to the valid range without a conditional branch that the branch predictor can exploit.

This mitigation was added in 2018 as part of the industry-wide Spectre response. The kernel source has hundreds of `array_index_nospec` calls throughout, each marking a place where speculative access could expose kernel memory.

## The SYSCALL_DEFINE Macros

You will see `SYSCALL_DEFINE1`, `SYSCALL_DEFINE2`, `SYSCALL_DEFINE5`, and so on throughout the kernel source. The number suffix is the argument count. These macros do more than define a function — they create a complete wrapper:

```c
// Simplified: what SYSCALL_DEFINE2(clone3, ...) expands to
asmlinkage long __se_sys_clone3(const struct pt_regs *regs);
static inline long __do_sys_clone3(struct clone_args __user *uargs, size_t size);

asmlinkage long __se_sys_clone3(const struct pt_regs *regs) {
    long ret = __do_sys_clone3(
        (struct clone_args __user *)regs->di,   // rdi = arg1
        (size_t)regs->si                         // rsi = arg2
    );
    return ret;
}
```

The `__se_` (syscall entry) function unpacks arguments from `pt_regs` and calls the `__do_` function that has the normal C signature. The `__do_` function is what you actually read in the source. This separation exists for tracing tools, ptrace, and security systems that want to inspect arguments before or after the call.

Before Linux 4.17, syscall functions directly took registers as arguments (via the old `asmlinkage` convention where arguments came from the stack in a specific order). The wrapper approach in `CONFIG_ARCH_HAS_SYSCALL_WRAPPER` makes it possible to do argument validation and sanitization at the boundary level rather than inside each syscall implementation.

## The Container-Relevant Syscall Subset

Every component of the Kubernetes stack — from the Go runtime in kubelet, to containerd, to runc, to the application process — communicates with the kernel through syscalls. The following are the ones that define container lifecycle:

| Syscall | Number | C entry | Purpose in containers |
|---------|--------|---------|----------------------|
| `clone3` | 435 | `sys_clone3` | Creates the container process with new namespaces. Accepts `struct clone_args` specifying `CLONE_NEWPID \| CLONE_NEWNET \| CLONE_NEWNS \| CLONE_NEWIPC \| CLONE_NEWUTS` |
| `unshare` | 272 | `sys_unshare` | Detaches the calling process from namespaces without forking. runc uses this to set up the mount namespace before `pivot_root` |
| `setns` | 308 | `sys_setns` | Joins an existing namespace by file descriptor. `kubectl exec` and CNI plugins use this to enter a running container's network namespace |
| `pivot_root` | 155 | `sys_pivot_root` | Replaces the root filesystem for the current mount namespace — the kernel mechanic that puts the OCI layer stack at `/` |
| `mount` | 165 | `sys_mount` | Attaches a filesystem. runc uses this to bind-mount `/proc`, `/sys`, `/dev/shm`, and secrets into the container namespace |
| `epoll_wait` | 232 | `sys_epoll_wait` | I/O event multiplexing. The Go runtime's netpoller is built on this — every gRPC connection, every kubelet watch, every API server call blocks here |
| `futex` | 202 | `sys_futex` | Fast userspace mutex. The Go runtime's goroutine scheduler parks goroutines with `FUTEX_WAIT` and wakes them with `FUTEX_WAKE` |
| `openat` | 257 | `sys_openat` | Opens files. Reading cgroup files, kubelet config, pod spec volumes — all `openat` |

All of these are `common` ABI — they work from both 64-bit and 32-bit compatibility paths. Their implementations live in `kernel/fork.c` (fork/clone/unshare), `kernel/nsproxy.c` (setns), and `fs/namespace.c` (mount, pivot_root).

## Live Observation

```bash
# Method 1: strace — count syscalls by name for containerd over 10 seconds
sudo strace -c -p "$(pidof containerd)" -e trace=all &
SPID=$!; sleep 10; kill -INT $SPID
# Look for clone3, setns, epoll_wait near the top

# Method 2: perf — hardware event counting for container-relevant calls
sudo perf stat \
    -e 'syscalls:sys_enter_clone3' \
    -e 'syscalls:sys_enter_setns' \
    -e 'syscalls:sys_enter_pivot_root' \
    -p "$(pidof containerd)" -- sleep 5

# Method 3: verify the tracepoints exist in the running kernel
ls /sys/kernel/debug/tracing/events/syscalls/ | grep sys_enter_clone
# sys_enter_clone3  sys_enter_clone

# Method 4: map a number to a name
ausyscall x86_64 435    # → clone3
ausyscall x86_64 272    # → unshare
grep '^\s*435\s' /usr/include/asm/unistd_64.h
# __NR_clone3 435

# bpftrace: observe every clone3 call system-wide with its flags
sudo bpftrace -e '
tracepoint:syscalls:sys_enter_clone3 {
    printf("comm=%-16s pid=%-8d flags=0x%llx\n",
           comm, pid, args->uargs->flags);
}'
```

## Key Kernel References

| Symbol | File | Link |
|--------|------|------|
| `syscall_64.tbl` | arch/x86/entry/syscalls/syscall_64.tbl | https://elixir.bootlin.com/linux/v6.9/source/arch/x86/entry/syscalls/syscall_64.tbl |
| `sys_call_table[]` | arch/x86/kernel/syscall_64.c | https://elixir.bootlin.com/linux/v6.9/source/arch/x86/kernel/syscall_64.c |
| `do_syscall_64()` | arch/x86/entry/common.c | https://elixir.bootlin.com/linux/v6.9/source/arch/x86/entry/common.c |
| `SYSCALL_DEFINE` macros | include/linux/syscalls.h | https://elixir.bootlin.com/linux/v6.9/source/include/linux/syscalls.h |
| `sys_clone3` | kernel/fork.c | https://elixir.bootlin.com/linux/v6.9/source/kernel/fork.c |
| `sys_setns` | kernel/nsproxy.c | https://elixir.bootlin.com/linux/v6.9/source/kernel/nsproxy.c |
| `array_index_nospec` | include/linux/nospec.h | https://elixir.bootlin.com/linux/v6.9/source/include/linux/nospec.h |
