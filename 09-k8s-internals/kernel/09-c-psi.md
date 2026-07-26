# 09-c: PSI — `struct psi_group`, `/proc/pressure/*`, Per-cgroup PSI, kubelet Eviction

## Measuring the Pressure That Doesn't Kill You

An OOM kill is visible. The kernel logs it, dmesg records it, the process disappears. But OOM kills are late-stage events — by the time the kernel is killing processes, the system has been degraded for minutes. The real performance problem is the invisible period before the kill: tasks stalling because they cannot allocate memory, page reclaim consuming CPU, applications experiencing microsecond-scale delays that accumulate into latency spikes. The OOM counter is always zero while this is happening.

Johannes Weiner at Facebook observed this pattern in production. Facebook's systems had enough memory that OOM kills were rare, but they still experienced memory pressure events that degraded service latency. There was no way to measure how much of the CPU's time was being wasted waiting for memory, or which cgroup was causing the pressure. The metrics available — free memory, swap usage, OOM count — were all lagging indicators that told you the situation after it had already affected users.

PSI (Pressure Stall Information) was Weiner's solution, merged in Linux 4.20 (December 2018). PSI measures how many tasks are stalled waiting for a resource — memory, CPU, or I/O — at any given moment, and tracks what fraction of wall clock time is lost to those stalls. The metric has two variants: "some" stall (at least one task is stalled) and "full" stall (all non-idle tasks are stalled — the entire CPU is wasted waiting). Full stall is the more severe signal: it means the system is making zero forward progress in the affected cgroup.

Kubernetes 1.22 (2021) added PSI as an eviction signal. kubelet watches `/proc/pressure/memory` and per-cgroup `memory.pressure` files via inotify, comparing the `some avg10` values against configured thresholds. When memory pressure exceeds the threshold, kubelet begins evicting BestEffort pods before the pressure degrades further. This is early eviction — triggered by pressure, not by OOM — the difference between gracefully shedding load and emergency triage.

## 1. Source Locations

