# 12 — KVM to Kubernetes: What the Node Doesn't Know

## The Invisible Layer

Every Kubernetes node running in a public cloud is a KVM guest. The kubelet does not know this. The container runtime does not know this. The scheduler does not know this — it assigns pods to "nodes" with certain CPU and memory capacity, unaware that the capacity is vCPU-based and that the actual compute bandwidth available to those vCPUs fluctuates with host load. Most of the Kubernetes observability stack (Prometheus, `kubectl top`, node metrics) reports what the guest OS sees, not what the host provides.

This gap between what Kubernetes thinks is happening and what KVM is actually doing produces four categories of failure that look like Kubernetes problems but are hypervisor problems. Understanding the connection means understanding which Kubernetes signal corresponds to which hypervisor mechanism.

## 1. CPU: Steal Time vs CFS Throttle

Chapter 08 covered CFS bandwidth controller: when a pod's CPU usage exceeds `limits.cpu`, the kernel throttles the pod by suspending all its tasks until the next accounting period. The symptom is latency. The diagnostic signal is `cpu.stat nr_throttled > 0`.

Steal time produces identical application symptoms — latency, timeouts, slow processing — with opposite diagnostic signals. `nr_throttled` is zero. `kubectl top` shows low CPU usage (the vCPU is waiting, not busy). The node CPU idle percentage looks high. But `/proc/stat`'s steal field is rising.

**Distinguishing steal from throttle:**

```bash
# Inside the node — check steal field in /proc/stat
# Fields: user nice system idle iowait irq softirq steal
awk '/^cpu /{print "steal:", $9/$2*100"%"}' /proc/stat

# node-exporter Prometheus metric for steal time:
# node_cpu_seconds_total{mode="steal"}
# Rate over 5 minutes:
rate(node_cpu_seconds_total{mode="steal"}[5m])
# > 0.05 (5%) is a signal; > 0.10 (10%) is a problem

# Throttle check (no steal involvement):
cat /sys/fs/cgroup/kubepods.slice/kubepods-pod<uid>.slice/.../cpu.stat | grep throttled
```

**The scheduling mismatch:** Kubernetes schedules pods onto nodes based on `requests.cpu`. A 4-vCPU node with 16 pods each requesting 250m CPU looks fully scheduled at 4 vCPUs of requests. But if the host overcommits and steals 20% of vCPU time, the effective capacity is 3.2 vCPUs — all 16 pods compete for less than they were promised. No Kubernetes mechanism detects this; the scheduler continues placing pods based on the original 4-vCPU capacity.

**Fix:** Use dedicated tenancy (AWS), sole-tenant nodes (GCP), or isolated vCPU pinning at the hypervisor level. Monitor `node_cpu_seconds_total{mode="steal"}` and alert above 5%.

## 2. Memory: Balloon Driver vs OOM Kill

Chapter 04 covered cgroup memory limits and the OOM killer. When a pod exceeds `limits.memory`, the cgroup OOM killer terminates it. The signal is `OOMKilled` in pod status and `dmesg` OOM messages.

Memory balloon inflation produces memory pressure without cgroup violation. The balloon driver holds pages that reduce the guest's `MemAvailable`. The kubelet reads `MemAvailable` from `/proc/meminfo` to determine node memory pressure. When `MemAvailable` drops below the eviction threshold (`--eviction-hard=memory.available<100Mi`), kubelet begins evicting pods in priority order — BestEffort first, then Burstable.

The pods being evicted have not exceeded their own memory limits. They are being evicted because the host is reclaiming guest memory through the balloon driver. The eviction events in Kubernetes look identical to legitimate memory pressure evictions.

**Detecting balloon inflation:**

```bash
# Check /proc/meminfo for balloon pages (from inside the node)
grep -E "MemAvailable|Balloon" /proc/meminfo
# Balloon: 524288 kB ← 512MB held by balloon driver

# Track balloon over time:
watch -n5 'grep -E "MemAvailable|Balloon" /proc/meminfo'

# lsmod check:
lsmod | grep virtio_balloon
# If loaded, balloon can be instructed to inflate by the hypervisor

# node-exporter exposes MemAvailable:
# node_memory_MemAvailable_bytes
# Sudden drops not correlated with pod memory usage → suspect balloon
```

**The burst correlation:** Cloud providers often inflate balloons during host memory pressure events — when many VMs on the same physical host simultaneously allocate memory. This causes correlated evictions across VMs on the same host, which appears in Kubernetes as a cluster-wide memory event even though individual nodes are operating normally.

## 3. Network: virtio vs SR-IOV vs eBPF Offload

Chapter 06 covered how pod network traffic flows through the guest kernel (Netfilter, conntrack) to the virtual NIC driver. The virtual NIC type determines the floor latency.

**virtio-net (standard):** All packet processing happens in the guest kernel. Netfilter runs. conntrack runs. The virtqueue notification crosses the hypervisor boundary once per batch. Round-trip latency: ~50-100μs for inter-VM traffic.

**SR-IOV VF (high performance):** The NIC VF bypasses the guest's virtio stack. Packets go directly from the NIC hardware into guest memory via DMA. The guest kernel receives them via the VF driver (e.g., `i40evf`). Netfilter and conntrack still run in the guest. Latency: ~10-20μs.

