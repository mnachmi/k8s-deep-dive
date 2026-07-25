# Chapter 03 — cgroups v2: Kubernetes Connection

The kubelet is responsible for translating Pod resource specifications into cgroup v2 filesystem writes. Every `resources.limits` and `resources.requests` field in a Pod spec maps to one or more files under `/sys/fs/cgroup/kubepods/`. This document shows the exact mapping, the QoS classification that determines the cgroup path, and how to diagnose OOM kills and CPU throttling at the cgroup level.

## Section 1 — Kubernetes QoS Classes and cgroup Hierarchy

Kubernetes assigns every Pod a Quality of Service (QoS) class at admission time, which determines where the pod's cgroup lives in the hierarchy.

| QoS Class | When assigned | cgroup path |
|-----------|--------------|-------------|
| Guaranteed | All containers: requests == limits for CPU AND memory | `/sys/fs/cgroup/kubepods/pod<uid>/` (directly under kubepods) |
| Burstable | At least one container has any request or limit | `/sys/fs/cgroup/kubepods/burstable/pod<uid>/` |
| BestEffort | No containers have any requests or limits | `/sys/fs/cgroup/kubepods/besteffort/pod<uid>/` |

### cgroup Driver Variants

The exact path on disk depends on which cgroup driver kubelet is configured to use.

**cgroupfs driver** (direct filesystem paths):
```
/sys/fs/cgroup/kubepods/pod<uid>/
/sys/fs/cgroup/kubepods/burstable/pod<uid>/
/sys/fs/cgroup/kubepods/besteffort/pod<uid>/
```

**systemd driver** (slice-based paths):
```
/sys/fs/cgroup/kubepods.slice/kubepods-pod<uid>.slice/
/sys/fs/cgroup/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod<uid>.slice/
/sys/fs/cgroup/kubepods.slice/kubepods-besteffort.slice/kubepods-besteffort-pod<uid>.slice/
```

Check which driver your cluster uses:

```bash
kubectl get node -o yaml | grep cgroupDriver
# or on the node directly:
cat /var/lib/kubelet/config.yaml | grep cgroupDriver
```

## Section 2 — Resource Limits → cgroup File Mapping

Every field in the Pod spec `resources` stanza maps to a specific cgroup v2 file. The kubelet performs these writes when creating the pod cgroup.

| Pod spec field | cgroup v2 file | Value written | Example |
|----------------|----------------|---------------|---------|
| `limits.memory` | `memory.max` | bytes (integer) | `256Mi` → `268435456` |
| `requests.memory` | `memory.min` | bytes (integer) | `128Mi` → `134217728` |
| `limits.cpu` | `cpu.max` | `<quota> 100000` | `1` → `100000 100000`; `500m` → `50000 100000` |
| `requests.cpu` | `cpu.weight` | weight formula | `500m` → `51` |
| `limits.memory` (init containers) | `memory.max` | same | set then reset after init completes |
| (kubelet `--pod-max-pids`) | `pids.max` | integer | `1024` |

### cpu.weight Formula

The Kubernetes source (`pkg/kubelet/cm/cgroup_manager_linux.go`) converts CPU milliCPU requests to cgroup weights using:

```
weight = int(math.Max(float64(MinShares), math.Min(float64(MaxShares), float64(milliCPU * SharesPerCPU) / CPUPeriod)))
```

Where:
- `MinShares = 2`
- `MaxShares = 262144`
- `SharesPerCPU = 1024`
- `CPUPeriod = 100000`

Example calculations:
- `100m` → `max(2, min(262144, (1024 * 100) / 100000))` = `max(2, min(262144, 1.024))` = `2`
- `500m` → `max(2, min(262144, (1024 * 500) / 100000))` = `max(2, min(262144, 5.12))` = `5`
- `1000m` (1 CPU) → `max(2, min(262144, (1024 * 1000) / 100000))` = `max(2, min(262144, 10.24))` = `10`

Note: `cpu.weight` values are relative weights between cgroups, not absolute CPU allocations. Higher weight means more CPU time when the node is contended.

### cpu.max Formula

For CPU limits, the kubelet writes `<quota> 100000` to `cpu.max`:
- `limits.cpu: 1` → `100000 100000` (100% of a CPU per 100ms period)
- `limits.cpu: 500m` → `50000 100000` (50% of a CPU per 100ms period)
- `limits.cpu: 2` → `200000 100000` (200% — two full CPUs)

## Section 3 — Verifying cgroup Setup for a Running Pod

```bash
# Get pod UID
POD_UID=$(kubectl get pod nginx -o jsonpath='{.metadata.uid}')
echo "Pod UID: $POD_UID"

# Find the cgroup directory (works for both cgroupfs and systemd drivers)
CGROUP_PATH=$(find /sys/fs/cgroup -type d -name "pod${POD_UID}" 2>/dev/null | head -1)
echo "Cgroup path: $CGROUP_PATH"

# Verify memory limit matches what you set:
cat "${CGROUP_PATH}/memory.max"
# Should match: kubectl get pod nginx -o jsonpath='{.spec.containers[0].resources.limits.memory}'

# Verify CPU quota:
cat "${CGROUP_PATH}/cpu.max"
# Format: "<quota> 100000" where quota = limits.cpu * 100000

# Current memory usage:
cat "${CGROUP_PATH}/memory.current"

# CPU usage and throttling stats:
cat "${CGROUP_PATH}/cpu.stat"

# List PIDs in this pod's cgroup:
cat "${CGROUP_PATH}/cgroup.procs"

# List per-container cgroups inside the pod:
ls "${CGROUP_PATH}/"
# Each subdirectory is a container's cgroup
```

## Section 4 — The OOM Kill Lifecycle

