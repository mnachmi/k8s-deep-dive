# Course Design: linux-to-k8s
## From Kernel Expert to Kubernetes Performance Engineer

**Date:** 2026-07-25  
**Status:** Approved

---

## 1. Identity & Philosophy

### Instructor Profile
The instructor is a Linux kernel device driver developer with deep kernel internals expertise. The course is written from that position of mastery — every kernel concept is explained with source-level precision, not approximation.

### Student Target Audience
Developers (backend, systems, DevOps) who work on Linux systems but do not have kernel-level expertise. They know how to use Linux but have not read kernel source, written syscall-level C, or debugged a cgroup hierarchy. The course builds them up from first principles.

### Core Philosophy
- **Kernel fundamentals are the course** — not a prerequisite, not a refresher. Every concept is taught from scratch with the depth a kernel engineer would apply.
- Every chapter teaches the Linux kernel mechanism first, with kernel source references, then shows exactly how Kubernetes leverages that mechanism.
- The student learns to see through Kubernetes abstractions to the kernel syscalls underneath.
- Performance is the primary lens throughout: every mechanism is taught alongside its tuning knobs and failure modes.
- Exercises exist at two levels: isolated PoC programs that prove one concept, and the long-running project that shows how concepts compose in production.

### Three Tracks Per Chapter
1. **Kernel Deep Dive** — teach the Linux mechanism from source: data structures, syscalls, kernel config options, `/proc`/`/sys` interfaces
2. **K8s Connection** — show how Kubernetes configures, limits, or depends on this mechanism; verify with `kubectl` and kernel tools
3. **Project Checkpoint** — add one capability to `kube-inspect`, the long-running project

### References (used throughout)
- Linux kernel source: https://elixir.bootlin.com/linux/latest/source
- LWN.net weekly kernel articles
- Linux kernel mailing list (lkml.org)
- Books: "Linux Kernel Development" (Love), "Understanding the Linux Kernel" (Bovet/Cesati), "Linux Programming Interface" (Kerrisk), "BPF Performance Tools" (Gregg), "Systems Performance" (Gregg)

---

## 2. The Long-Running Project: `kube-inspect`

A Go + libbpf per-pod performance diagnostic daemon built incrementally across all chapters.

### Final Capabilities (Chapter 10)
- Per-pod process tree (via `/proc` + PID namespace walk)
- Per-pod cgroup v2 resource stats (memory, CPU, IO)
- Per-pod PSI (Pressure Stall Information) metrics
- Per-pod OOM event history (via cgroup memory events)
- Per-pod network namespace interface stats
- Per-pod eBPF syscall frequency counter
- Per-pod CPU affinity and NUMA placement
- Kubelet eviction threshold overlay
- Prometheus `/metrics` endpoint
- JSON report mode for one-shot diagnostics

### Architecture
```
kube-inspect/
├── cmd/kube-inspect/main.go       # CLI: --pod, --node, --all, --prometheus
├── internal/
│   ├── proc/        # /proc walker, PID ns resolution
│   ├── cgroup/      # cgroup v2 reader (memory, cpu, io, psi)
│   ├── netns/       # network namespace stats via netlink
│   ├── ebpf/        # libbpf loader, map readers
│   ├── kubelet/     # kubelet API client (eviction thresholds)
│   └── metrics/     # Prometheus exporter + JSON reporter
├── bpf/
│   ├── vmlinux.h    # BTF-generated kernel headers
│   └── syscall_counter.bpf.c
└── CHECKPOINT.md    # per-chapter: what was added
```

### Build Requirements
- Go 1.22+
- clang/llvm 16+ (for BPF compilation)
- libbpf-dev
- Linux kernel 5.15+ (cgroup v2 unified, BTF enabled)

---

## 3. Chapter Map

| # | Title | Kernel Anchor | K8s Lens | Project Checkpoint |
|---|-------|--------------|----------|--------------------|
| 00 | Prologue: The Gap | syscall table, `entry_SYSCALL_64` | Why k8s is "just Linux" | Skeleton: CLI scaffold, no-op collectors |
| 01 | Process Model | `task_struct`, `clone()`, `do_fork()` | Pod lifecycle, PID ns, PID 1 problem | `proc/`: list pod processes via `/proc` |
| 02 | Namespaces | `nsproxy`, all 8 ns types, `setns()` | containerd/runc isolation model | `proc/`: namespace enumeration per pod |
| 03 | cgroups v2 | `css_set`, `cgroup_subsys`, controllers | kubelet QoS classes, resource limits | `cgroup/`: stats reader per pod |
| 04 | Memory | page tables, PSI, OOM killer, NUMA | Eviction manager, HugePages, Topology Manager | `cgroup/`: PSI + OOM events per pod |
| 05 | VFS & Storage | inode, dentry, overlayfs, `mount_tree()` | Image layers, CSI, ephemeral volumes | `proc/`: mount ns inspection, layer count |
| 06 | Network Stack | `sk_buff`, veth, netfilter, conntrack | CNI, kube-proxy DNAT, Service ClusterIP | `netns/`: interface stats per pod |
| 07 | eBPF | BPF VM, verifier, maps, CO-RE | Cilium, Hubble, bpftrace in k8s | `ebpf/`: syscall counter per pod |
| 08 | CPU Scheduler | CFS, `sched_entity`, cpuset, NUMA topo | CPU Manager, Topology Manager, RT pods | `cgroup/`: CPU affinity + NUMA placement |
| 09 | K8s Internals | inotify/epoll (informers), Raft (etcd) | API server, etcd MVCC, scheduler plugins | `kubelet/`: node pressure + eviction thresholds |
| 10 | Performance Profiling | perf, off-CPU, flamegraphs, sysctl | pprof in kubelet, k8s sysctl tuning | Full perf report — complete tool |
| 11 | Cluster Operations | kernel panic, NMI, kdump, kexec | Node recovery, rolling upgrades, etcd backup | Cluster health + kernel version audit |