**SR-IOV + DPDK/XDP offload:** The application polls the VF's DMA ring directly, bypassing the guest kernel's network stack entirely. Netfilter does not run. conntrack does not track the connection. kube-proxy's iptables rules have no effect. This requires Multus CNI + SR-IOV device plugin and is incompatible with standard Kubernetes network policies.

The Kubernetes `--network-plugin` selection and CNI choice does not expose this. A cluster can have some pods on virtio and others on SR-IOV without any Kubernetes-level indication of which is which.

## 4. Live Migration and Request Latency Spikes

Live migration produces a 50-200ms blackout during which no guest code executes. For a Kubernetes node, this means:

- All pod processes are suspended for 50-200ms
- kubelet cannot respond to API server for 50-200ms (may register as node NotReady briefly)
- Any TCP connections with tight timeouts may be dropped
- Any SLO tracking 99th percentile latency will show a spike

The migration itself is invisible to Kubernetes. After the blackout, the node resumes normally. The API server timeout is typically 5 minutes before a node is declared NotReady — a 200ms blackout rarely triggers it. But the latency spike appears in application metrics and traces with no corresponding Kubernetes event.

**Correlation:** A latency spike on all pods on a node simultaneously, lasting 50-300ms, with no node event in `kubectl describe node` and no throttle or OOM events, is a migration signature.

## 5. kube-inspect `--virt` Flag

The `--virt` flag detects the hypervisor environment and reports steal time, balloon pages, and estimated migration risk:

```
kube-inspect --virt
KVM Virtualization Status
══════════════════════════════════════════════════════
Hypervisor         : KVM (KVMKVMKVM)
vCPUs              : 4
CPU Steal Time     : 3.7%  ⚠ WARNING (threshold: 5%)
Steal (1min avg)   : 2.1%
Steal (5min avg)   : 3.9%  ⚠
Virtio Devices     : net×1  blk×1  balloon×1  rng×1
Balloon Pages Held : 524288 pages (2048 MB)  🔴 CRITICAL
MemAvailable       : 1843 MB
Balloon/RAM Ratio  : 33%  🔴 Host reclaiming memory
vhost-net Threads  : 2 (1 per virtqueue direction)
Live Migration     : Not detected (last 5min)
EPT                : Supported (kvm_intel.ept=Y)
IOMMU              : Active (DMAR group /dev/vfio/*)
SR-IOV VFs         : 2 assigned to pods

Recommendations:
  • Balloon at 33% means host is under memory pressure.
    Consider migrating to a host with more free RAM.
  • Steal time above 3%: monitor for degradation.
    Alert fires if steal > 5% for 5 minutes.
══════════════════════════════════════════════════════
```

## 6. Kubernetes Resource Requests in a VM Context

**`requests.cpu` in a VM:** `requests.cpu` represents guaranteed CPU bandwidth from the CFS bandwidth controller perspective inside the VM. The VM itself makes no CPU reservation guarantee from the hypervisor. A pod with `requests.cpu: 1` gets 1 vCPU of guaranteed host scheduling — unless steal time is non-zero, in which case it gets less.

**`limits.memory` vs balloon:** `limits.memory` enforces a cgroup memory hard limit. The balloon driver operates outside cgroup accounting — it reduces guest-wide `MemFree`, which affects all cgroups proportionally. A pod with `limits.memory: 1Gi` and plenty of headroom can still be evicted if the balloon steals enough guest memory to drop `MemAvailable` below the node eviction threshold.

**`requests.memory` and balloon:** `requests.memory` is a scheduling hint — the scheduler places the pod on a node with enough `Allocatable` memory. Balloon inflation reduces `MemAvailable` but not `Allocatable` (which is static). The scheduler continues placing pods on an inflated node as if it had full memory capacity.

## 7. Burstable Instances and Steal

Some cloud providers offer "burstable" instance types (AWS T-series, GCP E2) that provide a baseline CPU credit and allow bursting above it. These instances have a structural steal time ceiling: when credits are exhausted, the hypervisor enforces the baseline vCPU capacity by rate-limiting the guest. From inside the guest, this manifests as steal time — the vCPU thread is ready but the host will not schedule it.

Kubernetes is unaware of CPU credits. A T3.medium with exhausted credits running a pod that needs 2 vCPUs will show: high steal time, low throttle, pod latency spikes, normal `kubectl top` CPU reading. The scheduler considers the node healthy and continues scheduling workloads onto it.

## 8. Diagnostic Decision Tree

```
Pod latency spike / slow response
│
├── kubectl top shows HIGH cpu → check cpu.stat nr_throttled
│     High throttled? → CFS throttle (ch08), raise limits.cpu
│     Low throttled? → investigate further
│
├── kubectl top shows LOW cpu (unexpected) → check /proc/stat steal
│     Steal > 5%? → hypervisor overcommit
│       → change instance type / move to dedicated tenancy
│     Steal low? → check cgroup memory / balloon
│
├── PSI pressure rising / evictions without OOM
│     Check /proc/meminfo Balloon:
│     Balloon > 10%? → host memory pressure → escalate to infra
│     Balloon low? → legitimate pod memory pressure (ch04)
│
└── Periodic latency spikes (50-300ms) correlated across ALL pods on node
      No throttle, no steal, no OOM → live migration signature
      Check: /proc/uptime vs wall clock during spike window
```
