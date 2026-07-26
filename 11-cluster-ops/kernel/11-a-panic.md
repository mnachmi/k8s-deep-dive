# 11-a — Kernel Panic, Die Notifiers, Taint Flags, Oops Path

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
    ├─ 1. bust_spinlocks(1)                   // disable lockdep, make printk work
    ├─ 2. smp_send_stop()                     // IPI to halt all other CPUs
    ├─ 3. atomic_notifier_call_chain(          // call panic_notifier_list
    │       &panic_notifier_list, PANIC_ACTION, buf)
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
#define TAINT_CPU_OUT_OF_SPEC       2   // (S) SMP kernel on non-SMP hardware
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

```c
// kernel/panic.c
void oops_enter(void)
{
    tracing_off();          // stop ftrace
    /* if in_interrupt(): cannot kill a process → will become panic */
    if (in_interrupt())
        panic("Fatal exception in interrupt");
}

void oops_exit(void)
{
    tracing_on();
    print_oops_end_marker();
    if (panic_on_oops)
        panic("Fatal exception");
}
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
