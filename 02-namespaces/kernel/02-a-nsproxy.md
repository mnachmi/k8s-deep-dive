# `struct nsproxy` — The Namespace Proxy

## Source Location

`include/linux/nsproxy.h`
https://elixir.bootlin.com/linux/v6.9/source/include/linux/nsproxy.h

## Full Struct Definition

The complete definition from Linux 6.9:

```c
struct nsproxy {
    refcount_t count;
    struct uts_namespace  *uts_ns;
    struct ipc_namespace  *ipc_ns;
    struct mnt_namespace  *mnt_ns;
    struct pid_namespace  *pid_ns_for_children;
    struct net            *net_ns;
    struct time_namespace *time_ns;
    struct time_namespace *time_ns_for_children;
    struct cgroup_namespace *cgroup_ns;
};
```

**Important:** `struct user_namespace` is NOT in `nsproxy`. It lives in `task_struct->cred->user_ns`. Because user namespace membership governs UID/GID mapping and capability checks, it must be updated atomically with other credential data. Keeping it in `struct cred` allows `execve` and `setresuid` to swap the entire credential set under RCU without touching the namespace proxy.

## Field-by-Field Explanation

### `refcount_t count`

An atomic reference count on the `nsproxy` struct itself. It is incremented by `get_nsproxy()` and decremented by `put_nsproxy()`. When the count reaches zero, `put_nsproxy()` calls `free_nsproxy()`, which calls the appropriate `put_*` function for each member namespace pointer before freeing the proxy struct itself. The `refcount_t` type (as opposed to `atomic_t`) uses saturation semantics that detect use-after-free bugs in reference counting.

### `struct uts_namespace *uts_ns`

