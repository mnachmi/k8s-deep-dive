# Chapter 10 — Performance: Kubernetes Connection

## Performance Signals Live in the Kernel

Every performance metric that Prometheus scrapes from Kubernetes is a derived representation of a kernel counter. The container's CPU usage is computed from `cpuacct.usage` (or `cpu.stat` in cgroup v2). The memory usage is `memory.current`. The network bytes are aggregated from per-interface counters in the network namespace. The I/O throughput is `io.stat`. These Prometheus metrics are useful for dashboards and alerting, but they are aggregated, delayed, and transformed. When a performance problem is actively affecting users, the kernel counters are faster and more authoritative.

The performance investigation that starts with a Prometheus query typically ends with a kernel interface. High CPU utilization points to `cpu.stat` for throttle rate. High p99 latency with low CPU points to `schedstat` for wait time. Memory pressure without OOM kills points to `/proc/pressure/memory` for PSI values. Cache miss-driven slowness points to `perf stat -e cache-misses` for PMU counters. Network throughput degradation points to `ss -t -e` for TCP socket state and retransmit counters.

The sections below map each category of Kubernetes performance problem to its kernel signal and to the specific tool and file that provides the data. The goal is to cut the investigation path — from Prometheus anomaly to root cause — from hours to minutes.

## 1. Architecture Overview

| Performance Problem | Kernel Signal | K8s Interface |
|--------------------|--------------|---------------|
| CPU starvation | `/proc/<pid>/schedstat` field 2 (`run_delay`) | CPU manager static policy (cpuset isolation) |
| CPU throttling | `cpu.stat throttled_usec` rising | `resources.limits.cpu` too low |
| Cache miss storm | `PERF_COUNT_HW_CACHE_MISSES` via `perf_event_open` | `resources.limits.memory` (NUMA cross-socket) |
| Branch mispredicts | `PERF_COUNT_HW_BRANCH_MISSES` via `perf_event_open` | CPU pinning (Topology Manager) |
| Page fault flood | `PERF_COUNT_SW_PAGE_FAULTS_MAJ` via `perf_event_open` | `memory.events` `max` counter in cgroup |
| High involuntary context switches | `/proc/<pid>/status` `nonvoluntary_ctxt_switches` | RT task competing on shared cpuset |

The CFS bandwidth controller (`struct cfs_bandwidth` in `kernel/sched/sched.h`) is the bridge between `resources.limits.cpu` in a Pod spec and kernel scheduling enforcement. Kubelet translates the CPU limit to `cpu.max` (cgroupv2) or `cpu.cfs_quota_us` + `cpu.cfs_period_us` (cgroupv1). When a cgroup exhausts its quota in a period, `throttle_cfs_rq()` in `kernel/sched/fair.c` dequeues the CFS runqueue and sets it throttled until the next period fires via `sched_cfs_period_timer()`.

https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/sched.h
https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/fair.c

## 2. CPU Throttle Diagnosis Flow

```
Symptom: high p99 latency despite low average CPU
     │
     ▼
Check cpu.stat throttled_usec
     │  if rising → quota exhausted mid-request
     ▼
Compute throttle ratio:
     throttled_usec / (nr_periods × period_us)
     │  > 5% → increase limits.cpu
     │  spiky (high nr_throttled, low throttled_usec per event) → shorten period
     ▼
Check cpu.max: "<quota_us> <period_us>"
     │  default period = 100ms — long period amplifies burstiness
     │  → reduce period to 10ms: kubelet --cpu-cfs-period=10000
     ▼
If throttle ratio acceptable but latency high:
     check /proc/<pid>/schedstat field 2 (run_delay) vs field 1 (sum_exec_runtime)
     │  high ratio → scheduler contention (noisy neighbor)
     ▼
Identify noisy neighbor:
     perf sched latency -p <pid>
     bpftrace -e 'tracepoint:sched:sched_stat_wait { @[args->comm]=sum(args->delay); }'
```

**Reading `cpu.stat`:**

```bash
# On a cgroup v2 node, find the pod's cgroup by UID
PODUID=$(kubectl get pod <name> -o jsonpath='{.metadata.uid}')
CGUID=$(echo $PODUID | tr -d '-')
CGROUP=$(find /sys/fs/cgroup/kubepods.slice -maxdepth 2 -name "pod${CGUID}.slice" | head -1)
cat $CGROUP/cpu.stat
```

Example `cpu.stat` output for a throttled pod:

```
usage_usec       8321045
user_usec        6710000
system_usec      1611045
nr_periods       1200
nr_throttled     180
throttled_usec   18000000
nr_bursts        0
burst_usec       0
```

