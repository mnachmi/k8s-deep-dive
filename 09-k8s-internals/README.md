# Chapter 09 — Kubernetes Internals

Every pod lifecycle event is a sequence of kernel calls. This chapter follows kubelet from its API-server watch through the CRI gRPC call to containerd, through runc's clone() invocation, and into the kernel data structures that govern resource isolation and pressure reporting. It then dives into two mechanisms kubelet relies on most heavily at runtime: the OOM killer (which enforces memory limits when reclaim fails) and Pressure Stall Information (which tells kubelet when a node or cgroup is under resource stress).

## Learning Objectives

1. Understand how kubelet reads /proc and /sys/fs/cgroup to monitor pods
2. Trace a pod sandbox creation from RunPodSandbox (CRI) through runc to the kernel clone() call
3. Understand the OOM killer: struct oom_control, oom_badness(), oom_score_adj, memory.events
4. Understand PSI: struct psi_group, /proc/pressure/*, per-cgroup PSI files, kubelet eviction thresholds
5. Read node and pod resource pressure from the kernel's own accounting interfaces

## Prerequisites

- Chapter 01 — Process Model (task_struct, clone, namespaces)
- Chapter 03 — cgroups (cgroup v2 hierarchy, memory.max, cpu.max)
- Chapter 04 — Memory (OOM fundamentals, reclaim)
- Chapter 08 — Scheduler (QoS classes, cpu.weight, oom_score_adj values)

## Reading Order

| File | Topic |
|------|-------|
| `kernel/09-a-kubelet.md` | kubelet kernel interface: /proc, /sys/fs/cgroup, CRI, runc, clone() |
| `kernel/09-b-oom.md` | OOM killer: struct oom_control, oom_badness, oom_score_adj, memory.events |
| `kernel/09-c-psi.md` | PSI: struct psi_group, /proc/pressure/*, cgroup PSI, kubelet eviction |
| `k8s/09-k8s-connection.md` | Full pod lifecycle: API server → kubelet → containerd → runc → kernel |
| `exercises/oom-score-demo/` | C: read/write oom_score_adj; observe OOM badness score |
| `exercises/node-pressure-reader/` | Go: parse /proc/pressure/* and cgroup memory.events |
| `kube-inspect` checkpoint 09 | Node PSI + per-pod OOM events |

## Kernel to K8s Bridge

```
API server (etcd)
    │  watch event
    ▼
kubelet (user space)
    ├─ reads /proc/<pid>/oom_score_adj          → sets per-QoS OOM priority
    ├─ reads /sys/fs/cgroup/.../memory.events   → detects OOM events
    ├─ reads /proc/pressure/{cpu,memory,io}     → node resource pressure
    ├─ reads /sys/fs/cgroup/.../memory.pressure → per-cgroup PSI
    └─ CRI gRPC → containerd → runc
                                └─ clone(CLONE_NEWPID|CLONE_NEWNET|...) → kernel
```
