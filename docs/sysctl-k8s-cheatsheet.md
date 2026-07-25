# Kernel sysctl Tuning for Kubernetes

This cheatsheet documents kernel tuning parameters (`/proc/sys/`) commonly adjusted for Kubernetes deployments, organized by subsystem.

---

## Networking Stack

### TCP Connection Handling

#### `net.ipv4.tcp_max_syn_backlog`

**What it does:** Maximum number of SYN requests queued when the TCP stack is overwhelmed.

**Default:** 128 (very low for modern systems)

**Kubernetes context:** kube-proxy creates many short-lived connections; increase to prevent SYN drops.

**Recommended:** 2048–65536 depending on node traffic.

```bash
sysctl -w net.ipv4.tcp_max_syn_backlog=65536
echo "net.ipv4.tcp_max_syn_backlog=65536" >> /etc/sysctl.conf
```

**Kernel source reference:** `net/ipv4/tcp.c::tcp_max_syn_backlog`

---

#### `net.core.somaxconn`

**What it does:** Maximum length of the listen queue (backlog) for all sockets.

**Default:** 128 (too low for k8s)

**Kubernetes context:** API servers, kubelet, and services need deep backlog queues to handle connection spikes.

**Recommended:** 4096–65536 depending on workload.

```bash
sysctl -w net.core.somaxconn=65536
```

**Kernel source reference:** `include/linux/socket.h` (listen queue depth)

---

#### `net.ipv4.ip_local_port_range`

**What it does:** Ephemeral port range for client-side connections.

**Default:** 32768–60999 (only ~28k ports)

**Kubernetes context:** Pods, kube-proxy, and kubelet open many outbound connections. Default range exhausts quickly under load.

**Recommended:** 1024–65535 (full range, ~64k ports)

```bash
sysctl -w net.ipv4.ip_local_port_range="1024 65535"
```

**Kernel source reference:** `net/ipv4/inet_connection_sock.c::__inet_pick_port()`

**Observe:**
```bash
cat /proc/sys/net/ipv4/ip_local_port_range
ss -tan | wc -l  # Count established connections
```

---

#### `net.ipv4.tcp_tw_reuse`

**What it does:** Reuse TIME_WAIT sockets for new outbound connections (with timestamp validation).

**Default:** 0 (disabled)

**Kubernetes context:** Pods and services create many short-lived connections; TIME_WAIT sockets occupy port slots. Enabling this frees them faster.

**Recommended:** 1 (enable)

```bash
sysctl -w net.ipv4.tcp_tw_reuse=1
```

**Kernel source reference:** `net/ipv4/tcp_ipv4.c::tcp_v4_connect()` (checks TIME_WAIT reuse)

**Note:** Safe only with TCP timestamps enabled (default). Not recommended if connecting to systems without timestamp support.

---

#### `net.ipv4.tcp_fin_timeout`

**What it does:** How long (in seconds) a TCP socket stays in FIN_WAIT_2 state.

**Default:** 60 seconds

**Kubernetes context:** Long tail of FIN_WAIT_2 sockets consumes resources. Reduce if many short-lived connections.

**Recommended:** 30–45 seconds for k8s.

```bash
sysctl -w net.ipv4.tcp_fin_timeout=30
```

**Kernel source reference:** `net/ipv4/tcp.c` (FIN_WAIT timeout logic)

---

### Conntrack & NAT

#### `net.netfilter.nf_conntrack_max`

**What it does:** Maximum number of connection tracking entries in memory.

**Default:** ~65k on typical nodes (often too low)

**Kubernetes context:** kube-proxy uses conntrack for DNAT and SNAT on all services. High-traffic nodes need very large conntrack tables.

**Recommended:** 1M–4M on large nodes.

```bash
sysctl -w net.netfilter.nf_conntrack_max=1048576  # 1M
```

**Kernel source reference:** `net/netfilter/nf_conntrack_core.c::nf_conntrack_hash_resize()`

**Observe:**
```bash
cat /proc/net/nf_conntrack | wc -l  # Current entries
cat /proc/sys/net/netfilter/nf_conntrack_count
```

---

#### `net.netfilter.nf_conntrack_tcp_timeout_established`

**What it does:** How long (in seconds) to keep ESTABLISHED conntrack entries.

**Default:** 432000 (5 days) — very long

**Kubernetes context:** Reduces stale connections consuming conntrack budget. Services with many short-lived connections benefit from shorter timeout.

**Recommended:** 300–600 (5–10 minutes) for k8s.

```bash
sysctl -w net.netfilter.nf_conntrack_tcp_timeout_established=300
```

**Kernel source reference:** `net/netfilter/nf_conntrack_proto_tcp.c`

