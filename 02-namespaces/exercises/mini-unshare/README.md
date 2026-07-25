# mini-unshare

A minimal C clone of `unshare(1)` that demonstrates how the `unshare(2)` syscall works at the kernel level.

## What It Demonstrates

- **The `unshare(2)` syscall**: takes the same `CLONE_NEW*` flags as `clone()` but operates on the calling process instead of creating a child. One call detaches the process from one or more shared kernel namespace objects and attaches it to fresh copies.

- **Namespace identity**: the before/after inode numbers printed from `/proc/self/ns/` prove a new namespace was created. Each namespace is a kernel object with a unique inode in the `nsfs` pseudo-filesystem. A changed inode means a different namespace object.

- **The `CLONE_NEWPID` special case**: `unshare(CLONE_NEWPID)` only sets `pid_ns_for_children` on the calling process — it does not move the caller into the new PID namespace. You must `fork()` so that the child is born into it and sees itself as PID 1. mini-unshare does this automatically when `--pid` is requested.

- **How runc works**: this is exactly what runc does for every container — `unshare()` or `clone3()` to create new namespaces, then `exec()` the container process. mini-unshare is the simplified, readable version of that sequence.

## Build and Run

```bash
make build
# Requires gcc, produces: ./mini_unshare
```

### Demo 1 — UTS namespace (hostname isolation)

```bash
sudo make run-uts
```

Expected output:

```
[before unshare]:
  net                     net:[4026531992]
  uts                     uts:[4026531838]
  mnt                     mnt:[4026531841]
  ipc                     ipc:[4026531839]
  pid                     pid:[4026531836]
[after  unshare]:
  net                     net:[4026531992]     <- unchanged (not requested)
  uts                     uts:[4026532247]     <- NEW inode — new UTS namespace
  mnt                     mnt:[4026531841]     <- unchanged
  ipc                     ipc:[4026531839]     <- unchanged
  pid                     pid:[4026531836]     <- unchanged
container-demo
```

The hostname changed inside the new UTS namespace. The host's hostname is unaffected.

### Demo 2 — Network namespace (network isolation)

```bash
sudo make run-net
```

Expected output shows only the loopback interface (`lo`) — the new network namespace starts empty with no routes, no eth0, no IP connectivity. This is the starting state of every container's network namespace before the CNI plugin runs.

### Demo 3 — PID + Mount namespace (process isolation)

```bash
sudo make run-pid
```

After forking into the new PID namespace, the child (and any processes it creates) are the only visible processes. The `fork()` call is mandatory because `CLONE_NEWPID` only affects children of the process that called `unshare()`.

## Kernel References

| Call | Kernel path | Source |
|------|------------|--------|
| `unshare(CLONE_NEWUTS)` | `ksys_unshare()` → `unshare_nsproxy_namespaces()` → `copy_utsname()` | kernel/fork.c |
| `unshare(CLONE_NEWNS)` | → `copy_mnt_ns()` | fs/namespace.c |
| `unshare(CLONE_NEWNET)` | → `copy_net_ns()` | net/core/net_namespace.c |
| `unshare(CLONE_NEWPID)` | sets `pid_ns_for_children` only | kernel/pid_namespace.c |

All at: https://elixir.bootlin.com/linux/v6.9/source/kernel/fork.c

## Exercises

1. **Verify namespace identity from the host**: while the `--uts` demo is running, open a second terminal and run `lsns -t uts`. You should see two UTS namespace rows — the host's and the one created by mini_unshare. Note the matching inode numbers.

2. **Network namespace isolation**: run `sudo ./mini_unshare --net -- bash`. Inside the new shell, run `ip link add dummy0 type dummy`. Verify from the host that `dummy0` is not visible in `ip link show`. This is exactly how containers get isolated network stacks.

3. **Unprivileged user namespace**: run `./mini_unshare --user -- id`. Without sudo, you are UID 0 inside the user namespace but still your real UID on the host. This is rootless container technology.

## K8s Connection

runc performs exactly these operations for every container:

1. Creates a new user namespace (if rootless) or uses root privileges
2. Calls `clone3()` with `CLONE_NEWNS | CLONE_NEWUTS | CLONE_NEWPID | CLONE_NEWCGROUP` for app-container-specific namespaces
3. Calls `setns()` with the pause container's net/ipc namespace fds to join the pod's shared namespaces
4. `exec()`s the container entrypoint

mini_unshare is the simplified version of steps 2 and 4.
