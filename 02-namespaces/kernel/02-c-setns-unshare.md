# 02-c — `setns(2)`, `unshare(2)`, and the `/proc/<pid>/ns/` Interface

## Source Files

| File | Link |
|------|------|
| `kernel/nsproxy.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/nsproxy.c |
| `fs/nsfs.c` | https://elixir.bootlin.com/linux/v6.9/source/fs/nsfs.c |
| `kernel/fork.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/fork.c |

## The Three Namespace Syscalls

There are exactly three ways to change a process's namespace membership:

1. **`clone()`/`clone3()`** — create a child in a new namespace at fork time. This is how containers are created. The child is born already in its new namespace. The parent is unaffected.

2. **`unshare()`** — detach *the calling process itself* from namespaces it currently shares, without forking. After `unshare(CLONE_NEWNS)`, the calling process has a private copy of its mount namespace. Any future mounts it makes are invisible to other processes that were sharing the old namespace. Critically, the calling process retains its own position in the old namespace for most types — only its *children* will be born in the new namespace for PID and time namespaces.

3. **`setns()`** — join an *existing* namespace identified by a file descriptor. The file descriptor points to a namespace object via the `/proc/<pid>/ns/` filesystem. This is how `kubectl exec` works: the `nsenter` utility opens `/proc/<container-pid>/ns/net`, passes the fd to `setns()`, and then the calling process's network view switches to the container's. The container's namespace object is referenced-counted, so it persists as long as any process belongs to it or any open file descriptor refers to it.

Together these three syscalls form the complete namespace API. `clone()` creates, `setns()` enters, `unshare()` splits. Every container runtime operation maps to one of these three. Understanding their kernel implementation means understanding exactly what happens when `kubectl exec -it pod-name -- /bin/sh` runs.

`setns()` and `unshare()` are the two syscalls that allow a running process to change its namespace membership. Together with `clone()` they form the complete namespace API. `clone()` creates a child in a new namespace at fork time; `setns()` lets a process join a namespace that already exists (identified by a file descriptor); and `unshare()` lets a process detach itself from namespaces it currently shares, getting new private copies without forking. This document covers the kernel implementation of both syscalls plus the `/proc/<pid>/ns/` virtual filesystem that makes namespace objects addressable and referenceable from userspace.

## Section 1 — `/proc/<pid>/ns/` — Namespaces as File Descriptors

The namespace file interface is the bridge between userspace and the kernel's namespace objects. Each live process has a directory `/proc/<pid>/ns/` populated with symlinks — one per namespace type — that point into the `nsfs` pseudo-filesystem.

### What the directory contains

```
$ ls -la /proc/1/ns/
lrwxrwxrwx ... cgroup -> cgroup:[4026531835]
lrwxrwxrwx ... ipc    -> ipc:[4026531839]
lrwxrwxrwx ... mnt    -> mnt:[4026531840]
lrwxrwxrwx ... net    -> net:[4026531992]
lrwxrwxrwx ... pid    -> pid:[4026531836]
lrwxrwxrwx ... pid_for_children -> pid:[4026531836]
lrwxrwxrwx ... time   -> time:[4026531834]
lrwxrwxrwx ... time_for_children -> time:[4026531834]
lrwxrwxrwx ... user   -> user:[4026531837]
lrwxrwxrwx ... uts    -> uts:[4026531838]
```

All ten entries are always present: `cgroup`, `ipc`, `mnt`, `net`, `pid`, `pid_for_children`, `time`, `time_for_children`, `user`, `uts`.

The `pid` and `pid_for_children` entries can differ: `pid` is the process's own PID namespace (the one in which the process has its PID), while `pid_for_children` is the PID namespace that children spawned by this process will be born into. The two diverge after a call to `unshare(CLONE_NEWPID)` — the calling process remains in its original PID namespace (`pid` unchanged), but `pid_for_children` now points to the newly created namespace. The same relationship holds for `time` and `time_for_children`.