---

#### `net.netfilter.nf_conntrack_tcp_timeout_time_wait`

**What it does:** How long to keep TIME_WAIT conntrack entries.

**Default:** 120 seconds

**Kubernetes context:** Correlates with TCP TIME_WAIT cleanup. Reduce if TIME_WAIT_2 is high.

**Recommended:** 30–60 seconds.

```bash
sysctl -w net.netfilter.nf_conntrack_tcp_timeout_time_wait=60
```

**Kernel source reference:** `net/netfilter/nf_conntrack_proto_tcp.c`

---

#### `net.netfilter.nf_conntrack_hash_bucket_size`

**What it does:** Hash table bucket size for connection tracking (read-only, set at boot).

**Default:** 16384 (can be tuned at module load time)

**Kubernetes context:** Very high-scale deployments benefit from larger buckets to reduce hash collisions.

**Set at boot via module parameter:**
```bash
echo "options nf_conntrack hashsize=262144" >> /etc/modprobe.d/nf_conntrack.conf
modprobe -r nf_conntrack
modprobe nf_conntrack
```

---

### UDP & ICMP

#### `net.ipv4.udp_mem`

**What it does:** Memory limits (pages) for UDP sockets: min, pressure, max.

**Default:** System-dependent (often insufficient for k8s)

**Kubernetes context:** CoreDNS and other UDP-heavy services need more budget.

**Recommended:** Increase max to 1GB+.

```bash
# Check current
cat /proc/sys/net/ipv4/udp_mem

# Set (values in pages; 1GB ≈ 262144 pages)
sysctl -w net.ipv4.udp_mem="131072 262144 524288"  # ~512MB, 1GB, 2GB
```

**Kernel source reference:** `net/ipv4/udp.c::udp_memory_allocated()`

---

## Memory Management

### OOM (Out-of-Memory) Killer

#### `vm.overcommit_memory`

**What it does:** Policy for overcommitting virtual memory.

**Values:**
- 0 (default): Heuristic; kernel estimates swappable memory
- 1: Always allow (risky; pages can be allocated but not available)
- 2: Never overcommit; enforce committed memory <= RAM + swap

**Kubernetes context:** Set to 1 if relying on Kubernetes memory limits (pods declared memory < RAM). Set to 0 or 2 for safety on mixed workloads.

**Recommended:** 0 (heuristic) or 1 (if strict limits enforced).

```bash
sysctl -w vm.overcommit_memory=0
```

**Kernel source reference:** `mm/util.c::__vm_enough_memory()` (heuristic logic), `mm/mmap.c::acct_stack_growth()` (overcommit check)

---

#### `vm.panic_on_oom`

**What it does:** Panic the kernel if OOM killer cannot free memory.

**Default:** 0 (do not panic)

**Kubernetes context:** Controversial; panic can crash a node to preserve data. Kubernetes kubelet prefers eviction. Set to 0 unless running stateful workloads where data loss is worse than downtime.

**Recommended:** 0 for most k8s.

```bash
sysctl -w vm.panic_on_oom=0
```

---

### Memory Reclaim & Swappiness

#### `vm.swappiness`

**What it does:** How aggressively the kernel swaps memory to disk (0–100).

**Default:** 60

**Kubernetes context:** Pods with memory pressure may swap, reducing performance. Lower swappiness to favor dropping page cache over swapping.

**Recommended:** 0–10 for k8s (prefer page cache drop).

```bash
sysctl -w vm.swappiness=1
```

**Note:** Requires swap space to have any effect; many k8s nodes disable swap entirely.

**Kernel source reference:** `mm/vmscan.c::get_scan_count()` (swappiness calculation)

---

#### `vm.min_free_kbytes`

**What it does:** Minimum free memory (in KB) that the kernel maintains for emergencies.

**Default:** System-dependent (~3% of RAM on modern systems)

**Kubernetes context:** If too low, the kernel may not have memory for critical operations (I/O flushes, page fault handlers). Too high wastes memory.

**Recommended:** 5%–10% of total RAM for large nodes.

```bash
# For a 64GB node, set to ~3GB
sysctl -w vm.min_free_kbytes=3145728  # 3GB in KB
```

**Kernel source reference:** `mm/page_alloc.c::min_free_kbytes_sysctl_handler()`

---

### Page Cache & I/O

#### `vm.dirty_ratio` & `vm.dirty_background_ratio`

**What it does:** Percentage of memory that can be dirty (unflushed) before writes are forced.

**Defaults:** dirty_ratio=20%, dirty_background_ratio=10%

**Kubernetes context:** High ratios cause writeback delays, affecting container I/O latency. CSI drivers may timeout.

