# struct cgroup — Kernel Deep Dive

## Source Location

| File | Link |
|------|------|
| `include/linux/cgroup-defs.h` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/cgroup-defs.h |
| `kernel/cgroup/cgroup.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/cgroup/cgroup.c |

## The Problem Cgroups Solve

For most of Unix's history, resource management was a per-process affair. You could set a process's priority with `nice`, limit its open file count with `setrlimit`, and send it a signal to terminate it. But you could not say: "all the processes belonging to this web server, collectively, may use no more than 2 GB of RAM and 1.5 CPUs." There was no first-class kernel concept for a *group* of processes as a resource-accounting unit.

This gap mattered less when servers ran one application at a time. It became intolerable when machines started running dozens of services simultaneously, and catastrophic when containers arrived and every server began hosting hundreds of isolated applications. Without group accounting, a single runaway process could consume all available memory and bring down every other tenant on the machine. Without group enforcement, CPU guarantees between services were advisory at best and fictional at worst.

Control groups — cgroups — are the kernel's answer. A cgroup is an administrative boundary around a set of processes. The kernel tracks resource consumption for the group as a whole and enforces limits at the group level. Every container you have ever run is backed by a cgroup. Every Kubernetes pod is a cgroup. The `256Mi` memory limit and `500m` CPU request in a Pod spec are ultimately `memory.max` and `cpu.max` entries in a cgroup directory.

## The Two Eras: v1's Fragmentation and v2's Unified Hierarchy

Cgroups were not designed all at once. They grew organically from separate teams adding separate controllers for separate purposes, and the architecture reflected that chaos.

**cgroup v1** (introduced in Linux 2.6.24, 2008) mounted each controller independently:

```
/sys/fs/cgroup/memory/
/sys/fs/cgroup/cpu/
/sys/fs/cgroup/cpuacct/
/sys/fs/cgroup/blkio/
/sys/fs/cgroup/pids/
```

Each of these was a separate filesystem instance, a separate hierarchy, a separate tree. This meant a process could be in `/sys/fs/cgroup/memory/app/frontend/` but simultaneously in `/sys/fs/cgroup/cpu/app/` — at different depths, in different subtrees, with no requirement that the two hierarchies even agree on what "app" means.

The consequences were severe. There was no atomic way to move a process across all controllers simultaneously. A migration that touched memory first, then cpu, was a window during which the process was partially moved — observable by anyone walking either hierarchy. Controllers had different file names for equivalent concepts, different semantics for inheritance, and different interpretations of "limit." The `memory` controller used `memory.limit_in_bytes`; the `cpu` controller used `cpu.cfs_quota_us` and `cpu.cfs_period_us`; the `blkio` controller had yet another set of files and throttling semantics. Writing a container runtime that used all of them correctly was an exercise in handling a dozen independent, subtly-incompatible interfaces.

Tejun Heo spent years fixing these problems and ultimately concluded that the v1 architecture was not fixable incrementally. **cgroup v2** (merged in Linux 4.5, 2016; production-ready in 4.15+) starts from a different premise: there is exactly one hierarchy, and all controllers share it.

```
/sys/fs/cgroup/          (mounted once: -t cgroup2)
```

In v2, a process lives at exactly one place in the tree. Moving it to a different cgroup is a single atomic operation — one `cgroup.procs` write — that covers all controllers at once. Every controller sees the same tree, uses consistent file naming, and respects consistent inheritance rules. Kubernetes 1.25 made cgroup v2 the default precisely because proper QoS enforcement, PSI pressure metrics, and per-pod swap accounting require it.

```bash
# Verify which mode your system uses
mount | grep cgroup
# Pure v2:  "cgroup2 on /sys/fs/cgroup type cgroup2"
# Mixed:    also shows "cgroup on /sys/fs/cgroup/<controller> type cgroup"

# Force pure v2
# Add to kernel command line: cgroup_no_v1=all
```

## struct cgroup

With the design context in place, `struct cgroup` makes sense as a whole. It is the kernel's representation of one node in the cgroup tree — one directory under `/sys/fs/cgroup`. Everything that can be said about a group of processes from a resource-management perspective is reachable from this struct.

```c
// include/linux/cgroup-defs.h (key fields — struct is much larger in source)
struct cgroup {
    /* kernfs backing — the filesystem directory */
    struct kernfs_node        *kn;
    struct cgroup_file         procs_file;
    struct cgroup_file         events_file;

