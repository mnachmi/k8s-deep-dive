# 00-k8s — How `kubectl run` Becomes a `clone()` Call

## The Distance Between kubectl and the Kernel

Most Kubernetes operators have a working model of what `kubectl run nginx` does: it talks to the API server, the scheduler picks a node, kubelet creates the container. This is correct as far as it goes. But it leaves out the most important part: the container process that eventually runs nginx is created by exactly one Linux syscall — `clone3()` — and that syscall happens in response to an ordered chain of seven syscall types crossing three process boundaries.

Understanding this chain is not academic. When a container process is killed and you don't know why, you trace backward through this chain. When a pod takes 30 seconds to start instead of 3, you look for where the chain slows down. When a security audit flags unexpected syscalls from a container process, you understand exactly which layer introduced them. Operators who understand this chain debug from first principles. Operators who don't debug from intuition.

This document traces the chain from keypress to container process, naming every syscall at every boundary.

---

## The Full Syscall Chain: `kubectl run nginx`

The chain has seven distinct phases. Each one involves specific syscalls. Each one can fail in a specific way.

### Phase 1: kubectl → API Server

`kubectl` is a Go program. When you run `kubectl run nginx --image=nginx`:

1. **Reads `~/.kube/config`** — syscalls: `openat(AT_FDCWD, "~/.kube/config", O_RDONLY)`, `read()`, `close()`. The kubeconfig file contains the API server address and TLS credentials. Without this file, the chain ends here.