**Recommended:** dirty_ratio=5–10%, dirty_background_ratio=2–5%.

```bash
sysctl -w vm.dirty_ratio=5
sysctl -w vm.dirty_background_ratio=2
```

**Kernel source reference:** `mm/page-writeback.c::balance_dirty_pages()`, `mm/page-writeback.c::get_dirty_limits()`

---

#### `vm.dirty_expire_centisecs`

**What it does:** How long (in centiseconds) before dirty pages are considered expired and written to disk.

**Default:** 3000 (30 seconds)

**Kubernetes context:** High values delay writes; low values increase I/O overhead. Tune based on latency tolerance.

**Recommended:** 1000–3000 (10–30 seconds).

```bash
sysctl -w vm.dirty_expire_centisecs=1500
```

---

## Process & Resource Limits

### Process Limits

#### `kernel.pid_max`

**What it does:** Maximum PID number; also affects PID namespace capacity.

**Default:** 32768 (on 32-bit systems); 2^22 (4M) on 64-bit.

**Kubernetes context:** Long-running clusters with many pod cycles may wrap PIDs. Increase if you see "out of PIDs" errors.

**Recommended:** 4194304 (2^22) on 64-bit systems.

```bash
sysctl -w kernel.pid_max=4194304
```

**Kernel source reference:** `kernel/pid.c::alloc_pid()` (PID allocation)

**Observe:**
```bash
cat /proc/sys/kernel/pid_max
ps aux | wc -l  # Current processes
```

---

#### `kernel.threads-max`

**What it does:** System-wide maximum number of threads.

**Default:** ~4 × number of CPUs (usually sufficient)

**Kubernetes context:** Large clusters with many pods (each with multiple threads) may hit this. Increase if thread allocation fails.

**Recommended:** Keep at default or increase for very large nodes.

```bash
sysctl -w kernel.threads-max=500000
```

---

### File Descriptor Limits

#### `fs.file-max`

**What it does:** System-wide maximum number of open file descriptors.

**Default:** ~400k on modern systems (often sufficient)

**Kubernetes context:** Services that open many connections (databases, proxies) may hit this. CSI drivers and kubelet need significant FD budget.

**Recommended:** 2M–10M for large clusters.

```bash
sysctl -w fs.file-max=2097152
```

**Observe:**
```bash
cat /proc/sys/fs/file-max
lsof -p <pid> | wc -l  # Per-process FDs
```

---

#### `fs.file-nr` (read-only)

**What it does:** Current number of open file descriptors and related stats (read-only).

**Observe:**
```bash
cat /proc/sys/fs/file-nr  # Shows: allocated, unused, max
```

---

## Networking Security & DoS Protection

### Rate Limiting & Backpressure

#### `net.ipv4.tcp_syncookies`

**What it does:** Enable SYN cookies to defend against SYN flood attacks.

**Default:** 1 (enabled)

**Kubernetes context:** Recommended for all deployments. Allows accepting connections even under SYN flood.

**Recommended:** 1 (leave enabled).

```bash
sysctl -w net.ipv4.tcp_syncookies=1
```

**Kernel source reference:** `net/ipv4/tcp_input.c::tcp_v4_conn_request()`

---

#### `net.ipv4.tcp_max_tw_buckets`

**What it does:** Maximum number of TIME_WAIT sockets in the system.

**Default:** 65536 (can be too low for high-traffic nodes)

**Kubernetes context:** Many short-lived services accumulate TIME_WAIT sockets. Increase to prevent "cannot get port" errors.

**Recommended:** 1M–4M for large deployments.

```bash
sysctl -w net.ipv4.tcp_max_tw_buckets=2097152
```

**Kernel source reference:** `net/ipv4/tcp_ipv4.c::tcp_v4_connect()` (TIME_WAIT reuse check)

---

## Scheduler & CPU

### CPU Affinity & Load Balancing

#### `kernel.sched_migration_cost_ns`

**What it does:** Cost (in nanoseconds) of migrating a task between CPUs; used to decide if migration benefits outweigh overhead.

**Default:** 500000 (0.5ms) — often too high for modern CPUs

**Kubernetes context:** High values cause poor load balancing across CPUs. Lower values allow more aggressive balancing.

**Recommended:** 100000–300000 (0.1–0.3ms) for k8s.

```bash
sysctl -w kernel.sched_migration_cost_ns=200000
```

**Kernel source reference:** `kernel/sched/fair.c::should_we_balance()` (uses migration cost)

---

#### `kernel.sched_latency_ns`

**What it does:** Target scheduling latency; how long a task can run before being preempted.

