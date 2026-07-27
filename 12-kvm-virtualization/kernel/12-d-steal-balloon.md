# 12-d — CPU Steal Time, Memory Balloon, and Live Migration

## Three Invisible Taxes

Running Kubernetes inside a KVM VM imposes three costs that are entirely invisible from inside the node — invisible to `kubectl top`, invisible to Prometheus `node_cpu_seconds_total`, invisible to pod resource metrics, and invisible to the application. Each cost looks like something else when you don't know to look for it.

**CPU steal time** looks like CFS throttle or mysterious latency. The vCPU thread is waiting in the host's runqueue — not executing guest code — but the guest kernel sees time advancing without work being done. The application experiences latency; `cpu.stat` shows no throttle; `kubectl top` shows low CPU usage. The diagnostic dead end: the application is slow but nothing looks wrong.

**Memory balloon inflation** looks like PSI memory pressure or OOM kill. The host instructs the guest's balloon driver to allocate and hold pages, reducing the memory available to the guest OS and its cgroups. From inside the VM, the kernel's free memory is declining. PSI `some avg10` rises. kubelet may begin evicting pods. There is no OOM event from the container's perspective — the memory simply stopped being available.

**Live migration** looks like a GC pause, a burst of packet loss, or a sudden latency spike. During the migration's final phase, the VM is briefly paused while the last dirty pages are transferred to the destination host. This blackout typically lasts 50–200ms. Any in-flight TCP connections survive (network state was migrated with the VM), but all requests that arrived during the blackout experience that latency added to their processing time. It is periodic, clock-like, and correlates with nothing in the application's behavior.

## Source Locations

| File | Key Symbols | URL |
|------|-------------|-----|
| `kernel/sched/cputime.c` | `steal_account_process_tick()`, `account_steal_time()` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/cputime.c |
| `include/linux/kernel_stat.h` | `struct kernel_cpustat`, `CPUTIME_STEAL` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/kernel_stat.h |
| `drivers/virtio/virtio_balloon.c` | `fill_balloon()`, `release_balloon()`, `virtballoon_probe()` | https://elixir.bootlin.com/linux/v6.9/source/drivers/virtio/virtio_balloon.c |
| `arch/x86/kvm/x86.c` | `kvm_steal_clock()`, `kvm_get_steal_time()` | https://elixir.bootlin.com/linux/v6.9/source/arch/x86/kvm/x86.c |

## 1. CPU Steal Time: The Stolen Cycles

When the host runs more vCPUs than physical CPU threads, some vCPUs must wait in the host CFS runqueue. The guest cannot observe this wait — from inside the VM, time simply stops advancing on that vCPU. No instructions execute, no kernel timers fire, no network packets are processed. When the host CFS scheduler finally gives the vCPU a time slice, the guest resumes as if no time had passed — but wall clock time has advanced.

**The steal time mechanism:**

KVM provides a paravirtualized steal time interface. The guest kernel registers a shared memory page with the host (`MSR_KVM_STEAL_TIME`). Each time the host preempts a vCPU, it writes the accumulated stolen nanoseconds to this shared page. The guest kernel reads the page on each scheduler tick and accounts for the stolen time separately from user time, system time, and idle time.

```c
// arch/x86/kvm/x86.c — host side: update steal time in shared page
static void kvm_steal_time_set_preempted(struct kvm_vcpu *vcpu)
{
    struct kvm_steal_time __user *st;
    st = vcpu->arch.st.steal;
    ACCESS_ONCE(st->preempted) = KVM_VCPU_PREEMPTED;
}

// kernel/sched/cputime.c — guest side: account for stolen ticks
void account_steal_time(u64 cputime)
{
    struct rq *rq = this_rq();
    u64 *steal = &kcpustat_this_cpu->cpustat[CPUTIME_STEAL];
    *steal += cputime;          // add to /proc/stat steal field
}
```

**Reading steal time:**

```bash
# /proc/stat — 10th field is steal time in USER_HZ ticks
# cpu  user nice sys idle iowait irq softirq steal guest guest_nice
grep "^cpu " /proc/stat
# cpu  12345 67 890 45678 123 4 5 892 0 0
#                                      ^^^
#                             steal = 892 ticks accumulated since boot

# Convert to percentage (Prometheus does this automatically):
# steal% = (steal_delta / total_delta) * 100

# node_exporter metric:
# node_cpu_seconds_total{cpu="0",mode="steal"}
```

**Steal vs throttle — the diagnostic distinction:**

| Signal | CFS Throttle | CPU Steal |
|--------|-------------|-----------|
| `cpu.stat nr_throttled` | High | Normal |
| `/proc/stat steal` | Normal | Rising |
| Node CPU idle | High | Low (host is busy) |
| Other pods affected | No | Yes (shared host) |
| Fix | Raise `limits.cpu` | Change instance type or dedicated host |

