# 01-c — PID Namespaces: `struct pid_namespace`, `struct pid`, and the Container Init Problem

## Source Files

| File | Link |
|------|------|
| `include/linux/pid_namespace.h` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/pid_namespace.h |
| `include/linux/pid.h` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/pid.h |
| `kernel/pid_namespace.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/pid_namespace.c |
| `kernel/pid.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/pid.c |

## Two Processes, Two Different PIDs, One Kernel

Before PID namespaces existed, a process had exactly one PID — its position in the global process table. This was fine for a system running one set of workloads. It was a problem for running multiple isolated workloads on the same machine. If you wanted to run a monitoring agent inside a container that always appeared as PID 1, or if you wanted the processes inside a container to have small, predictable PID numbers rather than whatever large integers the host's process table happened to assign, you had no mechanism.

PID namespaces, added in Linux 3.8 (2013), solve this by making PID numbers *scoped*. A process can now have a different PID number in different namespaces simultaneously. From the host, it might be PID 34521. From inside its container's PID namespace, it is PID 7. From inside a nested container (if such a thing exists), it might be PID 2. The same physical process, the same `task_struct`, three different integer identifiers depending on which namespace you look from.

This requires a more complex data structure than a simple integer in `task_struct`. The kernel's solution is `struct pid`: an object that holds an array of `(PID number, namespace)` pairs — one per namespace level in the nesting hierarchy. When you ask "what is this process's PID?", you have to specify which namespace you want the answer in. When you kill a process by PID number, you kill it in the namespace that your process belongs to.

The correctness problem that trips up most container authors is what happens at the bottom of the hierarchy: PID 1. In Unix, PID 1 is init — the root of the process tree, the reaper of orphaned processes, the only process that cannot be killed by SIGKILL unless the sender is from an ancestor namespace. Every PID namespace has its own PID 1. If that process exits without a proper init implementation, the entire namespace and all processes in it are torn down. This is why the pause container exists, why tini was invented, and why Kubernetes's default pod structure puts an init process in every sandbox.

This document dissects the three data structures that implement this illusion — `struct pid_namespace`, `struct pid`, and `struct upid` — and explains the one correctness problem that kills more containers than any other: the PID 1 init problem.

---

## 1. `struct pid_namespace` — the Namespace Descriptor

### 1.1 Source location and full definition