**Default:** 6000000 (6ms) for many CPU counts

**Kubernetes context:** Lower values mean lower latency for interactive tasks. Very low values increase context switch overhead.

**Recommended:** Default is usually good; tune only if latency-sensitive workloads are affected.

```bash
sysctl -w kernel.sched_latency_ns=6000000
```

**Kernel source reference:** `kernel/sched/fair.c::__sched_period()` (uses sched_latency_ns)

---

### Real-Time Scheduling

#### `kernel.sched_rt_runtime_us` & `kernel.sched_rt_period_us`

**What it does:** RT tasks get `rt_runtime_us` microseconds per `rt_period_us` to run.

**Defaults:** rt_period_us=1000000 (1s), rt_runtime_us=950000 (95% of period)

**Kubernetes context:** Kubernetes CPU Manager can use RT scheduling for guaranteed QoS pods. Reserve enough bandwidth.

**Recommended:** Default is fine unless running RT workloads; then reserve 5–20% for best-effort.

```bash
# Leave 10% for best-effort (reduce rt_runtime_us to 90% of rt_period_us)
sysctl -w kernel.sched_rt_runtime_us=900000
```

**Kernel source reference:** `kernel/sched/rt.c::sched_rt_bandwidth_accounting()`

---

## Observing Current Settings

### View All sysctl Values

```bash
# List all current settings
sysctl -a

# Show only networking settings
sysctl -a -e | grep net

# Show only memory settings
sysctl -a -e | grep vm

# Show only kernel settings
sysctl -a -e | grep kernel
```

### Persist Changes

All changes via `sysctl -w` are temporary (lost on reboot). To persist:

```bash
# Add to /etc/sysctl.conf
echo "net.ipv4.ip_local_port_range=1024 65535" >> /etc/sysctl.conf
echo "net.core.somaxconn=65536" >> /etc/sysctl.conf

# Or create a separate file
echo "net.ipv4.ip_local_port_range=1024 65535" > /etc/sysctl.d/99-kubernetes.conf

# Apply changes
sysctl -p
# or
sysctl -p /etc/sysctl.d/99-kubernetes.conf
```

---

## Kubernetes-Specific Configurations

### kubelet & kube-proxy Tuning

**kubelet flags** (in kubelet config):
- `--max-open-files` — kubelet's file descriptor limit
- `--oom-score-adj` — priority for OOM killer (typically -999 to protect kubelet)
- `--system-reserved` — resources reserved for OS and kubelet itself

**kube-proxy flags:**
- `--iptables-sync-period` — how often to sync iptables rules (default 30s)
- `--min-sync-period` — minimum time between syncs (default 0)

### Docker/containerd Network Settings

If using Docker or containerd for workloads:

```bash
# Inside container, sysctl changes apply to the network namespace
docker exec <container> sysctl -w net.ipv4.ip_local_port_range="1024 65535"

# Or pass at container creation
docker run --sysctl net.ipv4.ip_local_port_range="1024 65535" <image>
```

---

## Kernel Source References Summary

| Parameter | Kernel File | Function |
|-----------|-------------|----------|
| `tcp_max_syn_backlog` | `net/ipv4/tcp.c` | N/A (data field) |
| `somaxconn` | `include/linux/socket.h` | N/A (listen queue) |
| `ip_local_port_range` | `net/ipv4/inet_connection_sock.c` | `__inet_pick_port()` |
| `tcp_tw_reuse` | `net/ipv4/tcp_ipv4.c` | `tcp_v4_connect()` |
| `nf_conntrack_max` | `net/netfilter/nf_conntrack_core.c` | `nf_conntrack_hash_resize()` |
| `pid_max` | `kernel/pid.c` | `alloc_pid()` |
| `min_free_kbytes` | `mm/page_alloc.c` | `min_free_kbytes_sysctl_handler()` |
| `dirty_ratio` | `mm/page-writeback.c` | `balance_dirty_pages()` |
| `sched_migration_cost_ns` | `kernel/sched/fair.c` | `should_we_balance()` |
| `sched_latency_ns` | `kernel/sched/fair.c` | `__sched_period()` |

---

## Further Reading

- **Kernel documentation:** https://docs.kernel.org/admin-guide/sysctl/
- **Red Hat tuning guide:** https://access.redhat.com/documentation/en-us/red_hat_enterprise_linux/9/html/monitoring_and_managing_system_status_and_performance/tuning-kernel-parameters_monitoring-and-managing-system-status-and-performance
- **Kubernetes performance:** https://kubernetes.io/docs/tasks/administer-cluster/reserve-compute-resources/
- **Brendan Gregg's performance tuning:** http://www.brendangregg.com/
