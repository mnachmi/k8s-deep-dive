# Kubernetes Memory — Kernel Connection

This document maps every Kubernetes memory construct to the kernel mechanisms covered in `04-memory/kernel/`. The goal is to make each K8s knob traceable to the exact kernel data structure that enforces it.

---

## Kubernetes Memory Architecture Overview

| K8s Construct | Kernel Mechanism | cgroup v2 File | Covered In |
|---------------|-----------------|----------------|------------|
| `resources.limits.memory` | `memory.max` — OOM kill if exceeded | `memory.max` | 04-c-oom-psi.md |
| `resources.requests.memory` | `memory.min` — protected from reclaim | `memory.min` | 04-b-page-allocator.md |
| Eviction manager | Watches `/proc/meminfo` + PSI | N/A (node-level) | 04-c-oom-psi.md |
| `hugepages-2Mi` limit | `hugetlb` cgroup controller | `hugetlb.2MB.limit_in_bytes` | 04-b-page-allocator.md |
| Topology Manager | `cpuset.cpus` + `cpuset.mems` → `mempolicy` | `cpuset.cpus`, `cpuset.mems` | 04-d-numa.md |
| QoS class → kill order | `oom_score_adj` per container PID | `/proc/<pid>/oom_score_adj` | 04-c-oom-psi.md |
| PSI-based eviction | `memory.pressure` poll threshold | `memory.pressure` | 04-c-oom-psi.md |

**The cgroup v2 chain for a Pod container:**

```
/sys/fs/cgroup/
  kubepods/                         # kubelet's root cgroup
    burstable/                      # QoS tier (or guaranteed/, besteffort/)
      pod<uid>/                     # one cgroup per Pod
        <container-id>/             # one cgroup per container
          memory.max                # ← resources.limits.memory
          memory.min                # ← resources.requests.memory
          memory.current            # current usage
          memory.events             # oom / oom_kill counters
          memory.pressure           # PSI stall metrics
          hugetlb.2MB.limit_in_bytes
          cpuset.cpus               # pinned CPUs (Topology Manager)
          cpuset.mems               # pinned NUMA nodes (Topology Manager)
```

---

## Kubelet Eviction Manager

The eviction manager is kubelet's pre-emptive defense against node OOM. It evicts pods **before** the kernel OOM killer fires, preserving graceful termination and avoiding the non-deterministic kill ordering of global OOM.

### What It Watches

```
kubelet eviction manager loop (default: every 10s):
  1. Read /proc/meminfo
       → compute AvailableMemory = MemAvailable (mm/page_alloc.c computes this)
  2. Read /sys/fs/cgroup/memory.pressure  (system-wide PSI — kernel/sched/psi.c)
       → check 'some' stall percentage for avg10 and avg60
  3. [if AvailableMemory < evictionHard["memory.available"]]
       → rank pods for eviction:
           BestEffort first    (oom_score_adj = 1000 — kernel kills these first anyway)
           then Burstable      (oom_score_adj proportional to request fraction)
           then Guaranteed     (oom_score_adj = -997 — protected)
       → kubelet sends DELETE to API server
       → pod receives SIGTERM → terminationGracePeriodSeconds → SIGKILL
  4. [if PSI memory 'some' avg10 > softEvictionPSIThreshold]
       → same pod ranking and eviction (softer trigger, earlier warning)
```

The pod QoS → `oom_score_adj` mapping is set at container start by kubelet writing to `/proc/<pid>/oom_score_adj` for the container's main process. This means even without hitting the eviction manager, the kernel's own OOM killer (`oom_badness()`) will naturally kill BestEffort processes first if node memory is exhausted — the kubelet eviction manager just gets there first with a graceful shutdown.

### Eviction Configuration (kubelet KubeletConfiguration)

```yaml
evictionHard:
  memory.available: "100Mi"     # evict pods immediately when node has <100Mi free
  nodefs.available: "10%"       # also evicts on low disk
evictionSoft:
  memory.available: "300Mi"     # soft: warn and begin grace period countdown
evictionSoftGracePeriod:
  memory.available: "1m30s"     # evict after 90 seconds at soft threshold
evictionMaxPodGracePeriod: 90   # cap on terminationGracePeriodSeconds during eviction
```

