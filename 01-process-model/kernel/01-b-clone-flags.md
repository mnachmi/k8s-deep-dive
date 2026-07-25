# 01-b — `clone()`, `clone3()`, and the Container-Creating Flags

> **Primary source file:**
> [`kernel/fork.c`](https://elixir.bootlin.com/linux/v6.9/source/kernel/fork.c)
> (Linux 6.9, x86-64)
>
> **Syscall entry:**
> [`kernel/fork.c:3111`](https://elixir.bootlin.com/linux/v6.9/source/kernel/fork.c#L3111) — `SYSCALL_DEFINE5(clone, ...)`
> [`kernel/fork.c:3178`](https://elixir.bootlin.com/linux/v6.9/source/kernel/fork.c#L3178) — `SYSCALL_DEFINE2(clone3, ...)`

When runc (or any container runtime) creates a container, it calls `clone3()` with a
bitmask of `CLONE_NEW*` flags. Those flags are the kernel's instruction sheet for which
isolation boundaries to erect. This document traces the complete path from the syscall
boundary down to `copy_process()` and explains every flag a container runtime uses.

---

## 1. `clone()` and `clone3()` — the Syscalls

### 1.1 The old `clone()` syscall

`clone()` has been the process-creation syscall since Linux 2.0. On x86-64 its
prototype (from userspace, `<sched.h>`) is:

```c
long clone(unsigned long flags,
           void *child_stack,
           int *ptid,
           int *ctid,
           unsigned long newtls);
```

The syscall entry point (x86-64) is at
[`kernel/fork.c:3111`](https://elixir.bootlin.com/linux/v6.9/source/kernel/fork.c#L3111):

```c
/* kernel/fork.c, line 3111 (Linux 6.9) */
SYSCALL_DEFINE5(clone, unsigned long, clone_flags,
                unsigned long, newsp,
                int __user *, parent_tidptr,
                int __user *, child_tidptr,
                unsigned long, tls)
{
    struct kernel_clone_args args = {
        .flags        = (lower_32_bits(clone_flags) & ~CSIGNAL),
        .pidfd        = parent_tidptr,
        .child_tid    = child_tidptr,
        .parent_tid   = parent_tidptr,
        .exit_signal  = (lower_32_bits(clone_flags) & CSIGNAL),
        .stack        = newsp,
        .tls          = tls,
    };
    return kernel_clone(&args);
}
```

`clone()` converts the flat bitmask and separate arguments into a
`struct kernel_clone_args` and calls `kernel_clone()`. This is the indirection
that unifies `fork()`, `vfork()`, `clone()`, and `clone3()` into one code path.

### 1.2 `clone3()` — added in Linux 5.3

`clone3()` was introduced in Linux 5.3 (commit `7f192e3cd316`) to solve a specific
problem: `clone()`'s five-argument interface was running out of space for new flags
and features. `clone3()` takes a single pointer to a `struct clone_args`:

```c
long clone3(struct clone_args *uargs, size_t size);
```

The syscall entry is at
[`kernel/fork.c:3178`](https://elixir.bootlin.com/linux/v6.9/source/kernel/fork.c#L3178):

```c
/* kernel/fork.c, line 3178 (Linux 6.9) */
SYSCALL_DEFINE2(clone3, struct clone_args __user *, uargs, size_t, size)
{
    int err;
    struct kernel_clone_args kargs;
    pid_t set_tid[MAX_PID_NS_LEVEL];

    BUILD_BUG_ON(offsetofend(struct clone_args, tls) !=
             CLONE_ARGS_SIZE_VER0);
    BUILD_BUG_ON(offsetofend(struct clone_args, set_tid_size) !=
             CLONE_ARGS_SIZE_VER1);
    BUILD_BUG_ON(offsetofend(struct clone_args, cgroup) !=
             CLONE_ARGS_SIZE_VER2);
    BUILD_BUG_ON(sizeof(struct clone_args) != CLONE_ARGS_SIZE_VER2);

    if (size < CLONE_ARGS_SIZE_VER0 || size > PAGE_SIZE)
        return -E2BIG;

    err = copy_clone_args_from_user(&kargs, uargs, size);
    if (err)
        return err;

    if (!clone3_args_valid(&kargs))
        return -EINVAL;

    return kernel_clone(&kargs);
}
```

### 1.3 `struct clone_args` — the `clone3()` argument structure

**Source:** [`include/uapi/linux/sched.h:91`](https://elixir.bootlin.com/linux/v6.9/source/include/uapi/linux/sched.h#L91)

```c
/* include/uapi/linux/sched.h, lines 91–136 (Linux 6.9) */
struct clone_args {
    __aligned_u64 flags;        /* Flags bitmask: CLONE_* values */
    __aligned_u64 pidfd;        /* Where to store PID file descriptor (CLONE_PIDFD) */
    __aligned_u64 child_tid;    /* Where to store child TID in child (CLONE_CHILD_SETTID) */
    __aligned_u64 parent_tid;   /* Where to store child TID in parent (CLONE_PARENT_SETTID) */
    __aligned_u64 exit_signal;  /* Signal to send to parent on child exit */
    __aligned_u64 stack;        /* New stack pointer, or 0 for copy */
    __aligned_u64 stack_size;   /* Size of new stack (used with stack) */
    __aligned_u64 tls;          /* TLS value for new thread (CLONE_SETTLS) */
    __aligned_u64 set_tid;      /* Pointer to array of desired PID values, one per namespace level */
    __aligned_u64 set_tid_size; /* Number of PIDs in set_tid array */
    __aligned_u64 cgroup;       /* cgroup fd to place child into (CLONE_INTO_CGROUP) */
};
```

**Field-by-field explanation:**

| Field | Type | Purpose |
|-------|------|---------|
| `flags` | `u64` | Bitmask of `CLONE_*` constants. Identical semantics to `clone()` flags but now 64-bit wide for future expansion. |
| `pidfd` | `u64` | Pointer to an `int` where the kernel writes the new pidfd file descriptor when `CLONE_PIDFD` is set. Container runtimes use pidfds to wait for container exit without a race. |
| `child_tid` | `u64` | If `CLONE_CHILD_SETTID` is set, the kernel writes the child's TID into this address inside the child's address space. glibc uses this for `pthread_self()`. |
| `parent_tid` | `u64` | If `CLONE_PARENT_SETTID` is set, the kernel writes the child's TID into this address inside the parent's address space. |
| `exit_signal` | `u64` | Signal number (e.g., `SIGCHLD`) to send to the parent when the child exits. Set to 0 for threads that should not send a signal. |
| `stack` | `u64` | Base address of the new stack. For `fork()`-style process creation this is 0 (child shares parent's stack until COW). For thread creation this must point to an allocated stack. |
| `stack_size` | `u64` | Size of the stack in bytes. Used together with `stack`. |
| `tls` | `u64` | TLS (Thread Local Storage) base pointer for the new thread when `CLONE_SETTLS` is set. On x86-64 this sets `fs_base`. |
| `set_tid` | `u64` | Pointer to array of desired PID values, one per namespace level. Allows container runtimes to request specific PIDs (e.g., to ensure the container process gets PID 1). Requires `CAP_SYS_ADMIN`. |
| `set_tid_size` | `u64` | Length of the `set_tid` array. Must not exceed the namespace nesting depth. |
| `cgroup` | `u64` | File descriptor of a cgroup (cgroups v2) to place the new child into atomically at creation. Added in Linux 5.7. This is how container runtimes ensure the new process is in the right cgroup before it runs. |

> **Why container runtimes prefer `clone3()`:** The `cgroup` field lets the runtime
> perform an atomic "create-and-place-in-cgroup" operation, eliminating a race window
> where the new process could run outside its cgroup limits for a brief interval between
> `clone()` and the `echo $PID > /sys/fs/cgroup/.../cgroup.procs` write.

### 1.4 The unified entry point: `kernel_clone()`

Both `clone()` and `clone3()` converge at `kernel_clone()` at
[`kernel/fork.c:2979`](https://elixir.bootlin.com/linux/v6.9/source/kernel/fork.c#L2979):

```c
/* kernel/fork.c, line 2979 (Linux 6.9) */
pid_t kernel_clone(struct kernel_clone_args *args)
{
    ...
    p = copy_process(NULL, trace, NUMA_NO_NODE, args);
    ...
    pid = get_task_pid(p, PIDTYPE_PID);
    nr = pid_vnr(pid);
    ...
    wake_up_new_task(p);
    ...
    return nr;
}
```

`kernel_clone()` calls `copy_process()` (see Section 3), then schedules the new task.

---

## 2. The Container-Creating Flags — Field by Field

The following flags, when passed to `clone3()`, are what separates a container from a
plain process. All flag constants are defined in
[`include/uapi/linux/sched.h`](https://elixir.bootlin.com/linux/v6.9/source/include/uapi/linux/sched.h).

### 2.1 `CLONE_NEWPID` — New PID Namespace

| Attribute | Value |
|-----------|-------|
| **Value** | `0x20000000` |
| **Kernel path** | `copy_pid_ns()` in [`kernel/pid_namespace.c`](https://elixir.bootlin.com/linux/v6.9/source/kernel/pid_namespace.c) |
| **Source** | [`include/uapi/linux/sched.h:24`](https://elixir.bootlin.com/linux/v6.9/source/include/uapi/linux/sched.h#L24) |

**Effect:** The child process is placed in a new PID namespace. The child becomes PID 1
in that namespace — regardless of its PID on the host.

**Kernel path:** Inside `copy_process()`, `copy_namespaces()` is called, which calls
`create_pid_namespace()` in
[`kernel/pid_namespace.c:121`](https://elixir.bootlin.com/linux/v6.9/source/kernel/pid_namespace.c#L121).
The new namespace has an empty IDR (PID allocator); the first process created in it
receives PID 1.

**Important constraint:** `CLONE_NEWPID` cannot be combined with `CLONE_THREAD` or
`CLONE_PARENT`. The new PID namespace requires a new process, not a new thread.

**Container relevance:** This is the flag that creates the illusion of a fresh system
with its own process tree. It is why `ps aux` inside a container shows only the
container's processes and why `/proc/1/` inside the container shows the container init.

---

### 2.2 `CLONE_NEWNET` — New Network Namespace

| Attribute | Value |
|-----------|-------|
| **Value** | `0x40000000` |
| **Kernel path** | `copy_net_ns()` in [`net/core/net_namespace.c`](https://elixir.bootlin.com/linux/v6.9/source/net/core/net_namespace.c) |
| **Source** | [`include/uapi/linux/sched.h:25`](https://elixir.bootlin.com/linux/v6.9/source/include/uapi/linux/sched.h#L25) |

**Effect:** The child gets a new `struct net` with its own interface list, routing
tables, netfilter rules, and socket table. The only interface present at creation is
the loopback device (`lo`). All other interfaces (veth pairs, bonds, bridges) must be
explicitly added by a CNI plugin.

**Kernel path:** `copy_net_ns()` calls `net_alloc()` to allocate a new `struct net`,
registers a new `net_generic` array for per-net data, and initialises the loopback
device. The init work (`register_pernet_operations` callbacks) runs asynchronously.

**Container relevance:** This is what gives each pod its own `eth0` interface and
isolated IP address. The CNI plugin runs after `clone3()` returns and uses
`ip link set <veth> netns <ns-fd>` to move a veth interface into the container's
network namespace.

---

### 2.3 `CLONE_NEWNS` — New Mount Namespace

| Attribute | Value |
|-----------|-------|
| **Value** | `0x00020000` |
| **Kernel path** | `copy_mnt_ns()` in [`fs/namespace.c`](https://elixir.bootlin.com/linux/v6.9/source/fs/namespace.c) |
| **Source** | [`include/uapi/linux/sched.h:14`](https://elixir.bootlin.com/linux/v6.9/source/include/uapi/linux/sched.h#L14) |

**Effect:** The child gets a copy of the parent's mount tree. After `clone3()` returns,
runc calls `pivot_root()` (or `chroot()` for older runtimes) to make the OCI overlay
filesystem the container's root. Because this happens in a private mount namespace,
the host sees none of these mounts.

**Kernel path:** `copy_mnt_ns()` walks the parent's mount tree and calls
`copy_tree()` for each mount point, producing a deep copy. The copy shares the
underlying `struct super_block` objects (the actual filesystems) — only the view of
which paths are mounted where is duplicated.

**The `MS_SHARED` / `MS_PRIVATE` distinction:** Mount propagation rules govern whether
mounts made inside the new namespace are visible to the parent or peers. runc sets
all container mounts as `MS_PRIVATE` before entering the namespace, preventing
accidental propagation.

**Container relevance:** Without `CLONE_NEWNS`, any `mount()` call inside a container
would modify the host's filesystem view. This flag is mandatory for container
filesystem isolation.

---

### 2.4 `CLONE_NEWUTS` — New UTS Namespace (Hostname)

| Attribute | Value |
|-----------|-------|
| **Value** | `0x04000000` |
| **Kernel path** | `copy_utsname()` in [`kernel/utsname.c`](https://elixir.bootlin.com/linux/v6.9/source/kernel/utsname.c) |
| **Source** | [`include/uapi/linux/sched.h:20`](https://elixir.bootlin.com/linux/v6.9/source/include/uapi/linux/sched.h#L20) |

**Effect:** The child gets a copy of the parent's UTS struct. The UTS namespace holds
the values returned by `uname(2)` — specifically `nodename` (hostname) and
`domainname`. After `clone3()`, runc calls `sethostname()` with the container's name.

**Kernel path:** `copy_utsname()` allocates a new `struct uts_namespace` and
`memcpy()`s the `new_utsname` struct (the `utsname` fields) from the parent's UTS
namespace. Since the copy is independent, `sethostname()` inside the container does
not affect the host.

**Container relevance:** This is why each pod has its own hostname. Kubernetes sets the
pod's `spec.hostname` (and `spec.subdomain`) through this mechanism.

---

### 2.5 `CLONE_NEWIPC` — New IPC Namespace

| Attribute | Value |
|-----------|-------|
| **Value** | `0x08000000` |
| **Kernel path** | `copy_ipcs()` in [`ipc/namespace.c`](https://elixir.bootlin.com/linux/v6.9/source/ipc/namespace.c) |
| **Source** | [`include/uapi/linux/sched.h:21`](https://elixir.bootlin.com/linux/v6.9/source/include/uapi/linux/sched.h#L21) |

**Effect:** The child gets a new IPC namespace with an empty set of System V IPC
objects (message queues, semaphores, shared memory segments) and POSIX message queues.
IPC objects created inside the container are invisible outside it.

**Kernel path:** `copy_ipcs()` calls `create_ipc_ns()` which allocates a new
`struct ipc_namespace`, initialises the `msg_ids`, `sem_ids`, and `shm_ids` IDR
tables, and zeroes all accounting limits to their defaults.

**Container relevance:** Prevents cross-container SysV shared memory attacks. Without
`CLONE_NEWIPC`, a container process could open a SysV shared memory segment by the
same key as a host process and read or corrupt its data.

---

### 2.6 `CLONE_NEWUSER` — New User Namespace

| Attribute | Value |
|-----------|-------|
| **Value** | `0x10000000` |
| **Kernel path** | `copy_user_ns()` in [`kernel/user_namespace.c`](https://elixir.bootlin.com/linux/v6.9/source/kernel/user_namespace.c) |
| **Source** | [`include/uapi/linux/sched.h:23`](https://elixir.bootlin.com/linux/v6.9/source/include/uapi/linux/sched.h#L23) |

**Effect:** The child gets a new user namespace where UID/GID mappings can be
configured. Inside the namespace, the process can appear to be UID 0 (root) while
mapping to an unprivileged UID on the host. Capabilities granted inside the namespace
are scoped to the namespace — they do not grant host privileges.

**Kernel path:** `copy_user_ns()` allocates a new `struct user_namespace`, sets
`ns->parent` to the current user namespace, and sets `ns->owner` and `ns->group` to
the caller's UID/GID. The UID/GID mappings (`uid_map`, `gid_map`) are configured
after `clone3()` returns by writing to `/proc/<child-pid>/uid_map` and
`/proc/<child-pid>/gid_map`.

**Container relevance:** `CLONE_NEWUSER` is used by rootless containers (runc in
rootless mode, Podman, Docker rootless). The container runtime runs as a non-root user
on the host but appears as root inside the container. The kernel enforces that the UID
map is written before the process can gain any capabilities inside the namespace.

**Special rule:** `CLONE_NEWUSER` is the only `CLONE_NEW*` flag that does not require
`CAP_SYS_ADMIN`. Any unprivileged process can create a user namespace. This is the
capability-bootstrap mechanism for rootless containers.

---

### 2.7 `CLONE_NEWCGROUP` — New Cgroup Namespace

| Attribute | Value |
|-----------|-------|
| **Value** | `0x02000000` |
| **Kernel path** | `copy_cgroup_ns()` in [`kernel/cgroup/namespace.c`](https://elixir.bootlin.com/linux/v6.9/source/kernel/cgroup/namespace.c) |
| **Source** | [`include/uapi/linux/sched.h:26`](https://elixir.bootlin.com/linux/v6.9/source/include/uapi/linux/sched.h#L26) |

**Effect:** The child gets a new cgroup namespace rooted at the container's current
cgroup. From inside the namespace, the container's cgroup appears as `/`. The host
cgroup hierarchy is not visible.

**Kernel path:** `copy_cgroup_ns()` allocates a new `struct cgroup_namespace` and sets
`new_ns->root_cset` to the current task's `css_set`. This records the "virtualization
root" for the namespace. When `/proc/self/cgroup` is read inside the container,
the kernel strips the host-side path prefix using `cgroup_path_ns()`.

**Container relevance:** Without `CLONE_NEWCGROUP`, a container process reading
`/proc/self/cgroup` would see the full host cgroup path
(`/kubepods/burstable/podXXX/containerXXX`), leaking information about the cluster
topology. With it, the container sees `/` as its cgroup root.

---

### 2.8 `CLONE_NEWTIME` — New Time Namespace

| Attribute | Value |
|-----------|-------|
| **Value** | `0x00000080` |
| **Kernel path** | `copy_time_ns()` in [`kernel/time_namespace.c`](https://elixir.bootlin.com/linux/v6.9/source/kernel/time_namespace.c) |
| **Source** | [`include/uapi/linux/sched.h:15`](https://elixir.bootlin.com/linux/v6.9/source/include/uapi/linux/sched.h#L15) |

**Effect:** The child can have independent `CLOCK_MONOTONIC` and `CLOCK_BOOTTIME`
clock readings. An offset can be applied to these clocks without affecting the host.
`CLOCK_REALTIME` (wall clock time) is not affected — it remains shared.

**Kernel path:** `copy_time_ns()` allocates a new `struct time_namespace` with zero
offsets for both clocks. The offsets are configurable by writing to
`/proc/<pid>/timens_offsets`. The vDSO (virtual dynamic shared object) in the new
namespace is remapped with the correct offset so that `clock_gettime(CLOCK_MONOTONIC)`
reads the adjusted time without a syscall.

**Container relevance:** Added in Linux 5.6. Used for checkpoint/restore (CRIU) where
a container's monotonic clock needs to match its pre-checkpoint value after restore.
Not used by default in Kubernetes but important for stateful workload migration.

**Key difference from other `CLONE_NEW*` flags:** `CLONE_NEWTIME` is special because it
**cannot be combined with `unshare()`** — the time namespace must be set up before the
first clock read, so it can only be created at `clone3()` time.

---

### 2.9 Thread-Sharing Flags

These flags do not create new namespaces but control what is shared between parent and
child. They are used internally by `pthread_create()` to create threads within a process.

| Flag | Value | Effect | `copy_process()` behavior |
|------|-------|--------|--------------------------|
| `CLONE_VM` | `0x00000100` | Share address space | `copy_mm()` skips `dup_mm()`, increments `mm_users` |
| `CLONE_FS` | `0x00000200` | Share filesystem context (root, cwd, umask) | `copy_fs()` increments `fs->users`, skips duplication |
| `CLONE_FILES` | `0x00000400` | Share file descriptor table | `copy_files()` increments `files->count`, skips duplication |
| `CLONE_SIGHAND` | `0x00000800` | Share signal handler table | `copy_sighand()` increments `sighand->count` |
| `CLONE_THREAD` | `0x00010000` | Same thread group (same `tgid`) | Sets `p->tgid = current->tgid`, links into `thread_group` list |

**Source:** [`include/uapi/linux/sched.h`](https://elixir.bootlin.com/linux/v6.9/source/include/uapi/linux/sched.h#L8)

**Container relevance:** Container runtimes never use these flags for container
creation — they need a fully separate process. These are relevant when understanding
why container processes show `Threads: N` in `/proc/<pid>/status` and why a
multi-threaded container process shares the same PID namespace even across threads.

### 2.10 Complete flag table

```c
/* include/uapi/linux/sched.h (Linux 6.9) — container-relevant flags */
#define CLONE_VM            0x00000100  /* share VM */
#define CLONE_FS            0x00000200  /* share fs info */
#define CLONE_FILES         0x00000400  /* share open files */
#define CLONE_SIGHAND       0x00000800  /* share signal handlers */
#define CLONE_PIDFD         0x00001000  /* set if a pidfd should be placed ... */
#define CLONE_PTRACE        0x00002000  /* set if we want to let tracing ... */
#define CLONE_VFORK         0x00004000  /* set if the parent wants the child ... */
#define CLONE_PARENT        0x00008000  /* set if we want to have the same parent ... */
#define CLONE_THREAD        0x00010000  /* Same thread group? */
#define CLONE_NEWNS         0x00020000  /* New mount namespace group */
#define CLONE_SYSVSEM       0x00040000  /* share system V SEM_UNDO semantics */
#define CLONE_SETTLS        0x00080000  /* create a new TLS for the child */
#define CLONE_PARENT_SETTID 0x00100000  /* set the TID in the parent */
#define CLONE_CHILD_CLEARTID 0x00200000 /* clear the TID in the child */
#define CLONE_DETACHED      0x00400000  /* Unused, ignored */
#define CLONE_UNTRACED      0x00800000  /* set if the tracing process can't ... */
#define CLONE_CHILD_SETTID  0x01000000  /* set the TID in the child */
#define CLONE_NEWCGROUP     0x02000000  /* New cgroup namespace */
#define CLONE_NEWUTS        0x04000000  /* New utsname namespace */
#define CLONE_NEWIPC        0x08000000  /* New ipc namespace */
#define CLONE_NEWUSER       0x10000000  /* New user namespace */
#define CLONE_NEWPID        0x20000000  /* New pid namespace */
#define CLONE_NEWNET        0x40000000  /* New network namespace */
#define CLONE_IO            0x80000000  /* Clone io_context */
#define CLONE_NEWTIME       0x00000080  /* New time namespace */
```

---

## 3. `copy_process()` — The 15-Step Walkthrough

`copy_process()` is the heart of process creation in the Linux kernel. It is called by
`kernel_clone()` and does all the real work. It is defined at
[`kernel/fork.c:2105`](https://elixir.bootlin.com/linux/v6.9/source/kernel/fork.c#L2105).

The function is ~400 lines long. The walkthrough below follows the actual order of
operations in Linux 6.9.

### Step 1: Validate flags

```c
/* kernel/fork.c:2120 — flag validity checks */
retval = security_task_create(clone_flags);
...
/*
 * Thread groups must share signals as well, and detached threads
 * can only be started up within the thread group.
 */
if ((clone_flags & CLONE_THREAD) && !(clone_flags & CLONE_SIGHAND))
    return ERR_PTR(-EINVAL);
if ((clone_flags & CLONE_SIGHAND) && !(clone_flags & CLONE_VM))
    return ERR_PTR(-EINVAL);
```

**What happens:** The kernel checks that the flag combination is consistent. For
example, `CLONE_THREAD` requires `CLONE_SIGHAND` (threads must share signal handlers),
and `CLONE_SIGHAND` requires `CLONE_VM` (you cannot share signal handlers without
sharing the address space). Namespace flags are also checked for compatibility.

### Step 2: Duplicate the task_struct

```c
/* kernel/fork.c:2195 — dup_task_struct() */
p = dup_task_struct(current, node);
if (!p)
    goto fork_out;
```

`dup_task_struct()` (at
[`kernel/fork.c:1019`](https://elixir.bootlin.com/linux/v6.9/source/kernel/fork.c#L1019))
allocates a new `task_struct` from the `task_struct_cachep` slab cache and copies all
fields from the parent with `memcpy()`. It then allocates a new kernel stack and wires
it into `thread_info`. After this step, the child is a bitwise copy of the parent —
the subsequent steps un-share the fields that should not be shared.

### Step 3: Copy credentials

```c
/* kernel/fork.c:2235 — copy_creds() */
retval = copy_creds(p, clone_flags);
if (retval < 0)
    goto bad_fork_free;
```

`copy_creds()` in
[`kernel/cred.c:310`](https://elixir.bootlin.com/linux/v6.9/source/kernel/cred.c#L310)
allocates a new `struct cred`, copies all credential fields from the parent (including
UID/GID, capabilities, and user namespace pointer), and sets `p->cred = p->real_cred`
to the new cred. For `CLONE_NEWUSER`, it additionally calls `create_user_ns()` to
allocate the new user namespace.

### Step 4: Initialize scheduler state

```c
/* kernel/fork.c:2266 — sched_fork() */
retval = sched_fork(clone_flags, p);
if (retval)
    goto bad_fork_cleanup_policy;
```

`sched_fork()` in
[`kernel/sched/core.c:4680`](https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/core.c#L4680)
initialises the child's scheduling entity (`se`). This sets the child's `vruntime` to
a value slightly above the parent's (preventing a newly forked process from monopolising
the CPU), assigns the scheduling class, and marks the task as `TASK_NEW` (not yet on
a run queue).

### Step 5: Copy or share signal handlers

```c
/* kernel/fork.c:2290 — copy_sighand() */
if (clone_flags & CLONE_SIGHAND) {
    atomic_inc(&current->sighand->count);
    p->sighand = current->sighand;
} else {
    retval = copy_sighand(p);
    ...
}
```

If `CLONE_SIGHAND` is set (threads), the child shares the parent's `sighand_struct`
by incrementing its refcount. Otherwise, a new `sighand_struct` is allocated with a
copy of the parent's signal handler table.

### Step 6: Copy signals

```c
/* kernel/fork.c:2300 — copy_signal() */
retval = copy_signal(clone_flags, p);
```

`copy_signal()` handles the `struct signal_struct`, which contains the resource limits
(`rlim[]`), process group and session IDs, and the thread list head. For threads
(`CLONE_THREAD`), this struct is shared; for new processes, a new one is allocated.

### Step 7: Copy (or share) the memory map

```c
/* kernel/fork.c:2320 — copy_mm() */
retval = copy_mm(clone_flags, p);
if (retval)
    goto bad_fork_cleanup_signal;
```

`copy_mm()` at
[`kernel/fork.c:1552`](https://elixir.bootlin.com/linux/v6.9/source/kernel/fork.c#L1552):

- If `CLONE_VM` is set (threads): increments `mm->mm_users` and shares the `mm_struct`.
- Otherwise: calls `dup_mm()` which allocates a new `mm_struct` and performs a
  copy-on-write copy of the page tables. After this, parent and child share the same
  physical pages but have separate page table hierarchies. Any write to a shared page
  triggers a page fault that copies the page.

### Step 8: Copy namespaces

```c
/* kernel/fork.c:2330 — copy_namespaces() */
retval = copy_namespaces(clone_flags, p);
if (retval)
    goto bad_fork_cleanup_mm;
```

`copy_namespaces()` at
[`kernel/nsproxy.c:152`](https://elixir.bootlin.com/linux/v6.9/source/kernel/nsproxy.c#L152)
is where the `CLONE_NEW*` flags are acted on. For each namespace type:

- If the corresponding `CLONE_NEW*` flag is set: allocate a new namespace instance.
- Otherwise: increment the refcount on the parent's namespace and share it.

The function calls each namespace-specific creation function in sequence:

```c
/* kernel/nsproxy.c:152 — copy_namespaces (Linux 6.9, simplified) */
int copy_namespaces(unsigned long flags, struct task_struct *tsk)
{
    struct nsproxy *old_ns = tsk->nsproxy;
    struct nsproxy *new_ns;

    if (likely(!(flags & (CLONE_NEWNS | CLONE_NEWUTS | CLONE_NEWIPC |
                          CLONE_NEWPID | CLONE_NEWNET |
                          CLONE_NEWCGROUP | CLONE_NEWTIME)))) {
        get_nsproxy(old_ns);
        return 0;
    }
    ...
    new_ns = create_new_namespaces(flags, tsk, user_ns, tsk->fs->root.mnt);
    ...
    tsk->nsproxy = new_ns;
    return 0;
}
```

Inside `create_new_namespaces()`, each subsystem's copy function is called:

| Flag | Function | Source |
|------|----------|--------|
| `CLONE_NEWUTS` | `copy_utsname()` | [`kernel/utsname.c`](https://elixir.bootlin.com/linux/v6.9/source/kernel/utsname.c) |
| `CLONE_NEWIPC` | `copy_ipcs()` | [`ipc/namespace.c`](https://elixir.bootlin.com/linux/v6.9/source/ipc/namespace.c) |
| `CLONE_NEWNS` | `copy_mnt_ns()` | [`fs/namespace.c`](https://elixir.bootlin.com/linux/v6.9/source/fs/namespace.c) |
| `CLONE_NEWPID` | `copy_pid_ns()` | [`kernel/pid_namespace.c`](https://elixir.bootlin.com/linux/v6.9/source/kernel/pid_namespace.c) |
| `CLONE_NEWCGROUP` | `copy_cgroup_ns()` | [`kernel/cgroup/namespace.c`](https://elixir.bootlin.com/linux/v6.9/source/kernel/cgroup/namespace.c) |
| `CLONE_NEWNET` | `copy_net_ns()` | [`net/core/net_namespace.c`](https://elixir.bootlin.com/linux/v6.9/source/net/core/net_namespace.c) |
| `CLONE_NEWTIME` | `copy_time_ns()` | [`kernel/time_namespace.c`](https://elixir.bootlin.com/linux/v6.9/source/kernel/time_namespace.c) |

### Step 9: Copy the I/O context

```c
/* kernel/fork.c:2340 — copy_io() */
retval = copy_io(clone_flags, p);
```

The I/O context (`struct io_context`) tracks I/O scheduling priority. If `CLONE_IO` is
set, the child shares the parent's I/O context. Otherwise (the default), the child gets
a new one. Container processes use separate I/O contexts, so I/O priority can be set
independently.

### Step 10: Copy the filesystem context

```c
/* kernel/fork.c:2345 — copy_fs() */
retval = copy_fs(clone_flags, p);
```

The `fs_struct` holds the process's root directory and current working directory. If
`CLONE_FS` is set (threads), it is shared. Otherwise a copy is made. For containers,
runc later calls `pivot_root()` on this copy to change the container's root.

### Step 11: Copy the file descriptor table

```c
/* kernel/fork.c:2352 — copy_files() */
retval = copy_files(clone_flags, p);
```

The `files_struct` holds the file descriptor table (`fdtable`). If `CLONE_FILES`, it
is shared (threads). Otherwise a copy is made with all `struct file *` pointers copied
and their refcounts incremented. This is why `fork()` results in both parent and child
sharing the same open file descriptions (seeking in one affects the other).

### Step 12: Set up the thread info / CPU state

```c
/* kernel/fork.c:2358 — copy_thread() */
retval = copy_thread(p, args);
if (retval)
    goto bad_fork_cleanup_io;
```

`copy_thread()` is architecture-specific (for x86-64 at
[`arch/x86/kernel/process.c:135`](https://elixir.bootlin.com/linux/v6.9/source/arch/x86/kernel/process.c#L135)).
It sets up the child's kernel stack so that when the child is first scheduled, it
returns from `ret_from_fork`, which lands in the child's userspace entry point. For
kernel threads, it sets the start function pointer. The parent's register state
(including the return value `0` for the child) is placed in the child's kernel stack.

### Step 13: Allocate the PID

```c
/* kernel/fork.c:2375 — alloc_pid() */
pid = alloc_pid(p->nsproxy->pid_ns_for_children, args->set_tid,
                args->set_tid_size);
if (IS_ERR(pid)) {
    retval = PTR_ERR(pid);
    goto bad_fork_cleanup_thread;
}
```

`alloc_pid()` in
[`kernel/pid.c:180`](https://elixir.bootlin.com/linux/v6.9/source/kernel/pid.c#L180)
allocates a `struct pid` with one `struct upid` entry per namespace level in the
hierarchy. For a container process in a nested PID namespace:
- `numbers[0]` gets a PID number in the initial (host) namespace.
- `numbers[1]` gets a PID number in the container's namespace (likely 1 if this is
  the first process).

The PID numbers are allocated from the IDR (radix tree) in each namespace.

### Step 14: Set the task state and attach the PID

```c
/* kernel/fork.c:2450 — attach_pid and final wiring */
WRITE_ONCE(p->__state, TASK_NEW);
...
p->pid = pid_nr(pid);
p->tgid = (clone_flags & CLONE_THREAD) ? current->tgid : p->pid;
...
attach_pid(p, PIDTYPE_PID);
attach_pid(p, PIDTYPE_TGID);
```

`pid_nr()` returns the PID number in the initial (level-0) namespace. The task's
`pid` field is always the host-level PID number. `attach_pid()` hashes the `struct pid`
into the global PID hash tables and links the task into the `struct pid.tasks[]` list
(which is how `find_task_by_vpid()` works).

### Step 15: Add to the task list

```c
/* kernel/fork.c:2490 — write_lock_irq(&tasklist_lock) */
write_lock_irq(&tasklist_lock);
...
list_add_tail(&p->sibling, &p->real_parent->children);
...
write_unlock_irq(&tasklist_lock);
```

The final step takes the global `tasklist_lock` write lock and inserts the new task
into the parent's `children` list. After this point the task is visible to all kernel
subsystems. `copy_process()` returns the new `task_struct *` to `kernel_clone()`, which
calls `wake_up_new_task()` to place it on a run queue.

### Summary table

| Step | Function | Source |
|------|----------|--------|
| 1 | `security_task_create()` + flag checks | [`kernel/fork.c:2120`](https://elixir.bootlin.com/linux/v6.9/source/kernel/fork.c#L2120) |
| 2 | `dup_task_struct()` | [`kernel/fork.c:2195`](https://elixir.bootlin.com/linux/v6.9/source/kernel/fork.c#L2195) |
| 3 | `copy_creds()` | [`kernel/fork.c:2235`](https://elixir.bootlin.com/linux/v6.9/source/kernel/fork.c#L2235) |
| 4 | `sched_fork()` | [`kernel/fork.c:2266`](https://elixir.bootlin.com/linux/v6.9/source/kernel/fork.c#L2266) |
| 5 | `copy_sighand()` | [`kernel/fork.c:2290`](https://elixir.bootlin.com/linux/v6.9/source/kernel/fork.c#L2290) |
| 6 | `copy_signal()` | [`kernel/fork.c:2300`](https://elixir.bootlin.com/linux/v6.9/source/kernel/fork.c#L2300) |
| 7 | `copy_mm()` | [`kernel/fork.c:2320`](https://elixir.bootlin.com/linux/v6.9/source/kernel/fork.c#L2320) |
| 8 | `copy_namespaces()` | [`kernel/fork.c:2330`](https://elixir.bootlin.com/linux/v6.9/source/kernel/fork.c#L2330) |
| 9 | `copy_io()` | [`kernel/fork.c:2340`](https://elixir.bootlin.com/linux/v6.9/source/kernel/fork.c#L2340) |
| 10 | `copy_fs()` | [`kernel/fork.c:2345`](https://elixir.bootlin.com/linux/v6.9/source/kernel/fork.c#L2345) |
| 11 | `copy_files()` | [`kernel/fork.c:2352`](https://elixir.bootlin.com/linux/v6.9/source/kernel/fork.c#L2352) |
| 12 | `copy_thread()` | [`kernel/fork.c:2358`](https://elixir.bootlin.com/linux/v6.9/source/kernel/fork.c#L2358) |
| 13 | `alloc_pid()` | [`kernel/fork.c:2375`](https://elixir.bootlin.com/linux/v6.9/source/kernel/fork.c#L2375) |
| 14 | `attach_pid()` + state init | [`kernel/fork.c:2450`](https://elixir.bootlin.com/linux/v6.9/source/kernel/fork.c#L2450) |
| 15 | `write_lock_irq(&tasklist_lock)` + list insert | [`kernel/fork.c:2490`](https://elixir.bootlin.com/linux/v6.9/source/kernel/fork.c#L2490) |

---

## 4. `/proc/self/ns/` — Namespace File Descriptors

### 4.1 What these files are

Every process has a `/proc/self/ns/` directory containing one symlink per namespace
type. Each symlink points to a pseudo-file in the `nsfs` filesystem:

```bash
ls -la /proc/self/ns/
# lrwxrwxrwx  cgroup -> cgroup:[4026531835]
# lrwxrwxrwx  ipc    -> ipc:[4026531839]
# lrwxrwxrwx  mnt    -> mnt:[4026531841]
# lrwxrwxrwx  net    -> net:[4026531840]
# lrwxrwxrwx  pid    -> pid:[4026531836]
# lrwxrwxrwx  pid_for_children -> pid:[4026531836]
# lrwxrwxrwx  time   -> time:[4026531834]
# lrwxrwxrwx  time_for_children -> time:[4026531834]
# lrwxrwxrwx  user   -> user:[4026531837]
# lrwxrwxrwx  uts    -> uts:[4026531838]
```

The number in brackets (e.g., `4026531835`) is the inode number of the namespace file
in `nsfs`. This inode number:
1. Uniquely identifies the namespace instance across the whole system.
2. Is stable for the lifetime of the namespace (even if all processes leave it, as long
   as a bind mount keeps it alive).
3. Is what appears in `lsns` output.

Two processes sharing the same namespace have identical inode numbers for that namespace
type. This is how you verify that two containers share a network namespace.

### 4.2 `pid` vs `pid_for_children`

There are two PID namespace entries:
- `pid` — the PID namespace that this process's PID is in.
- `pid_for_children` — the PID namespace that new child processes will be born into.

These differ after a call to `unshare(CLONE_NEWPID)`. After `unshare()`, the calling
process's own PID does not change (it cannot — PIDs are permanent), but its children
will be born in the new namespace. `pid_for_children` reflects the new namespace
while `pid` still shows the original.

### 4.3 `setns()` — attaching to a namespace

`setns(2)` lets a thread join an existing namespace by passing a file descriptor to
one of the `/proc/<pid>/ns/<type>` files:

```c
/* Syscall signature */
int setns(int fd, int nstype);
```

- `fd`: an open file descriptor pointing to a namespace file (opened via
  `open("/proc/<pid>/ns/net", O_RDONLY)` or via `clone3()` with `CLONE_PIDFD`).
- `nstype`: the expected namespace type (e.g., `CLONE_NEWNET`), or 0 to accept any.

**Common use case:** `nsenter` is a userspace tool that calls `setns()` on each
namespace file descriptor of a target process to "enter" a container:

```bash
# Enter all namespaces of a container and run ps
CPID=$(docker inspect --format '{{.State.Pid}}' mycontainer)
nsenter --pid=/proc/$CPID/ns/pid \
        --net=/proc/$CPID/ns/net \
        --mount=/proc/$CPID/ns/mnt \
        -- ps aux
```

**Kernel implementation:** `setns()` calls `commit_nsset()` in
[`kernel/nsproxy.c:558`](https://elixir.bootlin.com/linux/v6.9/source/kernel/nsproxy.c#L558)
which replaces the current task's `nsproxy` with one pointing to the target namespace.

### 4.4 `unshare()` — detaching without forking

`unshare(2)` creates new namespaces for the calling process without creating a new
process:

```c
/* Syscall signature */
int unshare(int flags);
```

**Effect:** For each `CLONE_NEW*` flag passed, the kernel creates a new namespace and
updates `current->nsproxy` to point to it. The process continues running with its
original PID but now has private namespace(s).

```bash
# Run a new shell in a new network namespace without forking
unshare --net -- bash
# Inside: ip link    # shows only lo
# Outside: still sees all interfaces
```

**Difference from `clone3()`:** `unshare()` changes namespaces for the current
process. `clone3()` creates namespaces for a new child process. For `CLONE_NEWPID`,
`unshare()` only affects `pid_ns_for_children` — the calling process's own PID is
unchanged.

**Kernel implementation:** `unshare()` calls `unshare_nsproxy_namespaces()` in
[`kernel/fork.c:3418`](https://elixir.bootlin.com/linux/v6.9/source/kernel/fork.c#L3418).

### 4.5 Bind-mounting a namespace to keep it alive

A namespace normally dies when the last process in it exits. To keep a namespace alive
with no processes in it:

```bash
# Bind-mount the network namespace file descriptor
touch /run/netns/myns
mount --bind /proc/<pid>/ns/net /run/netns/myns
# Now the namespace persists even after the process exits
# Other processes can join it:
nsenter --net=/run/netns/myns -- ip link
# Clean up:
umount /run/netns/myns
rm /run/netns/myns
```

`ip netns add` does exactly this: it bind-mounts a network namespace to
`/run/netns/<name>`. Kubernetes uses similar bind mounts to hold the pause container's
namespaces alive for the pod's lifetime.

---

## 5. Observation

### 5.1 Trace `clone3()` calls with flag decoding

```bash
# Trace all clone3() syscalls, decode namespace flags
bpftrace -e '
tracepoint:syscalls:sys_enter_clone3 {
    $flags = *(uint64 *)args->uargs;
    printf("pid=%-6d clone3 flags=0x%x NEWPID=%d NEWNET=%d NEWNS=%d NEWUTS=%d\n",
        pid,
        $flags,
        ($flags & 0x20000000) != 0,
        ($flags & 0x40000000) != 0,
        ($flags & 0x00020000) != 0,
        ($flags & 0x04000000) != 0
    );
}'
```

### 5.2 Trace `copy_namespaces()` at the kernel level

```bash
# Trace entry to copy_namespaces on Linux 6.9
# arg0 = clone_flags (unsigned long), arg1 = new task_struct *
bpftrace -e 'kprobe:copy_namespaces {
    printf("copy_namespaces: caller=%s flags=0x%lx\n",
        comm, (uint64)arg0);
}'
```

### 5.3 Inspect namespace membership

```bash
# See all namespace file descriptors for the current process
ls -la /proc/self/ns/

# Find all processes in the same network namespace as PID 1234
NS=$(readlink /proc/1234/ns/net)
for p in /proc/[0-9]*/ns/net; do
    [ "$(readlink "$p" 2>/dev/null)" = "$NS" ] && echo "$p"
done

# Use lsns to get a readable view of all namespaces on the system
lsns

# Show the PID namespace tree
lsns -t pid
```

### 5.4 Observe `kernel_clone` for all new process creation

```bash
# Note: kernel_clone replaced do_fork/kernel_thread in Linux 5.10
# On Linux 6.9, all process/thread creation goes through kernel_clone
bpftrace -e 'kprobe:kernel_clone {
    $args = (struct kernel_clone_args *)arg0;  /* kernel_clone: arg0 = struct kernel_clone_args * */
    printf("kernel_clone: caller=%s flags=0x%llx exit_signal=%llu\n",
        comm,
        $args->flags,
        $args->exit_signal);
}'
```

---

## Summary

`clone3()` with `struct clone_args` is the modern interface for creating containers.
Every `CLONE_NEW*` flag instructs `copy_process()` → `copy_namespaces()` to allocate a
fresh kernel data structure for the corresponding subsystem. The child process starts
with a private copy of that subsystem's state and all subsequent changes stay isolated
within the container.

The eight container-relevant namespace flags:

| Flag | Creates |
|------|---------|
| `CLONE_NEWPID` | A new PID number space; child is PID 1 |
| `CLONE_NEWNET` | A new network stack with only loopback |
| `CLONE_NEWNS` | A private mount tree copy |
| `CLONE_NEWUTS` | A private hostname/domainname |
| `CLONE_NEWIPC` | A private SysV IPC and POSIX MQ space |
| `CLONE_NEWUSER` | A private UID/GID mapping; enables rootless containers |
| `CLONE_NEWCGROUP` | A virtualized view of the cgroup hierarchy |
| `CLONE_NEWTIME` | Private monotonic/boottime clock offsets |

**Next:** [kernel/01-c-pid-namespaces.md](01-c-pid-namespaces.md) — `struct
pid_namespace`, `struct pid`, `struct upid`, and the PID 1 container init problem.