    /* hierarchy navigation */
    struct cgroup_root        *root;
    struct cgroup             *parent;
    u64                        id;
    int                        level;
    int                        max_depth;
    int                        nr_descendants;

    /* lifetime */
    struct percpu_ref           self;
    atomic_t                    online_cnt;

    /* subsystem state — one pointer per controller */
    struct cgroup_subsys_state __rcu *subsys[CGROUP_SUBSYS_COUNT];

    /* task membership */
    struct list_head            cset_links;
    int                         nr_tasks;
    int                         nr_populated_csets;

    /* resource pressure */
    struct psi_group            psi;

    /* BPF programs attached to this cgroup */
    struct cgroup_bpf           bpf;

    /* controller enablement */
    u16                         subtree_control;
    u16                         subtree_ss_mask;
};
```

### The Filesystem Layer: `kn`, `procs_file`, `events_file`

The `kn` field is a `struct kernfs_node *` — the node that backs this cgroup's directory in the cgroupfs filesystem. When you `ls /sys/fs/cgroup/kubepods/`, every entry you see corresponds to a kernfs node. When you `open()` `/sys/fs/cgroup/kubepods/pod123/memory.max`, the VFS resolves the path, reaches this node, and dispatches to the memory controller's registered file operations. kernfs is a pseudo-filesystem layer the kernel uses specifically for these kinds of "everything is a file" interfaces; it handles inode management, directory notifications, and file operation dispatch so that individual subsystems like the cgroup memory controller don't have to.

`procs_file` and `events_file` are wrapper structs around specific kernfs files within the cgroup directory. They are kept as embedded fields — not pointers to heap-allocated objects — because the kernel needs to efficiently trigger `kernfs_notify()` on them when state changes. When you `poll()` on `cgroup.events` waiting for a cgroup to become empty, the wakeup comes from a `kernfs_notify()` call on `events_file`. When you write a PID to `cgroup.procs`, the request lands in `cgroup_procs_write()`, which resolves to `procs_file` and from there to the migration machinery.

### The Tree: `root`, `parent`, `id`, `level`

`root` points to the `struct cgroup_root` that owns this hierarchy. In cgroup v2 there is exactly one root — `cgrp_dfl_root`, defined at the top of `kernel/cgroup/cgroup.c`. Every cgroup on the system traces back to it. `root` gives you fast access to which subsystems are active (`root->subsys_mask`) and the root cgroup itself (`root->cgrp`).

`parent` is the immediate parent in the tree — NULL only for the root cgroup itself. The parent relationship drives nearly everything: controller settings are inherited from parent to child during `mkdir`, resource limits walk up the tree to check for ancestor constraints, and `rmdir` refuses to proceed if a cgroup still has descendants. The NULL check on `parent` is the sentinel that prevents `cgroup_rmdir()` from attempting to delete the root.

`id` is a kernel-assigned 64-bit identifier, unique across the lifetime of the system and visible to userspace via `cgroup.id`. It is the value returned by `bpf_get_current_cgroup_id()` in BPF programs, used in audit logs, and referenced by systemd when it tracks cgroup lifetimes. The kernel assigns IDs via `idr_alloc()` and does not reuse them until the cgroup is destroyed and a new one created in its place.

`level` records depth from the root: 0 for the root, 1 for its direct children, and so on. On a Kubernetes node you will typically see pod cgroups at level 2 or 3 (`kubepods/besteffort/podUID`) and container cgroups one level deeper. It matters for enforcing `max_depth` limits and for tools that display the tree — `systemd-cgls` uses it to compute indentation.

### Lifetime: `self`, `online_cnt`

`self` is a `struct percpu_ref` — the primary reference counter governing when this `struct cgroup` can be freed. `percpu_ref` is not a plain atomic; it maintains a per-CPU counter array, so incrementing a reference from any CPU only touches that CPU's cache line rather than bouncing a shared atomic across cores. This matters because cgroup lookups happen on every memory allocation, every scheduler tick, and every syscall that the kernel must account — the reference paths are extremely hot.

The percpu design has a cost: summing all per-CPU slots is expensive, so it is only done when the refcount approaches zero and the system is trying to tear the cgroup down. The transition from "fast percpu mode" to "atomic mode" happens via `percpu_ref_kill()`, which marks the ref as dying and allows the sum to be computed. This is why cgroup destruction is an asynchronous process — you cannot immediately know the refcount is zero, so teardown is scheduled through a work queue.

`online_cnt` is a simpler `atomic_t` that tracks how many processes are currently running in this cgroup's subtree. Combined with `nr_descendants`, it determines the `populated` state that `cgroup.events` reports.

### The Bridge to Controllers: `subsys[]`

`subsys[CGROUP_SUBSYS_COUNT]` is the most important field in `struct cgroup`. It is an array of pointers, one slot per compiled-in controller, where each slot holds a pointer to that controller's per-cgroup state struct.

The memory controller's state for this cgroup lives at `subsys[memory_cgrp_id]`. It is a `struct cgroup_subsys_state *` — but that pointer actually points to the beginning of a `struct mem_cgroup`, because `mem_cgroup` embeds `struct cgroup_subsys_state` as its first member. The cpu controller's state at `subsys[cpu_cgrp_id]` is really a `struct task_group`. The pids controller's state is a `struct pids_cgroup`. In each case the common header (`struct cgroup_subsys_state`) is first, so a blind cast between the generic type and the controller-specific type is safe.

The `__rcu` annotation on these pointers means readers must use `rcu_dereference()` to access them. Controller state can be toggled while the system is running — enabling a controller on a cgroup causes a new state struct to be allocated and installed — and RCU ensures that code holding a pointer to an old controller state can finish using it safely even if it has been replaced.

When a process allocates memory, the allocator calls `mem_cgroup_charge()`, which reads `task->cgroups->subsys[memory_cgrp_id]` to find the `struct mem_cgroup` for this process's cgroup. The entire journey from "I need 4 KB" to "does this cgroup have budget for 4 KB" runs through this pointer chain on every allocation. Its performance matters enormously.

### Task Membership: `cset_links`, `nr_tasks`, `nr_populated_csets`

These fields answer the question "which tasks are in this cgroup?" but they do so indirectly, through `css_set` structs rather than a direct task list. The reason is architectural and worth understanding.

Naively, you might store a list of `task_struct *` directly on each cgroup. But a task belongs to multiple cgroups — one per controller. If the system has 12 controllers and a task changes its memory cgroup, you would need to atomically remove it from the old memory cgroup's task list and add it to the new one, while simultaneously keeping the cpu cgroup's list, the pids cgroup's list, and so on all consistent. That atomicity problem is intractable without a coarse lock that would serialize all I/O on all cgroup files.

The solution is `struct css_set` — a struct that captures one unique combination of (memory cgroup, cpu cgroup, pids cgroup, ...) for a set of tasks. Migrating a task to a new cgroup reduces to a single pointer swap: `task->cgroups = new_css_set`. The old and new css_set each contain all the controller pointers; the swap is atomic.

`cset_links` is the list head that connects this `struct cgroup` to all `css_set` instances that include this cgroup in their membership. To enumerate all tasks in a cgroup, the kernel walks `cset_links` to find every css_set that references this cgroup, then walks each css_set's task list. Reading `cgroup.procs` traverses this two-level structure.

`nr_tasks` counts processes directly in this cgroup (not descendants). It must be zero before `rmdir` will succeed. `nr_populated_csets` counts how many of those css_sets actually have tasks, avoiding a full walk to compute the `populated` state for `cgroup.events`.

### Resource Pressure: `psi`

`psi` is an embedded `struct psi_group` — the Pressure Stall Information accounting data for this cgroup. PSI tracks the fraction of time that tasks in the cgroup are stalled waiting for CPU, memory, or I/O. "Some" pressure means at least one task is stalled; "full" pressure means *all* runnable tasks are stalled, which is the more dangerous condition.

The mechanism works by noting the exact timestamp whenever a task transitions to a waiting state and recording when it resumes. The accumulated stall time, divided by elapsed wall time, gives a percentage. These percentages are computed as exponentially-weighted moving averages over 10-second, 60-second, and 300-second windows.

PSI was the prerequisite for Kubernetes's memory eviction improvements in 1.22+. Before PSI, the kubelet detected memory pressure by polling `/proc/meminfo`. PSI lets it detect per-cgroup memory pressure with millisecond latency by reading `memory.pressure`. The difference between "this node is under memory pressure" (system-wide) and "this specific pod is under memory pressure" (cgroup-scoped) is what allows the kubelet to evict the right pod rather than reacting to aggregate pressure.

### BPF Integration: `bpf`

`bpf` is a `struct cgroup_bpf` that holds BPF programs attached to this cgroup at each of several hook points: `BPF_CGROUP_INET_INGRESS`, `BPF_CGROUP_INET_EGRESS`, `BPF_CGROUP_SOCK_OPS`, `BPF_LSM_CGROUP`, and others. BPF programs attached to a parent cgroup are inherited by all descendants through an "effective program array" mechanism — when a packet arrives at a socket owned by a process in a child cgroup, the kernel runs both the child cgroup's programs and any inherited programs from ancestors.

This is the foundation of Cilium's network policy enforcement. When you create a Kubernetes NetworkPolicy, Cilium translates it into BPF programs attached to the `cgroup_bpf` of the relevant pod cgroups. There is no iptables, no netfilter — the policy runs as BPF code directly in the socket path, inspecting packets against the policy and dropping or allowing them before they ever reach the network stack.

### Controller Enablement: `subtree_control`, `subtree_ss_mask`

`subtree_control` is a bitmask of controllers enabled for the children of this cgroup. It is what you write to `cgroup.subtree_control`. Setting the memory bit here means that child cgroups created under this cgroup will have `memory.max`, `memory.current`, `memory.stat`, and the other memory controller files available. The bit does not enable the controller in this cgroup itself — it enables it for the cgroup's children.

The design follows a delegation principle: you can only enable a controller if the parent cgroup has enabled it for you. The kubelet enables `memory`, `cpu`, `io`, and `pids` on the `kubepods/` cgroup at node startup, which is why every pod cgroup automatically has those controller files. If you create a custom cgroup outside `kubepods/` and forget to enable a controller at each level of the path, the files will simply not appear.

`subtree_ss_mask` is the inverse view: which controllers have tasks *somewhere in this cgroup's subtree*. The kernel maintains this automatically. It prevents you from disabling a controller on a cgroup when tasks inside it are actively using that controller's accounting — doing so would leave those tasks' memory unaccounted, which would be a correctness violation.

## struct cgroup_root

Each hierarchy — in v2 there is only one — is represented by a `struct cgroup_root`:

```c
// include/linux/cgroup-defs.h (simplified)
struct cgroup_root {
    struct kernfs_root    *kf_root;     // the kernfs filesystem root
    unsigned int           subsys_mask; // bitmask of subsystems attached
    int                    hierarchy_id;
    struct cgroup          cgrp;        // the root cgroup (embedded, not a pointer)
    atomic_t               nr_cgrps;   // total cgroup count in this hierarchy
    struct list_head       root_list;  // list of all roots (v1 had many; v2 has one)
    unsigned int           flags;      // CGRP_ROOT_* flags
    char                   name[MAX_CGROUP_ROOT_NAMELEN];
};
```

Notice that `cgrp` is an embedded struct, not a pointer. The root cgroup is physically inside the `cgroup_root`. This means `&cgrp_dfl_root.cgrp` is literally the cgroup v2 root cgroup — no heap allocation required. In the cgroup v2 source, `cgrp_dfl_root` is defined as a file-scope global:

```c
// kernel/cgroup/cgroup.c
struct cgroup_root cgrp_dfl_root = { .cgrp.self.flags = CSS_NO_REF };
```

The `CSS_NO_REF` flag on the root cgroup's embedded `cgroup_subsys_state` tells the reference-counting machinery never to try to drop a reference on the root — the root cgroup is never freed.

## Lifecycle: Creation, Migration, and Destruction

A cgroup is born when userspace (or the kubelet) calls `mkdir` on the cgroupfs:

```
mkdir /sys/fs/cgroup/kubepods/pod123/
  └─ kernel_mkdir()
       └─ cgroup_mkdir()                     # kernel/cgroup/cgroup.c
            ├─ cgroup_create()
            │    ├─ kzalloc(sizeof(*cgrp))   # allocate the struct cgroup
            │    ├─ kernfs_create_dir()      # create the directory entry
            │    └─ for each active controller:
            │         css_alloc()            # allocate per-controller state
            │         css_online()           # make it active
            └─ cgroup_apply_control()        # propagate controller inheritance