2. **Opens a TCP connection to the API server** — syscalls: `socket(AF_INET, SOCK_STREAM, 0)`, `connect()`. The Go runtime performs the TLS handshake in userspace (Go's crypto/tls package handles it), but the underlying `read()` and `write()` syscalls carry the TLS record bytes. The TCP connection is how all kubectl-to-cluster communication flows.

3. **Sends the Pod spec to the API server** — syscall: `write()` carrying an HTTP POST to `/api/v1/namespaces/default/pods` with the Pod spec as JSON.

4. **Waits for a 201 Created response** — the Go runtime registers the connection fd with `epoll_ctl()` and blocks in `epoll_wait()`. When the API server acknowledges, `epoll_wait()` returns and kubectl prints its output.

The kubectl process never calls `clone()`. It never touches container namespaces. Its entire role is serializing a Go struct into JSON and sending it over a TCP connection. The kubectl process exits after step 4.

### Phase 2: API Server → etcd

The API server (`kube-apiserver`) is also a Go process, running on the control plane. On receiving the POST:

1. **Validates the Pod spec** — pure CPU work: admission controllers, defaulting, validation. No interesting syscalls.

2. **Persists the Pod spec to etcd** — the API server sends the object to etcd via gRPC over a TCP connection, syscalls: `write()` on the gRPC socket. etcd writes to its Write-Ahead Log using `mmap()` for the file mapping and `fdatasync()` for persistence. Every Pod creation involves an `fdatasync()` to disk — this is the fundamental latency floor for etcd-backed operations.

3. **Acknowledges to the API server** — etcd sends back a gRPC response. The API server's `epoll_wait()` wakes, and the API server's watch mechanism notifies subscribers of the new Pod object.

The critical syscall here is `fdatasync()`. It is what makes Pod creation crash-safe. The API server does not return a 201 until the Pod spec is durably written. This is why etcd I/O latency directly affects how long `kubectl apply` blocks.

### Phase 3: Scheduler

The Kubernetes scheduler (`kube-scheduler`) holds a long-lived HTTP/2 watch connection to the API server, filtering for Pods in `Pending` state with no `nodeName` assigned. This watch connection is an HTTP/2 streaming response — effectively a long-lived `read()` that the API server sends events into.

1. **The watch goroutine blocks in `epoll_wait()`** on the watch connection's file descriptor.
2. **The API server pushes the new Pod event** over the watch stream. `epoll_wait()` returns.
3. **The scheduling algorithm runs** — pure CPU: node scoring, affinity evaluation, taint/toleration matching. No syscalls for this work.
4. **The scheduler writes a Binding object** back to the API server, assigning a `nodeName` to the Pod. Syscalls: `write()` on the TLS connection.

The scheduler does not communicate with the target node. It only writes to the API server. The node doesn't know about the Pod assignment yet — that comes from the next phase.

### Phase 4: kubelet on the Target Node

kubelet runs on every worker node. It holds its own long-lived HTTP/2 watch on the API server, filtered for Pods whose `nodeName` matches the current node. When the scheduler's Binding is processed, the API server updates the Pod object's `nodeName` and notifies kubelet's watch.

1. **kubelet's watch goroutine wakes** from `epoll_wait()`.
2. **kubelet reads the pod spec and evaluates its local state** — syscalls: `openat()` reads various files under `/var/lib/kubelet/`, `readdir()` walks the existing pods directory.
3. **kubelet creates the pod working directory** — syscall: `mkdir()` creates `/var/lib/kubelet/pods/<pod-uid>/`.
4. **kubelet writes its initial status back** to the API server — the Pod transitions from `Pending` to `ContainerCreating`.

### Phase 5: kubelet → containerd (CRI gRPC)

kubelet does not create containers directly. It delegates to a Container Runtime Interface (CRI) daemon — typically containerd. The CRI is a gRPC API over a Unix domain socket (`/run/containerd/containerd.sock`). kubelet calls these RPCs in order:

```
kubelet                                    containerd
  │── RunPodSandbox(PodSandboxConfig) ──►  │  (creates pause container, allocates network namespace)
  │◄── RunPodSandboxResponse(SandboxID) ───│
  │── PullImage(ImageSpec) ──►             │  (if image not cached — involves TLS connection to registry)
  │◄── PullImageResponse() ───────────────│
  │── CreateContainer(SandboxID, Spec) ──► │
  │◄── CreateContainerResponse(ID) ───────│
  │── StartContainer(ContainerID) ──►      │
  │◄── StartContainerResponse() ──────────│
```

Every one of these messages travels as a `write()` on the Unix domain socket, and the response returns as a `read()`. `PullImage` involves a `connect()` to the image registry, `read()`/`write()` loops for the TLS handshake and HTTP layer download, and disk writes for the layer cache.

### Phase 6: containerd → containerd-shim → runc

containerd does not start containers directly either. It forks a **containerd-shim** process for each pod. The shim is a small long-running process that owns the pod's I/O pipes and exit status. If containerd is restarted (for a daemon upgrade, for example), the shim keeps running — the containers survive the restart because the shim is their keeper.

containerd forks the shim with a plain `clone(SIGCHLD)` or `fork()`, then uses `execve()` to run the `containerd-shim-runc-v2` binary in the child. The shim then calls `runc` as another child process:

```
containerd calls: clone(SIGCHLD)             ← forks containerd-shim
containerd-shim calls: execve("/usr/bin/runc", ["runc", "create", ...], envp)
```

`execve()` at this point replaces the containerd-shim's memory image with runc. runc is a Go binary that reads the OCI bundle (the container configuration in `config.json` and the container filesystem in `rootfs/`) and does the kernel-level setup.

### Phase 7: runc — the Kernel-Intensive Phase

This is where the containers are actually created. runc reads the OCI spec and issues a precise sequence of syscalls. Observed with `strace -f` on a real pod creation:

```
1.  unshare(CLONE_NEWNS)
        # Detach from the current mount namespace. This is necessary before
        # any bind mounts so that subsequent mount() calls do not affect the
        # host's namespace.

2.  mount("none", "/", NULL, MS_PRIVATE|MS_REC, NULL)
        # Make all mounts in the new namespace private (no propagation to host).

3.  mount(rootfs, rootfs, "bind", MS_BIND|MS_REC, NULL)
        # Bind-mount the container's rootfs (the OCI image overlay) over itself.
        # This is the trick that makes pivot_root work.

4.  [many bind mounts]
        # /dev, /proc, /sys, /etc/hostname, /etc/resolv.conf, secrets, configmaps —
        # all bind-mounted into the container's filesystem tree.

5.  pivot_root(new_root, put_old)
        # Replace the current mount namespace's root with the container's rootfs.
        # After this, "/" inside the container is the OCI image's filesystem.
        # The host's old root is available at put_old (briefly, then unmounted).

6.  umount2(put_old, MNT_DETACH)
        # Remove the host root from the container's view.

7.  clone3(&cl_args, sizeof(cl_args))
        # ━━━━━━━━━━━━━━━━━━━━━━━━━━
        # THIS IS THE MOMENT THE CONTAINER IS BORN.
        # ━━━━━━━━━━━━━━━━━━━━━━━━━━
        where cl_args.flags =
            CLONE_NEWPID    # new PID namespace: this process becomes PID 1
          | CLONE_NEWNET    # new network namespace: empty (CNI plugin wires it)
          | CLONE_NEWIPC    # new IPC namespace: isolated SysV IPC tables
          | CLONE_NEWUTS    # new UTS namespace: can have its own hostname
          | CLONE_NEWCGROUP # new cgroup namespace (virtualized cgroup view)
          | CLONE_NEWUSER   # (if user namespaces enabled)

8.  [in the child: execve("/bin/nginx", argv, envp)]
        # Replace the child's memory image with the actual container binary.
```

Steps 1–6 set up the container's filesystem in the current process's namespace. Step 7 is the actual container process creation — a new `task_struct` is allocated, a new `nsproxy` is populated with fresh namespace pointers, and the new process becomes PID 1 in its own PID namespace. Step 8 loads the actual container binary.

The `clone3()` call passes through the kernel entry path covered in `00-b-entry-path.md`:

```
rax = 435 (clone3 syscall number)
syscall instruction
  → entry_SYSCALL_64                          (arch/x86/entry/entry_64.S)
  → swapgs, switch CR3, build pt_regs
  → do_syscall_64()                           (arch/x86/entry/common.c)
  → seccomp filter check for runc's profile
  → sys_call_table[435](regs) → sys_clone3()  (kernel/fork.c:3178)
      → kernel_clone()                        (kernel/fork.c:2979)
          → copy_process()                    (kernel/fork.c:2105)
              ├── dup_task_struct()           — allocate new task_struct from slab
              ├── copy_namespaces()           — create new nsproxy + namespace structs
              ├── copy_mm()                   — COW the address space
              ├── alloc_pid()                 — allocate PID 1 in the new PID ns
              └── wake_up_new_task()          — enqueue in the CFS scheduler
```

After `clone3()` returns:
- In the **parent** (runc): returns the child's host PID. runc writes this PID to the pod cgroup (`/sys/fs/cgroup/kubepods/.../<container-id>/cgroup.procs`), establishing resource limits.
- In the **child** (the container process): returns 0. The child calls `execve()` to load the container binary.

At this point, the container process exists as an ordinary `struct task_struct` with a `nsproxy` pointing to fresh namespace structs and a `css_set` pointing into the pod's cgroup tree.

---

## Why Containers Are Not VMs

This is the most important conceptual distinction in the course. Not because it is surprising, but because the implications are non-obvious and most operators do not carry them through to their debugging practice.

A VM is a separate hardware environment emulated by software. When a VM runs a process, that process is scheduled by the VM's kernel, which is scheduled as a vCPU by the hypervisor. The host kernel has no direct knowledge of the VM's processes. A buggy process in the VM cannot directly exploit a syscall vulnerability in the host kernel — the hypervisor is a hardware-enforced boundary.

A container is a process on the host kernel. When a container process makes a syscall, it goes through `entry_SYSCALL_64` on the host kernel — the same `entry_SYSCALL_64` that handles the kubelet's syscalls, and the systemd's syscalls, and every other process on the node. The only isolation is:
- **Namespace isolation**: the container process's `nsproxy` points to a private set of namespace structs, so it cannot see the host's processes (PID namespace), network interfaces (net namespace), or mount tree (mount namespace).
- **cgroup resource limits**: the container process's `css_set` places it in a cgroup subtree with CPU/memory/pid limits.
- **seccomp policy**: a BPF filter intercepts syscalls in `syscall_enter_from_user_mode_work()` and blocks disallowed ones before they run.

| Dimension | Virtual Machine | Linux Container |
|-----------|-----------------|-----------------|
| Isolation boundary | Hardware MMU + hypervisor (VMX/SVM) | Kernel namespace pointers in `struct nsproxy` |
| Kernel | Each VM has its own kernel | All containers share the host kernel — `uname -r` returns the same output in every container |
| Process visibility | VM processes invisible to host | Container processes fully visible in host's `ps`; only invisible from *within* the container's PID namespace |
| Filesystem | Virtual block device (QCOW2, VMDK) | Overlay filesystem on top of host VFS — same kernel code, different mount namespace |
| Networking | Emulated NIC (virtio-net) with separate IP stack | veth pair — one end in container's net namespace, same TCP/IP code, different `struct net` |
| Memory | Guest physical memory via EPT/NPT page tables | Same DRAM, same page allocator, enforced only by cgroup memory limits |
| CPU scheduling | vCPU scheduled by hypervisor, then guest scheduler | Container `task_struct` scheduled directly by host CFS |
| Syscall path | Guest syscall → guest kernel → maybe hypercall | Container syscall → host `entry_SYSCALL_64` → seccomp filter → same kernel |
| Boot time | Seconds to minutes | Milliseconds (`clone3` + `execve`) |
| Shared kernel bugs | Not directly exploitable from guest | A kernel CVE affects every container on the node simultaneously |

The consequence that most surprises operators: **a kernel exploit in any container is a node-level exploit**. There is no hypervisor boundary. The process running in the container has the same access to unpatched kernel syscalls as any other process on the node. This is why the node's kernel version matters, why seccomp profiles exist, and why AppArmor/SELinux matter even when they feel like redundant controls.

The two struct fields that make a container:

```c
/* include/linux/sched.h (Linux 6.9) */
struct task_struct {
    ...
    struct nsproxy          *nsproxy;    /* what the container can see */
    struct css_set __rcu    *cgroups;    /* what resources it can use */
    ...
};
```

A VM's isolation is in hardware VMCS structures managed by the hypervisor. A container's isolation is in these two pointers in a struct allocated from the kernel slab allocator. It is not inferior — it is different, and it has different consequences.

---

## Verify It Yourself

### Experiment 1: strace a pod creation — watch the clone3() call

On a node with root access, trace the container-relevant syscalls made by containerd while creating a pod:

```bash
# Terminal 1: trace clone3, unshare, setns, pivot_root, mount from containerd
sudo strace -f \
     -e trace=clone,clone3,unshare,setns,pivot_root,mount,execve \
     -p "$(pidof containerd)" \
     2>&1 | tee /tmp/containerd-strace.txt &

# Terminal 2: create a pod
kubectl run test-pod --image=busybox --restart=Never -- sleep 60

# Watch for the key events:
grep -E "clone3|pivot_root|execve.*sleep" /tmp/containerd-strace.txt
```

You will see output like:

```
[pid 12345] clone3({flags=CLONE_VM|CLONE_FS|CLONE_FILES|CLONE_THREAD|...}, 88) = 12346
[pid 12346] execve("/usr/bin/containerd-shim-runc-v2", ...) = 0
[pid 12346] clone3({flags=CLONE_NEWPID|CLONE_NEWNET|CLONE_NEWNS|CLONE_NEWUTS|CLONE_NEWIPC}, 88) = 12347
[pid 12347] pivot_root(".", ".pivot_root") = 0
[pid 12347] execve("/bin/sleep", ["sleep", "60"], ...) = 0
```

The `[pid 12347]` process that calls `execve("/bin/sleep", ...)` is the container. PID 12347 is its **host PID** — it is visible in `ps aux` on the node.

### Experiment 2: bpftrace — observe the moment of container birth

```bash
sudo bpftrace -e '
tracepoint:syscalls:sys_enter_clone3 {
    $flags = *(uint64 *)args->uargs;
    printf("comm=%-16s pid=%-8d flags=0x%08llx  NEWPID=%d NEWNET=%d NEWNS=%d\n",
        comm, pid, $flags,
        ($flags & 0x20000000) != 0,
        ($flags & 0x40000000) != 0,
        ($flags & 0x00020000) != 0);
}'
```

Create a pod in another terminal. The line with `NEWPID=1 NEWNET=1 NEWNS=1` is the container process being born.

### Experiment 3: confirm the container process is an ordinary host process

```bash
# Find the sleep process
kubectl run pid-test --image=alpine --restart=Never -- sleep 999
kubectl wait pod/pid-test --for=condition=Ready --timeout=30s

PID=$(pgrep -f "sleep 999")
echo "Host PID: $PID"

# Verify it's in isolated namespaces (different inode numbers)
ls -la /proc/$PID/ns/
# pid -> pid:[4026532XXX]   ← container's PID namespace
# net -> net:[4026532YYY]   ← container's net namespace
# Compare to host: ls -la /proc/1/ns/

# You can kill it from the host — no hypervisor barrier
kill -0 $PID && echo "Process exists, killable from host"

# You can strace it from the host
sudo strace -p $PID -e trace=read,write 2>&1 | head -10
```

A VM process would not appear in the host's `ps` and could not be killed or strace'd from the host. A container process can. This is what "containers are processes" means at the kernel level.

### Experiment 4: verify seccomp is active

```bash
PID=$(pgrep -f "sleep 999")
cat /proc/$PID/status | grep Seccomp
# Seccomp: 2   → SECCOMP_MODE_FILTER (a BPF policy is installed)
# Seccomp: 0   → no policy (default deny not enforced)

# Attempt a blocked syscall from inside the container:
kubectl exec pid-test -- sh -c 'mount -t tmpfs tmpfs /tmp 2>&1'
# Expected: "mount: permission denied" or "Operation not permitted"
# Blocked by the container runtime's seccomp profile before sys_mount() runs

# Cleanup
kubectl delete pod pid-test test-pod
```

---

## The Complete Chain

```
kubectl run nginx
  │
  ├─ connect() → API server
  ├─ write() → POST /api/v1/.../pods (Pod spec as JSON)
  └─ epoll_wait() → 201 Created
                 │
                 ├─ etcd: write() → gRPC → fdatasync() (durable write)
                 └─ kube-scheduler: epoll_wait() wakes → scores nodes → write() Binding
                                 │
                                 └─ kubelet: epoll_wait() wakes → openat/mkdir → CRI gRPC
                                         │
                                         ├─ socket(AF_UNIX) → connect() → containerd.sock
                                         ├─ write() RunPodSandbox
                                         └─ write() CreateContainer + StartContainer
                                                 │
                                                 └─ containerd: clone(SIGCHLD) → shim
                                                             └─ shim: execve(runc)
                                                                 │
                                                                 ├─ unshare(CLONE_NEWNS)
                                                                 ├─ mount() × N
                                                                 ├─ pivot_root()
                                                                 └─ clone3(CLONE_NEWPID|NEWNET|...) ← CONTAINER IS BORN
                                                                         │
                                                                         ├─ copy_process() allocates task_struct
                                                                         ├─ copy_namespaces() creates nsproxy
                                                                         └─ execve(/bin/nginx)
```

Every Pod you have ever created in Kubernetes is the result of this chain executing successfully. When a Pod fails to start, it is always because one of these steps failed: the etcd write timed out, the image pull failed, the `pivot_root()` returned an error, or `clone3()` itself was blocked. Understanding the chain means you can name which step failed and why.

**Next:** [Chapter 01 — The Process Model](../../01-process-model/) — deep dive into `struct task_struct`, the kernel data structure that every container process is represented by.