### The symlink format

```
net:[4026531992]
```

The target string is `<type>:[<inum>]`. The number in brackets is the **inode number** of the namespace on the `nsfs` pseudo-filesystem. `nsfs` is mounted internally by the kernel and is not visible in `mount` or `findmnt` output, but its inodes are stable for the lifetime of a namespace instance.

### Namespace identity via inode

Two processes that share the same inode number for a given namespace type are in the same namespace instance. This is the canonical way to check namespace co-membership:

```bash
stat -L /proc/$PID1/ns/net /proc/$PID2/ns/net
# Same ino field → same network namespace instance
```

`lsns` uses exactly this mechanism: it walks `/proc/*/ns/*`, groups by inode, and reports unique namespaces.

### The `nsfs` filesystem

Source: `fs/nsfs.c` — https://elixir.bootlin.com/linux/v6.9/source/fs/nsfs.c

`nsfs` is a minimal pseudo-filesystem whose sole purpose is to give namespaces a stable inode so they can be referenced by file descriptors. The key function is `ns_get_path()`, which looks up or creates the inode for a namespace:

```c
// fs/nsfs.c
struct path ns_get_path(struct path *path, struct task_struct *task,
                        const struct proc_ns_operations *ns_ops)
```

Every namespace struct embeds `struct ns_common`, which holds the inode number and reference count (see Section 4). When a process opens `/proc/<pid>/ns/net`, the VFS resolves the symlink into `nsfs`, and `ns_get_path()` returns an inode whose number equals `ns_common.inum`. Holding the open file descriptor increments the namespace's reference count, keeping it alive.

### Keeping a namespace alive by bind-mounting

A namespace instance persists as long as at least one of the following is true:
- At least one process is a member of the namespace.
- At least one open file descriptor refers to it (an open `/proc/*/ns/<type>` file).
- The namespace has been bind-mounted somewhere.

Bind-mounting is the standard way to preserve a namespace after all its processes exit:

```bash
# Preserve a network namespace after all processes in it exit:
mount --bind /proc/$PID/ns/net /tmp/saved-netns

# /tmp/saved-netns now holds a reference; the netns survives
# To release: umount /tmp/saved-netns
```

This is how tools like `ip netns add` work under the hood: they create a new network namespace, bind-mount its `/proc/<pid>/ns/net` into `/var/run/netns/<name>`, then let the helper process exit.

### Opening a namespace fd

```c
// Open a namespace file descriptor — keeps the namespace alive,
// and can be passed directly to setns().
int nsfd = open("/proc/<pid>/ns/net", O_RDONLY | O_CLOEXEC);
if (nsfd < 0)
    err(1, "open ns");
```

The `O_CLOEXEC` flag is important in container runtimes: you do not want namespace fds leaking into the container process after `exec`.

## Section 2 — `setns(2)` — Joining an Existing Namespace

`setns()` makes the calling thread join a namespace that already exists, identified by a file descriptor pointing into `nsfs`.

### Syscall signature

```c
#include <sched.h>
int setns(int fd, int nstype);
```