`evictionHard` triggers immediate eviction with no grace period. `evictionSoft` gives pods time to shut down cleanly — useful for stateful workloads that need to flush buffers or commit transactions.

### Observation

```bash
# Node memory conditions (MemoryPressure=True when hard eviction threshold crossed):
kubectl describe node <node> | grep -A10 "Conditions:"

# Node /proc/meminfo (MemAvailable is what the eviction manager reads):
cat /proc/meminfo | grep -E "MemTotal|MemAvailable|Cached|SwapTotal"

# Watch PSI alongside node conditions:
watch -n 2 'cat /proc/pressure/memory; echo "---"; kubectl get nodes -o wide'

# Pod eviction events:
kubectl get events --field-selector reason=Evicting --all-namespaces

# Kubelet eviction log (look for "eviction manager"):
journalctl -u kubelet | grep -i "evict\|threshold\|pressure" | tail -30
```

---

## HugePages

Standard Linux uses 4 KiB pages. For memory-intensive workloads, 4 KiB pages mean the CPU's Translation Lookaside Buffer (TLB) must cache millions of entries — the TLB is finite, so cache misses cause page table walks that cost 100–300 ns each. Huge pages address this by reducing the number of TLB entries needed: one 2 MiB entry covers 512 × 4 KiB entries.

### The Two Mechanisms

**1. Static Huge Pages — pre-allocated at boot, never swapped**

The kernel reserves physically contiguous 2 MiB blocks at boot time when memory is unfragmented. Once allocated, they stay reserved and can never be used for normal 4 KiB allocations.

```bash
# Reserve 512 × 2 MiB = 1 GiB of huge pages at runtime:
sysctl vm.nr_hugepages=512

# Prefer setting at boot via GRUB: hugepages=512
# Check allocation status:
cat /proc/meminfo | grep Huge
# HugePages_Total:     512   ← reserved
# HugePages_Free:      510   ← available for mmap
# HugePages_Rsvd:        2   ← committed but not yet faulted
# HugePages_Surp:        0   ← surplus (from vm.nr_overcommit_hugepages)

# Hugepages filesystem (required for mmap by applications):
mount -t hugetlbfs none /dev/hugepages
```

Applications access static huge pages via `mmap(MAP_HUGETLB)` or by mapping the `hugetlbfs` filesystem. The buddy allocator's `MAX_ORDER` mechanism (covered in 04-b-page-allocator.md) is what allows the kernel to assemble physically contiguous 2 MiB blocks — 2 MiB = 2^9 × 4 KiB = order-9 allocation on x86_64.

**2. Transparent Huge Pages (THP) — kernel auto-promotes 4 KiB pages to 2 MiB**

THP requires no application changes. `khugepaged` (a kernel thread) scans anonymous VMAs and collapses 512 adjacent 4 KiB pages into a single 2 MiB page when conditions are met (pages contiguous in physical memory, VMA aligned).

```bash
# THP mode (system-wide):
cat /sys/kernel/mm/transparent_hugepage/enabled
# [always] madvise never
#   always  = promote all eligible VMAs automatically
#   madvise = only promote VMAs where app called madvise(MADV_HUGEPAGE)
#   never   = disable THP entirely

# THP stats (promotion rate, defrag failures):
grep thp /proc/vmstat | head -20
# thp_fault_alloc          1024   ← successful THP faults
# thp_collapse_alloc        512   ← khugepaged promotions
# thp_collapse_alloc_failed  32   ← failed (fragmentation)
# thp_split_page             8    ← THP split back to 4K (e.g., for CoW)
```

