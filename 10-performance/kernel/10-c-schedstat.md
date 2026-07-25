# 10-c — Scheduler Latency Accounting: `struct sched_statistics`, `/proc/schedstat`, `cpu.stat` Throttle

## 1. Source Locations

| File | Key Symbols | URL |
|------|-------------|-----|
| `kernel/sched/stats.h` | `schedstat_*` macros, `struct sched_statistics` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/stats.h |
| `kernel/sched/stats.c` | `/proc/schedstat`, `/proc/<pid>/schedstat` handlers | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/stats.c |
| `kernel/sched/fair.c` | `update_curr()`, `cfs_rq->exec_clock`, CFS latency stats | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/fair.c |
| `kernel/sched/fair.c` | `throttle_cfs_rq()`, `unthrottle_cfs_rq()`, `cfs_b->throttled_time` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/fair.c |
| `kernel/sched/cpufreq_schedutil.c` | `sugov_update_shared()` — cpufreq/scheduler interaction | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/cpufreq_schedutil.c |

## 2. `struct sched_statistics`

When `CONFIG_SCHEDSTATS=y`, each `struct task_struct` embeds `struct sched_statistics stats` via `struct sched_entity.statistics`. The entire struct is compiled out when `CONFIG_SCHEDSTATS` is not set — all fields and the `schedstat_*` helper macros become no-ops.

```c
// kernel/sched/stats.h (Linux 6.9)
struct sched_statistics {
#ifdef CONFIG_SCHEDSTATS
    u64         wait_start;            // timestamp when task entered runqueue
    u64         wait_max;              // max time waiting on runqueue (ns)
    u64         wait_count;            // number of times task waited on runqueue
    u64         wait_sum;              // total time waiting on runqueue (ns) ← KEY
    u64         iowait_count;
    u64         iowait_sum;

    u64         sleep_start;
    u64         sleep_max;
    s64         sum_sleep_runtime;

    u64         block_start;
    u64         block_max;
    u64         exec_max;
    u64         slice_max;

    u64         nr_migrations_cold;
    u64         nr_failed_migrations_affine;
    u64         nr_failed_migrations_running;
    u64         nr_failed_migrations_hot;
    u64         nr_forced_migrations;

    u64         nr_wakeups;
    u64         nr_wakeups_sync;
    u64         nr_wakeups_migrate;
    u64         nr_wakeups_local;
    u64         nr_wakeups_remote;
    u64         nr_wakeups_affine;
    u64         nr_wakeups_affine_attempts;
    u64         nr_wakeups_passive;
    u64         nr_wakeups_idle;
#endif
};
```

`wait_sum` accumulates every time `update_stats_wait_end()` is called when the task leaves the runqueue to execute. It stores the total scheduler latency experienced by the task since boot. `wait_start` is set by `update_stats_wait_start()` when the task is enqueued; the delta is added to `wait_sum` on dequeue.

The migration counters (`nr_failed_migrations_affine`, `nr_forced_migrations`, etc.) are useful for diagnosing NUMA locality problems: high `nr_forced_migrations` combined with elevated `wait_sum` can indicate that load balancing is fighting CPU affinity settings.

## 3. `/proc/<pid>/schedstat`

Format — 3 space-separated fields on a single line:

```
<sum_exec_runtime_ns> <wait_sum_ns> <nr_switches>
```

- `sum_exec_runtime_ns`: total nanoseconds the task has been running on CPU since it started
- `wait_sum_ns`: total nanoseconds the task has spent waiting on the runqueue (scheduler latency, not IO wait)
- `nr_switches`: total number of voluntary and involuntary context switches

Example:

```
# cat /proc/1234/schedstat
5234567890 876543210 4231
```

This task has spent 5.23 s on CPU, 876 ms waiting in the runqueue, and has been context-switched 4231 times.

**Scheduler latency ratio**: `wait_sum / (wait_sum + sum_exec_runtime)`. For the example above: `876 / (876 + 5234)` ≈ 14 %. A ratio above 10 % on a non-IO-bound process is a strong signal of CPU contention or CFS throttling. IO-bound workloads (databases, network proxies) naturally accumulate IO sleep time in other counters, but `wait_sum` reflects only runqueue wait — the time when the task was runnable but could not get a CPU.

Note: if `/proc/<pid>/schedstat` reports `0 0 0`, the kernel was built without `CONFIG_SCHEDSTATS=y`. Typical production distro kernels (Debian, Ubuntu, RHEL 9, GKE nodes, EKS nodes) enable `CONFIG_SCHEDSTATS` by default. Verify with `grep CONFIG_SCHEDSTATS /boot/config-$(uname -r)`.