Throttle ratio = `18000000 / (1200 × 100000)` = **15%** — well above the 5% threshold for latency-sensitive workloads. The fix is either to raise `resources.limits.cpu` or to reduce the period with `--cpu-cfs-period=10000` (keeping the quota proportional).

**Why short periods help:** with a 100ms period and a 50ms quota (0.5 CPU), a single burst at the start of the period exhausts the quota and throttles the container for the remaining 50ms of that period. With a 10ms period and a 5ms quota (same effective 0.5 CPU), the maximum throttle gap shrinks to 5ms — invisible to most request latencies.

## 3. `perf_event_open` Inside a Container

Running `perf` inside a container requires satisfying all three kernel gates:

1. **Capability**: `CAP_PERFMON` (Linux 5.8+) or `CAP_SYS_ADMIN` for system-wide profiling; per-process profiling of own processes also requires `CAP_PERFMON` if `perf_event_paranoid > 1`.
2. **`perf_event_paranoid` sysctl**: values and their effect:
   - `3`: block all `perf_event_open` for non-root (Debian/Ubuntu 20.04+ default)
   - `2`: allow per-process profiling for non-root (old default)
   - `1`: allow kernel profiling for non-root
   - `≤ 0`: allow all, including hardware PMU
3. **seccomp**: RuntimeDefault seccomp profile (the default on GKE, EKS, AKS, and most managed clusters) blocks `perf_event_open` (syscall 298 on x86-64). Use a custom seccomp profile or `securityContext.seccompProfile.type: Unconfined` with `CAP_PERFMON` for a profiling DaemonSet.

The `perf_event_open` syscall is defined in `kernel/events/core.c`. The capability check inside it is:

```c
// kernel/events/core.c — perf_event_open() permission path
if (perf_paranoid_kernel() && !capable(CAP_PERFMON))
    return -EACCES;
```

https://elixir.bootlin.com/linux/v6.9/source/kernel/events/core.c

**Profile a container process from the node (no container privileges needed):**

```bash
# Get the container PID from containerd
CPID=$(crictl inspect <container-id> | jq '.info.pid')

# perf stat: hardware counters for 10 seconds
perf stat -e cycles,instructions,cache-misses,branch-misses -p $CPID sleep 10

# Flamegraph: CPU sampling at 99 Hz for 30 seconds
perf record -F 99 -p $CPID -g -- sleep 30
perf script | stackcollapse-perf.pl | flamegraph.pl > flame.svg
```

**Profile inside a privileged DaemonSet pod (requires CAP_PERFMON + Unconfined seccomp):**

```yaml
# DaemonSet profiler pod spec excerpt
securityContext:
  capabilities:
    add: ["CAP_PERFMON"]
  seccompProfile:
    type: Unconfined
hostPID: true
```

With `hostPID: true`, the profiling pod sees all PIDs on the node and can run `perf stat -p <container-pid>` directly.

## 4. Scheduler Latency — Identifying CFS Throttle vs. CPU Starvation

`/proc/<pid>/schedstat` exposes three fields produced by `proc_pid_schedstat()` in `fs/proc/base.c`:

```
<sum_exec_runtime_ns>  <run_delay_ns>  <pcount>
```

- Field 1 (`sum_exec_runtime`): nanoseconds the task has run on CPU
- Field 2 (`run_delay`): nanoseconds the task has spent waiting on the runqueue (`sched_info.run_delay`, `CONFIG_SCHED_INFO`)
- Field 3 (`pcount`): number of times the task was scheduled onto a CPU

**Distinguishing throttle from starvation:**

| Condition | `run_delay` trend | `nr_throttled` | Diagnosis |
|-----------|------------------|----------------|-----------|
| CPU throttle | grows when at quota | high | Raise `limits.cpu` |
| CPU starvation (noisy neighbor) | grows continuously | low | CPU manager static policy |
| IO wait masquerading as latency | flat | low | Check PSI `io.some` |
| Correct sizing | flat or slow growth | 0 | No action |

A task that is throttled by CFS will show `run_delay` increasing in batches (each throttle event adds a burst of wait time) while `nr_throttled` in `cpu.stat` is elevated. A task suffering noisy-neighbor starvation will show `run_delay` growing steadily with `nr_throttled` near zero — the quota is not exhausted, but the scheduler queue is long because competing tasks are consuming CPU.

**Snapshot schedstat for all pod processes:**

