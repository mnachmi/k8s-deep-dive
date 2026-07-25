# 01 — Kubernetes and the Process Model: From kubelet to `task_struct`

This document bridges the kernel internals covered in
[01-a (task_struct)](../kernel/01-a-task-struct.md),
[01-b (clone flags)](../kernel/01-b-clone-flags.md), and
[01-c (PID namespaces)](../kernel/01-c-pid-namespaces.md)
to what you observe when operating a Kubernetes cluster. Every `kubectl apply` for a
Pod ultimately resolves to a sequence of kernel calls that produce one or more
`task_struct` entries in the kernel's process table.

---

## 1. How kubelet Creates a Pod Process

The chain from `kubectl apply` to a running container process spans five distinct
layers: the Kubernetes control plane, kubelet, the CRI daemon (containerd), the OCI
runtime (runc), and finally the Linux kernel. Each layer passes work downward until
the kernel issues the `clone3()` call that brings the container into existence.

### 1.1 kubelet watches the API server via epoll (HTTP/2 watch connection)

kubelet does not poll the API server. Instead it opens a long-lived HTTP/2 watch
connection (a Watch on its assigned Node's Pods resource). Under the hood, the
Go standard library uses `epoll_wait(2)` on the TCP socket file descriptor. When the
API server pushes a new Pod assignment over the wire, `epoll_wait` returns, the watch
handler fires, and kubelet places a work item on its pod worker queue.

The kernel mechanism that makes this efficient is the `epoll` event-driven I/O
multiplexer:
- kubelet's goroutine runtime registers the watch socket fd with `epoll_ctl(2)`.
- `epoll_wait(2)` blocks the goroutine's OS thread in `TASK_INTERRUPTIBLE` state
  (`task_struct.__state = 1`; see
  [`include/linux/sched.h:747`](https://elixir.bootlin.com/linux/v6.9/source/include/linux/sched.h#L747)).
- When data arrives, the kernel moves the task back to `TASK_RUNNING (0)` and it
  returns from `epoll_wait`.

### 1.2 kubelet calls containerd via CRI gRPC: RunPodSandbox

Once kubelet decides to create a pod it issues a gRPC call to containerd over the
CRI (Container Runtime Interface) Unix domain socket, typically at
`/run/containerd/containerd.sock`.

The CRI call sequence for a new pod:

```
kubelet                         containerd
  │                                 │
  │── RunPodSandbox(PodSandboxConfig) ──►│
  │                                 │  (creates pause container and pod-level namespaces)
  │◄── RunPodSandboxResponse(SandboxID) ─│
  │                                 │
  │── CreateContainer(SandboxID, ContainerConfig) ──►│
  │◄── CreateContainerResponse(ContainerID) ──────────│
  │                                 │
  │── StartContainer(ContainerID) ──►│
  │◄── StartContainerResponse() ─────│
```

kubelet sends one `RunPodSandbox` for the entire pod, then one `CreateContainer` +
`StartContainer` pair per container listed in the PodSpec.

### 1.3 containerd calls runc

containerd receives `RunPodSandbox` and prepares the OCI bundle:
1. Pulls or finds the pause image (`registry.k8s.io/pause:3.9`).
2. Constructs an OCI runtime spec (`config.json`) specifying the Linux namespace
   configuration, cgroup path, and the root filesystem.
3. Invokes runc as a child process:
   ```
   runc create --bundle /run/containerd/io.containerd.runtime.v2.task/<pod>/<id> \
               --pid-file /run/containerd/... <container-id>
   ```

containerd uses the `containerd-shim-runc-v2` binary as an intermediary so that
containerd itself can restart without orphaning running containers.

### 1.4 runc calls `clone3()` with namespace flags

runc reads the OCI runtime spec and calls the kernel to create the container process.
For a standard pod sandbox (the pause container), runc calls `clone3()` with:

```c
/* runc container sandbox creation — kernel perspective */
struct clone_args args = {
    .flags = CLONE_NEWPID    /* 0x20000000 — new PID namespace    */
           | CLONE_NEWNET    /* 0x40000000 — new network namespace */
           | CLONE_NEWNS     /* 0x00020000 — new mount namespace   */
           | CLONE_NEWUTS    /* 0x04000000 — new UTS namespace     */
           | CLONE_NEWIPC,   /* 0x08000000 — new IPC namespace     */
    .exit_signal = SIGCHLD,
    .cgroup = <pod-cgroup-fd>, /* cgroups v2: atomic placement */
};
syscall(SYS_clone3, &args, sizeof(args));
```

All five `CLONE_NEW*` flags are defined in
[`include/uapi/linux/sched.h`](https://elixir.bootlin.com/linux/v6.9/source/include/uapi/linux/sched.h).

The kernel routes `clone3()` through:

```
sys_clone3()          kernel/fork.c:3178
  └─ kernel_clone()   kernel/fork.c:2979
       └─ copy_process()  kernel/fork.c:2105
            ├─ dup_task_struct()      — allocate new task_struct
            ├─ copy_namespaces()      — kernel/nsproxy.c:152
            │    ├─ create_pid_namespace()   — new PID ns; child is PID 1
            │    ├─ copy_net_ns()            — new struct net with lo only
            │    ├─ copy_mnt_ns()            — private mount tree copy
            │    ├─ copy_utsname()           — private hostname copy
            │    └─ copy_ipcs()              — empty IPC object tables
            └─ alloc_pid()            — kernel/pid.c:180 — assigns host PID + PID 1 in namespace
```

See [`kernel/fork.c:2105`](https://elixir.bootlin.com/linux/v6.9/source/kernel/fork.c#L2105)
for the full `copy_process()` implementation.

### 1.5 cgroup membership setup

runc places the new container process into its pod cgroup immediately after `clone3()`
returns (or atomically at creation time if using the `cgroup` field in `struct clone_args`,
available since Linux 5.7).

On a cgroups v2 system:

```bash
# kubelet pre-creates the pod cgroup directory:
# /sys/fs/cgroup/kubepods/burstable/pod<pod-uid>/

# runc writes the new container PID to cgroup.procs:
echo <container-pid> > /sys/fs/cgroup/kubepods/burstable/pod<pod-uid>/<container-id>/cgroup.procs
```

At the kernel level, writing a PID to `cgroup.procs` calls `cgroup_attach_task()` in
[`kernel/cgroup/cgroup.c`](https://elixir.bootlin.com/linux/v6.9/source/kernel/cgroup/cgroup.c),
which updates `task_struct.cgroups` (the `css_set` pointer at
[`include/linux/sched.h:1309`](https://elixir.bootlin.com/linux/v6.9/source/include/linux/sched.h#L1309))
to reflect the new cgroup membership. After this write, the kernel enforces all cgroup
resource limits (cpu quota, memory limit, PID limit) defined in the pod's cgroup tree.

### 1.6 runc calls `execve()` with the container entrypoint

After setting up the container's filesystem (`pivot_root`), cgroup, and namespace
state, runc calls `execve(2)` with the container image's entrypoint (e.g.,
`/usr/sbin/nginx`) and arguments from the OCI spec. For the pause container, the
entrypoint is the minimal `/pause` binary.

`execve()` in the kernel:
- Calls `do_execveat_common()` in
  [`fs/exec.c`](https://elixir.bootlin.com/linux/v6.9/source/fs/exec.c).
- Loads the ELF binary and replaces the process's memory map (`mm_struct`).
- Sets `task_struct.comm` to the new binary's basename (see
  [`include/linux/sched.h:975`](https://elixir.bootlin.com/linux/v6.9/source/include/linux/sched.h#L975)).
- The `task_struct` created in step 1.4 is now running the container's actual binary.

### Complete chain summary

```
kubectl apply
  │
  ▼ (API server stores Pod in etcd, schedules to node)
kubelet epoll_wait() wakes on watch event
  │
  ▼ CRI gRPC: RunPodSandbox → containerd
containerd prepares OCI bundle, forks containerd-shim-runc-v2
  │
  ▼ containerd-shim executes: runc create
runc calls clone3(CLONE_NEWPID|CLONE_NEWNET|CLONE_NEWNS|CLONE_NEWUTS|CLONE_NEWIPC)
  │
  ▼ kernel: copy_process() → copy_namespaces() → alloc_pid()
new task_struct allocated; pause process is PID 1 in pod's PID namespace
  │
  ▼ runc: write PID to /sys/fs/cgroup/<pod-uid>/cgroup.procs
task_struct.cgroups updated; resource limits now enforced
  │
  ▼ runc: execve("/pause")
task_struct.comm = "pause"; process calls pause() syscall and sleeps
  │
  ▼ CRI gRPC: CreateContainer + StartContainer per app container
runc: clone3() without CLONE_NEWPID/NEWNET/NEWIPC → setns() into pause's ns
runc: execve("/usr/sbin/nginx") (or whatever the container image specifies)
```

---

## 2. Pod Process Tree on the Host

From the host's perspective, every pod's processes are fully visible. The kernel sees
no "containers" — it sees processes organized in a tree, each with its own `nsproxy`
pointer that happens to point to different namespace structs.

### 2.1 The expected process tree

```
systemd (pid 1)
├─ kubelet (pid ~1200)
├─ containerd (pid ~1350)
└─ containerd-shim-runc-v2 (pid ~4100)   ← re-parents to systemd at startup
   └─ pause (pid ~4110)
      ├─ nginx (pid ~4120)                ← same PID namespace as pause
      └─ sidecar (pid ~4125)              ← same PID namespace as pause
```

Note: from the kernel's `real_parent` perspective, `nginx` and `sidecar` are children
of the shim (not pause). The tree above shows the PID-namespace-grouped view that
tools like `pstree --ns` display.

The containerd-shim re-parents itself to systemd (PID 1) at startup — that is the
entire point of the shim design: containerd can restart without orphaning containers.
`nginx` and `sidecar` join `pause`'s PID namespace via `setns()`, so their
`task_struct.nsproxy->pid_ns_for_children` points to pause's PID namespace, but their
`task_struct.real_parent` points to the shim process. Use `pstree --ns` or `pstree -n`
to see the namespace-grouped view shown above.

### 2.2 Commands to observe the pod process tree

```bash
# Full process tree from kubelet downward (requires root or same user as kubelet)
pstree -p $(pidof kubelet)

# Show pause containers and shims with their host PIDs
ps -eo pid,ppid,comm,args | grep -E "pause|containerd-shim"

# Detailed view: show all container processes with PID, PPID, namespace inode
ps -eo pid,ppid,netns,pidns,comm | grep -v "^  PID"

# Show the namespace inode numbers — processes sharing one are in the same pod
lsns -t pid
lsns -t net
```

### 2.3 Mapping a container's host PID to its pod

```bash
# If you know the pod's UID (from kubectl get pod -o yaml | grep uid):
POD_UID="7f8c2d1a-b3e4-..."

# List all PIDs in that pod's cgroup:
cat /sys/fs/cgroup/kubepods/burstable/pod${POD_UID}/cgroup.procs

# For each PID, confirm it's in the pod's PID namespace:
PID=4120
cat /proc/${PID}/status | grep -E 'Name|NSpid|NStgid'
# Name: nginx
# NSpid: 4120 10     ← host PID 4120 = container PID 10

# Enter the container's namespaces and run ps to see container-local view:
nsenter --pid=/proc/${PID}/ns/pid \
        --net=/proc/${PID}/ns/net \
        --mount=/proc/${PID}/ns/mnt \
        -- ps aux
```

### 2.4 Observe namespace sharing with lsns

```bash
# Show all PID namespaces and which process is PID 1 in each:
lsns -t pid -o NS,TYPE,NPROCS,PID,USER,COMMAND

# Example output (two pods running):
#          NS TYPE NPROCS   PID USER COMMAND
# 4026531836 pid     142     1 root /sbin/init     ← host init namespace
# 4026532411 pid       3  4110 root /pause          ← pod A sandbox
# 4026532519 pid       2  5201 root /pause          ← pod B sandbox

# Cross-reference the PID namespace inode with /proc:
PAUSE_PID=4110
readlink /proc/${PAUSE_PID}/ns/pid   # shows pid:[4026532411]
readlink /proc/4120/ns/pid           # same inode → nginx is in pod A's ns
```

---

## 3. The pause Container

### 3.1 What pause is

The pause container is a minimal C program whose entire purpose is to hold the pod's
Linux namespaces open. Its source is at
`kubernetes/build/pause/pause.c` in the Kubernetes repository, and it compiles to
a binary that calls a single syscall:

```c
/* kubernetes/build/pause/pause.c (simplified) */
#include <signal.h>
#include <unistd.h>

static void handler(int sig) {}

int main(void) {
    signal(SIGCHLD, handler);   /* reap orphaned container children */
    for (;;)
        pause();                /* syscall: sleep until a signal arrives */
    return 0;
}
```

`pause(2)` suspends the process until a signal is delivered, consuming no CPU. At the
kernel level, `pause()` calls `do_pause()` which calls `schedule()` with the task in
`TASK_INTERRUPTIBLE` state. The process sits idle in memory, holding references to
its `nsproxy` (and thus to the pod's PID, net, mnt, UTS, and IPC namespace structs).

The pause container is started from the image `registry.k8s.io/pause:3.9`
(or the version configured in kubelet's `--pod-infra-container-image` flag).

### 3.2 Why the pause container exists: namespace anchoring

A Linux namespace lives as long as at least one process references it, OR a bind mount
or open file descriptor holds a reference to its `nsfs` inode. When all processes leave
a namespace with no other references, the kernel frees it.

Without pause, the pod's namespaces would be tied to the app container processes. If
the app container crashes and is restarted by kubelet/containerd, it would need to be
placed into a brand-new namespace — destroying all network state, restarting with a
fresh PID table, etc. This would break Service IP connectivity and cause init-zombie
issues.

The pause container solves this by being:
1. The first process in the pod's PID namespace → it becomes PID 1 (`pid_namespace.child_reaper`
   in [`include/linux/pid_namespace.h:10`](https://elixir.bootlin.com/linux/v6.9/source/include/linux/pid_namespace.h#L10)).
2. The holder of the pod's `nsproxy` pointer → as long as pause is alive, all six
   namespace structs referenced from its `task_struct.nsproxy` remain allocated.
3. A signal handler for `SIGCHLD` → it calls `pause()` in a loop, so it reaps any
   zombie processes that become orphans in the pod's PID namespace (acting as a minimal
   init reaper).

Container runtimes keep the pause container running for the entire pod lifetime.
kubelet will restart the entire pod (including pause) only if the pause container exits.

### 3.3 Which namespaces app containers share vs own

When runc creates an app container (nginx, a sidecar, etc.) in an existing pod, it does
NOT pass `CLONE_NEWPID`, `CLONE_NEWNET`, or `CLONE_NEWIPC` to `clone3()`. Instead,
after creating the new process, runc calls `setns(2)` to attach the new process to
pause's existing namespaces:

```c
/* runc app container creation — conceptual */

/* Step 1: clone3() without CLONE_NEWPID/NEWNET/NEWIPC (get a bare process) */
struct clone_args args = {
    .flags = CLONE_NEWNS     /* 0x00020000 — private mount ns for the container's fs */
           | CLONE_NEWUTS,   /* 0x04000000 — private UTS (hostname can differ) */
    .exit_signal = SIGCHLD,
};
pid_t child = syscall(SYS_clone3, &args, sizeof(args));

/* Step 2: in the child, call setns() to join pause's namespaces */
/* (runc does this before execve, from inside the child process) */
int pid_ns_fd  = open("/proc/<pause-pid>/ns/pid",  O_RDONLY);
int net_ns_fd  = open("/proc/<pause-pid>/ns/net",  O_RDONLY);
int ipc_ns_fd  = open("/proc/<pause-pid>/ns/ipc",  O_RDONLY);

setns(pid_ns_fd,  CLONE_NEWPID);   /* join pause's PID namespace */
setns(net_ns_fd,  CLONE_NEWNET);   /* join pause's network namespace */
setns(ipc_ns_fd,  CLONE_NEWIPC);   /* join pause's IPC namespace */

/* Step 3: execve into the actual container image entrypoint */
execve("/usr/sbin/nginx", argv, envp);
```

`setns(2)` is implemented in the kernel at
[`kernel/nsproxy.c:558`](https://elixir.bootlin.com/linux/v6.9/source/kernel/nsproxy.c#L558).
It updates `task_struct.nsproxy` to point to a new `nsproxy` that references the
target namespace, incrementing the namespace's refcount.

**Summary of namespace ownership per container type:**

| Namespace | pause (sandbox) | app container (nginx, etc.) |
|-----------|----------------|-----------------------------|
| PID (`CLONE_NEWPID`) | **owns** — creates it; is PID 1 | **joins** via `setns()` |
| Network (`CLONE_NEWNET`) | **owns** — creates it; gets `eth0` | **joins** via `setns()` |
| IPC (`CLONE_NEWIPC`) | **owns** — creates it | **joins** via `setns()` |
| Mount (`CLONE_NEWNS`) | owns its own (for `/dev` etc.) | **own** — each gets its own private mount ns for its filesystem image |
| UTS (`CLONE_NEWUTS`) | owns pod's UTS | typically **own** (each container has `pod.hostname`) |

The result: all containers in a pod share the same `struct net` (one IP address, one
`eth0`), the same IPC object tables, and see each other in `ps` under the same PID
namespace.

### 3.4 Verifying namespace sharing

```bash
# Get the pause container's host PID for a pod
PAUSE_PID=$(ps -eo pid,comm | awk '/pause/{print $1}' | head -1)

# Get the nginx container's host PID (adjust grep to match your container)
NGINX_PID=$(ps -eo pid,comm | awk '/nginx/{print $1}' | head -1)

# Compare their network namespace inodes — they must be identical:
readlink /proc/${PAUSE_PID}/ns/net
readlink /proc/${NGINX_PID}/ns/net
# Both should print: net:[4026532412]  (same inode)

# Compare their PID namespace inodes:
readlink /proc/${PAUSE_PID}/ns/pid
readlink /proc/${NGINX_PID}/ns/pid
# Both should print: pid:[4026532411]  (same inode)

# Use lsns to see a compact namespace ownership map:
lsns | grep -E "$(readlink /proc/${PAUSE_PID}/ns/net | grep -o '[0-9]*')|$(readlink /proc/${PAUSE_PID}/ns/pid | grep -o '[0-9]*')"
```

---

## 4. `shareProcessNamespace: true` — Kernel Mechanics

### 4.1 What it does

By default each container in a pod gets its own PID namespace (while sharing the
network and IPC namespaces with pause). When you set `shareProcessNamespace: true`
in the PodSpec, Kubernetes instructs the CRI to place all app containers into the
same PID namespace — the one owned by the pause container.

This means every container can see every other container's processes. A process in
container A can send signals to a process in container B. `ps aux` in any container
shows all pod processes.

### 4.2 Kernel mechanism: `setns(fd, CLONE_NEWPID)` for all app containers

With `shareProcessNamespace: false` (default):
- Container A gets its own PID namespace (`clone3(CLONE_NEWPID)` or `setns` into A's
  private ns).
- Container B gets its own PID namespace.
- They share the network and IPC namespaces of pause.

With `shareProcessNamespace: true`:
- The CRI calls `setns(fd_to_pause_pid_ns, CLONE_NEWPID)` for every app container
  before `execve()`.
- All containers join the same `struct pid_namespace` (the one created for pause).
- The pause process is PID 1 (`pid_namespace.child_reaper`) for all containers.
- All app containers' `task_struct.nsproxy->pid_ns_for_children` point to the same
  `struct pid_namespace`.

`setns` with `CLONE_NEWPID` is implemented at
[`kernel/nsproxy.c:558`](https://elixir.bootlin.com/linux/v6.9/source/kernel/nsproxy.c#L558).
The calling thread's `nsproxy` is replaced with one that references the target PID
namespace's `struct pid_namespace`.

### 4.3 The pause container as PID 1 and child reaper

With `shareProcessNamespace: true`, the pause container is PID 1 in the shared
namespace. This has an important consequence: if any container process exits without a
parent waiting for it (a zombie), pause's `SIGCHLD` handler and subsequent `pause()`
call allows the kernel to reparent the zombie to the pause container (PID 1), and pause
reaps it.

This is correct zombie-reaping behavior:
- `pid_namespace.child_reaper` (defined at
  [`include/linux/pid_namespace.h:10`](https://elixir.bootlin.com/linux/v6.9/source/include/linux/pid_namespace.h#L10))
  points to the pause process.
- When a container process's parent exits, the kernel reparents it to `child_reaper`.
- pause's `SIGCHLD` handler fires; pause calls `waitpid(-1, ...)` to reap.

### 4.4 Verifying with kubectl

```bash
# Deploy a pod with shareProcessNamespace: true and two containers
# Container 'a' runs sleep infinity; container 'b' runs ps aux once

kubectl run shared-ns --image=busybox --restart=Never \
  --overrides='{
    "spec": {
      "shareProcessNamespace": true,
      "containers": [
        {
          "name": "a",
          "image": "busybox",
          "command": ["sleep", "infinity"]
        },
        {
          "name": "b",
          "image": "busybox",
          "command": ["ps", "aux"]
        }
      ]
    }
  }'

# Wait for the pod to complete (container b runs ps and exits):
kubectl wait --for=condition=Succeeded pod/shared-ns --timeout=30s

# Read container b's output — it shows processes from container a:
kubectl logs shared-ns -c b
# PID   USER     TIME  COMMAND
#   1 root       0:00 /pause         ← pause is PID 1 in the shared namespace
#   7 root       0:00 sleep infinity ← container a's process visible in container b
#  13 root       0:00 ps aux

# Clean up:
kubectl delete pod shared-ns
```

The log output from container `b` shows `sleep infinity` (container `a`'s process)
and `/pause` (PID 1). This confirms that all three processes share a single PID
namespace at the kernel level.

### 4.5 Verifying at the kernel level

```bash
# Find the pause PID for your shared-namespace pod
PAUSE_PID=$(crictl pods --name shared-ns -q | xargs -I{} crictl inspectp {} | \
  python3 -c "import sys,json; d=json.load(sys.stdin); print(d['info']['pid'])")

# Find the sleep PID (container a)
SLEEP_PID=$(pgrep -x sleep)

# Verify they share the same PID namespace inode:
readlink /proc/${PAUSE_PID}/ns/pid
readlink /proc/${SLEEP_PID}/ns/pid
# Both print the same: pid:[4026532XXX]

# Confirm the NSpid values — sleep's container-local PID should be > 1:
cat /proc/${SLEEP_PID}/status | grep NSpid
# NSpid: <host-pid>  7    ← host PID and container PID 7 (matches ps aux output)
```

---

## 5. bpftrace Observations for Pod Lifecycle

The following bpftrace one-liners let you observe the kernel events as a pod is
created.

### 5.1 Watch clone3() calls from runc

```bash
# Trace every clone3() syscall, showing which process calls it and what flags are set
# Run this before deploying a pod with: kubectl run demo --image=nginx
bpftrace -e '
tracepoint:syscalls:sys_enter_clone3 {
    $flags = *(uint64 *)args->uargs;
    printf("pid=%-6d comm=%-16s flags=0x%08llx  NEWPID=%d NEWNET=%d NEWNS=%d NEWUTS=%d NEWIPC=%d\n",
        pid, comm, $flags,
        ($flags & 0x20000000) != 0,
        ($flags & 0x40000000) != 0,
        ($flags & 0x00020000) != 0,
        ($flags & 0x04000000) != 0,
        ($flags & 0x08000000) != 0);
}'
```

Expected output when a pod is created (runc creating the pause sandbox):
```
pid=89214  comm=runc:init       flags=0x6c020000  NEWPID=1 NEWNET=1 NEWNS=1 NEWUTS=1 NEWIPC=1
```

### 5.2 Trace new process creation through kernel_clone

```bash
# kernel_clone replaced do_fork in Linux 5.10 (commit cad6967ac298)
# arg0 = struct kernel_clone_args *
bpftrace -e '
kprobe:kernel_clone {
    $args = (struct kernel_clone_args *)arg0;
    printf("kernel_clone: caller=%-16s flags=0x%llx\n", comm, $args->flags);
}'
```

### 5.3 Watch namespace creation

```bash
# Trace PID namespace creation (fires when CLONE_NEWPID is set)
bpftrace -e '
kprobe:create_pid_namespace {
    printf("new pid_ns: created by pid=%d comm=%s\n", curtask->pid, curtask->comm);
}'
```

### 5.4 Watch setns() calls (when app containers join pause's namespaces)

```bash
# Trace setns() calls — these appear when app containers join pause's namespaces
bpftrace -e '
tracepoint:syscalls:sys_enter_setns {
    printf("setns: pid=%-6d comm=%-16s nstype=0x%x\n",
        pid, comm, args->nstype);
}'
```

Expected output when an app container is attached to the pod:
```
setns: pid=89301  comm=runc:init       nstype=0x20000000   ← CLONE_NEWPID
setns: pid=89301  comm=runc:init       nstype=0x40000000   ← CLONE_NEWNET
setns: pid=89301  comm=runc:init       nstype=0x8000000    ← CLONE_NEWIPC
```

### 5.5 Trace cgroup_attach_task (cgroup placement)

```bash
# Trace when a task is moved into a new cgroup (cgroup.procs write)
bpftrace -e '
kprobe:cgroup_attach_task {
    printf("cgroup_attach: pid=%d cgroup=%s\n",
        ((struct task_struct *)arg1)->pid,
        ((struct cgroup *)arg0)->kn->name);
}'
```

### 5.6 Watch execve for container entrypoints

```bash
# Trace every execve, filter for container-related binaries
bpftrace -e '
tracepoint:syscalls:sys_enter_execve
/ str(args->filename) == "/pause" || str(args->filename) == "/usr/sbin/nginx" /
{
    printf("execve: comm=%s file=%s\n", comm, str(args->filename));
}'
```

---

## 6. Key Kernel Source References

| Concept | File | Elixir Link |
|---------|------|-------------|
| `task_struct` definition | `include/linux/sched.h:737` | [link](https://elixir.bootlin.com/linux/v6.9/source/include/linux/sched.h#L737) |
| `task_struct.__state` | `include/linux/sched.h:747` | [link](https://elixir.bootlin.com/linux/v6.9/source/include/linux/sched.h#L747) |
| `task_struct.nsproxy` | `include/linux/sched.h:1020` | [link](https://elixir.bootlin.com/linux/v6.9/source/include/linux/sched.h#L1020) |
| `task_struct.cgroups` | `include/linux/sched.h:1309` | [link](https://elixir.bootlin.com/linux/v6.9/source/include/linux/sched.h#L1309) |
| `struct nsproxy` | `include/linux/nsproxy.h:31` | [link](https://elixir.bootlin.com/linux/v6.9/source/include/linux/nsproxy.h#L31) |
| `struct pid_namespace` | `include/linux/pid_namespace.h:10` | [link](https://elixir.bootlin.com/linux/v6.9/source/include/linux/pid_namespace.h#L10) |
| CLONE_* flags | `include/uapi/linux/sched.h` | [link](https://elixir.bootlin.com/linux/v6.9/source/include/uapi/linux/sched.h) |
| `sys_clone3()` | `kernel/fork.c:3178` | [link](https://elixir.bootlin.com/linux/v6.9/source/kernel/fork.c#L3178) |
| `kernel_clone()` | `kernel/fork.c:2979` | [link](https://elixir.bootlin.com/linux/v6.9/source/kernel/fork.c#L2979) |
| `copy_process()` | `kernel/fork.c:2105` | [link](https://elixir.bootlin.com/linux/v6.9/source/kernel/fork.c#L2105) |
| `copy_namespaces()` | `kernel/nsproxy.c:152` | [link](https://elixir.bootlin.com/linux/v6.9/source/kernel/nsproxy.c#L152) |
| `alloc_pid()` | `kernel/pid.c:180` | [link](https://elixir.bootlin.com/linux/v6.9/source/kernel/pid.c#L180) |
| `setns()` implementation | `kernel/nsproxy.c:558` | [link](https://elixir.bootlin.com/linux/v6.9/source/kernel/nsproxy.c#L558) |
| `cgroup_attach_task()` | `kernel/cgroup/cgroup.c` | [link](https://elixir.bootlin.com/linux/v6.9/source/kernel/cgroup/cgroup.c) |
| `do_execveat_common()` | `fs/exec.c` | [link](https://elixir.bootlin.com/linux/v6.9/source/fs/exec.c) |
| `create_pid_namespace()` | `kernel/pid_namespace.c:121` | [link](https://elixir.bootlin.com/linux/v6.9/source/kernel/pid_namespace.c#L121) |

---

## Summary

From a kernel perspective, a Kubernetes pod is:

1. **One `containerd-shim-runc-v2` process** per pod acting as the pod's host-side
   supervisor.
2. **One pause process** that owns the pod-level namespaces (net, pid, ipc, uts) and
   acts as PID 1 (the `child_reaper`) in the pod's PID namespace. Its entire existence
   is to hold references to the `nsproxy`-pointed namespace structs so they survive
   container restarts.
3. **One or more app container processes**, each with its own `task_struct`, its own
   mount namespace (for its filesystem), but sharing the pause container's network and
   PID namespace via `setns()`.
4. **A cgroup subtree** under `/sys/fs/cgroup/kubepods/<qos>/<pod-uid>/` where all
   pod processes are placed, enforcing the pod's CPU, memory, and PID resource limits.

The critical invariant: every container you see in `kubectl describe pod` is exactly
one call to `copy_process()` in `kernel/fork.c`, followed by one call to
`cgroup_attach_task()` in `kernel/cgroup/cgroup.c`, followed by one call to
`do_execveat_common()` in `fs/exec.c`. Kubernetes abstractions are built on top of
these three kernel operations.

**Next:** [Chapter 02 — Namespaces](../../02-namespaces/) — deep dive into each
Linux namespace type and how Kubernetes uses them.
