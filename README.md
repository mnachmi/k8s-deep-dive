# From Kernel Expert to Kubernetes Performance Engineer

Most Kubernetes operators have a working mental model of how the system operates: pods run on nodes, kubelet manages them, the API server stores state, the scheduler places workloads. This model is accurate enough to deploy services and write manifests. It is not accurate enough to diagnose why a pod is being OOM killed every six hours, why a container with `100m` CPU limit has 300ms p99 latency, why a node is occasionally marked NotReady with no obvious cause, or why a memory leak in one pod can degrade performance for pods in a different namespace.

The reason those problems are hard to diagnose from the Kubernetes layer is that they are not Kubernetes problems. They are Linux kernel problems that Kubernetes has abstracted. The OOM kill is the kernel's `oom_badness()` scoring function picking a victim based on `oom_score_adj`, which kubelet sets based on QoS class. The CPU latency is the CFS bandwidth controller pausing cgroup tasks that have exhausted their quota, enforced in the kernel scheduler, visible only in `cpu.stat`. The NotReady node is the NMI watchdog detecting a CPU lockup and calling `panic()`. These mechanisms exist in the kernel; the Kubernetes layer reports their effects.

This course builds your mastery of Linux kernel internals and shows how Kubernetes leverages them. You will read kernel source, write syscall-level C and Go, debug cgroup hierarchies, and trace system behavior with eBPF. By the end, you will see through Kubernetes abstractions to the kernel syscalls underneath.

Every chapter teaches a Linux kernel mechanism from scratch with kernel source precision, then shows exactly how Kubernetes uses it, then builds one capability into `kube-inspect` — a per-pod performance diagnostic daemon you build incrementally across all 13 chapters.

**This is not a refresher course — kernel fundamentals *are* the course.**

---

## What You Will Be Able To Do

After completing this course, the following investigations become straightforward:

**Memory**
```bash
# Why is this pod being OOM killed when the node has free memory?
cat /sys/fs/cgroup/kubepods/burstable/pod<uid>/memory.events
# → oom_kill 7 — the cgroup limit, not the node limit, is the constraint

# Which process in a multi-container pod gets killed first?
cat /proc/<pid>/oom_score_adj
# → 999 — it's a BestEffort container; oom_badness() scores it highest
```

**CPU**
```bash
# Why does my service have 200ms p99 with 40% node CPU idle?
cat /sys/fs/cgroup/kubepods/burstable/pod<uid>/cpu.stat
# → nr_throttled 847 — the CFS bandwidth controller, not node contention

# Is limits.cpu: 500m actually enforced as expected?
cat /sys/fs/cgroup/.../cpu.max
# → 50000 100000 — 50ms per 100ms period, exactly
```

**Networking**
```bash
# Why are some requests to my Service failing intermittently?
cat /proc/sys/net/netfilter/nf_conntrack_count
cat /proc/sys/net/netfilter/nf_conntrack_max
# → 131068 / 131072 — conntrack table 99.9% full; kube-proxy DNAT failing

# Did the CNI plugin actually create the pod's network namespace?
ls -la /proc/<pid>/ns/net
lsns -t net | grep <pid>
```

**Node health**
```bash
# Is this node's kernel in a degraded state?
cat /proc/sys/kernel/tainted
# → 4096 — TAINT_WARN: a kernel warning occurred; check dmesg

# Will this node capture a crash dump if it panics?
cat /sys/kernel/kexec_crash_loaded
# → 1 — crash kernel staged, kdump will fire on panic
```

---

## Prerequisites

| Requirement | Minimum | Notes |
|-------------|---------|-------|
| Linux kernel | 5.15+ | Ubuntu 22.04 LTS minimum; 6.x recommended |
| Compiler | gcc + clang/llvm 16+ | Both needed: gcc for kernel module style C, clang for eBPF |
| Go | 1.22+ | For kube-inspect and Go exercises |
| Kernel headers | linux-headers-$(uname -r) | Required for eBPF exercises |
| libbpf | libbpf-dev | eBPF loader library |
| Cluster | kind or k3s | kubectl + kubeconfig to a test cluster |
| Tools | bpftool, perf, strace, ss, ip | Most available via `apt install linux-tools-generic` |

