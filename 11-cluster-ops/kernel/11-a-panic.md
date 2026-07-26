# 11-a — Kernel Panic, Die Notifiers, Taint Flags, Oops Path

## The Decision to Halt

A kernel panic is not a crash. It is a decision. When the kernel detects a state it cannot safely recover from — a null pointer dereference in interrupt context, a stack overflow in a critical code path, a hardware error that corrupts in-flight data — it faces a binary choice: attempt to continue and risk silent data corruption, or halt the system immediately and loudly. The `panic()` function implements that choice.

The original Unix systems, and Linux through most of its early history, treated panics as terminal events. The kernel printed a message to the console, the machine froze, and a human had to walk over and press the reset button. This was acceptable in a world of physical machines attended by operators. It became problematic in cloud environments where machines might be in a data center across the country, and unacceptable for Kubernetes nodes that must fail fast and recover automatically.

The taint flag mechanism was added to communicate machine trustworthiness after non-fatal errors. When the kernel encounters a suspicious but non-fatal condition — a kernel module with no license, a machine check error that was corrected by ECC, an out-of-tree driver loaded — it sets a bit in the global `tainted` word. The taint word is exposed at `/proc/sys/kernel/tainted`. Support engineers, when debugging a reported kernel oops, look at taint flags first: a tainted kernel running proprietary drivers makes bug reproduction difficult and narrows what the upstream community can help with. Kubernetes uses the taint flags differently — through the node-problem-detector daemon, which reads dmesg for kernel error patterns and can apply Kubernetes node taints (`node.kubernetes.io/not-ready`, `NoSchedule`, `NoExecute`) when the kernel signals problems.

The `panic_timeout` sysctl — set to -1 on most production Kubernetes nodes — controls whether the kernel reboots automatically after a panic. A node that panics and reboots can rejoin the cluster and be rescheduled without human intervention. A node that panics and waits indefinitely for console interaction is dead to the cluster until someone presses a button.

## 1. Source Locations

| File | Key Symbols | URL |
|------|-------------|-----|
| `kernel/panic.c` | `panic()`, `oops_enter()`, `oops_exit()`, `panic_notifier_list` | https://elixir.bootlin.com/linux/v6.9/source/kernel/panic.c |
| `include/linux/kdebug.h` | `struct die_args`, `enum die_val`, `register_die_notifier()` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/kdebug.h |
| `include/linux/panic.h` | `TAINT_*` flags, `add_taint()`, `test_taint()` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/panic.h |
| `kernel/notifier.c` | `atomic_notifier_call_chain()`, `blocking_notifier_call_chain()` | https://elixir.bootlin.com/linux/v6.9/source/kernel/notifier.c |
| `arch/x86/kernel/dumpstack.c` | `die()`, `show_regs()`, x86 oops output | https://elixir.bootlin.com/linux/v6.9/source/arch/x86/kernel/dumpstack.c |

## 2. `struct die_args`

```c
// include/linux/kdebug.h (Linux 6.9)
struct die_args {
    struct pt_regs  *regs;      // CPU register state at time of exception
    const char      *str;       // description string (e.g. "general protection fault")
    long             err;       // error code (e.g. segment fault error code)
    int              trapnr;    // hardware trap number (e.g. X86_TRAP_GP = 13)
    int              signr;     // signal to deliver (e.g. SIGSEGV)
};
```

Used by `die()` to pass context to all registered die notifiers. Notifiers return `NOTIFY_STOP` to suppress further handling, `NOTIFY_OK` to continue.

## 3. The panic() Path

```
BUG() / NULL deref / oops_exit() + panic_on_oops
    │
    ▼
panic(const char *fmt, ...)                   // kernel/panic.c
    │
    ├─ 1. bust_spinlocks(1)                   // disable spinlock contention checking so printk can proceed in locked context
    ├─ 2. smp_send_stop()                     // IPI to halt all other CPUs
    ├─ 3. atomic_notifier_call_chain(          // call panic_notifier_list
    │       &panic_notifier_list, 0, buf)
    ├─ 4. kmsg_dump(KMSG_DUMP_PANIC)          // flush ring buffer to persistent storage
    ├─ 5. if (kexec_crash_loaded())           // kdump path
    │       crash_kexec(NULL)                 //   → machine_kexec() → crash kernel
    │
    ├─ 6. if (panic_timeout > 0)              // reboot after N seconds
    │       reboot_wait()
    │   elif (panic_timeout < 0)              // reboot immediately
    │       emergency_restart()
    │
    └─ 7. hlt_loop()                          // halt forever (panic_timeout=0)
```

The `panic_notifier_list` is an `atomic_notifier_head`. It uses `atomic_notifier_call_chain()` — safe to call from any context (spinlock-safe, no sleeping).

## 4. Taint Flags