```

`cgroup_create()` allocates the `struct cgroup`, initializes every field, then walks every controller enabled in the parent's `subtree_control` bitmask and calls that controller's `css_alloc()` callback. For the memory controller this allocates a `struct mem_cgroup`; for the cpu controller it allocates a `struct task_group` with per-CPU scheduling entities. If any `css_alloc()` fails — say, the system is critically low on memory — every previously allocated controller state is freed and the `mkdir` returns an error. There are no partial states left in the tree.

Once all controller states are allocated, `css_online()` is called on each. This is where controllers register their state with the rest of the kernel — the memory controller sets up per-NUMA memory zone accounting, the CPU controller links the new task group into the CFS scheduler hierarchy. Only after `css_online()` completes for all controllers is the cgroup visible to the task migration machinery.

Moving a task into the cgroup requires writing its PID to `cgroup.procs`. This triggers `cgroup_procs_write()`, which calls `cgroup_migrate()` after acquiring `cgroup_mutex`:

```
write("cgroup.procs", "12345\n")
  └─ cgroup_procs_write()
       └─ cgroup_migrate()
            ├─ subsys->can_attach(tset)    # pre-flight check by each controller
            ├─ cgroup_migrate_execute()
            │    └─ task->cgroups = new_css_set  # the atomic pivot
            └─ subsys->attach(tset)        # post-migration notifications