## 4. `/proc/schedstat` — System-Wide

`/proc/schedstat` exposes per-CPU runqueue statistics collected by the scheduler. The first line is always a version identifier:

```
version 15
```

Per-CPU lines follow this format:

```
cpu0 <yld_count> 0 <sched_count> <sched_goidle> <ttwu_count> <ttwu_local> <rq_cpu_time> <run_delay> <pcount>
```

Key fields (0-indexed from the `cpuN` label):

| Field | Position | Meaning |
|-------|----------|---------|
| `yld_count` | 1 | Number of times `sched_yield()` was called |
| `sched_count` | 3 | Number of scheduling decisions made |
| `sched_goidle` | 4 | Times the CPU went idle |
| `ttwu_count` | 5 | Total `try_to_wake_up()` calls |
| `ttwu_local` | 6 | Wakeups where task ran on same CPU |
| `rq_cpu_time` | 7 | Total CPU time spent running tasks (ns) |
| `run_delay` | 8 | Cumulative nanoseconds tasks waited on runqueue |
| `pcount` | 9 | Number of tasks that ran on this CPU |

`run_delay / pcount` gives the average scheduler latency per task-run slice on that CPU. Comparing this ratio across CPUs highlights imbalanced load: a CPU with disproportionately high `run_delay/pcount` may be oversubscribed or pinned with too many high-priority tasks.

Example parse:

```
# head -3 /proc/schedstat
version 15
timestamp 4294967295
cpu0 0 0 857432 10234 412876 301234 8765432100 2345678900 98765
```

For cpu0: average latency = 2345678900 ns / 98765 task-runs ≈ 23.7 ms per run — indicative of heavy contention.

## 5. CFS Throttle Metrics in `cpu.stat`

The CFS bandwidth controller enforces CPU quotas defined by `cpu.max` in cgroupv2. When a cgroup exhausts its quota within a period, `throttle_cfs_rq()` in `kernel/sched/fair.c` removes the CFS runqueue from the timeline by calling `__dequeue_entity()` and setting the `throttled` flag on `struct cfs_rq`. The cgroup accumulates the throttled duration in `struct cfs_bandwidth.throttled_time`. When the next period begins, `unthrottle_cfs_rq()` re-enqueues the runqueue.

This throttle accounting is exposed through `cpu.stat`:

```
# cat /sys/fs/cgroup/kubepods.slice/.../cpu.stat
usage_usec       5234567
user_usec        4100000
system_usec      1134567
nr_periods       1000          ← number of CFS periods elapsed
nr_throttled     50            ← periods where quota was exhausted
throttled_usec   5000000       ← total throttled time (microseconds)
nr_bursts        0
burst_usec       0
```

**Field meanings:**

- `nr_periods`: total number of CFS accounting periods elapsed since the cgroup was created
- `nr_throttled`: number of those periods during which the cgroup was throttled at least once
- `throttled_usec`: total microseconds spent in the throttled state (sum of all throttle durations)
- `nr_bursts` / `burst_usec`: periods and time where burst credit was used (requires `cpu.max` burst configuration)

**Throttle ratio** = `throttled_usec / (nr_periods × period_us)`

For `cpu.max = "50000 100000"` (0.5 CPU quota, 100 ms period):

- `period_us = 100000`
- Over 100 periods (10 s total): allowed CPU time = 100 × 50 ms = 5 s
- If `nr_throttled = 50` and `throttled_usec = 2500000` (2.5 s):
  - Throttle ratio = 2.5 s / 5 s = **50%** of allowed CPU time lost to throttling
  - Effective CPU utilization = 0.5 CPU × (1 − 0.5) = 0.25 CPU

A throttle ratio above 5% for a latency-sensitive workload (API servers, gRPC handlers, admission webhooks) is a strong signal to increase `resources.limits.cpu` or switch to a larger period with the same quota ratio. Note that `nr_throttled / nr_periods` (throttle frequency) and `throttled_usec / nr_throttled` (average throttle duration per event) together distinguish bursty short throttles from sustained heavy throttles.

## 6. Latency Top Observation