Points to [`struct uts_namespace`](https://elixir.bootlin.com/linux/v6.9/source/include/linux/utsname.h) defined in `include/linux/utsname.h`. The UTS namespace isolates the system hostname and NIS domainname — the values returned by `uname(2)` fields `nodename` and `domainname`. This is the simplest namespace to reason about: every container gets its own `uts_namespace` so that `hostname` inside the container is independent of the host. Created by `CLONE_NEWUTS`.

### `struct ipc_namespace *ipc_ns`

Points to [`struct ipc_namespace`](https://elixir.bootlin.com/linux/v6.9/source/include/linux/ipc_namespace.h) defined in `include/linux/ipc_namespace.h`. Isolates System V IPC objects (message queues, semaphore sets, shared memory segments) and POSIX message queues. Each IPC namespace has its own set of IPC keys, so two containers can both create a semaphore with key `0x1234` without conflict. Created by `CLONE_NEWIPC`.

### `struct mnt_namespace *mnt_ns`

Points to [`struct mnt_namespace`](https://elixir.bootlin.com/linux/v6.9/source/fs/mount.h) defined in `fs/mount.h`. Isolates the set of mount points visible to a process — the filesystem topology. When a new mount namespace is created, it starts as a copy of the parent's mount tree. Subsequent `mount(2)` and `umount(2)` calls inside the new namespace only affect that namespace. This is what allows a container to have its own root filesystem via a `pivot_root(2)` or `chroot(2)` sequence. Created by `CLONE_NEWNS` (the oldest namespace flag, predating the naming convention).

### `struct pid_namespace *pid_ns_for_children`

Points to [`struct pid_namespace`](https://elixir.bootlin.com/linux/v6.9/source/include/linux/pid_namespace.h) defined in `include/linux/pid_namespace.h`. This is the PID namespace that will be assigned to **new children** spawned by this process, not the PID namespace the current process itself belongs to. To get the current process's own PID namespace, the kernel uses `task_active_pid_ns(current)`, which walks the `task_struct->thread_pid` pointer. This two-pointer design is necessary for `unshare(CLONE_NEWPID)` semantics: after calling `unshare`, the calling process's own PID in the old namespace does not change, but the next child it forks will be PID 1 in the new namespace. Created by `CLONE_NEWPID`.

### `struct net *net_ns`

Points to [`struct net`](https://elixir.bootlin.com/linux/v6.9/source/include/net/net_namespace.h) defined in `include/net/net_namespace.h`. Isolates a complete network stack: network interfaces, IP routing tables, firewall rules (netfilter), sockets, `/proc/net`, and `/sys/class/net`. This is the most resource-heavy namespace — creating a new one allocates a full protocol-family registration table and per-protocol state. Every Pod in Kubernetes gets its own `net` namespace, which is why each Pod has its own `eth0` interface and independent IP address. Created by `CLONE_NEWNET`.

### `struct time_namespace *time_ns` and `struct time_namespace *time_ns_for_children`

Both point to [`struct time_namespace`](https://elixir.bootlin.com/linux/v6.9/source/include/linux/time_namespace.h) defined in `include/linux/time_namespace.h`. Time namespaces isolate `CLOCK_MONOTONIC` and `CLOCK_BOOTTIME` so that a process inside the namespace sees different values from the host for those clocks (wall-clock `CLOCK_REALTIME` is not isolated). The namespace stores per-clock offsets that the vDSO and `clock_gettime(2)` apply transparently.

The two-field design mirrors the `pid_ns_for_children` pattern. `CLONE_NEWTIME` is unique in that it cannot be used with `clone(2)` directly — only `unshare(CLONE_NEWTIME)` is permitted. After calling `unshare`, the calling process continues to see the old clock offsets (via `time_ns`) while new children it creates will use the new namespace (via `time_ns_for_children`). This prevents the paradox of a process suddenly experiencing a time jump mid-execution. Created/entered via `unshare(CLONE_NEWTIME)`.

### `struct cgroup_namespace *cgroup_ns`

Points to [`struct cgroup_namespace`](https://elixir.bootlin.com/linux/v6.9/source/include/linux/cgroup.h) defined in `include/linux/cgroup.h`. Isolates the view of the cgroup hierarchy — specifically, it controls what a process sees when it reads `/proc/self/cgroup` and what path is exposed under `/sys/fs/cgroup`. Without a cgroup namespace, a containerized process would see its absolute cgroup path (e.g., `/kubepods/burstable/pod<uid>/container<id>`). With a cgroup namespace, that path appears as `/` inside the container. The namespace does not change which cgroup the process actually belongs to; it only changes the view. Created by `CLONE_NEWCGROUP`.

## Lifecycle (Code Flow)

The kernel manages `nsproxy` across the full process lifetime. The primary paths are process creation and process exit.

```
copy_process()                          # kernel/fork.c
  └─ copy_namespaces(flags, p)          # kernel/nsproxy.c
       ├─ [no CLONE_NEW* flags]
       │    get_nsproxy(old_ns)         # increment refcount — share parent's nsproxy
       └─ [any CLONE_NEW* flag]
            create_new_namespaces()     # allocate new nsproxy, clone each flagged ns
              ├─ copy_mnt_ns()          # fs/namespace.c
              ├─ copy_utsname()         # kernel/utsname.c
              ├─ copy_ipcs()            # ipc/namespace.c
              ├─ copy_pid_ns()          # kernel/pid_namespace.c
              ├─ copy_net_ns()          # net/core/net_namespace.c
              ├─ copy_time_ns()         # kernel/time_namespace.c
              └─ copy_cgroup_ns()       # kernel/cgroup/namespace.c

do_exit() → exit_task_namespaces()
  └─ put_nsproxy(ns)
       └─ [refcount == 0] free_nsproxy(ns)
            └─ put_* each member namespace
```

Each `copy_*()` function either increments the reference count on the parent's namespace (when the corresponding `CLONE_NEW*` flag is absent) or allocates and initializes a fresh namespace struct (when the flag is present). The `create_new_namespaces()` call always allocates a new `nsproxy` struct to hold the result, even if only one namespace is being cloned.

## Locking Discipline

The `nsproxy` struct involves three distinct locking concerns.

`nsproxy.count` uses `refcount_t` atomic operations. No spinlock or mutex is needed — the CPU's atomic increment/decrement instructions provide the necessary ordering.

The `nsproxy` pointer stored in `task_struct` is protected on the write path by `task_lock()`, which acquires `task_struct.alloc_lock` (a spinlock). On the read path, the pointer is accessed via `task_nsproxy()` under RCU read-lock. This means a reader can dereference `task->nsproxy` without taking any lock, provided it holds an RCU read-side critical section that prevents the `nsproxy` from being freed under it.

Each member namespace struct (e.g., `struct uts_namespace`, `struct net`) carries its own internal lock for protecting its mutable fields. These per-namespace locks are described in detail in `02-b-namespace-types.md`.

## The Sharing Model

When a process forks without any `CLONE_NEW*` flag, `copy_namespaces()` simply calls `get_nsproxy()` on the parent's existing `nsproxy` and assigns the same pointer to the child. The parent and child now share a single `nsproxy` struct; the refcount is two. Any change to the underlying namespace objects (e.g., mounting a filesystem) is visible to both processes.

Threads created with `CLONE_THREAD` always share the parent's `nsproxy` by this same mechanism. This is why all threads in a process see identical namespace views: they literally point at the same struct.

Only an `unshare(2)` call or a `clone(2)`/`clone3(2)` call that includes at least one `CLONE_NEW*` flag will cause `create_new_namespaces()` to run and allocate a fresh `nsproxy`. For each namespace type where the corresponding flag is absent, the new `nsproxy` still holds a reference to the parent's namespace for that type — it does not deep-copy everything. Only the flagged namespaces get fresh instances.

## Object Graph

```
task_struct
  ├─ nsproxy ──┬─ uts_ns  ──────────────► struct uts_namespace
  │            ├─ ipc_ns  ──────────────► struct ipc_namespace
  │            ├─ mnt_ns  ──────────────► struct mnt_namespace
  │            ├─ pid_ns_for_children ──► struct pid_namespace
  │            ├─ net_ns  ──────────────► struct net
  │            ├─ time_ns ──────────────► struct time_namespace
  │            ├─ time_ns_for_children ─► struct time_namespace
  │            └─ cgroup_ns ────────────► struct cgroup_namespace
  └─ cred
       └─ user_ns ─────────────────────► struct user_namespace  (NOT in nsproxy!)
```

## Live Observation

The following commands let you observe namespace state on a running Linux system.

List all namespace symlinks for the init process (PID 1). Each symlink encodes the namespace type and a unique inode number — two processes with the same inode number for a given namespace type share that namespace.

```bash
ls -la /proc/1/ns/
```

List all namespace instances currently active on the host, with the type, inode, number of processes, and a sample PID for each.

```bash
lsns
```

Trace `copy_namespaces()` in the kernel to capture every process creation event. The output shows the new task's PID and the address of its `nsproxy` pointer — when two successive events show the same address, the child is sharing the parent's proxy; a new address indicates a fresh allocation.

```bpftrace
bpftrace -e 'kprobe:copy_namespaces {
    $p = (struct task_struct *)arg1;
    printf("pid=%d nsproxy=%llx\n", $p->pid, (uint64)$p->nsproxy);
}'
```

To check whether two processes share a network namespace, compare the inode numbers of their `net` symlinks. Replace `PID1` and `PID2` with actual process IDs. Matching inode numbers confirm they are in the same network namespace.

```bash
stat /proc/$PID1/ns/net /proc/$PID2/ns/net
```

## Key Kernel Source References

| Symbol | File | Elixir Link | Purpose |
|--------|------|-------------|---------|
| `struct nsproxy` | `include/linux/nsproxy.h` | [link](https://elixir.bootlin.com/linux/v6.9/source/include/linux/nsproxy.h) | The proxy struct holding all namespace pointers |
| `copy_namespaces()` | `kernel/nsproxy.c` | [link](https://elixir.bootlin.com/linux/v6.9/source/kernel/nsproxy.c) | Decides whether to share or clone namespaces on fork |
| `create_new_namespaces()` | `kernel/nsproxy.c` | [link](https://elixir.bootlin.com/linux/v6.9/source/kernel/nsproxy.c) | Allocates a new nsproxy and calls per-ns copy helpers |
| `get_nsproxy()` | `include/linux/nsproxy.h` | [link](https://elixir.bootlin.com/linux/v6.9/source/include/linux/nsproxy.h) | Increments refcount; used when sharing parent's proxy |
| `put_nsproxy()` | `kernel/nsproxy.c` | [link](https://elixir.bootlin.com/linux/v6.9/source/kernel/nsproxy.c) | Decrements refcount; calls free_nsproxy() at zero |
| `free_nsproxy()` | `kernel/nsproxy.c` | [link](https://elixir.bootlin.com/linux/v6.9/source/kernel/nsproxy.c) | Releases each member ns and frees the struct |
| `exit_task_namespaces()` | `kernel/nsproxy.c` | [link](https://elixir.bootlin.com/linux/v6.9/source/kernel/nsproxy.c) | Called from do_exit(); drops the task's nsproxy reference |
| `task_nsproxy()` | `include/linux/nsproxy.h` | [link](https://elixir.bootlin.com/linux/v6.9/source/include/linux/nsproxy.h) | RCU-safe accessor for task->nsproxy |
| `struct uts_namespace` | `include/linux/utsname.h` | [link](https://elixir.bootlin.com/linux/v6.9/source/include/linux/utsname.h) | Hostname and domainname isolation |
| `struct ipc_namespace` | `include/linux/ipc_namespace.h` | [link](https://elixir.bootlin.com/linux/v6.9/source/include/linux/ipc_namespace.h) | SysV IPC and POSIX MQ isolation |
| `struct mnt_namespace` | `fs/mount.h` | [link](https://elixir.bootlin.com/linux/v6.9/source/fs/mount.h) | Mount point topology isolation |
| `struct pid_namespace` | `include/linux/pid_namespace.h` | [link](https://elixir.bootlin.com/linux/v6.9/source/include/linux/pid_namespace.h) | PID numbering isolation |
| `struct net` | `include/net/net_namespace.h` | [link](https://elixir.bootlin.com/linux/v6.9/source/include/net/net_namespace.h) | Full network stack isolation |
| `struct time_namespace` | `include/linux/time_namespace.h` | [link](https://elixir.bootlin.com/linux/v6.9/source/include/linux/time_namespace.h) | CLOCK_MONOTONIC/CLOCK_BOOTTIME offset isolation |
| `struct cgroup_namespace` | `include/linux/cgroup.h` | [link](https://elixir.bootlin.com/linux/v6.9/source/include/linux/cgroup.h) | Cgroup hierarchy view isolation |
| `struct user_namespace` | `include/linux/user_namespace.h` | [link](https://elixir.bootlin.com/linux/v6.9/source/include/linux/user_namespace.h) | UID/GID mapping (in cred, not nsproxy) |
