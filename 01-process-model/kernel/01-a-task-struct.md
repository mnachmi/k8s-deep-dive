# 01-a — `struct task_struct`: The Complete Process Descriptor

> **Source file:**
> [`include/linux/sched.h`](https://elixir.bootlin.com/linux/v6.9/source/include/linux/sched.h#L737)
> (Linux 6.9, x86-64)
>
> **Size:** approximately 9,280 bytes on x86-64 (varies with kernel config).
> See Section 7 for how to measure it on your running kernel.

`struct task_struct` is the kernel's complete description of a running or runnable
process. Every process and every thread in the system — including kernel threads — is
represented by one `task_struct`. When Kubernetes creates a container, it is
ultimately asking the kernel to allocate and populate one of these structures.

Understanding `task_struct` is not optional background reading. It is the foundation
on which everything else in this course rests:

- A container's namespace isolation lives in `task_struct.nsproxy`
- A container's resource limits live in `task_struct.cgroups`
- A container's filesystem view lives in `task_struct.mm` and `task_struct.fs`
- A container's PID is stored in `task_struct.pid` / `task_struct.tgid`
- The scheduler's view of the process is `task_struct.se` (the CFS sched_entity)

---

## 1. Source Location and Size

### Elixir reference

The struct is defined starting at line 737 of
[`include/linux/sched.h`](https://elixir.bootlin.com/linux/v6.9/source/include/linux/sched.h#L737)
in Linux 6.9. It is one of the longest structs in the kernel — over 800 lines of
field definitions.

```c
/* include/linux/sched.h, line 737 (Linux 6.9) */
struct task_struct {
#ifdef CONFIG_THREAD_INFO_IN_TASK
    /*
     * For reasons of header soup this is the first member. It is always
     * in the same location (see asm/thread_info.h for the union).
     */
    struct thread_info		thread_info;
#endif
    unsigned int			__state;
    ...
```

### Measuring the struct size with pahole

`pahole` (part of the `dwarves` package) reads DWARF debug information from a
compiled kernel (`vmlinux`) and prints the exact field offsets, sizes, and padding.

```bash
# Install dwarves
sudo apt-get install dwarves    # Debian/Ubuntu
sudo dnf install dwarves        # Fedora/RHEL

# Print the first 80 lines of the struct layout
# vmlinux is at /usr/lib/debug/boot/vmlinux-$(uname -r) on most distros
# or /boot/vmlinux-$(uname -r) on some
pahole /usr/lib/debug/boot/vmlinux-$(uname -r) -C task_struct | head -80
```

Typical output (Linux 6.9, x86-64, standard Ubuntu/Fedora config):

```
struct task_struct {
        struct thread_info         thread_info;          /*     0    32 */
        unsigned int               __state;              /*    32     4 */

        /* XXX 4 bytes hole, try to pack */

        void *                     stack;                /*    40     8 */
        refcount_t                 usage;                /*    48     4 */
        unsigned int               flags;                /*    52     4 */
        unsigned int               ptrace;               /*    56     4 */

        /* XXX 4 bytes hole, try to pack */

        int                        on_cpu;               /*    64     4 */
        struct __call_single_data  wake_entry;           /*    68    72 */
        ...
        /* --- cacheline 1 boundary (64 bytes) --- */
        ...
        /* size: 9280, cachelines: 145, members: 318 */
        /* sum members: 8984, holes: 1, sum holes: 4 */
        /* paddings: 8, sum paddings: 65 */
        /* forced alignments: 9 */
        /* last cacheline: 0 bytes */
};
```

The final line from pahole shows approximately 9,280 bytes on a typical x86-64 build.
This is roughly 145 cache lines (at 64 bytes per cache line on x86-64).

---

## 2. Top-Level Field Groups and Cache-Line Layout

The struct is deliberately arranged so that the fields read and written most frequently
by the scheduler sit in the first few cache lines. Cache misses when accessing a
`task_struct` are expensive — the scheduler accesses them millions of times per second.

Fields are grouped into logical regions. Several are explicitly aligned to cache-line
boundaries using the `____cacheline_aligned` attribute:

```c
/* include/linux/sched.h (Linux 6.9) — simplified region view */

struct task_struct {
    /* === REGION 1: Thread info and misc state === */
    struct thread_info   thread_info;   /* line 741 */
    unsigned int         __state;       /* line 747 */
    void                *stack;         /* line 752 */
    refcount_t           usage;         /* line 753 */
    unsigned int         flags;         /* line 754 */

    /* === REGION 2: Scheduling (hot path) === */
    /* ---- cacheline boundary ---- */
    int                  on_cpu;
    struct __call_single_data wake_entry;
    unsigned int         wakee_flips;
    unsigned long        wakee_flip_decay_ts;
    struct task_struct  *last_wakee;
    int                  recent_used_cpu;
    int                  wake_cpu;
    int                  on_rq;             /* line 775 */
    int                  prio;              /* line 777 */
    int                  static_prio;
    int                  normal_prio;
    unsigned int         rt_priority;
    struct sched_entity  se;                /* line 785 — CFS entity */
    struct sched_rt_entity rt;             /* line 786 — RT entity */
    struct sched_dl_entity dl;             /* line 787 — DL entity */
    const struct sched_class *sched_class; /* line 789 */

    /* === REGION 3: Memory management === */
    struct mm_struct    *mm;              /* line 866 */
    struct mm_struct    *active_mm;       /* line 867 */

    /* === REGION 4: Process tree === */
    struct list_head     children;        /* line 910 */
    struct list_head     sibling;         /* line 911 */
    struct task_struct  *group_leader;    /* line 912 */
    struct list_head     thread_group;    /* line 914 */
    struct list_head     thread_node;     /* line 915 */

    /* === REGION 5: Identity === */
    pid_t                pid;             /* line 932 */
    pid_t                tgid;            /* line 933 */
    char                 comm[TASK_COMM_LEN]; /* line 975 */

    /* === REGION 6: Namespaces === */
    struct nsproxy      *nsproxy;         /* line 1020 */

    /* === REGION 7: Signal handling === */
    struct signal_struct *signal;
    struct sighand_struct __rcu *sighand;
    sigset_t             blocked;
    sigset_t             real_blocked;
    sigset_t             saved_sigmask;
    struct sigpending    pending;

    /* === REGION 8: Filesystem === */
    struct fs_struct    *fs;              /* line 1038 */
    struct files_struct *files;           /* line 1039 */

    /* === REGION 9: Credentials === */
    const struct cred __rcu *real_cred;   /* line 1118 */
    const struct cred __rcu *cred;        /* line 1119 */

    /* === REGION 10: Cgroups === */
    struct css_set __rcu *cgroups;        /* line 1309 */
    struct list_head     cg_list;         /* line 1310 */

    /* ... many more fields ... */
};
```

### Summary table

| Region | Key Fields | Hot/Cold | Purpose |
|--------|-----------|----------|---------|
| Thread info | `thread_info`, `stack`, `flags` | hot | per-CPU thread state, stack pointer |
| Scheduler | `__state`, `on_rq`, `prio`, `se`, `rt`, `dl`, `sched_class` | **very hot** | CFS/RT/DL scheduler state; accessed on every context switch |
| Memory | `mm`, `active_mm` | hot | address space; accessed on every page fault, every system call |
| Process tree | `children`, `sibling`, `group_leader`, `thread_group` | cold | traversed only on fork/exit/signal delivery |
| Identity | `pid`, `tgid`, `comm` | warm | read frequently by ptrace, audit, /proc |
| Namespaces | `nsproxy` | warm | one dereference per namespace operation |
| Signals | `signal`, `sighand`, `pending`, `blocked` | warm | accessed on signal delivery |
| Filesystem | `fs`, `files` | warm | accessed on every open()/read()/write() |
| Credentials | `real_cred`, `cred` | warm | checked on every permission check |
| Cgroups | `cgroups` | cold-warm | updated when moving between cgroups; read for resource accounting |

---

## 3. Key Fields — Full Detail

### 3.1 `unsigned int __state` — Task State

**Source:**
[`include/linux/sched.h:747`](https://elixir.bootlin.com/linux/v6.9/source/include/linux/sched.h#L747)

```c
/* include/linux/sched.h, line 747 */
unsigned int			__state;
```

The `__state` field (named `state` prior to 5.14; the rename added the `__` to prevent
accidental direct access) encodes the current execution state of the task.

**State constants** (defined at
[`include/linux/sched.h:83`](https://elixir.bootlin.com/linux/v6.9/source/include/linux/sched.h#L83)):

```c
/* include/linux/sched.h lines 83–115 (Linux 6.9) */
#define TASK_RUNNING                    0x00000000
#define TASK_INTERRUPTIBLE              0x00000001
#define TASK_UNINTERRUPTIBLE            0x00000002
#define __TASK_STOPPED                  0x00000004
#define __TASK_TRACED                   0x00000008
/* in tsk->exit_state: */
#define EXIT_DEAD                       0x00000010
#define EXIT_ZOMBIE                     0x00000020
/* in tsk->__state again: */
#define TASK_PARKED                     0x00000040
#define TASK_DEAD                       0x00000080
#define TASK_WAKEKILL                   0x00000100
#define TASK_WAKING                     0x00000200
#define TASK_NOLOAD                     0x00000400
#define TASK_NEW                        0x00000800
#define TASK_RTLOCK_WAIT                0x00001000
#define TASK_FREEZABLE                  0x00002000
#define __TASK_FREEZABLE_UNSAFE         0x00004000
#define TASK_FROZEN                     0x00008000
```

| State value | Meaning | Container context |
|-------------|---------|------------------|
| `TASK_RUNNING (0)` | On the run queue or currently executing on a CPU | Container process actively using CPU |
| `TASK_INTERRUPTIBLE (1)` | Sleeping; will wake on a signal or event | Container waiting on network I/O (epoll_wait) |
| `TASK_UNINTERRUPTIBLE (2)` | Sleeping; will NOT wake on a signal | Container waiting for disk I/O; appears as 'D' in `ps`, contributes to load average |
| `__TASK_STOPPED (4)` | Stopped by signal (SIGSTOP/SIGTSTP) or ptrace | Container paused; common in `kubectl debug` sessions using SIGSTOP |
| `EXIT_ZOMBIE (32)` | Has exited but parent has not called wait() | Container exited but PID still in namespace's PID table until containerd calls wait() |

**Who sets it:** The `set_current_state()` macro (for the calling task) and
`set_task_state()` (for another task). These macros include a memory barrier:

```c
/* include/linux/sched.h, line 210 (Linux 6.9) */
#define set_current_state(state_value)                  \
    do {                                                 \
        WRITE_ONCE(current->__state, (state_value));    \
        smp_mb();                                        \
    } while (0)
```

> **Note:** simplified for clarity. The actual Linux 6.9 implementation uses
> `smp_store_mb(current->__state, state_value)` — a combined store + full memory
> barrier — not two separate operations.

The `smp_mb()` is critical: it ensures that the state change is visible to all CPUs
before the task actually blocks. Without this barrier, the wakeup path could miss the
sleeping state.

> **Note:** `set_task_state()` was removed in Linux 5.17. On kernels 5.17+, use
> `WRITE_ONCE(tsk->__state, state_value)` directly.

**Who reads it:** The scheduler (`kernel/sched/core.c`) reads `__state` to decide
whether a task is eligible to run. The `try_to_wake_up()` function checks that the
task is not already `TASK_RUNNING` before adding it to the run queue.

**Live observation:**

```bash
# State is visible as the first character in /proc/<pid>/stat
cat /proc/self/stat   # 'R' = running, 'S' = interruptible sleep, 'D' = uninterruptible
# /proc/<pid>/status gives the full spelled-out state
cat /proc/self/status | grep State
# State: S (sleeping)
```

---

### 3.2 `pid_t pid` and `pid_t tgid` — Process and Thread Group IDs

**Source:**
[`include/linux/sched.h:932`](https://elixir.bootlin.com/linux/v6.9/source/include/linux/sched.h#L932)

```c
/* include/linux/sched.h, lines 932–933 */
pid_t				pid;
pid_t				tgid;
```

These two fields encode one of the most important distinctions in the Linux process
model — and one of the most commonly misunderstood.

**`pid` — the kernel thread ID:**

- Unique across all threads in the system (within the PID namespace).
- Assigned by `alloc_pid()` in [`kernel/pid.c`](https://elixir.bootlin.com/linux/v6.9/source/kernel/pid.c).
- When a new thread is created via `clone(CLONE_THREAD, ...)`, it gets a new unique `pid`
  value distinct from its parent thread.
- This is what gettid(2) returns.
- In `/proc/`, each thread appears as `/proc/<pid>/task/<tid>/`.

**`tgid` — the thread group ID:**

- All threads in the same process share the same `tgid`.
- `tgid` equals the `pid` of the first thread in the group (the group leader).
- This is what getpid(2) returns from userspace.
- When userspace code calls `getpid()`, glibc returns `tgid`, not `pid`.
- When you see a process in `ps aux` or `top`, you are seeing the `tgid`.

**The container PID question:**

Inside a PID namespace, `pid` and `tgid` contain the namespace-local PID. But
`task_struct` always stores the PID namespace-independent (host) value. The per-namespace
PID is stored in the `struct pid` structure referenced by `task_struct.thread_pid`.

```c
/* include/linux/sched.h (Linux 6.9) */
struct pid               *thread_pid;   /* PID structure with per-ns vpids */
```

```c
/* include/linux/pid.h (Linux 6.9) */
struct pid {
    refcount_t          count;
    unsigned int        level;        /* number of namespaces this pid exists in */
    spinlock_t          lock;
    struct hlist_head   tasks[PIDTYPE_MAX];
    struct hlist_head   inodes;
    wait_queue_head_t   wait_pidfd;
    struct rcu_head     rcu;
    struct upid         numbers[];    /* variable-length array: one upid per namespace level */
};

struct upid {
    int nr;                           /* the namespace-local PID number */
    struct pid_namespace *ns;         /* the namespace this nr is valid in */
};
```

So when a container process has host PID 12345 and container PID 1:
- `task_struct.pid` = 12345 (the host-level thread ID)
- `task_struct.thread_pid->numbers[0].nr` = 12345 (level 0 = initial PID namespace)
- `task_struct.thread_pid->numbers[1].nr` = 1 (level 1 = container's PID namespace)

**Who sets it:** `copy_process()` in
[`kernel/fork.c`](https://elixir.bootlin.com/linux/v6.9/source/kernel/fork.c)
calls `alloc_pid()` to allocate the `struct pid` with the correct namespace levels,
then sets `p->pid = pid_nr(pid)` where `pid_nr()` returns the number from the initial
(level-0) namespace.

**Who reads it:** The `/proc` filesystem, the audit subsystem, signal delivery code,
and anywhere a task is identified to userspace.

**Live observation:**

```bash
# From inside a container:
cat /proc/self/status | grep -E '^(Pid|Tgid|NSpid|NStgid)'
# Pid:   1          (tgid visible to container, which is the host tgid)
# Tgid:  1
# NSpid: 1          (namespace-local PID, same as Pid here since we are PID 1)
# NStgid: 1

# From the host, for a container process:
cat /proc/12345/status | grep -E '^(Pid|Tgid|NSpid|NStgid)'
# Pid:   12345
# Tgid:  12345
# NSpid: 12345 1    (host PID, then container PID)
# NStgid: 12345 1
```

---

### 3.3 `struct sched_entity se` — CFS Scheduling Entity

**Source:**
[`include/linux/sched.h:785`](https://elixir.bootlin.com/linux/v6.9/source/include/linux/sched.h#L785)

```c
/* include/linux/sched.h, line 785 */
struct sched_entity		se;
```

The `sched_entity` is the data structure on which the Completely Fair Scheduler (CFS)
operates. It is embedded directly inside `task_struct` (not a pointer — this is for
cache efficiency). The CFS red-black tree in `struct cfs_rq` stores pointers to
`sched_entity` nodes.

**Full struct definition** (from
[`include/linux/sched.h:551`](https://elixir.bootlin.com/linux/v6.9/source/include/linux/sched.h#L551)):

```c
/* include/linux/sched.h, lines 551–593 (Linux 6.9) */
struct sched_entity {
    /* For load-balancing: */
    struct load_weight              load;            /* entity's weight on the rq */
    struct rb_node                  run_node;        /* node in CFS red-black tree */
    u64                             deadline;        /* EEVDF deadline */
    u64                             min_vruntime;    /* EEVDF: min vruntime seen */

    struct list_head                group_node;      /* member of cfs_rq->tasks_timeline */
    unsigned int                    on_rq;           /* 1 if on a run queue, 0 if not */

    u64                             exec_start;      /* last time this entity started executing */
    u64                             sum_exec_runtime;/* total ns spent running */
    u64                             prev_sum_exec_runtime; /* sum at last task switch */
    u64                             vruntime;        /* virtual runtime — the CFS key */
    s64                             vlag;            /* EEVDF lag */
    u64                             slice;           /* scheduling slice in ns */

    u64                             nr_migrations;   /* times migrated between CPUs */

    /* Hierarchical scheduling: */
#ifdef CONFIG_FAIR_GROUP_SCHED
    int                             depth;
    struct sched_entity             *parent;    /* parent in cgroup sched hierarchy */
    struct cfs_rq                   *cfs_rq;    /* CFS rq this entity belongs to */
    struct cfs_rq                   *my_q;      /* CFS rq this entity is the root of */
    unsigned long                   runnable_weight; /* weight of runnable descendants */
#endif

    /* Per-entity load tracking (PELT): */
    struct sched_avg                avg;
};
```

**Key fields explained:**

| Field | Type | Purpose |
|-------|------|---------|
| `vruntime` | `u64` | The CFS sort key: accumulated virtual runtime in nanoseconds, weighted by the task's nice/priority. CFS always picks the entity with the lowest `vruntime`. This is what makes CFS "fair" — all tasks tend toward the same `vruntime`. |
| `exec_start` | `u64` | Timestamp (from `sched_clock()`) when the task last started running. Used to compute the runtime delta. |
| `sum_exec_runtime` | `u64` | Total wall-clock nanoseconds this task has spent running since it was created. Exposed as `/proc/<pid>/stat` field 14 (utime + stime in jiffies). |
| `load.weight` | `unsigned long` | The task's weight in the scheduler, derived from its nice value via `prio_to_weight[]`. Default nice=0 → weight=1024. Each +1 nice step reduces weight by ~10%. |
| `run_node` | `struct rb_node` | The node that is embedded in the CFS run queue's red-black tree. The tree is sorted by `vruntime`. |
| `on_rq` | `unsigned int` | 1 if this entity is currently on a run queue (either actually running or runnable). 0 if sleeping. Distinct from `task_struct.__state`. |

**CFS scheduling in 30 seconds:**

CFS maintains a per-CPU red-black tree (`struct cfs_rq.tasks_timeline`) where each
node is a `sched_entity`. The leftmost node (lowest `vruntime`) is always the next
task to run. When a task runs, CFS increments `vruntime` by the wall-clock time
divided by the task's weight. Heavier tasks (lower nice value) accumulate `vruntime`
more slowly, so they run more often.

**Container CPU limits:** When Kubernetes sets a CPU limit on a container, kubelet
configures the cgroup's `cpu.cfs_period_us` and `cpu.cfs_quota_us`. The kernel's
CFS bandwidth controller tracks the container's `sum_exec_runtime` per cgroup and
throttles it when the quota is exhausted. The `sched_entity` is where this accounting
happens.

**Live observation:**

```bash
# View the vruntime of all tasks on CPU 0's CFS run queue (requires root + debugfs)
cat /sys/kernel/debug/sched/debug | grep -A 5 "cfs_rq\[0\]"

# bpftrace: print vruntime of every task scheduled OUT (prev task leaving CPU)
bpftrace -e 'tracepoint:sched:sched_switch {
    printf("prev=%s vruntime=%llu\n",
        args->prev_comm,
        ((struct task_struct *)curtask)->se.vruntime);
}'
```

---

### 3.4 `struct nsproxy *nsproxy` — Namespace Pointer

**Source:**
[`include/linux/sched.h:1020`](https://elixir.bootlin.com/linux/v6.9/source/include/linux/sched.h#L1020)

```c
/* include/linux/sched.h, line 1020 */
struct nsproxy			*nsproxy;
```

**The `struct nsproxy` definition** (from
[`include/linux/nsproxy.h:31`](https://elixir.bootlin.com/linux/v6.9/source/include/linux/nsproxy.h#L31)):

```c
/* include/linux/nsproxy.h, lines 31–43 (Linux 6.9) */
struct nsproxy {
    refcount_t           count;           /* number of tasks sharing this nsproxy */
    struct uts_namespace *uts_ns;         /* CLONE_NEWUTS: hostname/domainname */
    struct ipc_namespace *ipc_ns;         /* CLONE_NEWIPC: SysV IPC, POSIX MQ */
    struct mnt_namespace *mnt_ns;         /* CLONE_NEWNS: mount tree */
    struct pid_namespace *pid_ns_for_children; /* CLONE_NEWPID: new child PIDs */
    struct net           *net_ns;         /* CLONE_NEWNET: network stack */
    struct time_namespace *time_ns;       /* CLONE_NEWTIME: CLOCK_MONOTONIC offset */
    struct time_namespace *time_ns_for_children;
    struct cgroup_namespace *cgroup_ns;   /* CLONE_NEWCGROUP: cgroup root view */
};
```

**The `nsproxy` mechanism is copy-on-write:**

All threads in the same thread group share the same `nsproxy` pointer. When a process
calls `unshare(CLONE_NEWNET)`, the kernel:
1. Allocates a new `struct nsproxy`.
2. Copies all the namespace pointers from the old `nsproxy`.
3. Allocates a new `struct net` for the network namespace.
4. Sets `new_nsproxy->net_ns` to the new `struct net`.
5. Atomically replaces `task_struct.nsproxy` under `rcu_read_lock()`.

Threads that were sharing the old `nsproxy` continue using it (via the `count`
refcount). The calling thread now has its own `nsproxy`.

**Container namespace setup:**

When runc calls `clone3(CLONE_NEWPID | CLONE_NEWNET | CLONE_NEWNS | CLONE_NEWUTS |
CLONE_NEWIPC, ...)`, `copy_process()` calls
[`copy_namespaces()`](https://elixir.bootlin.com/linux/v6.9/source/kernel/nsproxy.c#L152)
which allocates a new `nsproxy` and new instances of each namespace type flagged in the
`clone_flags`. Each namespace struct has its own initialization code:

- `create_pid_namespace()` — allocates a new PID namespace with an empty PID table;
  the first process to be created in it gets PID 1.
- `copy_net_ns()` — allocates `struct net` with a fresh loopback interface; the CNI
  plugin later adds the veth pair.
- `copy_mnt_ns()` — clones the current mount tree; runc then calls `pivot_root()` to
  set the OCI layer stack as the root.
- `copy_uts_ns()` — copies the UTS namespace; runc calls `sethostname()` to set the
  container's hostname.

**Who reads `nsproxy`:** Every namespace-aware kernel operation reads it:
- `task_nsproxy(task)` macro returns `rcu_dereference(task->nsproxy)`.
- File system operations read `nsproxy->mnt_ns`.
- Network operations read `nsproxy->net_ns`.
- The `/proc` filesystem reads `nsproxy->pid_ns_for_children` to translate host PIDs
  to container-local PIDs.

**Locking:** `nsproxy` is read under RCU. Replacing it requires holding `task_lock()`
(the task's `alloc_lock` spinlock). See Section 5 for full locking detail.

**Live observation:**

```bash
# The /proc/self/ns/ directory shows file descriptors pointing to each namespace
ls -la /proc/self/ns/
# lrwxrwxrwx cgroup -> cgroup:[4026531835]
# lrwxrwxrwx ipc    -> ipc:[4026531839]
# lrwxrwxrwx mnt    -> mnt:[4026531841]
# lrwxrwxrwx net    -> net:[4026531840]
# lrwxrwxrwx pid    -> pid:[4026531836]
# ...

# The inode number (e.g., 4026531835) uniquely identifies the namespace instance.
# Two processes sharing a namespace have the same inode number.
# Use this to find all processes in the same network namespace as a container:
NS=$(readlink /proc/<container-pid>/ns/net)
for p in /proc/[0-9]*/ns/net; do
    [ "$(readlink $p)" = "$NS" ] && echo "$p"
done

# bpftrace: traces copy_namespaces entry, showing which task is creating new namespaces
bpftrace -e 'kprobe:copy_namespaces {
    printf("copy_namespaces: task=%d flags=0x%lx\n",
        ((struct task_struct *)curtask)->pid,
        (uint64)arg0);
}'
```

---

### 3.5 `struct css_set __rcu *cgroups` — Cgroup Membership

**Source:**
[`include/linux/sched.h:1309`](https://elixir.bootlin.com/linux/v6.9/source/include/linux/sched.h#L1309)

```c
/* include/linux/sched.h, line 1309 */
struct css_set __rcu		*cgroups;
struct list_head		cg_list;   /* node in css_set->tasks list */
```

The `css_set` maps a task to all of its cgroup subsystem states simultaneously. It is
the "intersection" structure between a task and all the cgroup hierarchies.

**Full `struct css_set` definition** (from
[`include/linux/cgroup-defs.h:288`](https://elixir.bootlin.com/linux/v6.9/source/include/linux/cgroup-defs.h#L288)):

```c
/* include/linux/cgroup-defs.h, lines 288–340 (Linux 6.9, simplified) */
struct css_set {
    struct cgroup_subsys_state *subsys[CGROUP_SUBSYS_COUNT]; /* one css per subsystem */
    refcount_t              refcount;
    struct css_set          *dom_cset;    /* the "effective" css_set for dominance */
    struct cgroup           *dfl_cgrp;    /* the default hierarchy cgroup */
    int                     nr_tasks;     /* number of tasks using this css_set */

    /* hash table linkage */
    struct hlist_node       hlist;

    /* per-task-group list of tasks using this css_set */
    struct list_head        tasks;        /* list of tasks through task.cg_list */
    struct list_head        mg_src_preload_node;
    struct list_head        mg_dst_preload_node;
    struct list_head        mg_node;
    struct list_head        e_cset_node[CGROUP_SUBSYS_COUNT];
    struct list_head        dying_tasks;
    struct list_head        task_iters;
    struct rcu_head         rcu_head;
    bool                    dead;
    struct work_struct      release_work;
};
```

**`subsys[]` array:** Each element is a `struct cgroup_subsys_state *` for one
cgroup subsystem. The index is the subsystem's ID (e.g., `memory_cgrp_id`,
`cpu_cgrp_id`, `pids_cgrp_id`). The css holds a pointer to the `struct cgroup` that
the task belongs to in that subsystem hierarchy.

**Example subsystem mapping for a Kubernetes pod:**

```
css_set.subsys[cpu_cgrp_id]    → struct cgroup at /sys/fs/cgroup/kubepods/burstable/pod<uid>/<container-id>
css_set.subsys[memory_cgrp_id] → struct cgroup at /sys/fs/cgroup/kubepods/burstable/pod<uid>/<container-id>
css_set.subsys[pids_cgrp_id]   → struct cgroup at /sys/fs/cgroup/kubepods/burstable/pod<uid>/<container-id>
```

**Why `css_set` is a separate struct (not inline in task_struct):**

Multiple tasks that belong to the *exact same combination* of cgroups share a single
`css_set`. The kernel maintains a hash table of `css_set` objects. When a task is
moved to a new cgroup, the kernel computes which `css_set` matches the new combination
and either finds an existing one or allocates a new one. This avoids duplicating all
the cgroup references for every task in a pod.

**Who sets it:** `cgroup_attach_task()` in
[`kernel/cgroup/cgroup.c`](https://elixir.bootlin.com/linux/v6.9/source/kernel/cgroup/cgroup.c).
At process creation, `copy_process()` calls `cgroup_fork()` which sets the initial
`cgroups` pointer. When Kubernetes moves a task to a cgroup (e.g., when a pod is
created and the container process is assigned to its cgroup hierarchy), this pointer
is updated atomically.

**Locking:** The `cgroups` pointer is RCU-protected. Reads use `rcu_dereference()`.
Writes hold `cgroup_mutex`.

**Live observation:**

```bash
# Find which cgroup a container process belongs to
cat /proc/<pid>/cgroup
# 0::/kubepods/burstable/pod7f8c2d1a.../container-id/

# Verify via bpftrace that the css_set pointer is non-null
bpftrace -e 'kprobe:cgroup_attach_task {
    printf("attach: pid=%d cgroup=%s\n",
        ((struct task_struct *)arg1)->pid,
        ((struct cgroup *)arg0)->kn->name);
}'
```

---

### 3.6 `struct mm_struct *mm` and `struct mm_struct *active_mm`

**Source:**
[`include/linux/sched.h:866`](https://elixir.bootlin.com/linux/v6.9/source/include/linux/sched.h#L866)

```c
/* include/linux/sched.h, lines 866–867 */
struct mm_struct		*mm;
struct mm_struct		*active_mm;
```

**`mm`:** Points to the process's address space descriptor. This is the struct that
holds the page table root (`pgd`), the list of VMAs (`vm_area_struct`), the brk/mmap
pointers, and all memory accounting. For kernel threads, `mm` is `NULL` — they have no
user address space.

**`active_mm`:** A kernel thread cannot have `mm == NULL` on all code paths —
certain operations need an mm for TLB management on architectures that use ASID-based
TLB tagging. When a kernel thread is scheduled, it "borrows" the `mm` of the
previously running user process, which is stored in `active_mm`. For user processes,
`active_mm == mm` always.

**Why this matters for containers:** Each container process has its own `mm_struct`
(set up by `copy_mm()` in `copy_process()`). On `fork()`, the page tables are
copy-on-write — physical pages are shared until one process writes to them, at which
point the page fault handler copies the page. This is why container startup is fast
despite potentially large process images.

**`struct mm_struct` key fields** (from
[`include/linux/mm_types.h:638`](https://elixir.bootlin.com/linux/v6.9/source/include/linux/mm_types.h#L638)):

```c
struct mm_struct {
    struct {
        struct maple_tree   mm_mt;       /* maple tree of VMAs */
        unsigned long       mmap_base;   /* base of mmap area */
        unsigned long       task_size;   /* size of task vm space */
        pgd_t               *pgd;        /* page global directory (page table root) */
        atomic_t            mm_users;    /* threads using this mm */
        atomic_t            mm_count;    /* reference count */
        unsigned long       hiwater_rss; /* high water mark for RSS */
        unsigned long       hiwater_vm;  /* high water mark for VM size */
        unsigned long       total_vm;    /* total pages mapped */
        unsigned long       locked_vm;   /* pages that have PG_mlocked set */
        atomic64_t          pinned_vm;   /* refcount permanently increase */
        unsigned long       data_vm;     /* VM_WRITE & ~VM_SHARED & ~VM_STACK */
        unsigned long       exec_vm;     /* VM_EXEC & ~VM_WRITE & ~VM_STACK */
        unsigned long       stack_vm;    /* VM_GROWSDOWN */
        ...
    };
    /* mmap_lock: protects the VMA tree */
    struct rw_semaphore     mmap_lock;
    ...
};
```

**Live observation:**

```bash
# Memory maps for a container process
cat /proc/<pid>/maps       # all VMAs: address range, permissions, backing file
cat /proc/<pid>/smaps      # detailed per-VMA accounting including PSS
cat /proc/<pid>/status | grep -E '(VmRSS|VmSize|VmPeak)'
```

---

### 3.7 `const struct cred __rcu *real_cred` and `*cred`

**Source:**
[`include/linux/sched.h:1118`](https://elixir.bootlin.com/linux/v6.9/source/include/linux/sched.h#L1118)

```c
/* include/linux/sched.h, lines 1118–1119 */
const struct cred __rcu		*real_cred;
const struct cred __rcu		*cred;
```

**`real_cred`:** The "objective" credentials — the actual user identity of the process.
Used when the process is the *target* of a security check (e.g., "can process X send a
signal to this process?").

**`cred`:** The "subjective" credentials — the credentials the process uses when it
performs actions. Normally equal to `real_cred`. They differ when setuid/setgid
programs execute (the effective UID changes).

**`struct cred` definition** (from
[`include/linux/cred.h:111`](https://elixir.bootlin.com/linux/v6.9/source/include/linux/cred.h#L111)):

```c
/* include/linux/cred.h, lines 111–162 (Linux 6.9) */
struct cred {
    atomic_long_t       usage;
    kuid_t              uid;    /* real UID */
    kgid_t              gid;    /* real GID */
    kuid_t              suid;   /* saved UID */
    kgid_t              sgid;   /* saved GID */
    kuid_t              euid;   /* effective UID */
    kgid_t              egid;   /* effective GID */
    kuid_t              fsuid;  /* UID for VFS ops */
    kgid_t              fsgid;  /* GID for VFS ops */
    unsigned            securebits; /* SUID-less security management */
    kernel_cap_t        cap_inheritable; /* caps our children can inherit */
    kernel_cap_t        cap_permitted;   /* caps we're permitted */
    kernel_cap_t        cap_effective;   /* caps we can actually use */
    kernel_cap_t        cap_bset;        /* capability bounding set */
    kernel_cap_t        cap_ambient;     /* ambient capability set */
    struct user_struct  *user;           /* reference to user account */
    struct user_namespace *user_ns;      /* user namespace of this cred */
    struct ucounts      *ucounts;
    struct group_info   *group_info;     /* supplementary groups */
    union {
        int non_rcu;                 /* can we skip RCU? */
        struct rcu_head rcu;         /* RCU deletion hook */
    };
};
```

**Immutability and copy-on-write:** The `cred` struct is immutable after it is
committed to a task. To change credentials (e.g., `setuid()`, `execve()` of a setuid
binary), the kernel calls `prepare_creds()` to allocate a new `cred` (copying all
fields from the current one), modifies the copy, then calls `commit_creds()` which
atomically replaces `task_struct.cred` under RCU.

**Container user namespaces:** When a container runs with user namespace isolation
(`CLONE_NEWUSER`), the `cred->user_ns` pointer points to the container's user
namespace. UID 0 inside the container maps to a non-privileged UID on the host. The
`kernel_cap_t` fields only grant capabilities within the user namespace.

**Live observation:**

```bash
cat /proc/self/status | grep -E '(Uid|Gid|CapPrm|CapEff|CapBnd)'
# Uid: 1000 1000 1000 1000     (real, effective, saved, fs)
# CapPrm: 0000000000000000     (empty = no capabilities)
# CapEff: 0000000000000000

# For a container running as root (uid 0 in user ns):
cat /proc/<container-pid>/status | grep -E '(Uid|Gid|CapEff)'
```

---

### 3.8 `char comm[TASK_COMM_LEN]` — Command Name

**Source:**
[`include/linux/sched.h:975`](https://elixir.bootlin.com/linux/v6.9/source/include/linux/sched.h#L975)

```c
/* include/linux/sched.h, line 975 */
char				comm[TASK_COMM_LEN];
```

`TASK_COMM_LEN` is 16 (defined at
[`include/linux/sched.h:316`](https://elixir.bootlin.com/linux/v6.9/source/include/linux/sched.h#L316)).
This means the executable name is stored as a null-terminated string with a maximum
length of 15 characters plus the null terminator.

**Important:** This is not the full command line (which lives in `mm->arg_start`).
`comm` is only the basename of the executable, truncated to 15 chars. `nginx` becomes
`nginx`; `containerd-shim-runc-v2` becomes `containerd-shim` (15 chars).

**Who sets it:**
- `do_execveat_common()` in
  [`fs/exec.c`](https://elixir.bootlin.com/linux/v6.9/source/fs/exec.c) calls
  `set_task_comm()` with the basename of the executable when a new program is exec'd.
- `set_task_comm()` (defined at
  [`kernel/sched/core.c`](https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/core.c))
  acquires `task_lock()` (the `alloc_lock` spinlock) before writing `comm`.
- Threads set their own comm via `prctl(PR_SET_NAME, ...)`, which also calls
  `set_task_comm()`.

**Live observation:**

```bash
cat /proc/self/comm           # just the comm field
cat /proc/self/cmdline        # full command line (null-delimited)
cat /proc/self/exe            # symlink to the executable

# bpftrace: watch comm updates as processes exec new programs
bpftrace -e 'kprobe:set_task_comm {
    printf("pid=%d comm=%s\n",
        ((struct task_struct *)arg0)->pid, str(arg1));
}'
```

---

### 3.9 `struct list_head children` and `struct list_head sibling`

**Source:**
[`include/linux/sched.h:910`](https://elixir.bootlin.com/linux/v6.9/source/include/linux/sched.h#L910)

```c
/* include/linux/sched.h, lines 910–911 */
struct list_head		children;   /* list of child tasks (head) */
struct list_head		sibling;    /* linkage in parent's children list */
```

These two fields implement the process tree using the kernel's intrusive doubly-linked
list (`struct list_head`).

**`struct list_head`** (from
[`include/linux/types.h:178`](https://elixir.bootlin.com/linux/v6.9/source/include/linux/types.h#L178)):

```c
struct list_head {
    struct list_head *next, *prev;
};
```

**How the tree works:**

```
parent->children (head)
    │
    ▼ next
child_A->sibling (node) ←→ child_B->sibling (node) ←→ child_C->sibling (node)
    │                           │                           │
    ▼ (each child's children list)
child_A->children (head)
    │
    ▼
grandchild->sibling (node)
```

- `task_struct.children` is the **head** of the doubly-linked list of all direct
  children. It does not point to a `task_struct` directly; you use `list_entry()` or
  `list_for_each_entry()` to recover the containing `task_struct`.
- `task_struct.sibling` is the **node** that links this task into its parent's
  `children` list.
- To iterate over all children of a task:
  ```c
  list_for_each_entry(child, &parent->children, sibling) {
      /* child is a struct task_struct * */
  }
  ```

**The `real_parent` and `parent` fields:**

```c
/* include/linux/sched.h (Linux 6.9) */
struct task_struct __rcu	*real_parent;  /* biological parent — set at fork, never changes */
struct task_struct __rcu	*parent;       /* current parent — may be changed by ptrace */
```

`real_parent` is the task that called `fork()`. `parent` may differ if ptrace has
attached — ptrace can reparent a task temporarily.

**Why this matters for containers:**

The `kube-inspect` tool (Checkpoint 01) will walk this tree via `/proc`. From
userspace you reconstruct it using `/proc/<pid>/status` fields `PPid:` and by reading
the `stat` file. The `children`/`sibling` lists in `task_struct` are the kernel source
of truth that `/proc` exposes.

**Locking:** The process tree is protected by `tasklist_lock` (a read-write spinlock).
Writers (fork, exit) take it for writing. Readers (kill, ptrace, process tree walks)
take it for reading. See Section 5.

**Live observation:**

```bash
# Walk the process tree from userspace
pstree -p <pid>      # shows children using /proc/<pid>/task/ entries

# Find children of a specific PID via /proc
grep -r "PPid:.*<parent-pid>" /proc/*/status 2>/dev/null

# In the kernel with bpftrace: walk children list of init (PID 1)
# (illustrative — requires kernel pointer arithmetic)
bpftrace -e 'BEGIN {
    $init = (struct task_struct *)curtask;
    /* walk up to root */
    printf("init comm: %s\n", $init->comm);
}'
```

---

### 3.10 `struct task_struct *group_leader` and `struct list_head thread_group`

**Source:**
[`include/linux/sched.h:912`](https://elixir.bootlin.com/linux/v6.9/source/include/linux/sched.h#L912)

```c
/* include/linux/sched.h, lines 912–915 */
struct task_struct		*group_leader;  /* group leader: the task whose pid == tgid */
struct list_head		thread_group;   /* list of threads in the thread group */
struct list_head		thread_node;    /* node linking into signal->thread_head */
```

Whereas `children`/`sibling` implement the parent-child process tree, `thread_group`
implements the list of threads within a single process.

**`group_leader`:** Points to the thread whose `pid == tgid`. For a single-threaded
process, `group_leader` points back to the task itself. For a thread in a multi-threaded
program (e.g., the Go runtime spawning multiple OS threads), `group_leader` points to
the original thread that `fork()`ed the process. The group leader's `pid` is the `tgid`
of all other threads.

**`thread_group` list:** All threads in the same thread group are linked via
`thread_group`. This list is anchored at `group_leader->thread_group`. The kernel
iterates this list when:
- Delivering a signal to the whole thread group (`kill(tgid, ...)`)
- Waiting for all threads to exit (`group exit`)
- Counting threads for `RLIMIT_NPROC`

**Iterating threads:**

```c
/* kernel code pattern for iterating all threads in a group */
struct task_struct *thread;
rcu_read_lock();
for_each_thread(leader, thread) {
    /* thread is a member of leader's thread group */
}
rcu_read_unlock();
```

**Live observation:**

```bash
# Show all threads of a multi-threaded process
ls /proc/<pid>/task/          # each subdirectory is a thread (tid)
cat /proc/<pid>/status | grep Threads   # count of threads in thread group

# bpftrace: trace new thread creation (CLONE_THREAD flag)
# Note: since Linux 5.3, copy_process() takes struct kernel_clone_args * as its first argument
bpftrace -e 'kprobe:copy_process {
    $args = (struct kernel_clone_args *)arg0;
    if ($args->flags & 0x10000) {
        printf("new thread in tgid=%d\n",
            ((struct task_struct *)curtask)->tgid);
    }
}'
```

---

## 4. Lifecycle

The `task_struct` lifecycle spans from allocation via the slab allocator to deallocation
after process exit. Every container process follows this exact path.

```
Lifecycle of a task_struct
══════════════════════════

1. ALLOCATION
   ─────────
   alloc_task_struct_node()                 ← kernel/fork.c:190
     Uses task_struct_cachep (SLAB cache)
     Returns zeroed memory from KMEM_CACHE

2. CONSTRUCTION — copy_process()            ← kernel/fork.c:2105
   ──────────────────────────────
   dup_task_struct()                        ← kernel/fork.c:1019
     │  memcpy(tsk, orig, sizeof(*tsk))     copies all fields from parent
     │  alloc_thread_stack_node()           allocates a new 16KB kernel stack
     └► stack pointer wired into thread_info

   copy_creds()                             ← kernel/cred.c
     │  get_cred() on parent's cred         increments refcount (shared initially)
     └► new task starts with parent's credentials

   copy_namespaces()                        ← kernel/nsproxy.c:152
     │  If any CLONE_NEW* flags: allocate new nsproxy + namespace structs
     └► otherwise: get_nsproxy(old_nsproxy)  just increment refcount

   copy_mm()                                ← kernel/fork.c:1552
     │  If CLONE_VM: share mm (threads)
     └► else: dup_mm() → COW copy of page tables

   copy_thread()                            ← arch/x86/kernel/process.c
     └► arch-specific: set up register state for new task to start at ret_from_fork

   cgroup_fork()                            ← kernel/cgroup/cgroup.c
     └► copies parent's css_set reference (task placed in same cgroups initially)

   attach_pid()                             ← kernel/pid.c
     │  alloc_pid() → allocate struct pid with per-namespace numbers
     └► hash_pid() → insert into pid hash tables

   WRITE_ONCE(p->__state, TASK_NEW)   /* set_task_state() removed in Linux 5.17 */
   wake_up_new_task(p)                      ← sets TASK_RUNNING, adds to run queue


3. ACTIVE LIFETIME
   ──────────────
   schedule() / context_switch()            ← kernel/sched/core.c
     │  __switch_to() swaps CPU registers
     │  switch_mm() installs the new mm's page tables (CR3 load)
     └► task runs until it blocks or is preempted

   do_syscall_64()                          ← per every syscall
     └► calls sys_*() implementations


4. EXIT — do_exit()                         ← kernel/exit.c:806
   ─────────────────
   exit_mm()                                drops mm_struct reference; last user frees page tables
   exit_sem()                               release SysV semaphores
   exit_shm()                               release SysV shared memory
   exit_files()                             close all open file descriptors
   exit_fs()                                drop fs_struct reference
   exit_signals()                           flush pending signals; notify thread group
   exit_notify()                            send SIGCHLD to parent; zombie if parent not waiting
   cgroup_exit()                            remove task from cgroup membership
   rseq_syscall()                           rseq cleanup

   task enters EXIT_ZOMBIE state if parent has not called wait()
   parent calls wait() or SIGCHLD handler calls wait()
        └── release_task()                  ← kernel/exit.c:195
               │  __exit_signal()           detach from signal struct
               │  __unhash_process()        remove from pid hash tables, tasklist
               └── free_task()              ← kernel/fork.c
                     │  put_task_stack()    return thread stack to THREAD_INFO cache
                     └── free_task_struct() return task_struct to task_struct_cachep
```

### Key kernel source references for the lifecycle:

| Step | Function | Source |
|------|----------|--------|
| Allocation | `alloc_task_struct_node()` | [`kernel/fork.c:190`](https://elixir.bootlin.com/linux/v6.9/source/kernel/fork.c#L190) |
| Construction | `copy_process()` | [`kernel/fork.c:2105`](https://elixir.bootlin.com/linux/v6.9/source/kernel/fork.c#L2105) |
| Stack alloc | `dup_task_struct()` | [`kernel/fork.c:1019`](https://elixir.bootlin.com/linux/v6.9/source/kernel/fork.c#L1019) |
| Namespace copy | `copy_namespaces()` | [`kernel/nsproxy.c:152`](https://elixir.bootlin.com/linux/v6.9/source/kernel/nsproxy.c#L152) |
| MM copy | `copy_mm()` | [`kernel/fork.c:1552`](https://elixir.bootlin.com/linux/v6.9/source/kernel/fork.c#L1552) |
| PID allocation | `alloc_pid()` | [`kernel/pid.c:180`](https://elixir.bootlin.com/linux/v6.9/source/kernel/pid.c#L180) |
| Wakeup | `wake_up_new_task()` | [`kernel/sched/core.c:4868`](https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/core.c#L4868) |
| Exit | `do_exit()` | [`kernel/exit.c:806`](https://elixir.bootlin.com/linux/v6.9/source/kernel/exit.c#L806) |
| Reap | `release_task()` | [`kernel/exit.c:195`](https://elixir.bootlin.com/linux/v6.9/source/kernel/exit.c#L195) |
| Free | `free_task_struct()` | [`kernel/fork.c:410`](https://elixir.bootlin.com/linux/v6.9/source/kernel/fork.c#L410) |

---

## 5. Locking Discipline

`task_struct` fields are protected by several different locks, each optimised for the
access pattern of the fields it guards. Using the wrong lock for a field (or no lock)
is a kernel bug. This section documents all locks that touch `task_struct`.

### 5.1 `alloc_lock` — per-task spinlock

**Field:** `spinlock_t alloc_lock` at
[`include/linux/sched.h:928`](https://elixir.bootlin.com/linux/v6.9/source/include/linux/sched.h#L928).

**Access macro:** `task_lock(task)` / `task_unlock(task)`.

**Protects:**
- `comm[]` — set by `set_task_comm()` which calls `task_lock()` before writing
- `mm` / `active_mm` swaps (the actual swap during context switch uses stronger
  guarantees; `task_lock` protects `get_task_mm()` style access from other threads)
- `fs` and `files` — when a thread is detaching or copying these
- `nsproxy` — when replacing with a new nsproxy after `unshare()`

**Why spinlock:** These fields are accessed infrequently and the critical sections are
short. Contention is low.

### 5.2 `tasklist_lock` — global read-write spinlock

**Source:**
[`kernel/fork.c`](https://elixir.bootlin.com/linux/v6.9/source/kernel/fork.c) —
declared as `DEFINE_RWLOCK(tasklist_lock)`.

**Protects:**
- `task_struct.parent` / `real_parent` — reparenting
- `task_struct.children` / `sibling` lists — fork (write lock) and tree walks (read lock)
- `task_struct.ptraced` / `ptrace_entry` — ptrace attachment
- The `task_struct` itself from being freed while someone holds a read lock

**Access pattern:**
```c
/* Read access (safe to iterate process tree) */
read_lock(&tasklist_lock);
list_for_each_entry(child, &parent->children, sibling) { ... }
read_unlock(&tasklist_lock);

/* Write access (fork or exit modifying the tree) */
write_lock_irq(&tasklist_lock);
/* ... */
write_unlock_irq(&tasklist_lock);
```

**Performance note:** `tasklist_lock` is a global lock, which means that on systems
with very high process creation rate, it can become a bottleneck. The kernel has
been incrementally moving toward per-namespace locks, but `tasklist_lock` remains
for the core process tree operations.

### 5.3 `pid_lock` — per-PID-namespace spinlock

**Source:** `struct pid_namespace.pid_lock` at
[`include/linux/pid_namespace.h:17`](https://elixir.bootlin.com/linux/v6.9/source/include/linux/pid_namespace.h#L17).

**Protects:**
- The PID allocation idr (the radix tree mapping PID numbers to `struct pid`)
- The `task_struct.thread_pid` hash tables

**Access:** `alloc_pid()` and `free_pid()` in
[`kernel/pid.c`](https://elixir.bootlin.com/linux/v6.9/source/kernel/pid.c)
hold `pid_namespace.pid_lock` when allocating or releasing PID numbers.

### 5.4 `cgroup_mutex` and RCU for `cgroups`

**`task_struct.cgroups` pointer:**
- **Read:** Always under `rcu_read_lock()`. Use `rcu_dereference(task->cgroups)`.
- **Write:** Hold `cgroup_mutex` (defined in
  [`kernel/cgroup/cgroup.c`](https://elixir.bootlin.com/linux/v6.9/source/kernel/cgroup/cgroup.c))
  and then use `rcu_assign_pointer()`.

**Why RCU for `cgroups`:** cgroup membership changes happen during `cgroup attach`
operations (when kubelet moves a task to a new cgroup) but are read on every page fault
(for memory accounting) and every task run (for CPU accounting). RCU allows the common
case (reads) to be lock-free.

### 5.5 RCU for `real_cred` and `cred`

Both credential pointers are RCU-protected:
- **Read:** Under `rcu_read_lock()`. The `current_cred()` macro reads `current->cred`
  without RCU for the calling task only (safe because a task cannot change its own
  cred under its own feet).
- **Write:** `commit_creds()` uses `rcu_assign_pointer()` after the new cred is fully
  initialised.

### 5.6 RCU for `nsproxy`

- **Read:** `task_nsproxy(task)` uses `rcu_dereference()`.
- **Write:** `switch_task_namespaces()` uses `rcu_assign_pointer()` under `task_lock()`.

### 5.7 Summary table

| Field | Lock | Lock type | Access mode |
|-------|------|-----------|-------------|
| `comm[]` | `alloc_lock` | spinlock | `task_lock()` / `task_unlock()` |
| `mm`, `active_mm` | `alloc_lock` + mmap_lock | spinlock + rwsem | `task_lock()` for swap; `mmap_lock` for VMA ops |
| `fs`, `files` | `alloc_lock` | spinlock | `task_lock()` |
| `nsproxy` | `alloc_lock` + RCU | spinlock + RCU | write: `task_lock()`; read: `rcu_read_lock()` |
| `cgroups` | `cgroup_mutex` + RCU | mutex + RCU | write: `cgroup_mutex`; read: `rcu_read_lock()` |
| `real_cred`, `cred` | RCU | RCU | write: `commit_creds()`; read: RCU or `current_cred()` |
| `children`, `sibling` | `tasklist_lock` | rwlock | write: write_lock; read: read_lock |
| `parent`, `real_parent` | `tasklist_lock` | rwlock | same as children |
| `pid`, `tgid` | Set once at fork | — | read-only after `copy_process()` |
| `__state` | memory barrier | smp_mb | `set_current_state()` macro |
| `se` (sched_entity) | `rq->lock` (run queue lock) | raw_spinlock | scheduler hold rq lock |

---

## 6. Object Graph

The `task_struct` is the root of a large object graph. Each pointer in the task
leads to another kernel data structure with its own lifecycle. The diagram below
shows the complete graph relevant to containers.

```
struct task_struct
  │
  ├── thread_pid ──────────────► struct pid
  │                                │  .numbers[0] = {nr: 12345, ns: &init_pid_ns}
  │                                │  .numbers[1] = {nr: 1,     ns: &container_pid_ns}
  │                                └── container_pid_ns ──► struct pid_namespace
  │                                                            .idr  (PID→pid mapping)
  │                                                            .child_reaper (PID 1 task)
  │
  ├── nsproxy ─────────────────► struct nsproxy
  │                                ├── pid_ns_for_children ──► struct pid_namespace
  │                                ├── net_ns              ──► struct net
  │                                │                              .dev_base_head (network interfaces)
  │                                │                              .loopback_dev  (lo interface)
  │                                ├── mnt_ns              ──► struct mnt_namespace
  │                                │                              .root (the container's / mountpoint)
  │                                ├── uts_ns              ──► struct uts_namespace
  │                                │                              .name.nodename (hostname)
  │                                ├── ipc_ns              ──► struct ipc_namespace
  │                                └── cgroup_ns           ──► struct cgroup_namespace
  │                                                              .root_cset (css_set at cgroup root)
  │
  ├── cgroups ─────────────────► struct css_set
  │                                ├── subsys[cpu_cgrp_id]    ──► struct cgroup_subsys_state
  │                                │                                │
  │                                │                                └── cgroup ──► struct cgroup
  │                                │                                                 .kn (kernfs node)
  │                                │                                                 path: /kubepods/...
  │                                ├── subsys[memory_cgrp_id] ──► struct cgroup_subsys_state
  │                                │                                └── mem_cgroup (memory accounting)
  │                                └── subsys[pids_cgrp_id]   ──► struct cgroup_subsys_state
  │                                                                  └── pids_cgroup (pid limit)
  │
  ├── mm ──────────────────────► struct mm_struct
  │                                ├── pgd (page table root → loaded into CR3)
  │                                ├── mm_mt (maple tree of VMAs)
  │                                ├── mmap_base, task_size
  │                                └── vm_area_struct entries: stack, heap, libs, ...
  │
  ├── fs ──────────────────────► struct fs_struct
  │                                ├── root (root dentry/mount — set by pivot_root)
  │                                └── pwd  (current working directory)
  │
  ├── files ───────────────────► struct files_struct
  │                                └── fdt ──► struct fdtable
  │                                              └── fd[] array of struct file *
  │
  ├── real_cred / cred ─────────► struct cred
  │                                ├── uid, gid, euid, egid
  │                                ├── cap_effective (capability bitmask)
  │                                └── user_ns ──► struct user_namespace
  │
  ├── se ──────────────────────► struct sched_entity (embedded, not a pointer)
  │                                ├── vruntime (CFS sort key)
  │                                ├── run_node (rb_node in cfs_rq)
  │                                └── cfs_rq ──► struct cfs_rq (the run queue)
  │
  └── signal ──────────────────► struct signal_struct
                                    ├── thread_head (list of all threads in group)
                                    ├── tty (controlling terminal)
                                    ├── rlim[] (resource limits)
                                    └── stats (accounting)
```

---

## 7. Live Observation

### 7.1 Reading `task_struct` fields via `/proc`

The `/proc` filesystem is a window into running `task_struct` instances. The kernel
translates `task_struct` fields to text on every read from `/proc`:

```bash
# The most important file: a summary of the task_struct
cat /proc/self/status
# Name:     bash                    ← comm
# State:    S (sleeping)            ← __state
# Tgid:     1234                    ← tgid
# Ngid:     0
# Pid:      1234                    ← pid (same as tgid for main thread)
# PPid:     1200                    ← real_parent->tgid
# TracerPid: 0
# Uid:      1000 1000 1000 1000    ← cred->uid, euid, suid, fsuid
# Gid:      1000 1000 1000 1000
# FDSize:   256                     ← files_struct.max_fds
# Groups:   1000 4 24 27 ...       ← cred->group_info
# NStgid:   1234 1                  ← tgid in each namespace level
# NSpid:    1234 1
# NSpgid:   1234 1
# NSsid:    1234 1
# Threads:  1                       ← signal->nr_threads
# VmPeak:   12345 kB                ← mm->hiwater_vm
# VmSize:   11000 kB                ← mm->total_vm
# VmRSS:    3200 kB                 ← RSS (physical pages mapped)
# CapPrm:   0000000000000000       ← cred->cap_permitted
# CapEff:   0000000000000000
# CapBnd:   000001ffffffffff       ← cred->cap_bset

# Process tree position
cat /proc/self/stat   # 52-field space-separated summary including ppid, state, prio
cat /proc/self/wchan  # kernel function the task is sleeping in (task->__state == TASK_INTERRUPTIBLE)
cat /proc/self/comm   # the comm field, 15 chars max
cat /proc/self/cmdline | tr '\0' ' '  # full command line from mm->arg_start

# Namespace membership
ls -la /proc/self/ns/  # each entry is a file descriptor to a namespace inode

# Per-thread view (all threads in the process)
ls /proc/self/task/   # subdirectory per thread (each has its own tid)
```

### 7.2 Measuring struct layout with `pahole`

```bash
# Full task_struct layout with field offsets
pahole /usr/lib/debug/boot/vmlinux-$(uname -r) -C task_struct | head -100

# Find the offset of a specific field
pahole /usr/lib/debug/boot/vmlinux-$(uname -r) -C task_struct | grep -w 'nsproxy'
# struct nsproxy *   nsproxy;   /* 1544   8 */
# This means nsproxy is at byte offset 1544, 8 bytes (a pointer)

# Show only holes (wasted space between fields)
pahole /usr/lib/debug/boot/vmlinux-$(uname -r) -C task_struct | grep hole

# Compare struct sizes across kernel versions
pahole /path/to/vmlinux-5.15 -C task_struct | tail -5
pahole /path/to/vmlinux-6.9  -C task_struct | tail -5
```

### 7.3 Live field access with `bpftrace`

```bash
# Print comm and pid of every new task being created
bpftrace -e 'kprobe:wake_up_new_task {
    $task = (struct task_struct *)arg0;
    printf("new task: pid=%d tgid=%d comm=%s\n",
        $task->pid, $task->tgid, $task->comm);
}'

# Show the __state transition as a task goes to sleep
bpftrace -e 'kprobe:schedule {
    if (curtask->__state != 0) {  /* not TASK_RUNNING */
        printf("sleeping: pid=%d state=%d comm=%s\n",
            curtask->pid, curtask->__state, curtask->comm);
    }
}'

# Trace namespace creation on clone() with CLONE_NEWPID
bpftrace -e 'kprobe:create_pid_namespace {
    printf("new pid_ns: creating task pid=%d\n", curtask->pid);
}'

# Show vruntime of tasks being scheduled OUT (prev task leaving CPU)
bpftrace -e 'tracepoint:sched:sched_switch {
    printf("out: comm=%s vruntime=%llu\n",
        args->prev_comm,
        ((struct task_struct *)curtask)->se.vruntime);
}'

# Print nsproxy address for all tasks in a specific PID namespace
# First find the namespace inode:
NS_INO=$(stat -L --format '%i' /proc/$(pidof nginx)/ns/pid)
# Step 2: now use this inode to filter — example:
# Note: kprobe:do_fork was replaced by kprobe:kernel_clone in Linux 5.10 (commit cad6967ac298)
bpftrace -e "kprobe:kernel_clone {
    \$task = (struct task_struct *)curtask;
    printf(\"fork: pid=%d nsproxy=%p\\n\", \$task->pid, \$task->nsproxy);
}"
```

### 7.4 Watching `task_struct` allocation and deallocation

```bash
# Count task_struct allocations per second (proxy: clone() calls)
bpftrace -e 'tracepoint:sched:sched_process_fork { @[args->parent_comm] = count(); }
             interval:s:1 { print(@); clear(@); }'

# Watch task exit (do_exit entry)
bpftrace -e 'kprobe:do_exit {
    printf("exit: pid=%d comm=%s code=%ld\n",
        curtask->pid, curtask->comm, arg0);
}'

# Trace the slab allocator directly for task_struct objects
# (requires CONFIG_DEBUG_SLAB or perf)
perf trace -e 'kmem:kmem_cache_alloc' -- sleep 1 \
  | grep task_struct
```

### 7.5 Container-specific observations

```bash
# Find the host PID of the process with container PID 1
# Given a container ID from 'docker ps' or 'crictl ps':
CONTAINER_ID="abc123"

# Method 1: via cgroup path
CGROUP_PATH=$(cat /sys/fs/cgroup/kubepods/*/*/$CONTAINER_ID/cgroup.procs 2>/dev/null | head -1)
echo "Host PID: $CGROUP_PATH"

# Method 2: via /proc namespace inspection
# Find all processes in the same PID namespace as the container
CONTAINER_PID_NS=$(readlink /proc/$(pgrep -f $CONTAINER_ID)/ns/pid)
for p in /proc/[0-9]*/ns/pid; do
    if [ "$(readlink $p 2>/dev/null)" = "$CONTAINER_PID_NS" ]; then
        PID=${p%/ns/pid}
        PID=${PID##*/proc/}
        echo "PID: $PID  comm: $(cat /proc/$PID/comm 2>/dev/null)"
    fi
done

# View NSpid (shows both host and container PID) for container process
cat /proc/<host-pid>/status | grep NSpid
# NSpid: 12345 1     ← host PID is 12345; container PID is 1
```

---

## Summary

`struct task_struct` is the kernel's complete representation of a process. Its ~9,280
bytes contain everything the kernel needs to schedule, identify, isolate, and account
for a running process.

For Kubernetes operators, the critical facts are:

1. **`nsproxy`** is the pointer that creates container isolation — it is what makes
   a container process "see" a different process tree, network, and filesystem.

2. **`cgroups` (css_set)** is the pointer that enforces resource limits — it maps
   the task to every cgroup subsystem simultaneously.

3. **`pid`/`tgid`** vs namespace-local PIDs (in `thread_pid->numbers[level]`) explains
   why the same process has a different PID from inside versus outside the container.

4. **`se.vruntime`** is the CFS sort key — it determines CPU scheduling fairness within
   a cgroup's CPU shares.

5. **`cred`** is immutable after assignment and holds all capability information —
   understanding it is prerequisite to understanding container privilege escalation
   vulnerabilities.

The lifecycle — `copy_process()` to `do_exit()` to `free_task_struct()` — is a
complete, auditable sequence. Every container creation is one traversal of this path.

**Next:** [kernel/01-b-clone-flags.md](01-b-clone-flags.md) — how `CLONE_NEWPID`,
`CLONE_NEWNET`, and `CLONE_NEWUSER` are handled inside `copy_process()`.