When a container exceeds `memory.max`, the kernel follows this path before the kubelet observes a container restart:

1. Container process allocates memory → page fault → `handle_mm_fault()`
2. `mem_cgroup_charge()` → `try_charge()` → finds `usage > max`
3. Tries memory reclaim: `try_to_free_mem_cgroup_pages()` — scans LRU lists
4. Reclaim insufficient → `mem_cgroup_out_of_memory()` → `out_of_memory()`
5. `oom_kill_process()` selects the task with highest `oom_score_adj` in the cgroup
6. Sends `SIGKILL`
7. Container process exits
8. containerd detects exit via cgroup event (`memory.events`)
9. kubelet receives `ContainerDied` from containerd
10. kubelet sets `pod.status.containerStatuses[].lastState.terminated.reason = OOMKilled`
11. kubelet restarts the container (if `restartPolicy` allows)

### Diagnosing OOM Kills

```bash
# From kubectl:
kubectl describe pod nginx
# Look for: "Last State: Terminated, Reason: OOMKilled"

kubectl get events --field-selector reason=OOMKilling

# From the node (more detail):
dmesg | grep -i "out of memory\|oom-kill\|killed process" | tail -20

# Check the cgroup memory events counter:
cat "${CGROUP_PATH}/memory.events"
# oom N          — how many times the limit was hit
# oom_kill N     — how many processes were OOM killed

# OOM score of a specific process (higher = more likely to be killed):
cat /proc/$PID/oom_score
cat /proc/$PID/oom_score_adj    # Kubernetes sets this to adjust priority
```

### oom_score_adj by QoS Class

Kubernetes sets `oom_score_adj` based on the pod's QoS class, controlling which processes the kernel kills first when memory is scarce:

- **BestEffort** containers: `oom_score_adj = 1000` — killed first; they have no resource guarantees
- **Burstable** containers: `oom_score_adj` between 2 and 999, proportional to `requests/limits` ratio — intermediate priority
- **Guaranteed** containers: `oom_score_adj = -997` — almost never killed by the node OOM killer; only killed if no other candidates exist

## Section 5 — CPU Throttling Detection and Diagnosis

CPU throttling is a common, invisible performance problem. A container that sets `limits.cpu: 100m` can be throttled 99% of the time even if the node has spare CPU capacity — the CFS bandwidth controller enforces the quota absolutely, not relatively.

```bash
# Check if a pod is being throttled:
cat "${CGROUP_PATH}/cpu.stat"
# Key fields:
# nr_periods N        — total scheduling periods elapsed
# nr_throttled N      — periods where the cgroup was throttled (quota exhausted)
# throttled_usec N    — total microseconds of throttle time

# Calculate throttle percentage:
NR_PERIODS=$(grep nr_periods ${CGROUP_PATH}/cpu.stat | awk '{print $2}')
NR_THROTTLED=$(grep nr_throttled ${CGROUP_PATH}/cpu.stat | awk '{print $2}')
echo "Throttle rate: $(echo "scale=2; $NR_THROTTLED * 100 / $NR_PERIODS" | bc)%"

# Real-time throttle monitoring:
watch -n 1 'grep -E "nr_throttled|throttled_usec" /sys/fs/cgroup/kubepods/pod<uid>/cpu.stat'
```

### When Throttling Hurts

CPU throttling affects latency-sensitive workloads disproportionately. A web server handling requests will see tail latency spikes whenever it exhausts its CFS quota, even though the node has idle CPU capacity available. The throttle applies for the remainder of the 100ms scheduling period regardless of node load.

The fix is either to increase `limits.cpu` or remove the limit entirely for latency-critical services. Removing the limit means the container uses `cpu.weight`-based scheduling only, competing fairly with other pods but never being hard-throttled.

### bpftrace to Observe Throttling Live

```
bpftrace -e 'kprobe:throttle_cfs_rq {
    printf("throttle: pid=%d comm=%s\n", pid, comm);
}'
```

## Section 6 — Namespace vs cgroup: Complementary Isolation Layers

Namespaces (Chapter 02) and cgroups (Chapter 03) address different dimensions of container isolation. Both are required; neither is sufficient alone.

| Mechanism | Controls | Example |
|-----------|---------|---------|
| Namespaces (ch02) | Visibility — what a process can SEE | PID namespace: container sees only its own processes |
| cgroups (ch03) | Resource usage — how much a process can CONSUME | memory.max: container is OOM killed if it exceeds the limit |

- **Without namespaces:** processes can see and interfere with each other (inspect `/proc/<pid>/` of other containers, send signals, observe network connections)
- **Without cgroups:** one process can starve all others of memory or CPU — a single container can exhaust the node

### The cgroup Namespace

The cgroup namespace (`CLONE_NEWCGROUP`, covered in Chapter 02) controls what the cgroup hierarchy looks like from inside the container. From inside a container, `/proc/self/cgroup` shows:

```
0::/
```

Instead of the real host path:

```
0::/kubepods/burstable/pod<uid>/<container-id>
```

This prevents containers from discovering their position in the host cgroup hierarchy while still enforcing resource limits transparently.

## Section 7 — kube-inspect Integration

After completing the kube-inspect checkpoint 03, the `--cgroup` flag shows live resource stats per pod:

```bash
kube-inspect --pod <uid> --cgroup
```

Expected output:

```
Cgroup stats for pod <uid>:
  Path:             /sys/fs/cgroup/kubepods/burstable/pod<uid>
  memory.current:   45678592 bytes
  memory.max:       268435456
  memory.oom_kill:  0
  cpu.max:          50000 100000
  cpu.nr_throttled: 142
  cpu.throttled_us: 892341
  pids.current:     4
  pids.max:         max
```
