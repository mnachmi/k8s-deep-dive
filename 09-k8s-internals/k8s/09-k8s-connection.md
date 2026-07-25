# Chapter 09 — Kubernetes Internals: Kernel Connection

## 1. Architecture Overview

Full pod lifecycle — actor and kernel interface at each phase:

| Phase | Actor | Kernel Interface |
|-------|-------|-----------------|
| Watch | kubelet | HTTP long-poll watch via client-go informer (API server) |
| Admit | kubelet | Writes `cpu.max`, `memory.max`, `cpuset.cpus` to pod cgroup |
| Sandbox | containerd | `clone(CLONE_NEWPID|CLONE_NEWNET|CLONE_NEWNS|...)` |
| OOM setup | kubelet | Writes oom_score_adj to `/proc/<pid>/oom_score_adj` |
| Running | kubelet | Polls `/proc/pressure/*`, `memory.events`, `memory.current` |
| Eviction | kubelet | Sends SIGTERM via CRI StopContainer; SIGKILL after grace period |
| OOM kill | kernel | `oom_kill_process()` → SIGKILL; increments `memory.events oom_kill` |

## 2. Full Pod Creation Chain

```
kubectl apply → API server (etcd write)
     │
     │  Watch event (informer)
     ▼
kubelet.SyncPod()
     │  1. Creates cgroup hierarchy:
     │       mkdir /sys/fs/cgroup/kubepods.slice/kubepods-pod<uid>.slice/
     │       echo <memory_limit> > memory.max
     │       echo "<quota> <period>" > cpu.max
     │
     │  2. Calls CRI: containerd.RunPodSandbox()
     ▼
containerd
     │  pulls pause image if needed
     │  creates OCI spec (namespaces, cgroups, mounts, seccomp)
     │  calls: runc create --bundle /run/containerd/io.containerd.runtime.v2.task/<id>/
     ▼
runc (init process in new namespaces)
     │  clone(CLONE_NEWPID|CLONE_NEWNET|CLONE_NEWNS|CLONE_NEWIPC|CLONE_NEWUTS|SIGCHLD)
     │  pivot_root() → container rootfs
     │  exec(/pause)
     ▼
kernel: pause container PID 1 in pod network namespace
     │
     │  3. kubelet: writes echo <pid> > .../cgroup.procs
     │  4. kubelet: writes echo -998 > /proc/<pid>/oom_score_adj  (Guaranteed pod)
     │
     │  5. containerd.CreateContainer() for each app container:
     │       runc create + setns(net_fd, ipc_fd, uts_fd) + clone(CLONE_NEWPID) + exec
     │
     ▼
App container running in pod namespaces
```

## 3. OOM Kill Lifecycle

```
App container allocates memory
     │
     │  allocation hits memory.max
     ▼
kernel: direct reclaim (kswapd + direct)
     │  if reclaim insufficient:
     ▼
mem_cgroup_out_of_memory()
     │  select_bad_process() within cgroup
     │  oom_kill_process(victim)
     │     ├─ sends SIGKILL
     │     ├─ marks TIF_MEMDIE on victim
     │     └─ wakes oom_reaper to free anonymous memory
     │
     │  increments: memory.events oom += 1, oom_kill += 1
     ▼
kubelet EvictionManager
     │  inotify on memory.events fires
     │  reads oom_kill counter
     │  records OOMKilling event in pod status
     │  may evict pod (if OOM + resource pressure exceeds threshold)
```

## 4. Node Conditions and PSI

kubelet maps kernel PSI data to Kubernetes node conditions:

| Node Condition | Kernel Signal | Threshold (default) |
|----------------|--------------|---------------------|
| `MemoryPressure=True` | `/proc/pressure/memory` some avg10 | > 0% sustained |
| `DiskPressure=True` | `nodefs.available` | < 10% |
| `PIDPressure=True` | `/proc/sys/kernel/pid_max` vs active PIDs | > 95% |

When `MemoryPressure=True`, the scheduler marks the node with the `node.kubernetes.io/memory-pressure` taint. New BestEffort pods cannot be scheduled there. Eviction of existing pods follows the QoS order.

## 5. Eviction Decision Flow

```
kubelet EvictionManager (every 10s)
     │
     │  reads:
     │    /proc/pressure/memory  → some avg10, full avg10
     │    memory.current / memory.max for each pod cgroup
     │    memory.events oom_kill for each pod cgroup
     │    nodeCapacity from /proc/meminfo
     │
     ├─ if memory.available < 100Mi (hard eviction threshold):
     │    evict immediately, starting with BestEffort pods
     │
     └─ if memory.available < 1.5Gi (soft eviction threshold):
          start grace-period timer (default: 1m30s)
          if sustained: evict lowest-priority pod
```

## 6. Common Failure Patterns

| Symptom | Kernel Cause | Diagnosis |
|---------|-------------|-----------|
| Pod OOMKilled status | memory.events oom_kill > 0 | `cat <pod-cgroup>/memory.events` |
| Node MemoryPressure | /proc/pressure/memory some avg10 > threshold | `cat /proc/pressure/memory` |
| Container throttled | cpu.stat throttled_usec rising | `cat <pod-cgroup>/cpu.stat` |
| Slow pod startup | runc clone() > 100ms | bpftrace on sys_enter_clone |
| High involuntary ctxt switches | RT task preempting container | `cat /proc/<pid>/status` |
| PSI spikes but no eviction | Soft threshold not met | Tune `--eviction-soft` |

## 7. Verification Commands

```bash
# Check OOM events for all pods
for cg in /sys/fs/cgroup/kubepods.slice/kubepods-pod*.slice; do
  oom=$(awk '/oom_kill/{print $2}' $cg/memory.events 2>/dev/null)
  [ "$oom" -gt 0 ] 2>/dev/null && echo "$cg: oom_kill=$oom"
done

# Node memory pressure (PSI some avg10)
awk '/some/{printf "memory.some.avg10=%s\n", $2}' /proc/pressure/memory

# OOM score for a container process
cat /proc/$(pgrep -n nginx)/oom_score_adj
cat /proc/$(pgrep -n nginx)/oom_score

# runc clone flags when a pod is created
bpftrace -e 'tracepoint:syscalls:sys_enter_clone { if (comm == "runc:[2:INIT]") printf("flags=%lx\n", args->clone_flags); }'
```

## 8. Key Kernel References

| Symbol | File | URL |
|--------|------|-----|
| `out_of_memory()` | `mm/oom_kill.c` | https://elixir.bootlin.com/linux/v6.9/source/mm/oom_kill.c |
| `mem_cgroup_out_of_memory()` | `mm/memcontrol.c` | https://elixir.bootlin.com/linux/v6.9/source/mm/memcontrol.c |
| `psi_task_change()` | `kernel/sched/psi.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/psi.c |
| `cgroup_procs_write()` | `kernel/cgroup/cgroup.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/cgroup/cgroup.c |
| `kernel_clone()` | `kernel/fork.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/fork.c |
| `proc_oom_score_adj_write()` | `fs/proc/base.c` | https://elixir.bootlin.com/linux/v6.9/source/fs/proc/base.c |