```bash
# Compute wait ratio for every process in a kubepods cgroup
for pid in $(ls /proc | grep -E '^[0-9]+$'); do
  cg=$(cat /proc/$pid/cgroup 2>/dev/null | grep kubepods)
  [ -z "$cg" ] && continue
  read rt wait sw < /proc/$pid/schedstat 2>/dev/null
  [ "$rt" -gt 0 ] 2>/dev/null || continue
  ratio=$(awk "BEGIN{printf \"%.1f\", $wait * 100 / ($wait + $rt)}")
  comm=$(cat /proc/$pid/comm 2>/dev/null)
  echo "pid=$pid comm=$comm wait_ratio=${ratio}% nr_switches=$sw"
done
```

A `wait_ratio` above 10% for a non-IO-bound container process is a strong signal of CPU contention. Cross-reference with `cpu.stat nr_throttled` to distinguish throttle from starvation.

**Note on zero schedstat values:** if the output is `0 0 0`, the kernel lacks `CONFIG_SCHED_INFO=y`. Verify with `grep CONFIG_SCHED_INFO /boot/config-$(uname -r)`. GKE and EKS node kernels enable this by default.

## 5. K8s Performance Best Practices

| Practice | Kernel Mechanism | K8s Config |
|---------|-----------------|------------|
| Reduce CPU throttle | Shorter CFS period reduces max throttle gap | `--cpu-cfs-period=10000` (10ms) on kubelet |
| Eliminate noisy neighbor | Dedicated cpuset via `cpuset.cpus` | CPU manager static policy + Guaranteed QoS |
| Profile without privileges | `perf_event_paranoid≤2` allows per-process perf | DaemonSet with `hostPID` + `CAP_PERFMON` |
| NUMA-local memory | `cpuset.mems` pins memory to local NUMA node | Topology Manager `single-numa-node` policy |
| HugePages | THP + `hugetlbfs` reduces TLB miss rate | `resources.limits.hugepages-2Mi` in Pod spec |

**CPU manager static policy:** when kubelet runs with `--cpu-manager-policy=static`, Guaranteed pods with integer CPU requests receive exclusive CPUs written to `cpuset.cpus`. This eliminates noisy-neighbor CPU contention entirely for those workloads. The kernel enforces exclusivity via `cpuset_can_attach()` in `kernel/cgroup/cpuset.c` — no other task can run on those CPUs.

https://elixir.bootlin.com/linux/v6.9/source/kernel/cgroup/cpuset.c

**Topology Manager alignment:** for NUMA-sensitive workloads (ML inference, in-memory databases), `--topology-manager-policy=single-numa-node` ensures CPU, memory, and device (GPU/RDMA NIC) allocations all land on the same NUMA node. This avoids cross-socket cache miss storms that `PERF_COUNT_HW_CACHE_MISSES` reveals. Combine with `--memory-manager-policy=Static` so kubelet also pins `cpuset.mems`.