- `fd`: open file descriptor to a `/proc/*/ns/*` symlink target (or any `nsfs` inode kept alive by bind-mount)
- `nstype`: expected namespace type — `0` to accept any type, or one of `CLONE_NEWNET`, `CLONE_NEWPID`, `CLONE_NEWNS`, etc. to enforce a type check (returns `EINVAL` if the fd's type does not match)
- Returns `0` on success, `-1` with `errno` set on error

### Kernel path

```
sys_setns()                          # kernel/nsproxy.c
  └─ validate_ns()                   # check fd refers to a namespace file
       └─ get_proc_ns()              # retrieve struct ns_common from the fd
  └─ [per-namespace install]
       └─ ns->ops->install(ns, tsk)  # e.g., netns_install(), mntns_install()
```

Source: `SYSCALL_DEFINE2(setns, ...)` in `kernel/nsproxy.c` — https://elixir.bootlin.com/linux/v6.9/source/kernel/nsproxy.c

The implementation opens the fd, resolves it to an `nsfs` inode via `proc_ns_fget()`, extracts the `ns_common` pointer embedded in the inode's private data, validates the type against `nstype`, and then calls the namespace's `install` callback.

### Per-namespace install functions

Each namespace type registers a `struct proc_ns_operations` (see Section 4) whose `install` callback is responsible for attaching the calling task to the new namespace. Key examples:

- **`netns_install()`** in `net/core/net_namespace.c` — replaces `tsk->nsproxy->net_ns` with the target network namespace; increments its refcount.
- **`mntns_install()`** in `fs/namespace.c` — replaces `tsk->nsproxy->mnt_ns` with the target mount namespace; also updates `tsk->fs->root` and `tsk->fs->pwd` if needed.
- **`pidns_install()`** in `kernel/pid_namespace.c` — sets `tsk->nsproxy->pid_ns_for_children` to the target PID namespace (the calling task's own PID namespace is never changed by `setns()`).
- **`utsns_install()`** in `kernel/utsname.c` — replaces `tsk->nsproxy->uts_ns`.

Each `install` callback allocates a new `nsproxy` struct (via `create_nsproxy()`) with the updated namespace pointer, then swaps the task's `nsproxy` atomically.

### Restrictions

- **User namespace privilege**: Joining a user namespace requires that the calling process either has `CAP_SYS_ADMIN` in the target user namespace, or the target user namespace is a descendant of the caller's current user namespace and the caller has no capabilities that would be dropped.
- **PID namespace ancestry**: Cannot join a PID namespace that is an ancestor of the caller's current PID namespace (you can only descend, never ascend).
- **Mount namespace / user namespace pairing**: Cannot join a mount namespace that belongs to a different user namespace than the caller's current user namespace without also joining the associated user namespace first. This is a security boundary to prevent privilege escalation via mount operations.
- **Network namespace**: Requires `CAP_SYS_ADMIN` in the user namespace that owns the target network namespace.

### How runc uses `setns()`

runc calls `setns()` when adding an application container to an existing pod sandbox (the pause container). The sequence:

1. At pod sandbox creation, runc calls `clone3()` with `CLONE_NEWNET | CLONE_NEWIPC | ...` flags. The resulting pause process runs `pause(2)` forever, keeping all namespaces open.
2. When an app container is added, runc opens `/proc/<pause_pid>/ns/net` and `/proc/<pause_pid>/ns/ipc` to get file descriptors into the pause container's namespaces.
3. runc calls `setns(net_fd, CLONE_NEWNET)` then `setns(ipc_fd, CLONE_NEWIPC)` in the parent process. Now the runc worker thread shares the pod's network and IPC namespaces.
4. runc then calls `clone3()` with `CLONE_NEWNS | CLONE_NEWUTS | CLONE_NEWPID | CLONE_NEWCGROUP` to give the app container its own private namespaces for those types.

The result: the app container shares `net` and `ipc` with the pause container (pod-level namespaces) but has its own mount, UTS, PID, and cgroup namespaces.

### bpftrace to trace `setns()` calls

```
bpftrace -e 'kprobe:__sys_setns {
    printf("pid=%-6d comm=%-16s nstype=0x%08x\n",
        pid, comm, (uint32)arg1);
}'
```

`__sys_setns` is the C-level entry point called by the syscall wrapper; tracing it captures all callers including those using the raw `syscall()` instruction.

## Section 3 — `unshare(2)` — Detaching from Shared Namespaces

`unshare()` creates new private copies of the specified namespaces and attaches the calling process to them, without forking a child.

### Syscall signature

```c
#include <sched.h>
int unshare(int flags);
```

- `flags`: a bitmask of `CLONE_NEW*` constants specifying which namespaces to detach from — `CLONE_NEWNS`, `CLONE_NEWUTS`, `CLONE_NEWIPC`, `CLONE_NEWNET`, `CLONE_NEWPID`, `CLONE_NEWUSER`, `CLONE_NEWCGROUP`, `CLONE_NEWTIME`
- The calling process gets fresh, private copies of the requested namespaces
- No child is created (unlike `clone()`)
- Returns `0` on success, `-1` with `errno` set on error

### Kernel path

```
sys_unshare()                               # kernel/fork.c → ksys_unshare()
  └─ unshare_nsproxy_namespaces()           # allocate new nsproxy + new ns structs
       ├─ create_new_namespaces()           # kernel/nsproxy.c
       │    ├─ copy_mnt_ns()               # if CLONE_NEWNS
       │    ├─ copy_utsname()              # if CLONE_NEWUTS
       │    ├─ copy_ipcs()                 # if CLONE_NEWIPC
       │    ├─ copy_net_ns()               # if CLONE_NEWNET
       │    ├─ copy_pid_ns()               # if CLONE_NEWPID
       │    ├─ copy_cgroup_ns()            # if CLONE_NEWCGROUP
       │    └─ copy_time_ns()              # if CLONE_NEWTIME
       └─ switch_task_namespaces(tsk, ns)  # atomically replace nsproxy pointer
```

Source: `ksys_unshare()` in `kernel/fork.c` — https://elixir.bootlin.com/linux/v6.9/source/kernel/fork.c

`ksys_unshare()` first validates the flags (some combinations are disallowed or require privilege), then calls `unshare_nsproxy_namespaces()` which allocates a new `nsproxy` and populates it by calling each `copy_*_ns()` function for the requested types. Each `copy_*_ns()` creates a fresh namespace initialized as a copy of the current one. Finally, `switch_task_namespaces()` atomically swaps the task's `nsproxy` pointer and drops the reference to the old one.

### Key difference from `clone()`

With `clone()`, the calling process forks a child into the new namespace; the parent's namespaces are unchanged. With `unshare()`, the **calling process itself** changes its namespace membership — no fork occurs. After `unshare(CLONE_NEWNS)`, the calling process's `nsproxy->mnt_ns` points to a new, private copy of the mount namespace. Mount operations from that point forward affect only the caller's view; the original mount namespace (still held by any other processes that shared it) is unaffected.

### The `CLONE_NEWPID` special case

`unshare(CLONE_NEWPID)` does **not** change the calling process's own PID namespace. Instead, it allocates a new PID namespace and stores it in `pid_ns_for_children` within the new `nsproxy`. The calling process itself remains in its original PID namespace with its original PID unchanged.

Children created after the `unshare()` call will be born into the new PID namespace. The first such child (which must be created via `fork()` or `clone()`) will be assigned PID 1 in that namespace and must act as init — it must reap all other processes in the namespace, because a PID namespace is destroyed when its PID 1 exits.

This means: to become PID 1 in the new namespace (as required when creating a container), the calling process must `fork()` after `unshare(CLONE_NEWPID)` and have the child exec the container's init process.

### The `CLONE_NEWTIME` special case

`unshare(CLONE_NEWTIME)` follows the same design as `CLONE_NEWPID`. The call sets `time_ns_for_children` in the new `nsproxy` but leaves `time_ns` (the calling process's own time namespace) unchanged. Only children born after the `unshare()` will see the new clock offsets configured in `/proc/<child>/timens_offsets`. The calling process continues to see the original monotonic and boot-time clocks.

### Shell usage

```bash
# Standard unprivileged user namespace + PID + mount (used by podman rootless):
unshare --user --pid --mount --fork --map-root-user bash

# Just create a new network namespace (requires root):
unshare --net bash
# Now inside a new netns: only loopback is present
ip addr   # shows only lo

# Create a new UTS namespace and set a custom hostname:
unshare --uts bash
hostname container-test
hostname   # container-test (invisible to host)
```

The `--fork` flag to the `unshare(1)` utility is specifically needed for `--pid` because of the `CLONE_NEWPID` special case: the utility calls `unshare(CLONE_NEWPID)`, then forks, and the child becomes PID 1 in the new namespace and execs the shell.

### bpftrace to trace `unshare()` calls

```
bpftrace -e 'kprobe:ksys_unshare {
    printf("pid=%-6d comm=%-16s flags=0x%08x\n",
        pid, comm, (uint32)arg0);
}'
```

## Section 4 — `struct ns_common` — The Namespace Base Type

Every namespace struct in the kernel embeds `struct ns_common` as its first field, providing a uniform interface for the VFS and reference-counting machinery.

```c
// include/linux/ns_common.h
struct ns_common {
    atomic_long_t stashed;                     /* used by nsfs inode stash */
    const struct proc_ns_operations *ops;      /* type-specific operations */
    unsigned int inum;                         /* inode number (= namespace identity) */
    refcount_t count;                          /* reference count */
};
```

Source: https://elixir.bootlin.com/linux/v6.9/source/include/linux/ns_common.h

- `inum`: assigned at namespace creation from `proc_inum_alloc()`, stable for the lifetime of the namespace, and used as the inode number on `nsfs`. This is the value shown in the symlink target (`net:[4026531992]`).
- `count`: reference count; reaches zero only when all processes, fds, and bind-mounts referring to the namespace are released. When it reaches zero, the namespace's destructor is called.
- `ops`: pointer to the type-specific operations table, described below.
- `stashed`: used internally by `nsfs` to cache the `nsfs` inode associated with this namespace (avoids creating a new inode on every `open()`).

### The `proc_ns_operations` vtable

Each namespace type registers a `struct proc_ns_operations` that provides the callbacks used by `setns()`, `nsfs`, and `/proc`:

```c
struct proc_ns_operations {
    const char *name;           /* short name: "net", "mnt", "uts", etc. */
    const char *real_ns_name;   /* full name shown by lsns */
    int type;                   /* CLONE_NEW* flag for this type */

    /* Get the ns_common for a given task (increments refcount) */
    struct ns_common *(*get)(struct task_struct *task);

    /* Drop a reference obtained by get() */
    void (*put)(struct ns_common *ns);

    /* Called by setns(): attach nsset to this namespace */
    int (*install)(struct nsset *nsset, struct ns_common *ns);

    /* Return the user namespace that owns this namespace */
    struct user_namespace *(*owner)(struct ns_common *ns);

    /* Return the parent namespace (used for PID/user ns hierarchy) */
    struct ns_common *(*get_parent)(struct ns_common *ns);
};
```

For example, the network namespace registers:

```c
const struct proc_ns_operations netns_operations = {
    .name       = "net",
    .real_ns_name = "network",
    .type       = CLONE_NEWNET,
    .get        = netns_get,
    .put        = netns_put,
    .install    = netns_install,
    .owner      = netns_owner,
};
```

Source for network namespace operations: `net/core/net_namespace.c` — https://elixir.bootlin.com/linux/v6.9/source/net/core/net_namespace.c

## Section 5 — Putting It All Together: The runc Namespace Setup Sequence

The following pseudocode shows the exact sequence runc uses to set up a Kubernetes pod. Namespace setup happens in a multi-step process spanning multiple processes and multiple syscalls.

```c
// ── Step 1: Create the pod sandbox (pause container) ─────────────────────────
// The pause container holds all pod-level namespaces open.
// It runs pause(2) forever and never exits voluntarily.

pid_t pause_pid = (pid_t)syscall(SYS_clone3, &(struct clone_args){
    .flags = CLONE_NEWNS      |   // new mount namespace
             CLONE_NEWUTS     |   // new UTS namespace (pod hostname)
             CLONE_NEWIPC     |   // new IPC namespace (shared by pod)
             CLONE_NEWPID     |   // new PID namespace (pause = PID 1)
             CLONE_NEWNET     |   // new network namespace (pod network)
             CLONE_NEWCGROUP,     // new cgroup namespace
    .exit_signal = SIGCHLD,
}, sizeof(struct clone_args));
// pause_pid calls pause(2) — holds all pod namespaces alive indefinitely.


// ── Step 2: For each app container, join net + ipc from the pause container ──
// Open file descriptors into the pause container's namespaces.
// These fds can be passed to setns() from any thread.

int net_fd = open("/proc/<pause_pid>/ns/net", O_RDONLY | O_CLOEXEC);
int ipc_fd = open("/proc/<pause_pid>/ns/ipc", O_RDONLY | O_CLOEXEC);
if (net_fd < 0 || ipc_fd < 0) err(1, "open ns fd");

// Join net and ipc — the current thread now lives in the pod's namespaces.
if (setns(net_fd, CLONE_NEWNET) < 0) err(1, "setns net");
if (setns(ipc_fd, CLONE_NEWIPC) < 0) err(1, "setns ipc");

close(net_fd);
close(ipc_fd);
// (The namespaces remain alive because pause_pid is still running in them.)


// ── Step 3: Clone the app container with its own private namespaces ───────────
// Because we already called setns() above, the child inherits net+ipc
// from the pause container. The CLONE_NEW* flags here create fresh
// namespaces only for the types listed — not net, not ipc.

pid_t app_pid = (pid_t)syscall(SYS_clone3, &(struct clone_args){
    .flags = CLONE_NEWNS      |   // own mount namespace (container rootfs)
             CLONE_NEWUTS     |   // own UTS namespace (container hostname)
             CLONE_NEWPID     |   // own PID namespace (app = PID 1 inside)
             CLONE_NEWCGROUP,     // own cgroup namespace
    .exit_signal = SIGCHLD,
}, sizeof(struct clone_args));

// Result for app_pid:
//   net  → shared with pause_pid (same pod network)
//   ipc  → shared with pause_pid (same pod IPC)
//   mnt  → private (container's own rootfs overlay)
//   uts  → private (container's own hostname)
//   pid  → private (container's own PID space; app_pid is PID 1 inside)
//   cgroup → private
```

This two-phase approach — first `setns()` to inherit pod-level namespaces, then `clone3()` to create container-level namespaces — is why all containers in a Kubernetes pod share the same IP address and can communicate over localhost, while each container has its own filesystem and process tree.

## Section 6 — Live Verification Cheatsheet

| Goal | Command |
|------|---------|
| List all namespaces on host | `lsns` |
| List namespaces by type | `lsns -t net` |
| Show all ns symlinks for a PID | `ls -la /proc/$PID/ns/` |
| Check if two PIDs share a namespace | `stat -L /proc/$PID1/ns/net /proc/$PID2/ns/net` |
| Enter all namespaces of a PID | `nsenter --target $PID --mount --uts --ipc --net --pid -- bash` |
| Enter only net namespace | `nsenter --net=/proc/$PID/ns/net -- ip addr` |
| Create new mount+uts namespace | `unshare --mount --uts -- bash` |
| Trace `setns()` calls | `bpftrace -e 'kprobe:__sys_setns { printf("%s pid=%d\n", comm, pid); }'` |
| Trace `unshare()` calls | `bpftrace -e 'kprobe:ksys_unshare { printf("%s pid=%d flags=0x%x\n", comm, pid, (uint32)arg0); }'` |
| Show namespace offsets (time ns) | `cat /proc/$PID/timens_offsets` |
| Bind-mount to keep alive | `mount --bind /proc/$PID/ns/net /tmp/netns` |
| Release bind-mounted namespace | `umount /tmp/netns` |
| Show inode of a specific ns | `readlink /proc/$PID/ns/net` |
| Check two containers share netns | `stat -L /proc/$C1_PID/ns/net /proc/$C2_PID/ns/net` |