```

The `can_attach()` callbacks run before any state is changed. The memory controller uses this hook to perform limit checks: if moving this task into the target cgroup would cause an immediate OOM (because the cgroup is already at `memory.max`), `can_attach()` returns an error and the migration is rejected before anything is modified. This is the right behavior — it is far better to reject a migration than to complete it and immediately OOM kill the task.

The atomic pivot is the `task->cgroups = new_css_set` assignment inside `cgroup_migrate_execute()`. After this point, every memory allocation by this task charges the new cgroup. Every CPU scheduler tick accounts to the new cgroup's CPU quota. The task has, from the kernel's perspective, moved.

Destruction is the reverse, with one important constraint: a cgroup cannot be removed while it has tasks or live descendants. The check is strict:

```
rmdir /sys/fs/cgroup/pod123/
  └─ cgroup_rmdir()
       └─ cgroup_destroy_locked()       # only if nr_tasks==0 and nr_descendants==0
            ├─ for each controller:
            │    css_offline()          # mark controller state as dying
            │    schedule css_free()    # schedule deallocation via work queue
            └─ kernfs_remove()          # remove the directory entry
```

The asynchronous teardown via work queue is necessary because of RCU. Other CPUs may hold RCU read locks with references to the old `cgroup_subsys_state` pointers. The controller state cannot be freed until a full RCU grace period has elapsed. Work queues handle this naturally: by the time the work item runs, all previous RCU readers have completed.

## Locking Discipline

Four mechanisms protect cgroup data, each guarding a different scope:

**`cgroup_mutex`** is the global structural lock. It serializes `mkdir`, `rmdir`, controller enable/disable operations, and anything that reshapes the hierarchy. It is a `mutex` (sleeping lock), appropriate because these operations are not on the fast path. The rule is: if you are modifying the tree topology or a cgroup's controller configuration, you hold `cgroup_mutex`.

**`css_set_lock`** is a spinlock protecting task membership. It serializes writes to `task->cgroups`, modifications to css_set task lists, and changes to `css_set` reference counts. Because it can be acquired from code paths triggered by task exit — which can happen in interrupt context on some architectures — it must be acquired with IRQs disabled (`spin_lock_irq`). This is the lock that makes reading `cgroup.procs` on a busy cgroup measurably expensive: the kernel must hold it for the entire duration of the task list walk.

**RCU** is what makes the common case — "what cgroup is this task in right now?" — essentially free. `task->cgroups` is an RCU-protected pointer. Any code that needs to read a task's cgroup simply calls `rcu_read_lock()` and `rcu_dereference(task->cgroups)`. No cache-line bouncing, no blocking, no contention. The write side (task migration) holds `css_set_lock`, does the swap with `rcu_assign_pointer()`, and waits for a grace period before freeing the old css_set. The asymmetry is intentional: reads are a billion-times-more-common than writes, so the read path must be the one that pays zero overhead.

**`kernfs_rwsem`** (inside kernfs) protects the directory entries and the lifetime of kernfs nodes. It is largely an internal kernfs concern — cgroup code calls `kernfs_create_dir()` and `kernfs_remove()` which acquire it internally. The ordering constraint matters: `cgroup_mutex` must always be acquired before `kernfs_rwsem` if both are needed. Inverting this order causes deadlocks; the kernel's lockdep annotations encode this constraint and will warn if it is violated.

## Object Graph

The full data structure graph, from a Kubernetes pod perspective:

```
task_struct (container process)
  └─ cgroups ──────────────────────────► struct css_set
         (RCU ptr, one per task)             │
                                             ├─ subsys[memory_cgrp_id] ──► struct mem_cgroup
                                             │                                 └─ css.cgroup ──► struct cgroup
                                             │                                                        (pod memory cgroup)
                                             │                                 └─ memory.max = 256 MiB
                                             │
                                             ├─ subsys[cpu_cgrp_id] ──────► struct task_group
                                             │                                 └─ css.cgroup ──► struct cgroup
                                             │                                                        (same pod cgroup)
                                             │                                 └─ cfs_bandwidth.quota = 50000 µs
                                             │
                                             └─ subsys[pids_cgrp_id] ────► struct pids_cgroup
                                                                              └─ css.cgroup ──► struct cgroup
                                                                              └─ limit = 100 pids