**CFS burst (Linux 5.14+):** `cpu.max.burst` allows a cgroup to accumulate unused quota up to a burst ceiling, absorbing short CPU spikes without throttling. CFS burst support (kernel `cpu.max.burst`, Linux 5.14; check your Kubernetes version's feature gates for status). Use carefully: burst can cause latency spikes for neighboring workloads on shared nodes.

## 6. Common Failure Patterns

| Symptom | Kernel Cause | Diagnosis |
|---------|-------------|-----------|
| p99 >> p50 latency | CFS period throttle mid-burst | `cpu.stat throttled_usec` rising |
| CPU usage 90% but throttled | Quota exhausted before period ends | `nr_throttled / nr_periods` > 0.1 |
| `perf_event_open` returns `EPERM` | `perf_event_paranoid = 3` or seccomp blocking | `sysctl kernel.perf_event_paranoid`; check seccomp profile |
| Zero `/proc/<pid>/schedstat` values | `CONFIG_SCHEDSTATS=n` or `CONFIG_SCHED_INFO=n` | `grep CONFIG_SCHED_INFO /boot/config-$(uname -r)` |
| High cache miss rate | NUMA cross-socket access | `numastat -p <pid>` + Topology Manager alignment |
| Latency spike every 100ms | CFS period expiry during burst | Shorten period with `--cpu-cfs-period=10000` |
| Flamegraph shows kernel time in throttle path | Task woken from throttle on period boundary | Raise `limits.cpu` or enable CFS burst |

**Diagnosing `EPERM` from `perf_event_open`:**

```bash
# Check paranoid level
sysctl kernel.perf_event_paranoid

# Check if GKE default seccomp is active
kubectl get node -o jsonpath='{.items[0].metadata.annotations.container\.seccomp\.security\.alpha\.kubernetes\.io/pod}'

# Check pod's seccomp profile
kubectl get pod <name> -o jsonpath='{.spec.securityContext.seccompProfile}'
```

On clusters using the RuntimeDefault seccomp profile (the default on GKE, EKS, AKS, and most managed clusters), `perf_event_open` is blocked even with `CAP_PERFMON`. The workaround is a custom seccomp profile that allows syscall 298 (`perf_event_open`), applied via `securityContext.seccompProfile.type: Localhost` with the profile JSON stored on the node.

## 7. Verification Commands

```bash
# Full throttle report for all pods on the node
for cg in /sys/fs/cgroup/kubepods.slice/kubepods-pod*.slice; do
  uid=$(basename $cg | sed 's/.*pod//')
  periods=$(awk '/^nr_periods/{print $2}' $cg/cpu.stat 2>/dev/null)
  throttled=$(awk '/^nr_throttled/{print $2}' $cg/cpu.stat 2>/dev/null)
  echo "pod $uid: nr_periods=$periods nr_throttled=$throttled"
done

# Throttle ratio per pod (requires nr_periods > 0)
# NOTE: assumes default CFS period of 100ms (100000 µs); check cpu.max if period was changed
for cg in /sys/fs/cgroup/kubepods.slice/kubepods-pod*.slice; do
  uid=$(basename $cg | sed 's/.*pod//' | cut -c1-8)
  awk -v cg="$uid" '
    /^nr_periods/{p=$2}
    /^nr_throttled/{t=$2}
    /^throttled_usec/{tu=$2}
    END{
      if (p>0) printf "pod %s: throttle_ratio=%.1f%% nr_throttled=%d\n",
        cg, tu/(p*100000)*100, t
    }
  ' $cg/cpu.stat 2>/dev/null
done

# perf stat on a container process (from node)
perf stat -e cycles,instructions,cache-misses,branch-misses \
  -p $(pgrep -n nginx) sleep 5

# Check perf_event_paranoid
sysctl kernel.perf_event_paranoid

# Monitor throttling live for a specific pod
CGROUP=/sys/fs/cgroup/kubepods.slice/$(kubectl get pod <name> \
  -o jsonpath='{.metadata.uid}' | tr -d '-' | sed 's/\(.*\)/pod\1/')
watch -n1 "awk '/nr_periods|nr_throttled|throttled_usec/{print}' ${CGROUP}.slice/cpu.stat"

# Scheduler latency: bpftrace per-process wait time
bpftrace -e '
tracepoint:sched:sched_stat_wait {
    @wait_ns[args->comm] = sum(args->delay);
}
interval:s:5 { print(@wait_ns); clear(@wait_ns); }'

# Detect noisy neighbor: show top 10 processes by runqueue wait
for pid in $(ls /proc | grep -E '^[0-9]+$'); do
  cg=$(cat /proc/$pid/cgroup 2>/dev/null | grep kubepods) || continue
  [ -z "$cg" ] && continue
  read rt wait sw < /proc/$pid/schedstat 2>/dev/null || continue
  [ "${rt:-0}" -gt 0 ] || continue
  echo "$wait $pid $(cat /proc/$pid/comm 2>/dev/null)"
done | sort -rn | head -10
```

## 8. Key Kernel References

| Symbol | File | URL |
|--------|------|-----|
| `perf_event_open()` | `kernel/events/core.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/events/core.c |
| `struct sched_statistics` | `include/linux/sched.h` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/sched.h |
| `throttle_cfs_rq()` | `kernel/sched/fair.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/fair.c |
| `struct tracepoint` | `include/linux/tracepoint.h` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/tracepoint.h |
| `struct cfs_bandwidth` | `kernel/sched/sched.h` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/sched.h |
| `update_stats_wait_end()` | `kernel/sched/stats.h` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/stats.h |
| `proc_pid_schedstat()` | `fs/proc/base.c` | https://elixir.bootlin.com/linux/v6.9/source/fs/proc/base.c |
| `cpuset_can_attach()` | `kernel/cgroup/cpuset.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/cgroup/cpuset.c |

**Notes on kernel config requirements:**

- `struct sched_statistics` and `wait_sum` require `CONFIG_SCHEDSTATS=y`. GKE and EKS node kernels enable this by default.
- `sched_info.run_delay` (field 2 of `/proc/<pid>/schedstat`) requires `CONFIG_SCHED_INFO=y`. Verify: `grep CONFIG_SCHED_INFO /boot/config-$(uname -r)`.
- `perf_event_open` requires `CONFIG_PERF_EVENTS=y` (universally enabled on production kernels) and either `CAP_PERFMON` (Linux 5.8+) or `perf_event_paranoid ≤ 2 (for user-space only profiling) or ≤ 1 (for kernel profiling)`.
- The `sched_stat_wait` tracepoint works on any kernel with `CONFIG_TRACEPOINTS=y` and does not require `CONFIG_SCHEDSTATS`. It is the preferred production method for per-process scheduler latency measurement.
