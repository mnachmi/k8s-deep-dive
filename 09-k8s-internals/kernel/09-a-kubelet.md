# 09-a — kubelet's Kernel Interface and Pod Sandbox Creation

## The Agent at the Boundary

kubelet is the Kubernetes component that lives on every node and translates the control plane's intentions into kernel operations. It is, in a sense, a very sophisticated shell script: receive a PodSpec, create cgroup directories, write resource limits, call the container runtime, write OOM scores, poll health check endpoints, report status back to the API server, and clean up when pods are deleted.

What makes kubelet interesting from a kernel perspective is how thin its interface with the kernel actually is. kubelet does not make unusual syscalls. It does not load kernel modules. It does not use custom kernel interfaces. Its entire kernel interaction is through the VFS — reading and writing files under `/proc/`, `/sys/fs/cgroup/`, and Unix domain sockets. Every pod lifecycle event, from creation to OOM kill detection to graceful termination, is implemented by reading and writing files.

This thinness is not accidental — it is the design principle that makes Kubernetes portable across kernel versions. kubelet can run on Linux 5.4 (the minimum for most cloud providers) or Linux 6.9 because it does not depend on any new kernel ABI beyond the cgroup v2 interface. When a new kernel feature becomes available (like cgroup v2, like PSI, like pidfd), kubelet adds support for it as an optional improvement rather than a hard dependency.

Understanding kubelet's kernel interface means you can:
- Watch what kubelet is actually doing when a pod starts or stops (strace it)
- Verify that resource limits were written correctly (read the cgroup files directly)  
- See why a pod was OOM-killed (check memory.events before kubelet reports it)
- Understand why OOM score adjustment matters for which process gets killed first

## 1. Source Locations

| File | Key Symbols | URL |
|------|-------------|-----|
| `kernel/fork.c` | `copy_process()`, `kernel_clone()` | https://elixir.bootlin.com/linux/v6.9/source/kernel/fork.c |
| `fs/proc/base.c` | `/proc/<pid>/oom_score_adj`, `/proc/<pid>/oom_score` | https://elixir.bootlin.com/linux/v6.9/source/fs/proc/base.c |
| `kernel/cgroup/cgroup.c` | `cgroup_attach_task()`, `cgroup_procs_write()` | https://elixir.bootlin.com/linux/v6.9/source/kernel/cgroup/cgroup.c |
| `kernel/cgroup/cpuset.c` | `cpuset_attach()`, `cpuset_write_resmask()` | https://elixir.bootlin.com/linux/v6.9/source/kernel/cgroup/cpuset.c |
| `include/uapi/linux/sched.h` | `CLONE_NEWPID`, `CLONE_NEWNET`, `CLONE_NEWNS`, `CLONE_NEWIPC`, `CLONE_NEWUTS` | https://elixir.bootlin.com/linux/v6.9/source/include/uapi/linux/sched.h |

## 2. kubelet's Kernel Interface

kubelet communicates with the kernel exclusively through the VFS — no kernel modules, no custom syscalls:

| Interface | Path | What kubelet reads/writes |
|-----------|------|--------------------------|
| Process listing | `/proc/<pid>/status`, `/proc/<pid>/cgroup` | pid→cgroup mapping |
| OOM priority | `/proc/<pid>/oom_score_adj` | sets -998 (Guaranteed), 1000 (BestEffort) |
| Memory usage | `/sys/fs/cgroup/.../memory.current` | current RSS |
| Memory events | `/sys/fs/cgroup/.../memory.events` | oom, oom_kill counters |
| Node pressure | `/proc/pressure/memory`, `/proc/pressure/cpu` | PSI averages |
| cgroup PSI | `/sys/fs/cgroup/.../memory.pressure` | per-pod PSI |
| CPU quota | `/sys/fs/cgroup/.../cpu.max` | writes limits on pod admit |
| CPU pinning | `/sys/fs/cgroup/.../cpuset.cpus` | writes CPU affinity |

kubelet opens these files with `open(2)` and reads them with `read(2)` on a polling interval (default 10 s for resource metrics, 1 s for OOM events via inotify). For `memory.events` monitoring, kubelet registers an inotify watch so the kernel notifies it on each OOM event without polling.

## 3. Pod Sandbox Creation — Kernel View