`/proc/sys/kernel/tainted` is a bitmask. Each bit records a permanent kernel state change:

```c
// include/linux/panic.h (Linux 6.9) — selected flags
#define TAINT_PROPRIETARY_MODULE    0   // (P) out-of-tree proprietary module loaded
#define TAINT_FORCED_MODULE         1   // (F) module force-loaded
#define TAINT_CPU_OUT_OF_SPEC       2   // (S) CPU out of spec (overclocking, excessive thermals, hw errata)
#define TAINT_FORCED_RMMOD          3   // (R) module force-unloaded
#define TAINT_MACHINE_CHECK         4   // (M) MCE (hardware error) occurred
#define TAINT_BAD_PAGE              5   // (B) bad page accessed
#define TAINT_USER                  6   // (U) userspace set taint
#define TAINT_DIE                   7   // (D) kernel died (oops/BUG)
#define TAINT_OVERRIDDEN_ACPI_TABLE 8   // (A) ACPI table overridden
#define TAINT_WARN                  9   // (W) kernel WARN_ON fired
#define TAINT_CRAP                 10   // (C) staging driver loaded
#define TAINT_FIRMWARE_WORKAROUND  11   // (I) firmware bug workaround applied
#define TAINT_OOT_MODULE           12   // (O) out-of-tree module (non-proprietary)
#define TAINT_UNSIGNED_MODULE      13   // (E) unsigned module loaded
#define TAINT_SOFTLOCKUP           14   // (L) soft lockup detected
#define TAINT_LIVEPATCH            15   // (K) live kernel patch applied
#define TAINT_AUX                  16   // (X) auxiliary taint (driver-defined)
#define TAINT_RANDSTRUCT           17   // (T) struct layout was randomized
#define TAINT_TEST                 18   // (N) test module loaded
```

`add_taint(flag, lockdep_ok)` sets a bit. `test_taint(flag)` tests it. On Kubernetes nodes, key flags to watch: bit 9 (WARN), bit 7 (DIE/oops), bit 4 (MCE), bit 14 (soft lockup), bit 15 (livepatch).

## 5. oops_enter() and oops_exit()

An "oops" is a recoverable kernel fault (process killed, kernel continues). An "oops" becomes a panic if:
- `CONFIG_PANIC_ON_OOPS=y`
- `sysctl kernel.panic_on_oops=1`
- The fault occurs in interrupt context (no process to kill)
- The fault is in a kernel thread (no user process to kill)

`oops_enter()` (`kernel/panic.c`) turns off tracing and increments the global oops counter. When the fault is in interrupt context, `die()` in the architecture handler calls `panic("Fatal exception in interrupt")` before reaching `oops_enter()`.

`oops_exit()` (`kernel/panic.c`) re-enables tracing and prints the oops end marker. When `sysctl kernel.panic_on_oops=1`, the architecture die path calls `panic("Fatal exception")` after `oops_exit()` returns — the check is in the caller, not in `oops_exit()` itself.

```c
// Conceptual flow — see kernel/panic.c + arch/x86/kernel/dumpstack.c
oops_enter();              // tracing_off(), increment oops counter
    // architecture prints register dump, stack trace
oops_exit();               // tracing toggle, print_oops_end_marker()
    // if panic_on_oops: caller calls panic("Fatal exception")
```

## 6. Live Observation

```bash
# Read current taint state
cat /proc/sys/kernel/tainted        # decimal bitmask, 0 = clean kernel

# Decode taint flags (kernel provides this)
awk 'NR==1{t=$1; for(i=0;i<19;i++){if(and(t,2^i))printf "bit %d set\n",i}}' \
    /proc/sys/kernel/tainted

# Simulate a taint (user-settable bit 6)
echo 64 > /proc/sys/kernel/tainted   # requires CAP_SYS_ADMIN

# Configure panic behavior
sysctl kernel.panic_on_oops          # 0 or 1
sysctl kernel.panic                  # reboot timeout in seconds (0=halt, <0=immediate)

# Watch kernel messages for WARN/BUG
dmesg -T -w | grep -E 'BUG:|Oops|WARN|RIP:|Call Trace'

# bpftrace: trace panic() entry
bpftrace -e 'kprobe:panic { printf("PANIC: cpu=%d pid=%d comm=%s\n", cpu, pid, comm); }'
```

## 7. Key Kernel References

| Symbol | File | URL |
|--------|------|-----|
| `panic()` | `kernel/panic.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/panic.c |
| `struct die_args` | `include/linux/kdebug.h` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/kdebug.h |
| `TAINT_*` flags | `include/linux/panic.h` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/panic.h |
| `add_taint()` | `kernel/panic.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/panic.c |
| `register_die_notifier()` | `kernel/notifier.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/notifier.c |
| `oops_enter()` | `kernel/panic.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/panic.c |