**THP trade-off for production:** `always` mode can cause latency spikes when khugepaged does page compaction to assemble contiguous memory (covered in 04-b-page-allocator.md — the buddy allocator's fragmentation problem). Set THP to `madvise` and let latency-sensitive apps opt in explicitly.

### Kubernetes HugePages Resource

```yaml
# Pod spec: Guaranteed QoS required — requests must equal limits
resources:
  limits:
    hugepages-2Mi: 1Gi      # 512 × 2 MiB huge pages
    memory: 4Gi             # must also specify memory alongside huge pages
  requests:
    hugepages-2Mi: 1Gi      # requests MUST equal limits for hugepages (always Guaranteed)
    memory: 4Gi
```

HugePages always force **Guaranteed QoS** because `requests == limits` is required by validation. This also means `oom_score_adj = -997` for the pod, protecting it from kernel OOM kill.

Kubernetes exposes hugepages as a schedulable resource. The kubelet advertises `hugepages-2Mi` capacity in the node's `.status.allocatable` based on `HugePages_Free` from `/proc/meminfo`.

The kubelet enforces the limit via the `hugetlb` cgroup controller:

```bash
# cgroup enforcement for a hugepages pod:
cat /sys/fs/cgroup/kubepods/pod<uid>/hugetlb.2MB.limit_in_bytes
# 1073741824  (= 1 GiB)

# Current usage:
cat /sys/fs/cgroup/kubepods/pod<uid>/hugetlb.2MB.usage_in_bytes
```

When a container tries to `mmap` more huge pages than `hugetlb.2MB.limit_in_bytes`, the kernel returns `ENOMEM` from the `mmap` call — the process is not killed, it gets an error. This differs from `memory.max`, where the cgroup OOM killer fires.

### Verification

```bash
# Static hugepage reservation status:
cat /proc/meminfo | grep -i huge

# Pod's hugetlb cgroup limit:
cat /sys/fs/cgroup/kubepods/pod<uid>/hugetlb.2MB.limit_in_bytes

# THP compaction activity (spikes here correlate with latency events):
grep "thp_collapse_alloc\|compact_success\|compact_fail" /proc/vmstat

# Per-node hugepage availability:
cat /sys/devices/system/node/node*/hugepages/hugepages-2048kB/free_hugepages
```

---

## Topology Manager

For latency-critical workloads — trading engines, real-time audio/video, ML inference — even a single cross-NUMA memory access is unacceptable jitter. The Topology Manager (added in Kubernetes 1.18) coordinates the CPU Manager and memory placement to ensure a pod's CPUs and memory reside on the same NUMA node.

### How It Works

The Topology Manager acts as an arbiter during pod admission. Before kubelet creates a container, it queries all registered **hint providers** (CPU Manager, Device Manager) for their preferred NUMA topology. The Topology Manager combines these hints and either admits or rejects the pod based on its policy.

**Policy options** (set in `KubeletConfiguration`):

```yaml
topologyManagerPolicy: "single-numa-node"
# Options:
#   none         — default; no topology alignment, resources allocated independently
#   best-effort  — try to align, but admit even if alignment not possible
#   restricted   — require alignment for Guaranteed pods; reject if not achievable
#   single-numa-node — strictest: pod MUST fit entirely on one NUMA node or be rejected
```

### What `single-numa-node` Does

With `single-numa-node` and a Guaranteed pod:

1. **CPU Manager** selects CPUs all on the same NUMA node and writes `cpuset.cpus` in the container's cgroup:
   ```
   /sys/fs/cgroup/kubepods/pod<uid>/<container-id>/cpuset.cpus = "0-15"
   # CPUs 0-15 are on NUMA node 0
   ```

2. **Memory is restricted** to the same NUMA node via `cpuset.mems` in the cgroup:
   ```
   /sys/fs/cgroup/kubepods/pod<uid>/<container-id>/cpuset.mems = "0"
   # Only NUMA node 0 is allowed for memory allocations
   ```

3. The kernel enforces `cpuset.mems` via the cpuset subsystem. When the container's processes call `alloc_pages()`, the allocator checks the task's `cpuset_mems_allowed` mask (derived from `cpuset.mems`) and restricts allocation to those nodes. This is mechanically equivalent to `set_mempolicy(MPOL_BIND)` applied process-wide via the cgroup hierarchy.

4. If the pod cannot fit on a single NUMA node — because it requests more CPUs or memory than one node holds — the pod is **rejected** with `TopologyAffinityError` and `Reason: TopologyAffinityError` in the event log.

### Kernel Mechanism: `cpuset.mems` → `mempolicy`

The cpuset subsystem (under cgroup v1 and v2) enforces NUMA binding at the cgroup level. When `cpuset.mems = "0"` is written:

1. The kernel updates the cpuset's `mems_allowed` nodemask.
2. For every task in the cgroup, `task_struct->mems_allowed` is updated to intersect with the cgroup's nodemask.
3. On the next `alloc_pages()` call, `__alloc_pages_noprof()` checks `cpuset_current_mems_allowed()` and skips zones not in the allowed set — exactly the same as `MPOL_BIND` from `struct mempolicy` (covered in 04-d-numa.md).

This means the Topology Manager effectively applies `struct mempolicy` mode `MPOL_BIND` to the entire pod without requiring any application-level NUMA awareness.

### Verification

```bash
# CPUs assigned to the container (e.g., "0-15" = CPUs 0 through 15):
cat /sys/fs/cgroup/kubepods/pod<uid>/<container-id>/cpuset.cpus

# NUMA nodes allowed for memory allocation (e.g., "0" = only node 0):
cat /sys/fs/cgroup/kubepods/pod<uid>/<container-id>/cpuset.mems

# Map CPUs to NUMA nodes on the host:
for cpu in $(seq 0 $(nproc --all | xargs -I{} expr {} - 1)); do
    node=$(cat /sys/devices/system/cpu/cpu${cpu}/topology/physical_package_id 2>/dev/null)
    echo "cpu${cpu} → socket/node${node}"
done

# Verify process is getting local memory (N0=local, N1=remote):
cat /proc/$PID/numa_maps | head -10

# NUMA hit/miss ratio (numa_miss should be near zero for a well-pinned pod):
numastat -p $PID

# TopologyAffinityError events (pod rejected at admission):
kubectl get events --field-selector reason=TopologyAffinityError -n <namespace>
```

---

## Common Memory Failure Patterns

| Symptom | Likely Cause | Diagnosis Command |
|---------|-------------|------------------|
| Pod OOMKilled | Exceeded `memory.max` (cgroup limit = `resources.limits.memory`) | `kubectl describe pod <pod>` → `OOMKilled`; `dmesg \| grep -i oom` |
| Node MemoryPressure=True | Node-level eviction hard threshold crossed | `kubectl describe node <node>`; `cat /proc/meminfo` |
| Pod evicted, reason=MemoryPressure | Eviction manager triggered (soft or hard threshold) | `kubectl describe pod <pod>` → `Reason: Evicted`; check kubelet logs |
| High memory usage but no OOM | Working set exceeds `requests` (memory.min), no reclaim protection | `cat memory.current` vs `cat memory.max`; check `memory.pressure` avg10 |
| THP latency spikes | `khugepaged` compaction blocking on page migration (fragmentation) | `grep thp_collapse_alloc_failed /proc/vmstat`; set THP to `madvise` |
| NUMA remote memory access | Pod CPUs and memory span multiple NUMA nodes | `numastat -p <pid>`; check `cpuset.mems` for the pod's cgroup |
| HugePages allocation failure | Insufficient pre-allocated huge pages on node | `cat /proc/meminfo \| grep HugePages_Free`; check `hugetlb.2MB.limit_in_bytes` |
| TopologyAffinityError | Pod does not fit on a single NUMA node (`single-numa-node` policy) | `kubectl get events`; compare pod CPU/memory request to `numactl --hardware` |
| kswapd CPU spike | Zone dropped below `WMARK_LOW`; background reclaim running | `cat /proc/zoneinfo \| grep -A5 "Normal"`; check `vm_stat NR_SLAB_RECLAIMABLE` |

### Connecting the Failure Patterns to Kernel Internals

**OOMKilled** → The cgroup memory controller's `mem_cgroup_out_of_memory()` called `oom_kill_process()`. The `oom_kill` counter in `memory.events` will be > 0. This is the cgroup OOM path from 04-c-oom-psi.md — the process received SIGKILL from the kernel, not from kubelet.

**MemoryPressure eviction** → The kubelet read `MemAvailable` from `/proc/meminfo` below threshold. This is computed by `si_meminfo_node()` in `mm/page_alloc.c` using the zone watermarks (`WMARK_LOW`) from `struct zone` (04-d-numa.md and 04-b-page-allocator.md).

**THP latency spikes** → `khugepaged` called `collapse_huge_page()` which calls `compact_zone()` to assemble contiguous pages. The buddy allocator's fragmentation state (`free_area[MAX_ORDER]` in `struct zone` — 04-b-page-allocator.md) determines whether compaction succeeds quickly or stalls.

**NUMA remote memory** → Task is accessing pages in a different `pg_data_t` than its running CPU belongs to. `task_numa_fault()` (04-d-numa.md) will eventually detect and fix this, but until migration completes, every cache miss pays ~2× latency.
