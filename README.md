# From Kernel Expert to Kubernetes Performance Engineer

This course builds your mastery of Linux kernel internals and shows how Kubernetes leverages them. You will read kernel source, write syscall-level C and Go, debug cgroup hierarchies, and trace system behavior with eBPF. By the end, you will see through Kubernetes abstractions to the kernel syscalls underneath.

Every chapter teaches a Linux kernel mechanism from scratch with kernel source precision, then shows exactly how Kubernetes uses it, then builds one capability into `kube-inspect`, a per-pod performance diagnostic daemon.

This is not a refresher course — kernel fundamentals *are* the course.

---

## Prerequisites

- **Linux 5.15+** (host or VM; Ubuntu 22.04 LTS minimum)
- **gcc**, **clang/llvm 16+**, **make**
- **Go 1.22+**
- **libbpf-dev**, **linux-headers**
- **kubectl + kubeconfig** to a test cluster (kind or k3s acceptable)
- **Tools:** `bpftool`, `perf`, `numactl`, `strace`, `ss`, `ip`, `crash`

---

## How to Use This Course

1. **Read `kernel/` first** — understand the Linux mechanism from source
2. **Then read `k8s/`** — see how Kubernetes configures, limits, or depends on it
3. **Do the exercises** — write C and Go programs that prove the concept works
4. **Build the project** — add one checkpoint to `kube-inspect` per chapter

Each chapter is self-contained. Start with Chapter 00, then proceed in order.

---

## Chapter Index

| # | Title | Kernel Anchor | K8s Focus | Status |
|---|-------|---------------|-----------|--------|
| 00 | Prologue: The Gap | syscall table, `entry_SYSCALL_64` | Why k8s is "just Linux" | coming soon |
| 01 | Process Model | `task_struct`, `clone()`, `do_fork()` | Pod lifecycle, PID ns, PID 1 | coming soon |
| 02 | Namespaces | `nsproxy`, 8 ns types, `setns()` | containerd/runc isolation | coming soon |
| 03 | cgroups v2 | `css_set`, `cgroup_subsys`, controllers | kubelet QoS classes, resource limits | coming soon |
| 04 | Memory | page tables, PSI, OOM killer, NUMA | Eviction manager, HugePages, Topology Manager | coming soon |
| 05 | VFS & Storage | inode, dentry, overlayfs, `mount_tree()` | Image layers, CSI, ephemeral volumes | coming soon |
| 06 | Network Stack | `sk_buff`, veth, netfilter, conntrack | CNI, kube-proxy DNAT, Service ClusterIP | coming soon |
| 07 | eBPF | BPF VM, verifier, maps, CO-RE | Cilium, Hubble, bpftrace in k8s | coming soon |
| 08 | CPU Scheduler | CFS, `sched_entity`, cpuset, NUMA topo | CPU Manager, Topology Manager, RT pods | coming soon |
| 09 | K8s Internals | inotify/epoll (informers), Raft (etcd) | API server, etcd MVCC, scheduler plugins | coming soon |
| 10 | Performance Profiling | perf, off-CPU, flamegraphs, sysctl | pprof in kubelet, k8s sysctl tuning | coming soon |
| 11 | Cluster Operations | kernel panic, NMI, kdump, kexec | Node recovery, rolling upgrades, etcd backup | coming soon |

---

## The Long-Running Project: `kube-inspect`

A Go + libbpf per-pod performance diagnostic daemon built incrementally across all chapters.

### Final Capabilities (Chapter 11)

- Per-pod process tree and namespace enumeration
- Per-pod cgroup v2 resource stats (memory, CPU, IO) and PSI metrics
- Per-pod network namespace interface stats
- Per-pod eBPF syscall frequency counter
- CPU affinity and NUMA placement analysis
- Kubelet eviction threshold overlay
- Prometheus `/metrics` endpoint
- JSON report mode for one-shot diagnostics

### Architecture

```
kube-inspect/
├── cmd/kube-inspect/main.go       # CLI: --pod, --node, --json
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

---

## Quick Start

```bash
# Clone and enter the course repo
cd /path/to/k8s-deep-dive

# Read the prologue
cd 00-prologue
cat README.md

# Build and run the first exercise
cd exercises/syscall-tracer
make build
make run

# Start the kube-inspect project
cd ../../kube-inspect
make build
./kube-inspect --help
```

---

## References & Resources

See [docs/references.md](docs/references.md) for kernel source, LWN.net, LKML, books, and tools.

See [docs/sysctl-k8s-cheatsheet.md](docs/sysctl-k8s-cheatsheet.md) for kernel tuning parameters relevant to Kubernetes.

See [docs/ebpf-cheatsheet.md](docs/ebpf-cheatsheet.md) for eBPF examples used throughout the course.

---

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for build requirements, code style, exercise verification, and how to add new chapters.

---

## License

This course and all code examples are open source. Kernel source citations use Linux kernel source available at https://elixir.bootlin.com/.
