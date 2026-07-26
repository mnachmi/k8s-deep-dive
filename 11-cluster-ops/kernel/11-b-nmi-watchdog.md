# 11-b NMI, Hardlockup/Softlockup Watchdog

## When the Kernel Cannot Tell You It's Stuck

A kernel panic is at least communicative. It prints a message, records a stack trace, and either reboots or halts with visible output. A lockup is worse: the machine becomes unresponsive without dying. A CPU gets stuck in a tight loop holding a spinlock. The interrupt handler that would normally preempt it cannot run because interrupts are disabled. No panic is triggered because no bug detection code runs — the kernel is simply stuck, forever executing the same instructions. From the outside, the machine looks hung. No log messages. No recovery. Kubernetes marks the node `NotReady` eventually (after the node lease expires), but cannot force a recovery.

This scenario — a CPU stuck with interrupts disabled, unable to be preempted by the scheduler, unable to respond to user input — is what the NMI watchdog exists to detect and terminate. NMI stands for Non-Maskable Interrupt. Unlike ordinary hardware interrupts, an NMI cannot be disabled by the CPU's interrupt flag. Even a CPU that has executed `cli` (clear interrupt flag) to disable all other interrupts will still receive and handle an NMI. The CPU has no choice — it is a hardware mechanism that bypasses the software interrupt mask entirely.

The NMI watchdog works by programming the CPU's Performance Monitoring Unit (PMU) to fire a PMU overflow interrupt — which is delivered as an NMI — every few hundred milliseconds. A separate per-CPU "hardlockup detector" records a timestamp each time this NMI fires. A second watchdog thread ("softlockup detector") runs as a highest-priority kernel thread and resets a per-CPU counter. If the hardlockup NMI fires and finds that the softlockup counter has not been reset in `watchdog_thresh` seconds (default 10), a CPU is stuck. The kernel calls `panic()`. The node crashes cleanly rather than hanging indefinitely.

For Kubernetes, this means the difference between a node that fails fast (enabling pod rescheduling) and a node that hangs until the node lease TTL expires and the API server finally marks it `NotReady`. The `/proc/sys/kernel/watchdog_thresh` sysctl controls sensitivity. Setting it too low on a heavily loaded node causes false positives; setting it too high delays detecting real lockups. Production Kubernetes nodes typically leave this at the default — 10 seconds for hardlockup, 20 for softlockup — which provides detection fast enough to trigger the node lease timeout recovery path.

## 1. Source Locations

| File | Key Symbols | URL |
|------|-------------|-----|
| `arch/x86/include/asm/nmi.h` | `nmi_handler_t`, `register_nmi_handler()`, `NMI_LOCAL`, `NMI_UNKNOWN` | https://elixir.bootlin.com/linux/v6.9/source/arch/x86/include/asm/nmi.h |
| `arch/x86/kernel/nmi.c` | `do_nmi()`, `nmi_handle()`, NMI handler dispatch | https://elixir.bootlin.com/linux/v6.9/source/arch/x86/kernel/nmi.c |
| `kernel/watchdog.c` | `watchdog_enable()`, `watchdog_timer_fn()`, `is_hardlockup()`, softlockup hrtimer | https://elixir.bootlin.com/linux/v6.9/source/kernel/watchdog.c |
| `kernel/watchdog_perf.c` | hardlockup detector: `watchdog_overflow_callback()` | https://elixir.bootlin.com/linux/v6.9/source/kernel/watchdog_perf.c |
| `include/linux/nmi.h` | `touch_nmi_watchdog()`, `touch_softlockup_watchdog()` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/nmi.h |

## 2. NMI Overview

The Non-Maskable Interrupt cannot be blocked by `cli` (the `IF` flag in RFLAGS does not mask NMIs). On x86, NMI sources include:
- Hardware errors (ECC memory errors, PCI SERR)
- Watchdog timer overflow (PMU → perf_event overflow → NMI)
- IPMI/BMC management interrupts
- `int 2` instruction (software NMI for testing)

NMI vector: `X86_TRAP_NMI = 2`. Entry point: `do_nmi()` in `arch/x86/kernel/nmi.c`.

## 3. NMI Handler Registration