---

## 4. Chapter Internal Structure

Every chapter folder follows this layout:

```
NN-chapter-name/
├── README.md              # Entry point: objectives, reading order, what you'll build
├── kernel/
│   ├── NN-a-topic.md      # Kernel mechanism taught from scratch (source-level)
│   ├── NN-b-topic.md      # Kernel mechanism #2
│   └── NN-c-topic.md      # Kernel mechanism #3
├── k8s/
│   └── NN-k8s-connection.md  # How k8s uses this chapter's kernel mechanisms
└── exercises/
    ├── exercise-name-c/
    │   ├── README.md
    │   ├── Makefile
    │   └── main.c
    └── exercise-name-go/
        ├── README.md
        ├── go.mod
        └── main.go
```

### Kernel Document Convention
Each `.md` file in `kernel/` must contain:
- Kernel source link (elixir.bootlin.com) to the exact struct/function
- Full explanation of the mechanism: what problem it solves, how it is implemented
- **Data structure deep dive (mandatory for every key struct):**
  - Full struct definition quoted from kernel source with source path + line number
  - Every field explained: type, purpose, valid range/values, who sets it, who reads it
  - Memory layout and size (use `pahole` output where relevant)
  - Alignment and cache-line implications for performance-critical structs
  - Lifecycle: when is it allocated, initialized, modified, freed — and by which kernel path
  - Locking discipline: which locks protect which fields, what RCU rules apply
  - Relationships: which structs embed it, which structs it points to — draw the object graph
  - How to observe it live: `crash`, `bpftool`, `/proc/slabinfo`, `/sys/kernel/debug/`
- The `/proc`, `/sys`, or syscall interface the student can touch from userspace
- A minimal C or Go program or shell command that proves the concept is observable
- Reference to LWN article, mailing list thread, or book chapter where relevant

### K8s Connection Document Convention
Each `k8s/` document must contain:
- Which Kubernetes component owns this mechanism (kubelet, kube-proxy, containerd, the scheduler, etc.)
- How to observe the kernel mechanism being used by k8s (`kubectl`, `cat /sys/fs/cgroup/...`, `ip netns`, `bpftool`, etc.)
- Tuning knobs exposed by Kubernetes (resource limits, feature gates, kubelet flags)
- Common failure modes and how to diagnose them from the kernel level

### Exercise Convention
Each exercise must:
- Compile and run on Linux 5.15+ with standard toolchain
- Have a `README.md` with: what it demonstrates, how to build, expected output
- Be standalone (no dependency on kube-inspect)
- Include comments pointing to kernel source for key syscalls

---

## 5. Toolchain & Prerequisites

### Student Environment
- Linux 5.15+ host or VM (Ubuntu 22.04 LTS minimum)
- gcc, clang/llvm 16+, make
- Go 1.22+
- libbpf-dev, linux-headers
- kubectl + kubeconfig to a test cluster (kind or k3s acceptable)
- `bpftool`, `perf`, `numactl`, `strace`, `ss`, `ip`

### No Container-in-Container Tricks
Exercises that need a real cluster use `kind` (Kubernetes in Docker). Namespace/cgroup exercises run directly on the host — no abstraction layers hiding the kernel behavior.

---

## 6. Quality Standards

- Every kernel reference must cite a specific file path and function name in the kernel source
- Every claim about Kubernetes behavior must be verifiable with a `kubectl` command or kernel tool (`cat /sys/fs/cgroup/...`, `ip netns exec ...`, etc.)
- C exercises must compile cleanly with `-Wall -Wextra -Werror`
- Go exercises must pass `go vet` and `golangci-lint run`
- No "it works on my machine" — exercises include `Makefile` targets: `build`, `run`, `clean`

---

## 7. Out of Scope (for now)

- Windows/Mac environments (Linux-only course)
- Managed Kubernetes (EKS/GKE/AKS) — focus is on bare-metal/self-managed
- Service mesh internals (Istio/Linkerd) — eBPF chapter touches the boundary
- GPU scheduling — separate course topic
