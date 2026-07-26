## Kubernetes Namespace Isolation: From kubelet to runc

## The Distance Between the YAML and the Kernel

When you deploy a pod with `hostNetwork: false` and no `hostPID`, you are making eight separate kernel decisions. You are specifying that the pod's processes will run in a new PID namespace, a new network namespace, a new UTS namespace, a new IPC namespace, a new mount namespace, a new cgroup namespace, and — unless explicitly shared — new user namespace capabilities. These decisions travel from your `kubectl apply` to the API server, from the API server to kubelet, from kubelet through containerd, from containerd to runc, and finally from runc to the `clone3(2)` and `setns(2)` syscalls that actually create the isolation. That is a long chain with many places for something to go silently wrong.

The abstraction is helpful but costly. When a pod's DNS resolution fails, the debugging path that starts with "check the CoreDNS logs" can take an hour. The path that starts with "check whether the pod is actually in the network namespace you think it is" takes thirty seconds. When `kubectl exec` into a pod cannot see the expected processes, the question is not "what does containerd think?" but "what does `/proc/<pid>/ns/pid` say?" The kernel's view of namespace membership is the ground truth; everything else is a representation of it.

Kubernetes delegates all container isolation to the container runtime. The kubelet sends gRPC calls (defined by the Container Runtime Interface) to containerd; containerd calls runc; runc makes the actual `clone3()` and `setns()` system calls into the Linux kernel. Understanding this chain lets you diagnose namespace-related issues at the kernel level — reading `/proc/<pid>/ns/` entries, running `lsns`, or attaching a bpftrace probe — rather than depending on opaque container runtime logs that rarely tell you which namespace operation failed and why.

## Section 1 — Which Namespaces Each Container Gets

A default Kubernetes pod consists of a pause container (the namespace anchor) and one or more app containers. The table below shows what each namespace type does for an app container.

| Namespace | App container behaviour | How | Notes |
|-----------|------------------------|-----|-------|
| Network | Joins pause container's | `setns(pause_netns_fd, CLONE_NEWNET)` | All containers in pod share the same IP |
| IPC | Joins pause container's | `setns(pause_ipcns_fd, CLONE_NEWIPC)` | Enables shared memory between containers |
| PID | New namespace per container | `clone3(CLONE_NEWPID)` | Default; overridable with `shareProcessNamespace: true` |
| Mount | New namespace per container | `clone3(CLONE_NEWNS)` | Each container gets its own filesystem view |
| UTS | New namespace per container | `clone3(CLONE_NEWUTS)` | Hostname is set to the container name by runc |
| User | NOT used by default | — | Only in rootless container runtimes |
| Cgroup | New namespace per container | `clone3(CLONE_NEWCGROUP)` | Container sees its own cgroup root as `/` |
| Time | NOT used | — | Not supported by runc by default |

## Section 2 — The Pause Container: Namespace Anchor

Every Kubernetes pod has a pause container (sometimes called the "infra" container) that containerd creates before any app containers start. Its only job is to hold the pod's network and IPC namespaces open: it calls `pause(2)` and sleeps until the pod is deleted.

The image is `registry.k8s.io/pause:3.9`. The entrypoint is a small C program that installs signal handlers and then calls `pause()` in a loop. It does nothing else.

The design solves a real problem: if the first app container in a pod were the owner of the network namespace and it exited, the kernel would destroy the namespace immediately, tearing down the network of every other container in the pod. By keeping a dedicated, minimal process alive solely to own those namespaces, Kubernetes ensures that any individual app container can restart or exit without affecting the others. In a pod that has `shareProcessNamespace: false` (the default), the pause container is also PID 1 of the pod's sandbox PID namespace, which makes it the child reaper — it collects zombie processes from any app containers that fork and exit.

The lifecycle of namespace creation looks like this:

```
kubelet → containerd: RunPodSandbox gRPC
  containerd creates pause container:
    clone3(CLONE_NEWNS | CLONE_NEWUTS | CLONE_NEWIPC |
           CLONE_NEWPID | CLONE_NEWNET | CLONE_NEWCGROUP)
    pause container calls pause(2) → sleeps forever

kubelet → containerd: CreateContainer (for each app container)
  containerd → runc:
    1. open("/proc/<pause_pid>/ns/net", O_RDONLY)
    2. open("/proc/<pause_pid>/ns/ipc", O_RDONLY)
    3. setns(net_fd, CLONE_NEWNET)
    4. setns(ipc_fd, CLONE_NEWIPC)
    5. clone3(CLONE_NEWNS | CLONE_NEWUTS | CLONE_NEWPID | CLONE_NEWCGROUP)
    6. exec app container binary
```

