# Chapter 03 — Cgroups v2

cgroups v2 (control groups, unified hierarchy) is the Linux kernel mechanism that enforces resource limits for containers. Every Kubernetes pod maps to a cgroup directory under `/sys/fs/cgroup/kubepods/`. When you write `resources.limits.memory: 256Mi` in a Pod spec, the kubelet translates that to `echo 268435456 > /sys/fs/cgroup/kubepods/pod<uid>/memory.max`. Without cgroups there are no resource limits, no QoS classes, no OOM isolation — a buggy pod could consume all memory on the node.

## Learning Objectives

1. Navigate the cgroup v2 unified hierarchy under `/sys/fs/cgroup/` and locate a pod's cgroup directory
2. Explain `struct cgroup` field-by-field including subsystem state embedding, kernfs backing, and the reference-counting model
3. Trace `struct css_set` and how `task_struct->cgroups` links a process to its cgroup memberships
4. Describe all four Kubernetes-relevant controllers (memory, cpu, io, pids) including their enforcement paths
5. Use bpftrace and `/sys/fs/cgroup/` to diagnose OOM kills and CPU throttling live

## Prerequisites

- **Chapter 01** — task_struct, process lifecycle (`struct task_struct` embeds the `css_set` pointer via `task_struct->cgroups`)
- **Chapter 02** — cgroup namespace (`CLONE_NEWCGROUP` makes the container see its own cgroup root as `/`)

## Reading Order

| Order | Document | What you learn |
|-------|----------|----------------|
| 1 | kernel/03-a-cgroup-struct.md | struct cgroup field-by-field, lifecycle, locking, hierarchy |
| 2 | kernel/03-b-controllers.md | memory, cpu, io, pids controllers — structs and enforcement paths |
| 3 | kernel/03-c-css-set.md | struct css_set, task-to-cgroup membership, migration |
| 4 | k8s/03-k8s-connection.md | QoS classes, resource limits, OOM lifecycle, CPU throttling |
| 5 | exercises/cgroup-demo/ | C: create a cgroup and observe memory limit enforcement |
| 6 | exercises/cgroup-stats/ | Go: read cgroup v2 stats for any cgroup path or PID |
| 7 | kube-inspect checkpoint 03 | Add cgroup v2 stats reader to kube-inspect |

## Cgroup Hierarchy on a Kubernetes Node

```
/sys/fs/cgroup/                    (cgroup v2 unified root)
├── cgroup.controllers             (available: memory cpu io pids)
├── kubepods/                      (all pod cgroups live here)
│   ├── kubepods.slice/ or direct pod dirs depending on systemd config
│   ├── besteffort/
│   │   └── pod<uid>/
│   │       ├── cgroup.procs      (PIDs in this pod)
│   │       ├── memory.current    (current memory usage)
│   │       ├── memory.max        (limit from resources.limits.memory)
│   │       ├── cpu.max           (quota from resources.limits.cpu)
│   │       ├── cpu.stat          (usage_usec, nr_throttled, ...)
│   │       └── <container-id>/   (per-container cgroup)
│   │           ├── cgroup.procs
│   │           └── memory.current
│   └── burstable/
│       └── pod<uid>/...
└── system.slice/
    └── (system services)
```

The hierarchy mirrors Kubernetes QoS classes directly:
- `kubepods/guaranteed/` — pods where every container has equal `requests` and `limits`
- `kubepods/burstable/` — pods with `requests` set but `limits` unset or higher than requests
- `kubepods/besteffort/` — pods with no `requests` or `limits` set at all

Each pod gets its own directory (`pod<uid>/`) and each container within it gets a subdirectory named after its container ID. The kubelet writes the container's PID into `cgroup.procs` and sets `memory.max`, `cpu.max`, and `pids.max` from the pod spec.