```bash
# Read /proc/<pid>/schedstat (3-field format: exec_ns wait_ns switches)
cat /proc/$(pgrep -n nginx)/schedstat

# System-wide runqueue latency per CPU
awk '/^cpu[0-9]/{
    run_delay=$9; pcount=$10;
    if (pcount > 0) printf "%s avg_latency=%.2fms\n", $1, run_delay/pcount/1e6
}' /proc/schedstat

# Monitor throttling for a pod (watch cpu.stat change)
CGROUP=/sys/fs/cgroup/kubepods.slice/$(kubectl get pod <name> -o json | jq -r '.metadata.uid' | sed 's/-//g')
watch -n1 "awk '/nr_throttled|throttled_usec|nr_periods/{print}' $CGROUP/cpu.stat"

# bpftrace: measure time spent waiting on runqueue
bpftrace -e '
tracepoint:sched:sched_stat_wait {
    @wait_ns[args->comm] = sum(args->delay);
}
interval:s:5 { print(@wait_ns); clear(@wait_ns); }'
```

The `sched_stat_wait` tracepoint fires each time a task leaves the runqueue to execute; `args->delay` is the nanosecond wait since enqueue. This mirrors exactly what `wait_sum` accumulates in `struct sched_statistics`, but aggregates it live by process name without needing `CONFIG_SCHEDSTATS`.

To find a pod's cgroup path more reliably on containerd nodes:

```bash
# Get container ID from kubectl
CID=$(kubectl get pod <name> -o jsonpath='{.status.containerStatuses[0].containerID}' | sed 's|containerd://||')
# Find its cgroup
find /sys/fs/cgroup/kubepods.slice -name "cgroup.controllers" -path "*${CID:0:12}*" | head -1 | xargs dirname
```

## 7. Live Observation — CFS Throttle Tracing

```bash
# Trace CFS throttle events via bpftrace kprobe
bpftrace -e 'kprobe:throttle_cfs_rq { printf("throttle: cpu=%d\n", cpu); }'

# Watch cpu.stat for rising nr_throttled
while true; do
    awk '/nr_throttled/{print systime(), $0}' \
        /sys/fs/cgroup/kubepods.slice/kubepods-pod*.slice/cpu.stat 2>/dev/null
    sleep 5
done
```

To correlate throttle events with specific containers, extend the kprobe to capture the cgroup name:

```bash
bpftrace -e '
kprobe:throttle_cfs_rq {
    printf("throttle cpu=%d cgroup=%s\n", cpu,
           cgroup);
}' 2>/dev/null
```

On kernels where the `cgroup` builtin is available in bpftrace (≥ 0.16), this prints the cgroupv2 path for the throttled runqueue. On older bpftrace, use:

```bash
# Alternative: use ftrace to capture throttle_cfs_rq with cgroup context
echo 1 > /sys/kernel/debug/tracing/events/cgroup/cgroup_throttle_cfs_rq/enable
cat /sys/kernel/debug/tracing/trace_pipe
```

To compute per-container throttle rate from `cpu.stat` snapshots:

```bash
# Snapshot throttled_usec, sleep 10s, snapshot again — rate = delta/10
for STAT in /sys/fs/cgroup/kubepods.slice/*/cpu.stat; do
    T1=$(awk '/^throttled_usec/{print $2}' "$STAT")
    sleep 10
    T2=$(awk '/^throttled_usec/{print $2}' "$STAT")
    RATE=$(( (T2 - T1) / 10 ))
    echo "$STAT: ${RATE} usec/s throttled"
done
```

A container with throttled_usec growing at more than 5000 usec/s (0.5% of wall time) under production load warrants a CPU limit increase.

## 8. Key Kernel References

| Symbol | File | URL |
|--------|------|-----|
| `struct sched_statistics` | `kernel/sched/stats.h` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/stats.h |
| `update_stats_wait_end()` | `kernel/sched/stats.h` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/stats.h |
| `/proc/<pid>/schedstat` handler | `kernel/sched/stats.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/stats.c |
| `throttle_cfs_rq()` | `kernel/sched/fair.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/fair.c |
| `struct cfs_bandwidth` | `kernel/sched/sched.h` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/sched.h |
| `update_curr()` | `kernel/sched/fair.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/fair.c |

**Notes on availability:**

`struct sched_statistics` is only compiled in when the kernel is built with `CONFIG_SCHEDSTATS=y`. If `/proc/<pid>/schedstat` returns `0 0 0`, the kernel lacks this option. Verify: `grep CONFIG_SCHEDSTATS /boot/config-$(uname -r)`. Typical production distro kernels (Debian bookworm, Ubuntu 22.04/24.04, RHEL 9, GKE nodes, EKS nodes) ship with `CONFIG_SCHEDSTATS=y`.

The `sched_stat_wait` tracepoint in section 6 does not require `CONFIG_SCHEDSTATS` and works on any kernel with `CONFIG_TRACEPOINTS=y` (universal on modern distros). It is the preferred method for per-process scheduler latency measurement in production environments where kernel recompilation is not feasible.