When kubelet calls the CRI `RunPodSandbox`:

```
kubelet
  │  gRPC: RunPodSandbox(PodSandboxConfig)
  ▼
containerd (containerd.sock)
  │  starts pause container (holds network/IPC/UTS namespaces)
  │  calls: runc create --bundle <ocispec>
  ▼
runc
  │  reads OCI spec: namespaces, cgroups, mounts, seccomp
  │  calls: clone(CLONE_NEWPID|CLONE_NEWNET|CLONE_NEWNS|CLONE_NEWIPC|CLONE_NEWUTS|SIGCHLD)
  ▼
kernel: kernel_clone() → copy_process()
  │  copy_process() allocates task_struct, copies mm, files, signal, nsproxy
  │  creates new namespaces for each CLONE_NEW* flag
  │  returns: child PID visible in parent's namespace
  ▼
runc (child process in new namespaces)
  │  unshare(CLONE_NEWUSER) if user namespace enabled
  │  pivot_root() to container rootfs
  │  mount(MS_BIND) for volumes and /dev
  │  exec("/pause") — the pause container main loop
```

The pause binary runs an infinite sleep loop. Its sole purpose is to hold the network, IPC, and UTS namespaces alive so that app containers can join with `setns(2)`.

## 4. Joining Namespaces — `setns(2)` Path

When kubelet calls `CreateContainer` for an app container:

```
containerd
  │  opens /proc/<pause_pid>/ns/net   (fd for network namespace)
  │       /proc/<pause_pid>/ns/ipc   (fd for IPC namespace)
  │       /proc/<pause_pid>/ns/uts   (fd for UTS namespace)
  │  calls: runc create (second container)
  ▼
runc (app container)
  │  setns(net_fd, CLONE_NEWNET)   → joins pause's network namespace
  │  setns(ipc_fd, CLONE_NEWIPC)  → joins pause's IPC namespace
  │  setns(uts_fd, CLONE_NEWUTS)  → joins pause's UTS namespace
  │  clone(CLONE_NEWPID)          → NEW PID namespace for this container
  │  exec(container_entrypoint)
  ▼
kernel: setns() → switch_task_namespaces()
  │  replaces nsproxy fields: net_ns, ipc_ns, uts_ns
  │  increments ns reference counts
```

## 5. cgroup Assignment

After fork, runc writes the new process PID to the pod's cgroup:

```bash
echo <pid> > /sys/fs/cgroup/kubepods.slice/kubepods-pod<uid>.slice/<container>.scope/cgroup.procs
```

The kernel's `cgroup_procs_write()` in `kernel/cgroup/cgroup.c` moves the task to the new cgroup, updating all per-cgroup resource counters (memory.current, cpu.stat, etc.). After this write, the cgroup's memory controller begins charging the task's allocations to the pod's memory.max limit.

## 6. Live Observation

```bash
# Watch kubelet's file descriptor activity (what it reads at pod create)
sudo strace -f -e openat,read -p $(pidof kubelet) 2>&1 | grep -E "cgroup|pressure|oom_score"

# Trace all clone() calls from containerd (see namespace flags)
bpftrace -e '
tracepoint:syscalls:sys_enter_clone {
    if (comm == "runc:[2:INIT]") {
        printf("runc clone flags=0x%lx pid=%d\n", args->clone_flags, pid);
    }
}'

# Watch cgroup.procs writes (task assignment to pod cgroup)
bpftrace -e 'kprobe:cgroup_procs_write { printf("cgroup assign pid=%d comm=%s\n", pid, comm); }'
```

## 7. Key Kernel References

| Symbol | File | URL |
|--------|------|-----|
| `kernel_clone()` | `kernel/fork.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/fork.c |
| `copy_process()` | `kernel/fork.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/fork.c |
| `CLONE_NEW*` flags | `include/uapi/linux/sched.h` | https://elixir.bootlin.com/linux/v6.9/source/include/uapi/linux/sched.h |
| `setns(2)` → `switch_task_namespaces()` | `kernel/nsproxy.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/nsproxy.c |
| `cgroup_procs_write()` | `kernel/cgroup/cgroup.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/cgroup/cgroup.c |
| `proc_oom_score_adj_write()` | `fs/proc/base.c` | https://elixir.bootlin.com/linux/v6.9/source/fs/proc/base.c |