**Source:** [`include/linux/pid_namespace.h:10`](https://elixir.bootlin.com/linux/v6.9/source/include/linux/pid_namespace.h#L10)

```c
/* include/linux/pid_namespace.h, lines 10–74 (Linux 6.9) */
struct pid_namespace {
    struct idr           idr;           /* PID allocator: maps PID numbers → struct pid */
    struct rcu_head      rcu;           /* RCU callback for deferred freeing */
    unsigned int         pid_allocated; /* number of PIDs allocated in this namespace */
    struct task_struct  *child_reaper;  /* PID 1 of this namespace (container init) */
    struct kmem_cache   *pid_cachep;    /* slab cache for struct pid in this namespace */
    unsigned int         level;         /* nesting depth: 0 = initial ns, 1 = child, ... */
    struct pid_namespace *parent;       /* parent pid namespace (NULL for init_pid_ns) */
#ifdef CONFIG_BSD_PROCESS_ACCT
    struct fs_pin       *bacct;         /* BSD accounting file */
#endif
    struct user_namespace *user_ns;     /* user namespace that owns this pid namespace */
    struct ucounts      *ucounts;       /* per-user resource accounting (RLIMIT_NPROC) */
    spinlock_t           pid_lock;      /* protects the IDR allocator; held by alloc_pid() during idr_alloc_cyclic() */
    int                  reboot;        /* group exit code, set by sys_reboot(RESTART2) */
    struct ns_common     ns;            /* generic namespace data (inode, ops) */
} __randomize_layout;
```

### 1.2 Field-by-field explanation

#### `struct idr idr` — the PID allocator

**Source:** [`include/linux/idr.h`](https://elixir.bootlin.com/linux/v6.9/source/include/linux/idr.h)

The IDR (ID Radix tree) is the kernel's integer-to-pointer mapping data structure.
Inside a PID namespace, the IDR maps PID numbers (integers) to `struct pid *` pointers.

- **Allocation:** `alloc_pid()` calls `idr_alloc_cyclic()` to find the next available
  integer in the range `[RESERVED_PIDS, pid_max)`. On each allocation the IDR advances
  its search position cyclically to avoid reusing recent PID numbers.
- **Lookup:** `find_pid_ns(nr, ns)` calls `idr_find(&ns->idr, nr)` to return the
  `struct pid *` for PID number `nr` in namespace `ns`.
- **Release:** `free_pid()` calls `idr_remove(&ns->idr, pid->numbers[ns->level].nr)`.
- **Size:** The IDR grows on demand. Each namespace starts with a single IDR node
  capable of holding 64 PID entries. Most containers have far fewer than 64 processes,
  so this is the only allocation.

**Protected by:** `pid_namespace.pid_lock` — a per-namespace spinlock that serialises
concurrent PID allocations. This is a key scalability improvement over the older
approach that used a global `pidmap_lock`.

#### `spinlock_t pid_lock` — IDR allocator lock

Protects the IDR allocator (`idr` field) against concurrent access. `alloc_pid()`
acquires `pid_lock` via `spin_lock_irq(&tmp->pid_lock)` before calling
`idr_alloc_cyclic()`, and releases it immediately after. This per-namespace lock
allows processes in different PID namespaces to allocate PIDs concurrently without
contending on a single global lock.

#### `struct task_struct *child_reaper` — the namespace's PID 1

`child_reaper` is the most important field in the struct. It points to the task that
acts as PID 1 for this namespace. When any process in the namespace becomes an orphan
(its parent exits before it), the orphan is reparented to `child_reaper`.

When `child_reaper` itself exits, the kernel calls `zap_pid_ns_processes()` (see
Section 3).

For the initial (host) PID namespace, `child_reaper` is the `init` process (systemd
or sysvinit). For a container's PID namespace, `child_reaper` is the container init
process — whatever was the first process started in the namespace.

`child_reaper` is set in `copy_process()` when a process is created with
`CLONE_NEWPID`: at that point the kernel sets
`new_pid_ns->child_reaper = NULL` (because the new namespace is empty). The first
process to run in the namespace sets itself as `child_reaper` in `attach_pid()`.

Specifically, in `copy_process()` at
[`kernel/fork.c:2460`](https://elixir.bootlin.com/linux/v6.9/source/kernel/fork.c#L2460):

```c
/* kernel/fork.c — simplified (Linux 6.9) */
if (is_child_reaper(pid)) {
    ns_of_pid(pid)->child_reaper = p;
    p->signal->flags |= SIGNAL_UNKILLABLE;
}
```

The `SIGNAL_UNKILLABLE` flag is critical: it makes PID 1 inside a namespace immune to
`SIGKILL` sent from within the namespace. A container process cannot kill its own init.
(Signals from the host, from outside the namespace, do bypass this protection.)

#### `struct kmem_cache *pid_cachep` — the slab cache

Each PID namespace has its own slab cache for `struct pid` objects. This is because
`struct pid` has a variable-length tail array (`numbers[]`) whose size depends on the
namespace nesting depth (`level + 1` elements). A process in a namespace at level 2
needs a `struct pid` with 3 `upid` entries, while one at level 0 needs only 1.

By giving each namespace its own cache, the kernel avoids wasted memory from over-
allocating the `numbers[]` array. When a namespace is destroyed, its entire
`pid_cachep` slab cache is destroyed with it, freeing all associated `struct pid`
objects efficiently.

**Source:** `create_pid_cachep()` at
[`kernel/pid_namespace.c:42`](https://elixir.bootlin.com/linux/v6.9/source/kernel/pid_namespace.c#L42):

```c
/* kernel/pid_namespace.c, line 42 (Linux 6.9) */
static struct kmem_cache *create_pid_cachep(unsigned int level)
{
    /* struct pid + level+1 upid entries */
    unsigned int len = sizeof(struct pid) +
                       (level + 1) * sizeof(struct upid);
    ...
    return kmem_cache_create_usercopy("pid", len, ...);
}
```

#### `unsigned int level` — namespace nesting depth

- `level == 0`: the initial (host) PID namespace. All processes on the host exist here.
- `level == 1`: a first-level container's PID namespace (e.g., a standard Docker container).
- `level == 2`: a container-inside-a-container.
- Maximum: `MAX_PID_NS_LEVEL == 32`. The kernel refuses to create a PID namespace
  deeper than 32 levels. This prevents stack overflows when the kernel recursively
  operates on namespace hierarchies.

**Source:** [`include/linux/pid_namespace.h:7`](https://elixir.bootlin.com/linux/v6.9/source/include/linux/pid_namespace.h#L7):

```c
/* include/linux/pid_namespace.h, line 7 (Linux 6.9) */
#define MAX_PID_NS_LEVEL 32
```

#### `struct pid_namespace *parent` — the parent namespace

Points to the namespace one level up. For `init_pid_ns`, this is `NULL`. The parent
chain forms a tree: every nested container's namespace has a parent pointer back to
the enclosing namespace (ultimately back to the host namespace).

This parent chain is walked when translating PIDs across namespace levels. To find a
process's PID as seen from two levels up, the kernel walks `ns->parent->parent` and
looks up the `struct upid` entry for that level.

#### `struct ns_common ns` — generic namespace fields

**Source:** [`include/linux/nsproxy.h:24`](https://elixir.bootlin.com/linux/v6.9/source/include/linux/nsproxy.h#L24)

```c
/* include/linux/nsproxy.h, lines 24–28 (Linux 6.9) */
struct ns_common {
    struct dentry    *stashed;  /* cached dentry for /proc/*/ns/* files */
    const struct proc_ns_operations *ops;  /* namespace-type-specific operations */
    unsigned int      inum;     /* inode number — the namespace's unique ID */
    refcount_t        count;    /* reference count */
};
```

- `inum` — the inode number that appears in `/proc/self/ns/pid`. This is the value
  you see as `pid:[4026531836]` in `ls -la /proc/self/ns/`. Two processes in the same
  PID namespace have the same `inum`. `lsns` uses this to group processes.
- `count` — reference count incremented by each process in the namespace, each open
  file descriptor on the namespace, and each bind mount. The namespace is freed when
  `count` drops to zero.
- `ops` — function pointer table for namespace operations including `get()`,
  `put()`, `install()`, and `get_parent()`. For PID namespaces this is `pidns_operations`
  defined at
  [`kernel/pid_namespace.c:430`](https://elixir.bootlin.com/linux/v6.9/source/kernel/pid_namespace.c#L430).

### 1.3 The initial PID namespace

**Source:** [`include/linux/pid_namespace.h:78`](https://elixir.bootlin.com/linux/v6.9/source/include/linux/pid_namespace.h#L78)

```c
/* include/linux/pid_namespace.h, line 78 */
extern struct pid_namespace init_pid_ns;
```

`init_pid_ns` is the statically allocated PID namespace for the system. All processes
on the host are visible in `init_pid_ns`. Every other PID namespace is a descendant.

### 1.4 Lifecycle

```
create_pid_namespace()          ← kernel/pid_namespace.c:121
  │  kmalloc(pid_namespace)
  │  idr_init(&ns->idr)
  │  create_pid_cachep(level)
  │  ns->level = parent->level + 1
  │  ns->parent = parent
  │  ns->child_reaper = NULL     ← first process in ns sets this
  └► ns_common.inum = new inode number

first process forks into ns:
  │  copy_process() → alloc_pid()
  └► is_child_reaper(pid) == true
       ns->child_reaper = p
       SIGNAL_UNKILLABLE set

namespace runs (processes come and go)

last process exits:
  │  do_exit() → exit_notify()
  └► zap_pid_ns_processes(ns)    ← kills all remaining processes in ns
       pidns_put() eventually called
         │ idr_destroy(&ns->idr)
         │ kmem_cache_destroy(pid_cachep)
         └► kfree(ns)
```

---

## 2. `struct pid` and `struct upid` — the PID Object

### 2.1 Source location and full definition

**Source:** [`include/linux/pid.h:57`](https://elixir.bootlin.com/linux/v6.9/source/include/linux/pid.h#L57)

```c
/* include/linux/pid.h, lines 57–88 (Linux 6.9) */
struct pid
{
    refcount_t          count;         /* reference count */
    unsigned int        level;         /* namespace nesting depth for this pid */
    spinlock_t          lock;          /* protects tasks[] and wait_pidfd */
    /* lists of tasks that use this pid */
    struct hlist_head   tasks[PIDTYPE_MAX];
    /* wait queue for pidfd poll */
    struct hlist_head   inodes;
    wait_queue_head_t   wait_pidfd;
    struct rcu_head     rcu;           /* deferred free under RCU */
    struct upid         numbers[];    /* flexible array: one upid per namespace level */
};
```

And `struct upid`:

```c
/* include/linux/pid.h, lines 50–54 (Linux 6.9) */
struct upid {
    int                  nr;   /* the PID number in this namespace */
    struct pid_namespace *ns;  /* pointer to the namespace where nr is valid */
};
```

### 2.2 Field-by-field explanation

#### `refcount_t count` — the reference count

Every object that needs to hold a reference to a PID (a task using the PID, an open
`/proc/<pid>/` directory, a pidfd file descriptor, etc.) increments `count` via
`get_pid()`. When the count drops to zero, `free_pid()` is called which removes the
PID from all IDR tables and returns the memory to the `pid_cachep` slab.

A task's `task_struct.thread_pid` holds a reference to the PID. When the task exits,
`do_exit()` eventually calls `put_pid()` which decrements the count. The PID survives
until all `/proc/<pid>/` file descriptors are closed.

#### `unsigned int level` — namespace depth

The `level` field records how many namespace levels this PID exists in. A process in
a doubly-nested container has `level == 2`, meaning its `numbers[]` array has 3
entries (indices 0, 1, 2 for the three levels from host down to the container).

This matches the `pid_namespace.level` of the deepest namespace the process belongs to.

#### `struct hlist_head tasks[PIDTYPE_MAX]` — reverse mapping

`PIDTYPE_MAX` is 4 (defined at
[`include/linux/pid.h:8`](https://elixir.bootlin.com/linux/v6.9/source/include/linux/pid.h#L8)):

```c
/* include/linux/pid.h, lines 8–16 (Linux 6.9) */
enum pid_type
{
    PIDTYPE_PID,    /* 0: thread/process PID */
    PIDTYPE_TGID,   /* 1: thread group ID */
    PIDTYPE_PGID,   /* 2: process group ID */
    PIDTYPE_SID,    /* 3: session ID */
    PIDTYPE_MAX,    /* 4: sentinel */
};
```

`tasks[PIDTYPE_PID]` is a hash list of `task_struct` objects that use this `struct pid`
as their thread ID. In most cases this list has exactly one entry. The `tasks[]` array
is the kernel's mechanism for going from a `struct pid` pointer back to a
`task_struct`.

```c
/* Kernel code to find the task for a given struct pid */
struct task_struct *pid_task(struct pid *pid, enum pid_type type)
{
    struct task_struct *result = NULL;
    if (pid) {
        struct hlist_node *first;
        first = rcu_dereference_check(hlist_first_rcu(&pid->tasks[type]), ...);
        if (first)
            result = hlist_entry(first, struct task_struct, pid_links[type]);
    }
    return result;
}
```

#### `struct hlist_head inodes` — inode back-references

`inodes` is a hash list linking all inode objects (in `/proc` and `nsfs`) that
reference this PID. It is used by `proc_flush_pid()` to invalidate `/proc` entries
when the process exits: the kernel walks `pid->inodes` and removes the associated
dentries from the dcache, ensuring that stale `/proc/<pid>/` entries do not linger
after the process is gone.

#### `wait_queue_head_t wait_pidfd` — pidfd polling

When a process opens a `pidfd` (a file descriptor referring to a process, introduced
in Linux 5.3), it can `poll()` on the fd to wait for the process to exit. The
`wait_pidfd` wait queue is where those `poll()` callers sleep. When the process exits,
`do_exit()` wakes this queue.

#### `struct upid numbers[]` — the per-namespace PID numbers

This is the crucial flexible array at the end of the struct. It has `level + 1`
entries. Each entry is a `struct upid` giving the PID number in one namespace level:

```c
struct upid {
    int                  nr;   /* PID number in namespace ns */
    struct pid_namespace *ns;  /* the namespace where nr is valid */
};
```

For a process with host PID 47382 inside a container (PID namespace at level 1) where
it is PID 1:

```
numbers[0] = { .nr = 47382, .ns = &init_pid_ns      }  /* host namespace */
numbers[1] = { .nr = 1,     .ns = &container_pid_ns }  /* container namespace */
```

`numbers[0]` is always the entry for the initial (host) namespace. `numbers[level]`
is the entry for the deepest namespace the process belongs to.

### 2.3 How `getpid()` works across namespace levels

When a process inside a container calls `getpid()`:

1. `sys_getpid()` returns `task_tgid_vnr(current)`.
2. `task_tgid_vnr()` calls `pid_vnr(task_tgid(current))`.
3. `pid_vnr()` calls `pid_nr_ns(pid, task_active_pid_ns(current))`.
4. `task_active_pid_ns(current)` returns `current->nsproxy->pid_ns_for_children->parent`
   — the deepest namespace the process is a member of.
5. `pid_nr_ns()` scans `pid->numbers[level].ns` to find the entry matching the target
   namespace and returns `pid->numbers[level].nr`.

For the container process above, step 5 returns `numbers[1].nr == 1`. The container
process calls `getpid()` and gets `1`.

From the host, reading `/proc/47382/status` returns:

```
NSpid:   47382  1
```

The kernel walks the namespace hierarchy from `init_pid_ns` down: at level 0 the PID
is 47382; at level 1 it is 1.

### 2.4 PID allocation: `alloc_pid()`

**Source:** [`kernel/pid.c:180`](https://elixir.bootlin.com/linux/v6.9/source/kernel/pid.c#L180)

```c
/* kernel/pid.c, line 180 (Linux 6.9) — simplified */
struct pid *alloc_pid(struct pid_namespace *ns,
                      pid_t *set_tid, size_t set_tid_size)
{
    struct pid *pid;
    enum pid_type type;
    int i, nr;
    struct pid_namespace *tmp;

    pid = kmem_cache_alloc(ns->pid_cachep, GFP_KERNEL);
    ...
    pid->level = ns->level;

    for (i = ns->level; i >= 0; i--) {
        int pid_min = 1;

        tmp = ns;
        while (tmp->level > i)
            tmp = tmp->parent;

        if (set_tid_size) {
            /* requested PID from set_tid array */
            nr = set_tid[i];
        }

        idr_preload(GFP_KERNEL);
        spin_lock_irq(&tmp->pid_lock);
        nr = idr_alloc_cyclic(&tmp->idr, NULL, pid_min, pid_max, GFP_ATOMIC);
        spin_unlock_irq(&tmp->pid_lock);
        idr_preload_end();

        pid->numbers[i].nr = nr;
        pid->numbers[i].ns = tmp;
    }

    ...
    return pid;
}
```

The outer loop runs from `ns->level` (deepest namespace) down to 0 (host namespace),
allocating a PID number in each namespace and recording it in `numbers[i]`. Each
allocation is serialised by the per-namespace `pid_lock` spinlock.

### 2.5 Lifecycle

```
alloc_pid(ns, set_tid, set_tid_size)    ← kernel/pid.c:180
  │  kmem_cache_alloc(ns->pid_cachep)   — allocates struct pid sized for level+1 upids
  │  for i = ns->level down to 0:
  │      spin_lock_irq(&tmp->pid_lock)
  │      idr_alloc_cyclic(&tmp->idr, ...)  — reserves PID number in each namespace level
  │      spin_unlock_irq(&tmp->pid_lock)
  │      pid->numbers[i] = { .nr = nr, .ns = tmp }
  └►  refcount_set(&pid->count, 1)      — initial reference held by copy_process()

process runs (task_struct.thread_pid holds a reference via get_pid())

put_pid(pid)                            ← called by do_exit() → release_task()
  │  if refcount_dec_and_test(&pid->count):
  │      free_pid(pid)
  │        for i = 0..pid->level:
  │            idr_remove(&ns->idr, pid->numbers[i].nr)
  └►      kmem_cache_free(ns->pid_cachep, pid)
```

`alloc_pid()` allocates a `struct pid` from the per-namespace slab cache and fills
the `numbers[]` array with one IDR-allocated integer per namespace level. `free_pid()`
is reached via `put_pid()` when the reference count hits zero — this removes the PID
from all IDR tables and returns memory to the slab. Open `/proc/<pid>/` directories
and pidfd file descriptors each hold their own reference, so the `struct pid` may
outlive the `task_struct` briefly.

---

## 3. PID 1 — the Container Init Problem

### 3.1 `child_reaper` and orphan reparenting

In the Linux kernel, when a process exits before its children, those children become
orphans. The kernel reparents them to the `child_reaper` of their PID namespace. For
a container, this means all orphaned container processes are reparented to the
container's PID 1 — not to host PID 1.

This reparenting happens in `forget_original_parent()` at
[`kernel/exit.c:629`](https://elixir.bootlin.com/linux/v6.9/source/kernel/exit.c#L629):

```c
/* kernel/exit.c — simplified (Linux 6.9) */
static struct task_struct *find_new_reaper(struct task_struct *father,
                                           struct task_struct *child_reaper)
{
    ...
    /* Walk up through thread group looking for a live thread */
    ...
    /* Fall back to child_reaper (PID 1 of the namespace) */
    return child_reaper;
}
```

The reparenting logic walks the process's thread group looking for a live thread to
adopt the orphans. If none is found, it falls back to `pid_namespace->child_reaper`.

### 3.2 If container PID 1 exits — `zap_pid_ns_processes()`

When `child_reaper` exits (container PID 1 exits), the kernel cannot reparent its
children to any other process in the namespace (there is no higher-level process
within the namespace). Instead, it kills the entire namespace.

This is implemented in `zap_pid_ns_processes()` at
[`kernel/pid_namespace.c:183`](https://elixir.bootlin.com/linux/v6.9/source/kernel/pid_namespace.c#L183):

```c
/* kernel/pid_namespace.c, line 183 (Linux 6.9) — simplified */
void zap_pid_ns_processes(struct pid_namespace *pid_ns)
{
    int nr;
    int rc;
    struct task_struct *task, *me = current;
    int init_pids = thread_group_leader(me) ? 1 : 2;
    struct pid *pid;

    /* Don't allow any more processes into the namespace */
    pid_ns->pid_allocated = PIDNS_ADDING_INIT;

    /* Send SIGKILL to every process in the namespace except init */
    rcu_read_lock();
    for (nr = 2; nr < pid_max; nr++) {
        pid = idr_find(&pid_ns->idr, nr);
        if (!pid)
            continue;
        task = pid_task(pid, PIDTYPE_PID);
        if (task && !__fatal_signal_pending(task))
            group_send_sig_info(SIGKILL, SEND_SIG_PRIV, task, PIDTYPE_TGID);
    }
    rcu_read_unlock();

    /* wait for all of them to die */
    do {
        clear_thread_flag(TIF_SIGPENDING);
        rc = sys_wait4(-1, NULL, __WALL, NULL);
    } while (rc != -ECHILD);
    ...
}
```

The sequence is:
1. Mark the namespace as "draining" (`pid_allocated = PIDNS_ADDING_INIT`).
2. Walk the IDR and send `SIGKILL` to every process in the namespace except PID 1.
3. Wait for all processes to die using `wait4()`.
4. Return to `do_exit()` which frees the namespace.

**Why this matters:** If you run a container without a proper init process and that
process exits (even with exit code 0), the entire container is killed. There is no
way to keep the container alive after PID 1 exits.

### 3.3 The zombie accumulation problem

A correct PID 1 must call `waitpid()` for all child processes — including processes it
did not directly create (adopted orphans). If PID 1 does not call `waitpid()`, exited
processes remain in `EXIT_ZOMBIE` state. Their `task_struct` and associated resources
are not freed. In a container with a high process churn rate (e.g., a web server that
forks a process per request), zombie accumulation can exhaust the PID namespace's
`pid_max` limit and cause `EAGAIN` on all subsequent `fork()` calls.

The zombie problem affects:

- **Shell scripts as PID 1:** A shell script does not call `waitpid()` for background
  processes it spawns. If the script launches a background process which then orphans
  its children, those children are adopted by PID 1 (the shell), but the shell will
  not call `waitpid()` for them.

- **Simple binaries as PID 1:** A single-binary application (e.g., `nginx`, `node`,
  `python app.py`) is written to be a normal process, not an init daemon. It handles
  `SIGCHLD` by reaping its own children, but not for orphaned processes adopted from
  elsewhere in the container.

- **`CMD ["sleep", "infinity"]` in Docker:** The `sleep` binary has no signal handler
  and never calls `waitpid()`. If other processes run in the same container and
  exit, they become permanent zombies.

### 3.4 `SIGTERM` forwarding

Beyond zombie reaping, a correct container init must also forward signals to its
children. When `docker stop` sends `SIGTERM` to PID 1, the container runtime expects
PID 1 to forward `SIGTERM` to all its children and wait for them to exit gracefully
before exiting itself.

The `SIGNAL_UNKILLABLE` flag means `SIGKILL` from within the namespace is ignored
by PID 1. But `docker stop` sends from outside the namespace (via `kill(host_pid,
SIGTERM)`) which bypasses `SIGNAL_UNKILLABLE`. If PID 1 does not handle `SIGTERM`
gracefully, Docker sends `SIGKILL` after a timeout (default 10 seconds).

**tini** and **dumb-init** exist specifically to solve this:

#### tini

`tini` (tiny init) is a minimal init binary (around 500 lines of C) designed to be
the container's PID 1. It:
1. Registers `SIGCHLD` and `SIGTERM` handlers.
2. Forks and execs the specified command as a child.
3. Enters an infinite `waitpid(-1, ...)` loop, reaping any zombie that appears.
4. On `SIGTERM`, forwards the signal to its child process group.
5. When its direct child exits, exits with the same exit code.

Docker ships a bundled `tini` and enables it with `--init`:

```bash
docker run --init myimage mycommand
# Equivalent to:
# PID 1 = tini
# PID 2 = mycommand (child of tini)
```

In Kubernetes pod specs, you configure tini (or equivalent) via the container image's
`ENTRYPOINT`.

#### dumb-init

`dumb-init` (Yelp's implementation) behaves similarly to tini but has one additional
behavior: it runs the child in a new process group (`setsid()`) and forwards signals
to the entire process group, not just the direct child. This is useful for containers
that spawn child processes (e.g., `nginx` forks workers).

### 3.5 Kubernetes `shareProcessNamespace`

**Source:** [Kubernetes API reference — `v1.PodSpec`](https://kubernetes.io/docs/reference/kubernetes-api/workload-resources/pod-v1/#PodSpec)

By default, each container in a Kubernetes pod has its own PID namespace. Processes
in container A cannot see processes in container B.

Setting `shareProcessNamespace: true` in the pod spec makes all containers in the
pod share a single PID namespace. In this mode:
- All processes in the pod are visible to each other via `ps`.
- The pod's pause container (the "sandbox" container) holds the shared namespace and
  acts as PID 1 for all containers.
- Each container's main process gets a PID > 1 in the shared namespace.

```yaml
# kubernetes pod spec — shareProcessNamespace example
apiVersion: v1
kind: Pod
metadata:
  name: shared-pid-example
spec:
  shareProcessNamespace: true         # all containers share one PID namespace
  containers:
  - name: nginx
    image: nginx:1.25
  - name: debug
    image: busybox
    command: ["sh", "-c", "ps aux && sleep 3600"]
    # This container can see nginx's processes because they share a PID namespace
```

**Kernel mechanics:** When `shareProcessNamespace: true` is set, the kubelet configures
runc to use the pause container's PID namespace for all containers in the pod. This is
done by passing the pause container's `/proc/<pid>/ns/pid` file descriptor to
`clone3()` without `CLONE_NEWPID`, or by calling `setns()` on the namespace fd before
forking the container processes.

The pause container (`registry.k8s.io/pause:3.x`) is a minimal binary that:
1. Calls `prctl(PR_SET_CHILD_SUBREAPER, 1)` — this makes it the reaper for all processes
   in its subtree even without being PID 1. In `shareProcessNamespace` mode, the pause
   container effectively acts as the init for the shared namespace.
2. Enters an infinite sleep loop, doing nothing but keeping the namespace alive and
   reaping zombies.

`PR_SET_CHILD_SUBREAPER` (at
[`kernel/sys.c`](https://elixir.bootlin.com/linux/v6.9/source/kernel/sys.c)) sets the
`task_struct.is_child_subreaper` flag. Orphaned processes in the subtree are reparented
to this task rather than to namespace PID 1.

### 3.6 The `PR_SET_CHILD_SUBREAPER` alternative

For containers where you cannot change PID 1 to tini/dumb-init (e.g., legacy images),
`prctl(PR_SET_CHILD_SUBREAPER, 1)` is a modern alternative. When set, the calling
process becomes the reaper for all processes in its descendant subtree — similar to
being PID 1 but without needing to actually be PID 1.

Many modern container runtimes use this internally. The Go standard library's
`os/exec.Cmd` does not call `prctl` automatically; you must call it yourself if
writing a process-managing daemon.

---

## 4. Observation

### 4.1 Inspect the PID namespace hierarchy

```bash
# See the PID namespace of the current process
ls -la /proc/self/ns/pid
# lrwxrwxrwx  pid -> pid:[4026531836]

# The inode number 4026531836 identifies init_pid_ns on most systems

# List all PID namespaces on the system with their processes
lsns -t pid
# NS TYPE   NPROCS   PID USER    COMMAND
# 4026531836 pid      143     1 root    /sbin/init
# 4026532512 pid        3  8192 1000    /pause
# 4026532513 pid        2  8194 1000    nginx: master process

# Show all processes and their PID namespace memberships
ps axo pid,pidns,comm
```

### 4.2 Find the host PID of a container's PID 1

```bash
# Given a container ID from docker ps or crictl ps:
CONTAINER_ID="abc123def456"

# Method 1: docker/containerd inspect
CPID=$(docker inspect --format '{{.State.Pid}}' $CONTAINER_ID)
echo "Host PID of container PID 1: $CPID"

# Method 2: crictl (for Kubernetes pods)
CPID=$(crictl inspect --output go-template \
    --template '{{.info.pid}}' $CONTAINER_ID)

# Verify by checking NSpid
cat /proc/$CPID/status | grep NSpid
# NSpid:  47382  1      ← host PID is 47382; container PID is 1
```

### 4.3 Enter a container's PID namespace

```bash
# Enter the PID namespace only (see container processes, but path is still host path)
CPID=$(docker inspect --format '{{.State.Pid}}' mycontainer)
nsenter --pid=/proc/$CPID/ns/pid -- ps aux

# Enter PID + mount namespace (see container filesystem and processes)
nsenter --pid=/proc/$CPID/ns/pid \
        --mount=/proc/$CPID/ns/mnt \
        -- ps aux
```

### 4.4 Watch namespace creation and PID allocation with bpftrace

```bash
# Trace new PID namespace creation
# On Linux 6.9: create_pid_namespace() is the allocation function
bpftrace -e 'kprobe:create_pid_namespace {
    printf("new pid_ns: caller=%s caller_pid=%d\n",
        comm, curtask->pid);
}'

# Trace alloc_pid() calls — see each new PID being allocated
# arg0 = struct pid_namespace *, arg1 = pid_t *, arg2 = size_t
bpftrace -e 'kprobe:alloc_pid {
    $ns = (struct pid_namespace *)arg0;
    printf("alloc_pid: ns_level=%u caller=%s\n",
        $ns->level, comm);
}'

# Trace PID 1 assignment in a new namespace
# When is_child_reaper() returns true, the task becomes PID 1
# Linux 6.9 copy_process signature:
#   static struct task_struct *copy_process(struct pid *pid, int trace,
#                                           int node, struct kernel_clone_args *args)
# So struct kernel_clone_args * is arg3, not arg0.
bpftrace -e 'kprobe:copy_process {
    $args = (struct kernel_clone_args *)arg3;
    if ($args->flags & 0x20000000) {  /* CLONE_NEWPID */
        printf("CLONE_NEWPID: caller=%s pid=%d creating new pid namespace\n",
            comm, curtask->pid);
    }
}'
```

### 4.5 Observe zombie accumulation without a proper init

```bash
# Run a container WITHOUT tini and watch zombie accumulation
# (educational example — demonstrates the problem)
docker run --rm ubuntu:22.04 bash -c '
    # Fork 10 background processes that exit immediately
    for i in $(seq 1 10); do
        (sleep 0.1) &
    done
    # bash (PID 1) does not reap background children automatically
    sleep 2
    # Check for zombies
    ps aux | grep Z
'
# You will see Z (zombie) entries

# Contrast with --init flag (tini as PID 1)
docker run --init --rm ubuntu:22.04 bash -c '
    for i in $(seq 1 10); do
        (sleep 0.1) &
    done
    sleep 2
    ps aux | grep Z
'
# No zombies — tini reaps them immediately
```

### 4.6 Inspect struct pid from a running kernel with crash

```bash
# Use crash or /proc/kcore to inspect a struct pid (requires root + debug symbols)
# Find the pid struct address for a given PID
crash /usr/lib/debug/boot/vmlinux-$(uname -r) /proc/kcore

# In the crash shell:
crash> pid 1234           # find task by PID
crash> struct pid <addr>  # print the struct pid at address
# Output shows count, level, numbers[] array
```

---

## 5. Object Graph: PID Namespace Hierarchy

```
Host (init_pid_ns, level=0)
│
│  struct pid_namespace init_pid_ns
│    .idr           = {PID 1→init, PID 2→kthreadd, ..., PID 47382→container-proc}
│    .child_reaper  = &task_struct(init/systemd, host PID 1)
│    .level         = 0
│    .parent        = NULL
│
├── Container A (pid_ns_A, level=1)
│   │
│   │  struct pid_namespace pid_ns_A
│   │    .idr           = {PID 1→nginx, PID 2→nginx-worker, PID 3→...}
│   │    .child_reaper  = &task_struct(nginx, host PID 47382, container PID 1)
│   │    .level         = 1
│   │    .parent        = &init_pid_ns
│   │
│   │  struct pid (for nginx process)
│   │    .count     = 2  (task ref + /proc open)
│   │    .level     = 1
│   │    .numbers[0] = { .nr=47382, .ns=&init_pid_ns }   ← host PID
│   │    .numbers[1] = { .nr=1,     .ns=&pid_ns_A    }   ← container PID
│   │
│   └── (nested container B would be level=2, parent=pid_ns_A)
│
└── Container C (pid_ns_C, level=1)
    │
    │  struct pid_namespace pid_ns_C
    │    .idr           = {PID 1→java, PID 2→java-worker1, ...}
    │    .child_reaper  = &task_struct(java, host PID 51000, container PID 1)
    │    .level         = 1
    │    .parent        = &init_pid_ns
```

A process's PID is different at every namespace level it belongs to. The translation
table lives in `struct pid.numbers[]`. The kernel performs this translation on every
call to `getpid()`, `kill()`, `wait()`, and every `/proc` read that returns a PID.

---

## Summary

The PID namespace system involves three cooperating structures:

1. **`struct pid_namespace`** — the namespace itself. Contains the IDR allocator,
   the `child_reaper` pointer (PID 1), the nesting `level`, and the `ns.inum` inode
   number visible in `/proc/self/ns/pid`. Maximum nesting depth is 32.

2. **`struct pid`** — the kernel's PID object. Contains `numbers[]`, a flexible array
   with one `struct upid` per namespace level. This is where per-namespace PID numbers
   are stored. A process in a doubly-nested container has three entries in `numbers[]`.

3. **`struct upid`** — a pair of (PID number, namespace pointer). One per namespace
   level. `upid.nr` is the number visible in that namespace; `upid.ns` is the namespace
   where that number is valid.

The PID 1 init problem is the most common operational pitfall:
- Container PID 1 exits → `zap_pid_ns_processes()` kills the entire container.
- Container PID 1 does not call `waitpid()` → zombies accumulate → `EAGAIN` on fork.
- Solution: use `tini` or `dumb-init` as PID 1, or set `--init` in Docker.

In Kubernetes, `shareProcessNamespace: true` merges all containers in a pod into a
single PID namespace rooted at the pause container's PID 1. The pause container uses
`PR_SET_CHILD_SUBREAPER` to reap orphans from all containers in the pod.

**Previous:** [kernel/01-b-clone-flags.md](01-b-clone-flags.md) — `clone()`, `clone3()`, and all `CLONE_*` flags.
**Next:** [02-namespaces/](../../02-namespaces/README.md) — all eight namespace types in depth.