```c
// arch/x86/include/asm/nmi.h (Linux 6.9)
typedef int (*nmi_handler_t)(unsigned int type, struct pt_regs *regs);

// Handler types
#define NMI_LOCAL    0   // local (in-CPU) NMI
#define NMI_UNKNOWN  1   // NMI from unknown source
#define NMI_SERR     2   // PCI SERR
#define NMI_IO_CHECK 3   // I/O check

// In Linux 6.9, register_nmi_handler() is a **macro** defined in
// arch/x86/include/asm/nmi.h. The underlying C function is:
//   int __register_nmi_handler(unsigned int, struct nmiaction *);
// where struct nmiaction holds the handler pointer, flags, and name.
// The macro wraps the nmiaction struct construction automatically.
int register_nmi_handler(unsigned int type, nmi_handler_t handler,
                         unsigned long flags, const char *name);
void unregister_nmi_handler(unsigned int type, const char *name);
```

All NMI handlers must complete in < ~100 µs. No sleeping, no locks that might be held by interrupted code.

## 4. Softlockup Watchdog

The softlockup detector uses a per-CPU **hrtimer** (`watchdog_hrtimer`,
function `watchdog_timer_fn()` in `kernel/watchdog.c`). The hrtimer fires
every `watchdog_thresh / 5` seconds (the "sample period"). Each tick:
1. `touch_softlockup_watchdog()` updates `watchdog_touch_ts` (per-CPU ns timestamp)
2. `watchdog_timer_fn()` checks if `current` has been running for > `2 × watchdog_thresh`
   seconds without being scheduled away — indicating a soft lockup

If a soft lockup is detected:
```
BUG: soft lockup - CPU#0 stuck for 22s! [nginx:1234]
```

Key sysctls:
- `kernel.watchdog_thresh` (default 10s) — softlockup triggers at `2 × thresh`
- `kernel.softlockup_panic` — 0/1 (default 0: print only)
- `kernel.nmi_watchdog` — 0=disabled, 1=enabled (hardlockup)

## 5. Hardlockup Watchdog

The hardlockup detector uses the CPU's PMU (Performance Monitoring Unit) to generate an NMI every `watchdog_thresh` seconds. If the NMI fires and the softlockup `hrtimer_interrupts` counter hasn't changed (meaning no hrtimer interrupts, meaning the CPU was completely locked), it reports a hardlockup:

```
NMI watchdog: Watchdog detected hard LOCKUP on cpu 0
```

Implementation in `kernel/watchdog_perf.c`:
```c
// Per-CPU perf_event configured as:
//   type = PERF_TYPE_HARDWARE
//   config = PERF_COUNT_HW_CPU_CYCLES (or NMI-capable PMU counter)
//   sample_period = hw_nmi_get_sample_period(watchdog_thresh)
//   overflow_handler = watchdog_overflow_callback

static void watchdog_overflow_callback(struct perf_event *event,
                                       struct perf_sample_data *data,
                                       struct pt_regs *regs)
{
    /* NMI fires at ~watchdog_thresh rate via PMU overflow.
     * If hrtimer hasn't fired recently → hardlockup */
    watchdog_hardlockup_check(smp_processor_id(), regs);
}
```

The connection to Chapter 10: the hardlockup watchdog uses `perf_event_open`-equivalent infrastructure internally — a PMU counter configured to overflow at a fixed rate, generating an NMI.

## 6. Live Observation

```bash
# Check watchdog state
cat /proc/sys/kernel/nmi_watchdog        # 1=enabled
cat /proc/sys/kernel/watchdog_thresh     # 10 (seconds)
cat /proc/sys/kernel/softlockup_panic    # 0 or 1

# Temporarily disable watchdog (for long-running kernel operations)
sysctl kernel.nmi_watchdog=0
sysctl kernel.watchdog=0

# Check for past lockup events
dmesg -T | grep -E 'lockup|NMI|watchdog'

# bpftrace: count softlockup touches per CPU
bpftrace -e 'kprobe:touch_softlockup_watchdog { @[cpu]++; }
interval:s:5 { print(@); clear(@); }'

# Check per-CPU watchdog perf_event fds
ls /proc/$(pgrep -n watchdog)/fd 2>/dev/null
```

## 7. Key Kernel References

| Symbol | File | URL |
|--------|------|-----|
| `register_nmi_handler()` | `arch/x86/include/asm/nmi.h` | https://elixir.bootlin.com/linux/v6.9/source/arch/x86/include/asm/nmi.h |
| `do_nmi()` | `arch/x86/kernel/nmi.c` | https://elixir.bootlin.com/linux/v6.9/source/arch/x86/kernel/nmi.c |
| `watchdog_overflow_callback()` | `kernel/watchdog_perf.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/watchdog_perf.c |
| `touch_softlockup_watchdog()` | `include/linux/nmi.h` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/nmi.h |
| `watchdog_enable()` | `kernel/watchdog.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/watchdog.c |
| `is_hardlockup()` | `kernel/watchdog.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/watchdog.c |