```bash
# Ubuntu 22.04 / 24.04 setup
sudo apt install -y gcc clang llvm libbpf-dev linux-headers-$(uname -r) \
    linux-tools-generic bpftool strace iproute2 numactl crash
```

---

## How to Use This Course

**Structure of every chapter:**

```
NN-chapter-name/
├── README.md              # Chapter overview, objectives, what you will build
├── kernel/                # Linux kernel internals — source-level deep dives
│   ├── NN-a-topic.md      #   struct field-by-field, lifecycle, locking, live observation
│   ├── NN-b-topic.md
│   └── NN-c-topic.md
├── k8s/
│   └── NN-k8s-connection.md  # Exact mapping: K8s API → kernel data structure
└── exercises/
    ├── topic-demo/        # C program: syscall-level proof of concept
    └── topic-reader/      # Go program: production-style inspection tool
```

**Learning sequence:**

1. **Read `kernel/` first** — understand the Linux mechanism from source. Every struct is shown field-by-field with elixir.bootlin.com links for Linux 6.9.
2. **Read `k8s/`** — see the exact translation from Kubernetes YAML to kernel data structure.
3. **Build and run the exercises** — C programs prove the mechanism works at the syscall level; Go programs build reusable inspection tools.
4. **Extend `kube-inspect`** — each chapter's checkpoint adds one flag to the daemon. By chapter 11, it covers the full diagnostic surface.

Each chapter is self-contained. Start with Chapter 00 and proceed in order — later chapters assume the kernel vocabulary from earlier ones.

---

## Chapter Index

| # | Chapter | Kernel Anchor | K8s Connection | kube-inspect Flag |
|---|---------|---------------|----------------|-------------------|
| **00** | **Prologue: The Gap** | `entry_SYSCALL_64`, `sys_call_table` | Every pod op is a syscall chain | — |
| **01** | **Process Model** | `task_struct`, `clone3()`, PID namespaces | Pod = task_struct + nsproxy + css_set | `--namespaces` |
| **02** | **Namespaces** | `nsproxy`, 8 `CLONE_NEW*` types, `setns()` | runc isolation model, `kubectl exec` path | `--namespaces` |
| **03** | **cgroups v2** | `css_set`, controllers, `try_charge()` | QoS classes, resource limits, OOM isolation | `--cgroup` |
| **04** | **Memory** | Page tables, buddy/SLUB, OOM killer, PSI | Eviction manager, HugePages, Topology Manager | `--psi` |
| **05** | **VFS & Storage** | `super_block`, `inode`, OverlayFS, blk-mq | Image layers, ConfigMap mounts, CSI volumes | `--mounts` |
| **06** | **Networking** | `sk_buff`, veth, Netfilter, conntrack | CNI, kube-proxy DNAT, NetworkPolicy | `--netns` |
| **07** | **eBPF** | BPF VM, verifier, maps, XDP/TC hooks | Cilium, Tetragon, Hubble, Falco | `--ebpf` |
| **08** | **CPU Scheduler** | CFS vruntime, `struct rq`, NUMA balancing | CPU requests/limits, CPU Manager, throttle | `--sched` |
| **09** | **Kubernetes Internals** | kubelet VFS interface, OOM killer, PSI | Pod lifecycle, eviction, crash detection | `--pressure` |
| **10** | **Performance** | PMU counters, ftrace, schedstat | CPU throttle diagnosis, cache miss analysis | `--perf` |
| **11** | **Cluster Operations** | `panic()`, NMI watchdog, kexec/kdump | Node recovery, rolling upgrades, post-mortem | `--health` |
| **12** | **KVM Virtualization** | `struct kvm_vcpu`, EPT two-level page walk, virtio ring buffer, `CPUTIME_STEAL` | Steal ≠ throttle, balloon ≠ OOM, SR-IOV for CNI | `--virt` |
| **13** | **RPi5 Lab** | ARM64 EL0-EL3, `el0_svc`, RVWMO memory model, TTBR0/TTBR1, ARM PMUv3 | k3s two-node cluster, ARM64 portability validation | `--arch` |

