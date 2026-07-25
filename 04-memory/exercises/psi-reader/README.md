# psi-reader

A Go tool that reads and displays Linux PSI (Pressure Stall Information) metrics from `/proc/pressure/` and per-cgroup pressure files.

## What It Demonstrates

- **Parsing PSI files** from `/proc/pressure/{cpu,memory,io}` and per-cgroup `{cpu,memory,io}.pressure` files
- **The some/full distinction**: `some` counts time when at least one task was stalled; `full` counts time when ALL runnable tasks were stalled simultaneously
- **avg10/avg60/avg300**: exponential moving averages over the last 10, 60, and 300 seconds (expressed as a percentage of wall-clock time)
- **Total microsecond counters**: monotonically increasing count of total stall time in microseconds since boot
- **Per-cgroup PSI**: each cgroup in `/sys/fs/cgroup/` exposes the same pressure files, scoped to tasks within that cgroup

## Build and Run

```bash
# System-wide PSI only
make run

# System-wide + PSI for a specific PID's cgroup
make run-pid PID=1

# System-wide + PSI for a specific cgroup path
make run-cgroup CGROUP=/sys/fs/cgroup/user.slice
```

Or build and run directly:

```bash
make build
./psi-reader
./psi-reader --pid 1
./psi-reader --cgroup /sys/fs/cgroup/user.slice
```

## PSI Value Interpretation

PSI averages represent the **percentage of time** in the given window where tasks experienced stalls:

| Value | Meaning |
|-------|---------|
| `avg10=5.00` | 5% of the last 10 seconds had at least one stalled task |
| `avg60=2.00` | 2% of the last 60 seconds had stalls (smoothed over a longer window) |
| `avg300=0.50` | Sustained low pressure over the last 5 minutes |

**When to be concerned:**

- `memory some avg60 > 10%` — active reclaim is happening; the kernel is under memory pressure
- `memory full > 0%` — serious memory pressure; all runnable tasks are blocked waiting for memory
- `io full > 5%` — storage is becoming a bottleneck
- High `full` values often precede OOM kills and pod evictions

`cpu` has no `full` line because CPU pressure is always "some" (there is always at least one task that can run).

## Kernel Reference

PSI averages are updated every 2 seconds by `psi_avgs_work()` in the kernel scheduler:

- Source: [`kernel/sched/psi.c:psi_avgs_work()`](https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/psi.c)
- The exponential decay uses the same EWMA formula as CPU load averages
- PSI was introduced in Linux 4.20 (2018) by Johannes Weiner (Facebook)

## Exercises

**(a) Generate memory pressure and watch PSI climb:**

```bash
# In one terminal, apply memory pressure
stress-ng --vm 1 --vm-bytes 4G --vm-keep

# In another terminal, watch PSI every 2 seconds
watch -n 2 ./psi-reader
```

**(b) Add a `--watch` flag** that loops every 2 seconds and clears the screen between updates, so you can observe PSI values changing in real time as memory pressure increases.

**(c) Add a `--json` flag** that outputs PSI data as JSON instead of the human-readable table, suitable for ingestion by monitoring pipelines or `jq` processing.

## Kubernetes Connection

The **kubelet eviction manager** reads these same PSI files to decide when to evict pods:

- Classic eviction (`evictionHard: memory.available: "100Mi"`) triggers when `/proc/meminfo` AvailableMemory falls below the threshold
- PSI-based eviction (available from kubelet v1.27+) triggers when `memory.pressure some avg10` exceeds a configured threshold, *before* memory is actually exhausted
- This allows earlier, more graceful eviction compared to waiting for OOM conditions

Watching PSI rise during a `stress-ng` run lets you see exactly what the kubelet observes before it starts evicting pods.
