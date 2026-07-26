# kernel-health-reader

Reads kernel health state from `/proc` and `/sys` to answer the most common
node audit questions an operator needs before a production incident.

## What It Shows

| Part | Source | What you learn |
|------|--------|---------------|
| a | `/proc/version` | Kernel version string |
| b | `/proc/sys/kernel/tainted` | Active taint flags (decoded per-bit) |
| c | `/proc/sys/kernel/nmi_watchdog` | Watchdog enabled + threshold |
| d | `/proc/sys/kernel/panic*` | Panic reboot and oops-panic config |
| e | `/sys/kernel/kexec_loaded` + `/proc/cmdline` | kdump readiness |

## Build and Run

```
make
./kernel_health_reader
```

## Kernel Paths

```
/proc/sys/kernel/tainted
  → include/linux/panic.h — TAINT_* bit definitions
  → kernel/panic.c — add_taint()

/sys/kernel/kexec_loaded
  → kernel/kexec_core.c — kexec_crash_loaded()

/proc/sys/kernel/nmi_watchdog
  → kernel/watchdog.c — watchdog_enable()
```

Sources:
https://elixir.bootlin.com/linux/v6.9/source/include/linux/panic.h
https://elixir.bootlin.com/linux/v6.9/source/kernel/kexec_core.c
https://elixir.bootlin.com/linux/v6.9/source/kernel/watchdog.c

## Exercises

a) Add Part f: read `/proc/sys/kernel/dmesg_restrict` and
   `/proc/sys/kernel/perf_event_paranoid`. Print their values and explain
   the security implications for an unprivileged container.

b) Add taint reason detection: for bit 7 (D: oops), scan `dmesg` for the
   most recent "Oops:" line and print the timestamp.

c) Extend Part e to also read `/proc/iomem` and print the "Crash kernel"
   reserved memory range if present.