---

## The Long-Running Project: `kube-inspect`

A Go diagnostic daemon built incrementally across all chapters. Each chapter adds one flag that exposes the kernel mechanism you just learned.

### Usage

```bash
# Build
cd kube-inspect && make build

# Inspect a specific pod
./kube-inspect --pod <pod-uid> --cgroup --psi --sched --perf

# Inspect node health
./kube-inspect --health

# Full diagnostic dump as JSON
./kube-inspect --pod <pod-uid> --cgroup --psi --mounts --netns --sched --perf --json

# All pods on this node
./kube-inspect --node
```

### Flag Reference

| Flag | Added In | Shows |
|------|----------|-------|
| `--namespaces` | ch01/02 | Namespace inode IDs per process (PID, net, mnt, UTS, IPC, cgroup) |
| `--cgroup` | ch03 | memory.{current,max,events}, cpu.{max,weight,stat}, pids.{current,max} |
| `--psi` | ch04 | Per-pod PSI memory/cpu/io pressure — some/full avg10/avg60/avg300 |
| `--mounts` | ch05 | Mount namespace table, OverlayFS layer count, volume bind mounts |
| `--netns` | ch06 | Network namespace interface stats, socket counts per pod |
| `--ebpf` | ch07 | BPF programs and maps loaded by processes in the pod |
| `--sched` | ch08 | CPU affinity mask, NUMA node placement, cpu.weight, cpu.max |
| `--pressure` | ch09 | Node PSI from /proc/pressure/*, pod memory.events OOM counters |
| `--perf` | ch10 | CPU throttle rate (nr_throttled/nr_periods), per-process wait time |
| `--health` | ch11 | Kernel version, taint flags, watchdog config, kdump readiness |
| `--virt` | ch12 | KVM hypervisor: steal time %, balloon pages, virtio devices, EPT/IOMMU state |
| `--arch` | ch13 | CPU architecture: ARM64 exception levels, PMU type, ASID width, memory model |

### Example Output

```
$ sudo ./kube-inspect --pod a1b2c3d4-... --cgroup --sched --perf

Pod: a1b2c3d4-e5f6-7890-abcd-ef1234567890
Cgroup: /sys/fs/cgroup/kubepods/burstable/poda1b2c3d4-.../

  Memory:
    current:     47,185,920 bytes (45 MiB)
    max:        268,435,456 bytes (256 MiB)
    oom_kill:   0
    anon:        41,943,040 bytes

  CPU:
    cpu.max:     50000 100000   (500m limit)
    cpu.weight:  51             (500m request → weight)
    nr_periods:  18420
    nr_throttled: 3847          (throttle rate: 20.9%)
    throttled_us: 192,350,000

  Scheduler:
    allowed CPUs: 0-7
    NUMA node:    0
    preferred:    0

  Perf:
    wait_sum:    4,832,100 µs
    run_delay:   avg 261 µs/period
```

### Architecture

```
kube-inspect/
├── cmd/kube-inspect/main.go       # CLI flags, output routing
├── internal/
│   ├── proc/        # /proc walker, PID namespace resolution, ns inode table
│   ├── cgroup/      # cgroup v2 reader: memory, cpu, io, pids, PSI
│   ├── netns/       # network namespace stats via netlink
│   ├── ebpf/        # BPF fd inspection via /proc/<pid>/fdinfo
│   ├── sched/       # CPU affinity, NUMA placement, cpu.stat
│   ├── kubelet/     # kubelet API client (eviction thresholds)
│   ├── health/      # kernel version, taint, watchdog, kdump
│   ├── virt/        # KVM hypervisor detection, steal time, balloon, virtio (ch12)
│   ├── arch/        # CPU architecture info: ARM64 EL, PMU, memory model (ch13)
│   └── metrics/     # Prometheus exporter + JSON reporter
├── bpf/
│   └── syscall_counter.bpf.c     # eBPF program: per-pod syscall frequency
└── CHECKPOINT.md    # what each chapter added
```

---

## Diagnostic Reference

When you encounter a Kubernetes symptom in production, these chapters contain the kernel explanation:

| Symptom | Kernel Mechanism | Chapter | Key File to Check |
|---------|-----------------|---------|-------------------|
| Container OOMKilled | `oom_badness()`, `try_charge()` | 03, 04 | `memory.events`, `oom_score_adj` |
| High p99 latency, low CPU usage | CFS bandwidth controller throttle | 08, 10 | `cpu.stat` → `nr_throttled` |
| Pod memory usage climbs without OOMKill | PSI pressure, page cache growth | 04, 09 | `/proc/pressure/memory`, `memory.stat` |
| Intermittent Service connection failures | conntrack table overflow | 06 | `/proc/sys/net/netfilter/nf_conntrack_count` |
| Node marked NotReady, no obvious cause | NMI watchdog, softlockup | 11 | `dmesg`, `/proc/sys/kernel/tainted` |
| kubectl exec hangs | setns() failure, namespace lifecycle | 02 | `/proc/<pid>/ns/`, `lsns` |
| Pod stuck in ContainerCreating | clone3()/mount namespace setup | 01, 05 | `/proc/<pid>/mountinfo`, `dmesg` |
| Container sees wrong /proc entries | PID namespace isolation | 01, 02 | `/proc/<pid>/ns/pid`, `lsns` |
| Performance degrades on multi-socket node | NUMA cross-socket memory access | 04, 08 | `numastat`, `/proc/<pid>/numa_maps` |
| eBPF tool cannot attach to pod process | cgroup/namespace context | 07 | `/proc/<pid>/cgroup`, `bpftool prog list` |
| High pod latency, low throttle, zero OOM | CPU steal time (host overcommit) | 12 | `/proc/stat` steal field, `node_cpu_seconds_total{mode="steal"}` |
| PSI memory pressure, evictions, no OOM | virtio_balloon inflation | 12 | `/proc/meminfo Balloon:`, `dmesg | grep balloon` |
| Periodic 100-200ms latency spike on all pods | VM live migration blackout | 12 | bpftrace wall-clock gap, no throttle/OOM event |
| x86 code race-free, ARM64 corrupts data | RVWMO weak memory model, missing barriers | 13 | `dsb`/`dmb` required where x86 TSO was implicit |

---

## Quick Start

```bash
# Chapter 00: trace the syscall boundary
cd 00-prologue/exercises/syscall-tracer
make && sudo ./syscall_tracer

# Chapter 01: walk the /proc process tree
cd 01-process-model/exercises/proc-walker
go run . --pid $$

# Chapter 03: read cgroup stats for a pod
cd 03-cgroups/exercises/cgroup-stats
go run . --pod <pod-uid>

# Chapter 07: inspect eBPF objects in a pod
cd 07-ebpf/exercises/bpf-fdinfo-reader
go run . --pid <container-pid>

# kube-inspect: full diagnostic
cd kube-inspect && make build
sudo ./kube-inspect --pod <pod-uid> --cgroup --sched --perf
sudo ./kube-inspect --health
```

---

## References

| Resource | What It Is |
|----------|-----------|
| [elixir.bootlin.com/linux/v6.9](https://elixir.bootlin.com/linux/v6.9/source) | All kernel source cross-references in this course link here |
| [docs/references.md](docs/references.md) | Books, LWN articles, LKML threads, and tools used throughout |
| [docs/sysctl-k8s-cheatsheet.md](docs/sysctl-k8s-cheatsheet.md) | Kernel tuning parameters relevant to Kubernetes production |
| [docs/ebpf-cheatsheet.md](docs/ebpf-cheatsheet.md) | bpftrace one-liners for every chapter's live observation section |
| [Robert Love — Linux Kernel Development](https://www.amazon.com/Linux-Kernel-Development-Robert-Love/dp/0672329468) | The writing style this course aspires to |

---

## License

This course and all code examples are open source. Kernel source citations use Linux 6.9 source available at [elixir.bootlin.com](https://elixir.bootlin.com/).
