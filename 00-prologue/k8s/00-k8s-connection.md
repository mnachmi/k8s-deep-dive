# 00-k8s — How `kubectl run` Becomes a `clone()` Call

> This document bridges the previous two kernel documents with the Kubernetes control
> plane. It traces a single `kubectl run nginx` command from the moment the user presses
> Enter to the exact `clone3()` syscall that creates the container process.

---

## 1. The Full Syscall Chain: `kubectl run nginx`

The chain has seven distinct phases. Each one involves specific syscalls.

### Phase 1: kubectl → API Server (TCP connection)

`kubectl` is a Go program. When you run `kubectl run nginx --image=nginx`, it:

1. Reads `~/.kube/config` — syscalls: `openat(AT_FDCWD, "~/.kube/config", O_RDONLY)`, `read()`, `close()`
2. Resolves the API server hostname — syscalls: `socket(AF_INET, SOCK_STREAM, 0)`, `connect()` to the kube-apiserver address
3. Performs a TLS handshake — syscalls: `read()`, `write()` in a loop (TLS is implemented in Go's crypto/tls package in userspace)
4. Sends an HTTP POST to `/api/v1/namespaces/default/pods` with the Pod spec as JSON — syscalls: `write()`, `read()` (response)
5. Waits for the 201 Created response — the Go runtime uses `epoll_wait()` via its netpoller

The kubectl process itself never calls `clone()`. Its job ends here.

### Phase 2: API Server → etcd (persist the Pod spec)

The API server is also a Go process (`kube-apiserver`). On receiving the POST it:

1. Validates the Pod spec (pure CPU work, no interesting syscalls)
2. Writes the Pod object to etcd via gRPC — syscalls: `write()` on the gRPC socket
3. etcd uses a memory-mapped file (`mmap()`) and `fdatasync()` to persist the write to its WAL (Write-Ahead Log) on disk
4. etcd acknowledges — syscalls: `read()` on the gRPC socket

The critical syscall here is `fdatasync()` — it is what makes etcd writes durable.
Every Pod creation involves a `fdatasync()` to disk.

### Phase 3: Scheduler watches etcd and assigns a node

The scheduler (`kube-scheduler`) has a long-running `List+Watch` against the API
server for pods in `Pending` state. This is a long-lived HTTP/2 streaming response.

1. The scheduler's watch goroutine is blocked in `epoll_wait()` on the connection fd
2. When the API server pushes the new pod event, `epoll_wait()` returns
3. The scheduling algorithm runs (pure CPU: node scoring, affinity, taints)
4. The scheduler writes a `Binding` object back to the API server — same TLS/write path as Phase 1

### Phase 4: kubelet watches etcd and receives the assigned pod

kubelet runs on the target node. It also watches the API server for pods assigned to
its node. The watch fires, kubelet receives the pod spec, and begins reconciliation.

Key kubelet syscalls at this point:
- `openat()` — reads the pod spec and various config files under `/var/lib/kubelet/`
- `mkdir()` — creates the pod directory: `/var/lib/kubelet/pods/<pod-uid>/`
- `write()` — writes the pod status back to the API server

### Phase 5: kubelet → containerd (via CRI gRPC)

kubelet delegates container creation to the container runtime via the **Container
Runtime Interface (CRI)**, a gRPC API. kubelet calls:

1. `RunPodSandbox` — create the pod's network namespace and pause container
2. `PullImage` — if the image is not cached (may involve `connect()`, `read()`, `write()` to a registry over TLS)
3. `CreateContainer` — create the container spec
4. `StartContainer` — actually start the container

These are all gRPC calls over a Unix domain socket
(`/run/containerd/containerd.sock`). The syscalls are:
`socket(AF_UNIX, SOCK_STREAM, 0)`, `connect()`, `write()` (gRPC request), `read()` (gRPC response).

### Phase 6: containerd → containerd-shim → runc

containerd forks a **containerd-shim** process for each container. The shim is a
small process that persists even if containerd restarts. Forking the shim involves:

```
containerd calls: clone(SIGCHLD, ...)   ← creates containerd-shim process
```

The shim then calls `runc` (or another OCI runtime). runc is invoked as a subprocess:

```
containerd-shim calls: execve("/usr/bin/runc", ["runc", "create", ...], envp)
```

`execve` is syscall 59. It replaces the process image with the runc binary.

### Phase 7: runc sets up namespaces and calls `clone3()`

This is the kernel-intensive phase. runc reads the OCI bundle (the container spec in
`config.json` and the extracted image layers in `rootfs/`) and creates the container.

The sequence of syscalls runc makes (observed with `strace -f`):

```
1.  unshare(CLONE_NEWNS)                    # detach from current mount namespace
2.  mount("none", "/", NULL, MS_PRIVATE|MS_REC, NULL)  # make all mounts private
3.  mount(rootfs, rootfs, "bind", MS_BIND|MS_REC, NULL) # bind-mount rootfs
4.  [various bind mounts for /dev, /proc, /sys, /etc/resolv.conf ...]
5.  pivot_root(new_root, put_old)           # switch to container rootfs
6.  umount2(put_old, MNT_DETACH)            # detach old root
7.  clone3(&cl_args, sizeof(cl_args))       # CREATE THE CONTAINER PROCESS
        where cl_args.flags = CLONE_NEWPID | CLONE_NEWNET | CLONE_NEWIPC |
                               CLONE_NEWUTS | CLONE_NEWCGROUP
8.  [in the child: execve("/bin/nginx", ...)]
```

The `clone3()` call at step 7 is the exact moment the container process is born.
Its `struct clone_args` encodes which namespaces to create.

Kernel source for `clone3`:
[`kernel/fork.c`](https://elixir.bootlin.com/linux/v6.9/source/kernel/fork.c) —
`SYSCALL_DEFINE2(clone3, struct clone_args __user *, uargs, size_t, size)`.

After `clone3()` returns in the child, runc sets up the container's filesystem, drops
capabilities (via `prctl(PR_SET_SECCOMP, ...)` and `prctl(PR_CAP_AMBIENT, ...)`), and
calls `execve()` to replace itself with the container entrypoint (nginx).

```
timeline of clone3() syscall:
  rax = 435 (clone3 syscall number)
  syscall instruction
  → entry_SYSCALL_64
  → do_syscall_64
  → seccomp filter check (runc's own seccomp profile for the shim)
  → sys_call_table[435](regs) → sys_clone3()
      → copy_process()             # allocates new task_struct
      → copy_namespaces()          # creates new nsproxy with new ns
      → copy_mm()                  # new address space (if not CLONE_VM)
      → wake_up_new_task()         # scheduler: enqueue new task
  → return: child PID in parent, 0 in child
```

The key kernel function is `copy_process()` in
[`kernel/fork.c`](https://elixir.bootlin.com/linux/v6.9/source/kernel/fork.c).
It allocates a new `struct task_struct`, copies or shares resources from the parent
(depending on the `clone` flags), and returns the new task to the scheduler.

---

## 2. Containers Are Not VMs

This is the most important conceptual point in the course. The table below makes the
distinction precise using kernel data structures.

| Dimension | Virtual Machine | Linux Container |
|---|---|---|
| **Isolation boundary** | Hardware + hypervisor (VMX/SVM) | Kernel namespaces in `struct nsproxy` |
| **Kernel** | Each VM runs its own kernel (e.g. Linux 5.15 in a VM on a Linux 6.9 host) | All containers share the host kernel (`uname -r` returns the same string everywhere) |
| **Process visibility** | VM processes are invisible to the host (separate VMX guest state) | Container processes are visible on the host via `ps` and `/proc`; only hidden from *within* the container's PID namespace |
| **Filesystem** | Virtual disk (e.g. QCOW2 image) presented to a paravirtual SCSI driver | Overlay filesystem assembled from OCI layers, mounted in the container's mount namespace; same VFS code path |
| **Networking** | Emulated NIC (e.g. virtio-net) with a separate MAC/IP stack | A `veth` pair — one end in the container's net namespace, one end on the host's bridge; the same TCP/IP stack, just a different `struct net` instance |
| **Memory** | Guest physical memory mapped via EPT/NPT page tables | Same physical DRAM, managed by the same `struct mm_struct` / page allocator, with cgroup memory limits enforced by `mem_cgroup_charge()` |
| **CPU scheduling** | vCPUs scheduled by the hypervisor's scheduler; then the guest's scheduler inside the VM | Container processes are `struct task_struct` entries scheduled directly by the host kernel's CFS scheduler |
| **Syscall path** | Guest syscall → guest kernel → possibly hypercall to hypervisor | Container syscall → host kernel `entry_SYSCALL_64` → optionally intercepted by seccomp BPF |
| **Boot time** | Seconds to minutes (firmware, kernel boot, init system) | Milliseconds (`clone3()` + `execve()`) |
| **Attack surface** | Hypervisor escape (CVE-2019-5736, CVE-2015-3456 etc.) | Kernel exploit (any unpatched syscall in the host kernel affects all containers) |

### The key kernel data structures

A container process has exactly the same `struct task_struct` as any other process.
What makes it a "container" is the contents of two pointers within that struct:

```c
/* include/linux/sched.h (Linux 6.9) */
struct task_struct {
    ...
    struct nsproxy          *nsproxy;   /* pointer to namespace set */
    struct css_set __rcu    *cgroups;   /* pointer to cgroup membership */
    ...
};
```

- `nsproxy` points to a
  [`struct nsproxy`](https://elixir.bootlin.com/linux/v6.9/source/include/linux/nsproxy.h)
  that holds pointers to the six namespace types (mnt, pid, net, ipc, uts, user).
  When `clone3(CLONE_NEWPID)` is called, a new `struct pid_namespace` is allocated and
  placed in this `nsproxy`.

- `cgroups` points to a
  [`struct css_set`](https://elixir.bootlin.com/linux/v6.9/source/include/linux/cgroup.h)
  that holds references to the cgroup subsystem states. The memory controller,
  CPU scheduler limits, and I/O limits are all enforced through this structure.

A VM process has none of this — its isolation is in EPT page table entries and VMCS
structures managed by the hypervisor, not in `task_struct` fields.

---

## 3. Verify It Yourself

### Experiment 1: strace a pod creation

On a node where you have root, trace the clone/unshare/setns calls made by containerd
while creating a pod:

```bash
# Terminal 1: start tracing containerd (adjust path if needed)
sudo strace -f \
     -e trace=clone,clone3,unshare,setns,pivot_root,mount,execve \
     -p "$(pidof containerd)" \
     2>&1 | tee /tmp/containerd-strace.txt &

# Terminal 2: create a pod
kubectl run test-pod --image=busybox --restart=Never -- sleep 60

# Terminal 3: watch the output
tail -f /tmp/containerd-strace.txt
```

You will see output similar to:

```
[pid 12345] clone3({flags=CLONE_VM|CLONE_FS|CLONE_FILES|CLONE_SIGHAND|CLONE_THREAD|...}, 88) = 12346
[pid 12346] execve("/usr/bin/containerd-shim-runc-v2", ...) = 0
[pid 12346] clone3({flags=CLONE_NEWNS|CLONE_NEWPID|CLONE_NEWIPC|CLONE_NEWNET|CLONE_NEWUTS}, 88) = 12347
[pid 12347] unshare(CLONE_NEWNS) = 0
[pid 12347] pivot_root(".", ".pivot_root") = 0
[pid 12347] execve("/bin/sleep", ["sleep", "60"], 0x... /* 4 vars */) = 0
```

The `[pid 12347]` line with `execve("/bin/sleep", ...)` is the container process
starting. That PID (12347 on the host) will show up in `ps aux` on the node.

Check it:
```bash
# On the node: find the sleep process
ps aux | grep "sleep 60"

# Verify it is inside namespaces
PID=$(ps aux | grep "sleep 60" | grep -v grep | awk '{print $2}')
ls -la /proc/$PID/ns/
# You will see different inode numbers for pid, net, mnt — those are the container's ns
```

### Experiment 2: bpftrace — watch clone3 calls in real time

This requires `bpftrace` installed and kernel ≥ 4.9 with BTF support:

```bash
sudo bpftrace -e '
tracepoint:syscalls:sys_enter_clone3 {
    printf("%-16s pid=%-8d flags=0x%x\n",
           comm, pid, args->uargs->flags);
}'
```

While this runs, create a pod in another terminal. You will see output like:

```
containerd       pid=98234    flags=0x11e0f00
containerd-shim  pid=98235    flags=0x30000000  <- CLONE_NEWPID|CLONE_NEWNET|...
```

### Experiment 3: Confirm the container PID is visible on the host

```bash
# Create a pod with a known command
kubectl run pid-test --image=alpine --restart=Never -- sleep 999

# Wait for it to run
kubectl wait pod/pid-test --for=condition=Ready --timeout=30s

# Find it on the host (from the node)
ps aux | grep "sleep 999"
# e.g.: root  9876  0.0  0.0  1620   4  pts/0  Ss   00:00   0:00 sleep 999

# Check its namespaces
ls -la /proc/9876/ns/
# pid -> pid:[4026532xxx]   <- different from host's pid:[4026531836]
# net -> net:[4026532yyy]   <- different from host
# mnt -> mnt:[4026532zzz]   <- different from host
# uts -> uts:[4026532www]   <- different from host
# ipc -> ipc:[4026532vvv]   <- different from host
# user -> user:[4026531837] <- SAME as host (if user namespaces not used)
```

The process `sleep 999` is a normal Linux process. It has the same PID namespace
inode (`4026532xxx`) as the other processes in its pod. You can kill it from the
host with `kill 9876`. You can `strace` it. You can read its memory maps from
`/proc/9876/maps`. There is no hypervisor between you and the process.

### Experiment 4: Verify the seccomp profile is active

```bash
# On a node, find the container's PID
PID=$(ps aux | grep "sleep 999" | grep -v grep | awk '{print $2}')

# Read the seccomp mode from /proc
cat /proc/$PID/status | grep Seccomp
# Seccomp:    2
# Mode 2 = SECCOMP_MODE_FILTER (a BPF program is installed)
# Mode 0 = disabled, Mode 1 = strict

# Try to call a blocked syscall from within the container
kubectl exec pid-test -- sh -c 'mount -t tmpfs tmpfs /tmp 2>&1'
# mount: permission denied (or similar) — blocked by seccomp
```

---

## Summary

`kubectl run nginx` is ultimately a sequence of:
1. TCP `connect()` from kubectl to the API server
2. HTTP/2 `write()`/`read()` for the Pod spec and etcd persistence
3. gRPC `write()`/`read()` from kubelet to containerd
4. `clone()` to fork containerd-shim
5. `execve()` to run runc
6. `unshare()`, `mount()`, `pivot_root()` to set up the container filesystem
7. `clone3(CLONE_NEWPID|CLONE_NEWNET|...)` to create the container process
8. `execve()` to run nginx inside the new namespaces

The container process that results is an ordinary `struct task_struct` with a
different `nsproxy` (namespace set) and `css_set` (cgroup membership) compared to
the processes that created it. There is no virtualisation layer, no separate kernel,
and no emulated hardware. The same kernel code that runs your shell runs the nginx
container — only the namespace context differs.

This is why kernel bugs affect all containers on a node simultaneously, why a
misbehaving container can affect node-level kernel resources (like the conntrack
table), and why understanding the kernel is not optional for Kubernetes operators.