```

The critical pattern is `struct cgroup_subsys_state` — the common header that appears at offset 0 in every controller-specific struct:

```c
struct cgroup_subsys_state {
    struct cgroup        *cgroup;   // back-pointer to the cgroup
    struct cgroup_subsys *ss;       // which controller this belongs to
    struct percpu_ref     refcnt;   // reference count
    unsigned long         flags;    // CSS_NO_REF, CSS_ONLINE, CSS_DYING, CSS_DEAD
    struct cgroup_subsys_state *parent; // parent's css for this controller
};

struct mem_cgroup {
    struct cgroup_subsys_state css;  // MUST be first — enables the cast
    // ... memory controller fields ...
};
```

Because `css` is first, a `struct cgroup_subsys_state *` that points to a `mem_cgroup` can be safely cast to `struct mem_cgroup *` using `container_of`. This single-inheritance-by-embedding pattern appears throughout the kernel. It gives the cgroup core a uniform interface — it deals only in `struct cgroup_subsys_state *` — while letting each controller add whatever fields it needs.

The css_set is the deduplication key: if 50 containers on a node all have the same memory limit and cpu quota, they may share one css_set (50 tasks, refcount=50) that points to the same `mem_cgroup` and `task_group`. When one of those containers gets a different CPU limit, its tasks get a new css_set pointing to a new `task_group`, while the shared `mem_cgroup` remains. This sharing is invisible to userspace but has a real effect on kernel memory usage at node scale.

## Live Observation

```bash
# Find a process's cgroup path (v2 shows one line: "0::<path>")
cat /proc/$PID/cgroup

