# Chapter 08 — CPU Scheduler: Kubernetes Connection

## 1. Architecture Overview

| K8s Concept | Kernel Primitive | File / Interface |
|-------------|-----------------|------------------|
| CPU request (`resources.requests.cpu`) | `cpu.weight` (cgroup v2) / `cpu.shares` (v1) | `/sys/fs/cgroup/.../cpu.weight` |
| CPU limit (`resources.limits.cpu`) | `cpu.max` (cgroup v2) / `cpu.cfs_quota_us` (v1) | `/sys/fs/cgroup/.../cpu.max` |
| CPU pinning (static policy) | `cpuset.cpus` (cgroup cpuset) | `/sys/fs/cgroup/.../cpuset.cpus` |
| NUMA affinity | `cpuset.mems` (cgroup cpuset) | `/sys/fs/cgroup/.../cpuset.mems` |
| QoS Guaranteed | `cpu.weight` = 10 per CPU (max 10000) + static cpuset eligible | kubelet CPU manager |
| QoS BestEffort | lowest `cpu.weight` (2) | kubelet CPU manager |

## 2. CPU Requests → `cpu.weight`

In cgroup v2, `cpu.weight` ranges 1–10000 (default 100). Kubelet sets it as:

```
cpu.weight = max(1, min(10000, milliCPU × 10 / 1000))
# For 250m: weight = max(1, 250×10/1000) = 2  (rounds down)
# For 1000m: weight = 10
# For 4000m: weight = 40
```

This maps directly to `task_group->shares`, which feeds into each CFS entity's `load.weight` via `calc_group_shares()`. A container requesting 4× more CPU than another gets roughly 4× the CFS weight → 4× the runtime when both are runnable.

Cgroup v1 equivalent: `cpu.shares` (default 1024). Kubelet computes `shares = max(2, milliCPU × 1024 / 1000)`.

## 3. CPU Limits → `cpu.max` / CFS Bandwidth

`cpu.max` format: `<quota_us> <period_us>`. For `limits.cpu: "0.5"`, kubelet writes:
```
50000 100000
```
meaning the container may use 50ms of CPU per 100ms period. The kernel enforces via `struct cfs_bandwidth`:
- `cfs_b->quota = 50000000 ns` (50 ms)
- `cfs_b->period = 100000000 ns` (100 ms)
- `throttle_cfs_rq()` removes the runqueue at quota exhaustion
- `sched_cfs_period_timer()` fires each period and calls `distribute_cfs_runtime()` to unthrottle

Diagnosis: throttled pods show `throttled_usec` growing in `cpu.stat`:
```bash
cat /sys/fs/cgroup/kubepods.slice/kubepods-pod<uid>.slice/cpu.stat
```
Fields: `usage_usec`, `user_usec`, `system_usec`, `nr_throttled`, `throttled_usec`, `nr_bursts`, `burst_usec`.

## 4. CPU Manager — Static Policy

When kubelet runs with `--cpu-manager-policy=static`, Guaranteed pods with integer CPU requests get exclusive CPUs:

1. Kubelet maintains a CPU pool (all CPUs minus `--reserved-cpus` and system overhead).
2. On pod admission, it allocates `floor(cpu_request)` exclusive CPUs.
3. Writes them to `cpuset.cpus`: e.g., `0-3` for a 4-CPU Guaranteed pod.
4. Also writes `cpuset.mems` to the corresponding NUMA nodes.
5. Remaining BestEffort / Burstable pods share the "default" cpuset (all non-exclusive CPUs).

The kernel enforces via `cpuset_can_attach()` + `cpuset_attach()` in `kernel/cgroup/cpuset.c`: tasks in the cpuset cgroup can only be scheduled on CPUs in `cpuset.cpus`.

## 5. Topology Manager

`--topology-manager-policy` aligns CPU, memory, and device (GPU/RDMA NIC) NUMA hints:

| Policy | Behavior |
|--------|----------|
| `none` (default) | No topology alignment |
| `best-effort` | Prefer aligned NUMA, admit even if impossible |
| `restricted` | Require aligned NUMA; reject pod if impossible |
| `single-numa-node` | All resources must be on same NUMA node |

Hint providers: CPU Manager, Memory Manager (`--memory-manager-policy`), Device Manager. Each returns a `TopologyHint` (a bitmask of NUMA nodes that can satisfy the request). The Topology Manager intersects all hints to find the best affinity.

## 6. QoS Classes

| QoS Class | Condition | cpu.weight | cpuset eligible |
|-----------|-----------|-----------|-----------------|
| `Guaranteed` | All containers: requests == limits | 10 per milliCPU | Yes (static policy) |
| `Burstable` | At least one container: requests < limits | proportional to requests | No |
| `BestEffort` | No requests or limits set | 2 (minimum) | No |

The OOM killer in the kernel also uses these classes: BestEffort processes are killed first (oom_score_adj ≈ 1000), Burstable next, Guaranteed last (oom_score_adj = -998 for Guaranteed).

## 7. Common Failure Patterns

| Symptom | Kernel Cause | Fix |
|---------|-------------|-----|
| Pod throttled at < limit | CFS period too short (burst spikes exceed quota) | Increase `--cpu-cfs-period` or add `cpu.burst` |
| Noisy neighbor spikes | BestEffort pod on shared cpuset | Use CPU manager static policy for latency-sensitive pods |
| NUMA remote memory faults | Container cpuset spans NUMA nodes | Set `--topology-manager-policy=single-numa-node` |
| High involuntary context switches | RT task competing with CFS | Pin RT workload to dedicated CPUs via cpuset |
| `cpu.stat` shows `nr_throttled` > 0 | Quota exhausted mid-burst | Raise `limits.cpu` or enable `cpuCFSBurst` feature |
| Latency spike every 100ms | CFS period expiry during burst | Diagnose with `bpftrace tracepoint:sched:sched_cfs_throttle_max_vruntime` |

## 8. Verification Commands

```bash
# Show cpu.weight and cpu.max for a pod
PODUID=<uid>
CGROUP=/sys/fs/cgroup/kubepods.slice
find $CGROUP -name "pod${PODUID}.slice" | head -1 | xargs -I{} sh -c \
  'echo weight=$(cat {}/cpu.weight); echo max=$(cat {}/cpu.max)'

# List all throttled pods
find /sys/fs/cgroup/kubepods.slice -name cpu.stat | xargs grep -l 'nr_throttled [^0]'

# Show CPU pinning for a Guaranteed pod
cat /sys/fs/cgroup/kubepods.slice/kubepods-guaranteed-pod${PODUID}.slice/cpuset.cpus
cat /sys/fs/cgroup/kubepods.slice/kubepods-guaranteed-pod${PODUID}.slice/cpuset.mems
```

## 9. Key Kernel References

| Symbol | File | URL |
|--------|------|-----|
| `throttle_cfs_rq()` | `kernel/sched/fair.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/fair.c |
| `struct cfs_bandwidth` | `kernel/sched/sched.h` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/sched.h |
| `cpuset_can_attach()` | `kernel/cgroup/cpuset.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/cgroup/cpuset.c |
| `cpu_shares_write_u64()` (v1 shares) | `kernel/sched/core.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/core.c |
| `tg_set_cfs_bandwidth()` | `kernel/sched/fair.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/fair.c |
| `calc_group_shares()` | `kernel/sched/fair.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/fair.c |
