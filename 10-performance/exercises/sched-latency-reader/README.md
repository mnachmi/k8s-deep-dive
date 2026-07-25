# sched-latency-reader

Reads scheduler latency from `/proc/<pid>/schedstat` (wait time on the runqueue)
and CPU throttle metrics from a pod cgroup's `cpu.stat`.

## Build and Run

```
go build -o sched-latency-reader .

# Scheduler latency for one process
./sched-latency-reader --pid 1234

# CPU throttle report for a pod
./sched-latency-reader --pod <pod-uid>

# Both: throttle report + per-process schedstat
./sched-latency-reader --pod <pod-uid> --all
```

## Sample Output

```
=== /proc/1234/schedstat ===
  pid=1234    comm=nginx            runtime=5234.1ms  wait=  87.3ms  wait_ratio= 1.6%  switches=4231

=== CPU throttle for pod abc123 ===
  cgroup: /sys/fs/cgroup/kubepods.slice/kubepods-burstable-podabc123.slice
  usage_usec:     5234567
  nr_periods:     1000
  nr_throttled:   50
  throttled_usec: 5000000
  throttle_ratio: 5.0%
```

## Kernel Paths

| Field | Kernel Source |
|-------|--------------|
| `/proc/<pid>/schedstat` | `kernel/sched/stats.c` — `sched_statistics.wait_sum` |
| `cpu.stat nr_throttled` | `kernel/sched/fair.c` — `struct cfs_bandwidth.nr_throttled` |
| `cpu.stat throttled_usec` | `kernel/sched/fair.c` — `cfs_bandwidth.throttled_time` |

Sources:
https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/stats.c
https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/fair.c

## Exercises

a) Add `--watch` flag that re-runs every 5 seconds and prints the delta in
   wait_ns (to compute wait rate in ns/s rather than cumulative total).

b) Read `/proc/schedstat` (system-wide) and compute average runqueue
   latency per CPU: `run_delay / pcount` for each CPU line.

c) Add parsing of `cpu.max` alongside `cpu.stat` and compute the
   theoretical maximum throttle-free throughput for the pod.