# Navigate to the cgroup
CGROUP=$(cat /proc/$PID/cgroup | sed 's/0:://')
ls /sys/fs/cgroup$CGROUP

# Read the cgroup's kernel-assigned unique ID
cat /sys/fs/cgroup$CGROUP/cgroup.id

# List all processes directly in this cgroup
cat /sys/fs/cgroup$CGROUP/cgroup.procs

# List all processes including descendants
find /sys/fs/cgroup$CGROUP -name cgroup.procs -exec cat {} +

# Which controllers are available at the root
cat /sys/fs/cgroup/cgroup.controllers
# Example: cpuset cpu io memory hugetlb pids rdma misc

# Which controllers are enabled for children of this cgroup
cat /sys/fs/cgroup$CGROUP/cgroup.subtree_control

# Memory usage and limits
cat /sys/fs/cgroup$CGROUP/memory.current   # bytes in use right now
cat /sys/fs/cgroup$CGROUP/memory.max       # hard limit ("max" = unlimited)
cat /sys/fs/cgroup$CGROUP/memory.stat      # anon, file, kernel, slab breakdown

# Check CPU quota and throttling
cat /sys/fs/cgroup$CGROUP/cpu.max          # "<quota_us> <period_us>"
cat /sys/fs/cgroup$CGROUP/cpu.stat         # nr_throttled tells you if it's hitting limits