## Section 3 — Verifying Namespace Sharing Per Pod

The following commands work on any Kubernetes node that has `crictl` and `jq` installed (both are standard on nodes that use containerd).

```bash
# Get the pause container PID for a running pod
# (requires crictl, which is available on every k8s node)
POD_ID=$(crictl pods --name nginx-pod -q | head -1)
PAUSE_PID=$(crictl inspectp $POD_ID | jq -r '.info.pid')

# Get an app container PID from the same pod
CONTAINER_ID=$(crictl ps --pod $POD_ID -q | head -1)
APP_PID=$(crictl inspect $CONTAINER_ID | jq -r '.info.pid')

echo "Pause PID: $PAUSE_PID"
echo "App PID: $APP_PID"

# Verify network namespace IS shared (same inode)
echo "=== Network namespace ==="
stat -L /proc/$PAUSE_PID/ns/net /proc/$APP_PID/ns/net
# Both should show same ino

# Verify IPC namespace IS shared
echo "=== IPC namespace ==="
stat -L /proc/$PAUSE_PID/ns/ipc /proc/$APP_PID/ns/ipc

# Verify mount namespace is NOT shared (different inodes)
echo "=== Mount namespace ==="
stat -L /proc/$PAUSE_PID/ns/mnt /proc/$APP_PID/ns/mnt
# Should show different ino

# Verify PID namespace is NOT shared
echo "=== PID namespace ==="
stat -L /proc/$PAUSE_PID/ns/pid /proc/$APP_PID/ns/pid
```

The `stat -L` flag follows the symlink so you read the inode of the namespace file itself. Equal inodes mean the two processes are inside the same namespace instance.

You can also use `lsns` to get a summary view:

```bash
# List all namespaces, filter by PID
lsns -p $PAUSE_PID
lsns -p $APP_PID
# net and ipc show same NS-ID; mnt and pid differ
```

`lsns` reads from `/proc` and `/sys/fs/cgroup`. The `NS` column is the inode number of the namespace pseudo-file. When two processes share a namespace, their `NS` values for that type are identical.

## Section 4 — Pod Spec Options That Change Namespace Setup

Kubernetes exposes four pod-level fields that alter which namespaces runc creates versus joins.

**`hostNetwork: true`**

```yaml
spec:
  hostNetwork: true
```