| File | Key Symbols | URL |
|------|-------------|-----|
| `kernel/sched/psi.c` | `psi_task_change()`, `psi_group_change()`, `psi_avgs_work()` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/psi.c |
| `include/linux/psi_types.h` | `struct psi_group`, `enum psi_task_count`, `enum psi_states` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/psi_types.h |
| `kernel/sched/psi.c` | `psi_show()` — formats /proc/pressure/* output | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/psi.c |
| `kernel/cgroup/cgroup.c` | PSI per-cgroup registration | https://elixir.bootlin.com/linux/v6.9/source/kernel/cgroup/cgroup.c |
| `include/linux/psi.h` | `psi_memstall_enter()`, `psi_memstall_leave()` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/psi.h |

## 2. PSI Overview

Pressure Stall Information (PSI, introduced in Linux 4.20, always enabled in modern distros) measures the fraction of time tasks are stalled waiting for a resource. It answers: "What fraction of my compute capacity am I losing to resource contention?"

Three resources tracked: **memory** (reclaim stalls), **CPU** (runqueue wait), **IO** (I/O wait).

Two stall categories:
- **some**: at least one runnable task is stalled (partial loss)
- **full**: ALL runnable tasks are stalled simultaneously (total loss; CPU has no "full" since there is always the idle task)

## 3. `struct psi_group`

```c
// include/linux/psi_types.h (simplified, Linux 6.9)
struct psi_group {
    struct mutex            avgs_lock;
    struct psi_group_cpu __percpu *pcpu;    // per-CPU stall state
    u64                     avg_last_update;
    u64                     avg_next_update;
    struct delayed_work     avgs_work;       // periodic average update (2s interval)

    u64                     total[NR_PSI_STATES];   // cumulative stall times (ns)
    unsigned long           avg[NR_PSI_TASK_COUNTS * 3]; // 10/60/300s exponential averages

    /* trigger/polling support */
    struct mutex            trigger_lock;
    struct list_head        triggers;
    u32                     nr_triggers[NR_PSI_STATES];
    u32                     poll_states;
    wait_queue_head_t       poll_wait;
    atomic_t                poll_scheduled;
    struct kthread_worker   *poll_kworker;
    struct kthread_delayed_work poll_work;

    bool                    enabled;
};
```

`NR_PSI_STATES = 7` (combinations of CPU/IO/memory stall bits). `NR_PSI_TASK_COUNTS = 3` (IOWAIT, MEMSTALL, RUNNING).

## 4. `/proc/pressure/*` File Format

```
# /proc/pressure/memory
some avg10=0.12 avg60=0.05 avg300=0.01 total=123456789
full avg10=0.04 avg60=0.01 avg300=0.00 total=45678901

# /proc/pressure/cpu
some avg10=2.45 avg60=1.23 avg300=0.89 total=987654321
# (no "full" line for CPU)

# /proc/pressure/io
some avg10=0.50 avg60=0.30 avg300=0.10 total=234567890
full avg10=0.20 avg60=0.10 avg300=0.03 total=89012345
```

- `avg10/60/300`: exponential moving averages over 10/60/300 seconds; value is percentage (0.00–100.00)
- `total`: cumulative stall time in **microseconds** since boot

The averages are updated every 2 seconds by `avgs_work` (a `delayed_work` in the PSI group). Each CPU updates its per-CPU stall counters on every scheduler tick and on task state transitions via `psi_task_change()`.

## 5. Per-cgroup PSI

Each cgroup v2 directory exposes:
- `memory.pressure` — memory PSI for tasks in this cgroup and its descendants
- `cpu.pressure` — CPU PSI
- `io.pressure` — I/O PSI

```bash
# /sys/fs/cgroup/kubepods.slice/kubepods-pod<uid>.slice/memory.pressure
some avg10=0.80 avg60=0.30 avg300=0.05 total=56789012
full avg10=0.20 avg60=0.05 avg300=0.01 total=12345678
```

The per-cgroup PSI is computed by the same `psi_group` machinery, but each cgroup has its own `struct psi_group psi` embedded directly in `struct cgroup` (defined in `include/linux/cgroup-defs.h`).

## 6. PSI Trigger Interface (inotify-free polling)

The kernel provides a **poll-based trigger** for PSI: userspace writes a threshold to `/proc/pressure/memory` and polls the file descriptor. The kernel wakes the poller when the accumulated stall exceeds the threshold:

```bash
# Register a trigger: wake if >50ms stall in any 1-second window
echo "some 50000 1000000" > /proc/pressure/memory
# Then poll() on the file descriptor
```

When the `MemoryPressureEviction` feature gate is enabled, kubelet can use this mechanism for memory pressure alerts — more efficient than periodic `stat(2)` polling. The default kubelet EvictionManager polls cgroup memory stats and `/proc/meminfo` for `memory.available` directly.

## 7. kubelet Eviction Thresholds

kubelet EvictionManager computes `memory.available` by reading cgroup memory stats and comparing to the node's total allocatable memory from `/proc/meminfo`.

Default eviction thresholds (configurable via `--eviction-hard`):

| Signal | Hard threshold | Effect |
|--------|---------------|--------|
| `memory.available` | `< 100Mi` | Immediate pod eviction |
| `nodefs.available` | `< 10%` | Immediate pod eviction |
| `imagefs.available` | `< 15%` | Immediate pod eviction |

Soft eviction thresholds (graceful, via `--eviction-soft`):

| Signal | Soft threshold | Grace period | Effect |
|--------|---------------|-------------|--------|
| `memory.available` | `< 1.5Gi` | 1m30s | Evict after sustained pressure |

Eviction order: BestEffort pods first, then Burstable pods exceeding their requests, then Guaranteed pods (only if the node itself is under pressure).

## 8. Live Observation

```bash
# Node-level PSI snapshot
cat /proc/pressure/memory
cat /proc/pressure/cpu

# Watch per-pod PSI (requires cgroup v2)
watch -n1 "cat /sys/fs/cgroup/kubepods.slice/kubepods-pod<uid>.slice/memory.pressure"

# bpftrace: trace PSI stall entry (when a task starts stalling on memory)
bpftrace -e 'kprobe:__psi_memstall_enter { printf("memstall pid=%d comm=%s\n", pid, comm); }'

# Monitor PSI totals and compute delta (poor man's pressure meter)
while true; do
    awk '/some/{printf "mem_some_total=%s\n", $NF}' /proc/pressure/memory
    sleep 5
done
```

## 9. Key Kernel References

| Symbol | File | URL |
|--------|------|-----|
| `struct psi_group` | `include/linux/psi_types.h` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/psi_types.h |
| `psi_task_change()` | `kernel/sched/psi.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/psi.c |
| `psi_avgs_work()` | `kernel/sched/psi.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/psi.c |
| `psi_show()` | `kernel/sched/psi.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/psi.c |
| `psi_memstall_enter()` | `include/linux/psi.h` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/psi.h |
| `psi_memstall_leave()` | `include/linux/psi.h` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/psi.h |