# PSI pressure — per-cgroup stall percentages
cat /sys/fs/cgroup$CGROUP/memory.pressure  # some/full, 10s/60s/300s averages

# Tree view
systemd-cgls
systemd-cgls /kubepods.slice    # scoped to Kubernetes pod cgroups

# bpftrace: trace every cgroup creation (every mkdir on cgroupfs)
bpftrace -e 'kprobe:cgroup_mkdir {
    printf("pid=%d comm=%s creating new cgroup\n", pid, comm);
}'

# bpftrace: trace task migration between cgroups
bpftrace -e 'kprobe:cgroup_migrate_finish {
    printf("pid=%d comm=%s completed cgroup migration\n", pid, comm);
}'

# bpftrace: trace OOM kills within cgroups
bpftrace -e 'kprobe:mem_cgroup_out_of_memory {
    printf("OOM kill: pid=%d comm=%s\n", pid, comm);
}'

# bpftrace: observe the cgroup ID on every fork (matches bpf_get_current_cgroup_id())
bpftrace -e 'tracepoint:sched:sched_process_fork {
    printf("fork: parent=%d child=%d cgroup_id=%llu\n",
        args->parent_pid, args->child_pid, cgroup_id);
}'
```

## Key Kernel References

| Symbol | File | Link | Purpose |
|--------|------|------|---------|
| `struct cgroup` | include/linux/cgroup-defs.h | https://elixir.bootlin.com/linux/v6.9/source/include/linux/cgroup-defs.h | Core cgroup struct |
| `struct cgroup_root` | include/linux/cgroup-defs.h | https://elixir.bootlin.com/linux/v6.9/source/include/linux/cgroup-defs.h | Hierarchy root struct |
| `struct cgroup_subsys_state` | include/linux/cgroup-defs.h | https://elixir.bootlin.com/linux/v6.9/source/include/linux/cgroup-defs.h | Common header for per-cgroup controller state |
| `cgroup_mkdir()` | kernel/cgroup/cgroup.c | https://elixir.bootlin.com/linux/v6.9/source/kernel/cgroup/cgroup.c | Create a cgroup (mkdir handler) |
| `cgroup_migrate()` | kernel/cgroup/cgroup.c | https://elixir.bootlin.com/linux/v6.9/source/kernel/cgroup/cgroup.c | Move a task between cgroups |
| `cgroup_destroy_locked()` | kernel/cgroup/cgroup.c | https://elixir.bootlin.com/linux/v6.9/source/kernel/cgroup/cgroup.c | Tear down a cgroup |
| `cgrp_dfl_root` | kernel/cgroup/cgroup.c | https://elixir.bootlin.com/linux/v6.9/source/kernel/cgroup/cgroup.c | The single cgroup v2 root instance |
| `css_set_lock` | kernel/cgroup/cgroup.c | https://elixir.bootlin.com/linux/v6.9/source/kernel/cgroup/cgroup.c | Spinlock protecting css_set membership |
