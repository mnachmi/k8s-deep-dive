# 11-b NMI, Hardlockup/Softlockup Watchdog

## 1. Source Locations

| File | Key Symbols | URL |
|------|-------------|-----|
| `arch/x86/include/asm/nmi.h` | `nmi_handler_t`, `register_nmi_handler()`, `NMI_LOCAL`, `NMI_UNKNOWN` | https://elixir.bootlin.com/linux/v6.9/source/arch/x86/include/asm/nmi.h |
| `arch/x86/kernel/nmi.c` | `do_nmi()`, `nmi_handle()`, NMI handler dispatch | https://elixir.bootlin.com/linux/v6.9/source/arch/x86/kernel/nmi.c |
| `kernel/watchdog.c` | `watchdog_enable()`, softlockup watchdog kthread | https://elixir.bootlin.com/linux/v6.9/source/kernel/watchdog.c |
| `kernel/watchdog_hld.c` | hardlockup detector: `watchdog_overflow_callback()` | https://elixir.bootlin.com/linux/v6.9/source/kernel/watchdog_hld.c |
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

int register_nmi_handler(unsigned int type, nmi_handler_t handler,
                         unsigned long flags, const char *name);
void unregister_nmi_handler(unsigned int type, const char *name);
```

All NMI handlers must complete in < ~100 µs. No sleeping, no locks that might be held by interrupted code.

## 4. Softlockup Watchdog

The softlockup detector uses a per-CPU high-priority kthread (`watchdog/N`, SCHED_FIFO priority 99). The system timer tick function `update_process_times()` stamps a per-CPU `watchdog_touch_ts` timestamp via `touch_softlockup_watchdog()`. The watchdog kthread resets `watchdog_report_ts`. If the kthread hasn't run for `2 × watchdog_thresh` seconds, a soft lockup is reported:

```
BUG: soft lockup - CPU#0 stuck for 22s! [nginx:1234]
```

Key sysctls:
- `kernel.watchdog_thresh` (default 10s) — trigger at `2 × thresh`
- `kernel.softlockup_panic` — 0/1 (default 0: print only)
- `kernel.nmi_watchdog` — 0=disabled, 1=enabled (hardlockup)

## 5. Hardlockup Watchdog

The hardlockup detector uses the CPU's PMU (Performance Monitoring Unit) to generate an NMI every `watchdog_thresh` seconds. If the NMI fires and the softlockup `hrtimer_interrupts` counter hasn't changed (meaning no hrtimer interrupts, meaning the CPU was completely locked), it reports a hardlockup:

```
NMI watchdog: Watchdog detected hard LOCKUP on cpu 0
```

Implementation in `kernel/watchdog_hld.c`:
```c
// Per-CPU perf_event configured as:
//   type = PERF_TYPE_HARDWARE
//   config = PERF_COUNT_HW_CPU_CYCLES (or NMI-capable PMU counter)
//   sample_period = hw_nmi_get_sample_period(watchdog_thresh)
//   overflow_handler = watchdog_overflow_callback

static void watchdog_overflow_callback(struct perf_event *event,
                                       struct perf_hw_data *data,
                                       struct pt_regs *regs)
{
    if (is_hardlockup())
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
| `watchdog_overflow_callback()` | `kernel/watchdog_hld.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/watchdog_hld.c |
| `touch_softlockup_watchdog()` | `include/linux/nmi.h` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/nmi.h |
| `watchdog_enable()` | `kernel/watchdog.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/watchdog.c |
| `is_hardlockup()` | `kernel/watchdog_hld.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/watchdog_hld.c |