runc does not create a new network namespace. Instead it opens `/proc/1/ns/net` (the host's init network namespace) and calls `setns()` to join it — or simply omits `CLONE_NEWNET` from the `clone3()` flags so the child inherits the parent's netns. The effect is that the pod sees all host network interfaces and can bind to host ports directly. This is required for DaemonSets that need to listen on host ports, such as node-exporter or Calico's felix component.

**`hostPID: true`**

```yaml
spec:
  hostPID: true
```

`CLONE_NEWPID` is not passed to `clone3()`. The container runs in the host PID namespace: `ps aux` inside the container shows all host processes. A process inside can send signals to any host process by PID and can read `/proc/<host-pid>/` for any process on the node. This is a significant security boundary removal and should only be used when the workload explicitly requires it (e.g., a debugging DaemonSet like `kubectl-node-shell`).

**`hostIPC: true`**

```yaml
spec:
  hostIPC: true
```

`CLONE_NEWIPC` is not passed. The container shares the host IPC namespace and can enumerate, attach to, or destroy all System V shared memory segments and semaphores visible on the host. Applications that use `ipcs(1)` on the node will see them from inside such a container.

**`shareProcessNamespace: true`** (pod-level, not `hostPID`)

```yaml
spec:
  shareProcessNamespace: true
```

All containers in the pod share a single PID namespace created by the pause container's `clone3(CLONE_NEWPID)` call. App containers join via `setns()` instead of creating their own. This allows one container's process to send signals to another container's process within the same pod — useful for sidecar patterns where an init or debug container needs to interact with the app process.

To verify: run `cat /proc/self/status | grep NSpid` from different containers in the same pod. If they share a PID namespace, the second field of `NSpid` (the namespace-local PID) will differ between containers but the namespace inode (from `stat -L /proc/self/ns/pid`) will be identical.

## Section 5 — Diagnosing Namespace Issues

**Scenario 1: "Network not ready" — container starts before netns is configured**

The CNI plugin runs after the pause container is created but before app containers are expected to be reachable. If the CNI plugin fails or races, the app container's network namespace exists but has no interfaces beyond loopback.

```bash
# Check if container joined the right netns
crictl inspect $CONTAINER_ID | jq '.info.pid'
# Then verify:
nsenter --net=/proc/$APP_PID/ns/net -- ip addr
# If lo is the only interface, the CNI plugin hasn't run yet
```

If only `lo` appears, check CNI plugin logs (`journalctl -u kubelet` and the plugin's own log path under `/var/log/pods/`) and verify the CNI configuration files in `/etc/cni/net.d/`.

**Scenario 2: Privileged container escaping namespace**

A privileged container (`securityContext.privileged: true`) runs with `CAP_SYS_ADMIN` and can call `setns()` to join any namespace it can open a file descriptor for. Detect whether a container's network namespace matches the host:

```bash
# A privileged container can call setns() to join any namespace
# Detect: check if process namespace matches host
HOST_NETNS=$(stat -L /proc/1/ns/net | grep Inode | awk '{print $2}')
CONT_NETNS=$(stat -L /proc/$APP_PID/ns/net | grep Inode | awk '{print $2}')
[ "$HOST_NETNS" = "$CONT_NETNS" ] && echo "WARNING: container in host network namespace"
```

**Scenario 3: `lsns` shows unexpected namespace sharing**

```bash
# Find all processes in the same network namespace as a container
lsns -t net -o NS,TYPE,NPROCS,PID,COMMAND
# Each unique NS value is a distinct network namespace
# Number in NPROCS column tells you how many processes share it
```

A high `NPROCS` count for a single network namespace inode typically means either a multi-container pod (expected) or a privileged container that has joined the host netns (a security concern). Cross-reference the `PID` column with `ps -p <PID> -o pid,ppid,comm,args` to identify what is sharing the namespace.

## Section 6 — bpftrace: Tracing the Full Container Startup Sequence

The following bpftrace script captures the namespace setup sequence in real time. It hooks `kernel_clone` (the kernel function behind `clone3(2)` on Linux 6.9) to log when a new namespace is created, and `__sys_setns` to log when a process joins an existing namespace.

```
#!/usr/bin/env bpftrace

// Trace clone3() calls with namespace flags
kprobe:kernel_clone {
    $args = (struct kernel_clone_args *)arg0;
    $flags = $args->flags;
    if ($flags & 0x40000000) {  // CLONE_NEWNET
        printf("[%s pid=%d] clone3 CLONE_NEWNET+others flags=0x%lx\n",
            comm, pid, $flags);
    }
}

// Trace setns() calls
kprobe:__sys_setns {
    printf("[%s pid=%d] setns nstype=0x%x\n",
        comm, pid, (uint32)arg1);
}
```

Run this script as root on the node, then in a separate terminal trigger pod creation:

```bash
kubectl run test --image=nginx --restart=Never
```

You will see a sequence of `clone3 CLONE_NEWNET` from the `runc` process (pause container creation) followed by multiple `setns` calls where `nstype=0x40000000` (CLONE_NEWNET) and `nstype=0x8000000` (CLONE_NEWIPC) appear — these are the app container joining the pause container's namespaces. The `clone3` calls without `CLONE_NEWNET` that follow correspond to the app container creating its own mount, PID, UTS, and cgroup namespaces.

The `0x40000000` value for CLONE_NEWNET and `0x8000000` for CLONE_NEWIPC match the constants defined in `include/uapi/linux/sched.h` in the kernel source tree.

## Section 7 — kube-inspect Integration

After completing kube-inspect checkpoint 02, the `--namespaces` flag enumerates `/proc/<pid>/ns/` for every process belonging to a pod and formats the namespace inodes in a human-readable table:

```bash
# On the k8s node, after building kube-inspect:
kube-inspect --pod <uid> --namespaces
```

Expected output shows that the pause container and all app containers share identical `net` and `ipc` inode values, while `mnt`, `pid`, `uts`, and `cgroup` inodes differ between the pause container and each app container. If `shareProcessNamespace: true` is set on the pod, the `pid` inode also matches across all containers.

This output is derived from the same kernel data that `lsns` reads — the symlink targets under `/proc/<pid>/ns/` — making kube-inspect a thin convenience wrapper over standard `/proc` inspection rather than a separate data source.
