# node-health-reader

Reads Linux kernel health state for Kubernetes node audit: version, taint
flags, watchdog config, panic settings, and kdump readiness.

## Build and Run

```
go build -o node-health-reader .

# Full health report
./node-health-reader

# Include /dev/kmsg scan for OOM/BUG/WARN events
./node-health-reader --kmsg

# JSON output (for integration with monitoring)
./node-health-reader --json
```

## Sample Output

```
=== Kernel Health Report ===
  release:          6.9.3-cloud-amd64
  version:          Linux version 6.9.3 ...

=== Taint State ===
  tainted: 512
    bit  9 (W): WARN_ON fired

=== Panic Configuration ===
  kernel.panic:             10  (0=halt, >0=reboot after Ns)
  kernel.panic_on_oops:     1   (1=oops triggers panic)
  kernel.softlockup_panic:  0   (1=soft lockup triggers panic)

=== Watchdog State ===
  kernel.nmi_watchdog:      1   (1=hardlockup detection on)
  kernel.watchdog_thresh:   10s (softlockup at 20s, hardlockup at ~10s)

=== kdump Readiness ===
  kexec_loaded:  1  (1=crash kernel loaded)
  crashkernel:   512M
```

## Kernel Paths

| Source | Kernel Reference |
|--------|-----------------|
| `/proc/sys/kernel/tainted` | `include/linux/panic.h` — `TAINT_*` flags |
| `/sys/kernel/kexec_loaded` | `kernel/kexec_core.c` — `kexec_crash_loaded()` |
| `/dev/kmsg` | `kernel/printk/printk.c` — structured ring buffer |
| `/proc/sys/kernel/nmi_watchdog` | `kernel/watchdog.c` |

Sources:
https://elixir.bootlin.com/linux/v6.9/source/include/linux/panic.h
https://elixir.bootlin.com/linux/v6.9/source/kernel/kexec_core.c

## Exercises

a) Add `--taint <bit>` flag that explains what a specific taint bit means
   and shows how to check if it is set.

b) Extend `scanKmsg` to parse the timestamp field and show relative time
   (e.g., "3m ago") for each event.

c) Add a `--watch` flag that re-runs the health check every 30 seconds
   and prints a diff of what changed.