## 2. Memory Balloon Driver: The Silent Memory Reclaim

The memory balloon (`virtio_balloon`) driver runs inside the guest as a kernel module and exposes a paravirtualized interface the host uses to reclaim memory from the VM without live migration or suspension.

**How it works:**

When the host is under memory pressure, it instructs the balloon driver to inflate: allocate physical pages from the guest OS and report them to the host. The host removes those pages from the guest's memory map and returns them to the host memory pool. The guest sees its available memory shrink without any explicit notification.

```c
// drivers/virtio/virtio_balloon.c
static void fill_balloon(struct virtio_balloon *vb, size_t num)
{
    // allocate pages from the guest OS (using GFP_HIGHUSER_MOVABLE)
    // report page frame numbers to the host via the inflate virtqueue
    // host then unmaps these pages from the guest's address space
}

static void release_balloon(struct virtio_balloon *vb, size_t num)
{
    // return pages to the guest OS
    // host reclaims them from its pool
}
```

**The guest-side consequence:** Pages held by the balloon driver are allocated from the guest kernel's buddy allocator. From the guest's perspective, available memory is declining as if a process were allocating it — because it is. The balloon driver is a process. It appears in `ps` as `[vballoon]`. Its memory consumption appears in `/proc/meminfo` under `Balloon:`. PSI pressure rises because cgroup memory charging competes with the balloon for the remaining free pages.

```bash
# Check balloon driver state inside a KVM guest
grep -i balloon /proc/meminfo
# Balloon:        524288 kB  ← 512MB held by balloon driver

# lsmod: verify balloon driver is loaded
lsmod | grep virtio_balloon

# dmesg for balloon events
dmesg | grep -i balloon
# [12345.678] virtio_balloon virtio2: Inflation of balloon by 131072 pages
```

**The Kubernetes consequence:** A balloon inflation event reduces the guest's `MemAvailable` (from `/proc/meminfo`), which kubelet reads to determine node memory pressure. If the balloon inflates enough, kubelet sees `MemAvailable < eviction.hard.memory.available` and begins evicting BestEffort pods. The evictions are correct from kubelet's perspective — memory is genuinely scarce — but the cause is the host hypervisor, not the workloads.

## 3. Live Migration: The Pause That Isn't a Pause

Live migration moves a running VM from one physical host to another without shutting it down. The motivation is host maintenance — hardware replacement, kernel patching, power management — without forcing guest downtime.

**The migration phases:**

```
Phase 1: Pre-copy (ongoing, 10-60 seconds)
  Host copies guest memory pages to destination host in the background
  Guest continues running; dirty pages are re-transferred iteratively
  Memory bandwidth between hosts limits transfer speed

Phase 2: Stop-and-copy (the blackout, 50-200ms)
  Guest vCPUs are paused
  Remaining dirty pages (the "working set") are transferred
  Guest CPU state (registers, VMCS) is transferred
  Duration depends on dirty rate at pause time

Phase 3: Resume
  Guest vCPUs start on destination host
  Network state was transferred; existing TCP connections survive
  Guest resumes exactly where it paused
```

From inside the guest, phase 2 is completely invisible except as a gap in wall clock time. Network packets that arrived during the blackout were buffered by the destination NIC; they appear when the guest resumes. From the application's perspective, all requests that were in-flight during the blackout see 50-200ms of added latency.

**Detecting live migration:**

```bash
# dmesg shows migration events if KVM provides them
dmesg | grep -i "migration\|hypervisor"

# Sudden jump in /proc/uptime vs system clock
# (uptime continues, but wall clock advanced during blackout)

# Clock source discrepancy
cat /sys/kernel/debug/kvm-clock | head -5

# Heuristic: periodic ~100ms latency spikes with no traffic correlation
# Use bpftrace to timestamp intervals:
bpftrace -e 'tracepoint:sched:sched_switch { @[nsecs / 1000000000] = count(); }'
# A 200ms gap in the per-second counts indicates a migration pause
```

## 4. Key Kernel References

| Symbol | File | URL |
|--------|------|-----|
| `account_steal_time()` | `kernel/sched/cputime.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/cputime.c |
| `CPUTIME_STEAL` | `include/linux/kernel_stat.h` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/kernel_stat.h |
| `fill_balloon()` | `drivers/virtio/virtio_balloon.c` | https://elixir.bootlin.com/linux/v6.9/source/drivers/virtio/virtio_balloon.c |
| `kvm_steal_clock()` | `arch/x86/kvm/x86.c` | https://elixir.bootlin.com/linux/v6.9/source/arch/x86/kvm/x86.c |
| `MSR_KVM_STEAL_TIME` | `arch/x86/include/uapi/asm/kvm_para.h` | https://elixir.bootlin.com/linux/v6.9/source/arch/x86/include/uapi/asm/kvm_para.h |
