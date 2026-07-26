# struct css_set — Kernel Deep Dive

## Source Locations

| File | Link |
|------|------|
| `include/linux/cgroup-defs.h` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/cgroup-defs.h |
| `include/linux/sched.h` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/sched.h |
| `kernel/cgroup/cgroup.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/cgroup/cgroup.c |

## The Atomicity Problem

Every process belongs to exactly one cgroup per controller. On a typical Linux system with the memory, cpu, io, pids, cpuset, and hugetlb controllers enabled, every process has six controller memberships simultaneously. A container on a Kubernetes node might use all twelve or more controllers that are compiled in.

The obvious representation would be to store a `struct cgroup *` per controller directly in `task_struct`:

```c
/* Hypothetical naive design — not what the kernel does */
struct task_struct {
    /* ... */
    struct cgroup *memory_cgroup;
    struct cgroup *cpu_cgroup;
    struct cgroup *pids_cgroup;
    struct cgroup *io_cgroup;
    /* ... and so on for each controller */
};
```

This representation has a fundamental problem the moment you need to move a task to a different cgroup. If you update `memory_cgroup` first and `cpu_cgroup` second, there is a window — however brief — during which a reader could observe the task in the new memory cgroup but still in the old cpu cgroup. No single atomic operation can update twelve pointers simultaneously. You could protect the set of writes with a lock, but then every read of a task's cgroup membership — which happens on *every memory allocation* and *every scheduler tick* — would require acquiring that lock. At thousands of allocations per second per process, the contention would be catastrophic.

## css_set: One Pointer to Rule Them All

The kernel's solution captures all twelve controller memberships in a single struct, `struct css_set`. A pointer to this struct is what `task_struct` actually contains:

```c
// include/linux/sched.h
struct task_struct {
    struct css_set __rcu *cgroups;  /* single RCU-protected pointer */
    struct list_head      cg_list; /* node in css_set.tasks */
    /* ... */
};
```

Moving a task to a new cgroup reduces to a single `rcu_assign_pointer()` call on `task->cgroups`. The old css_set and the new css_set are fully constructed before the swap happens. Any reader that loaded the old pointer before the swap sees a fully consistent view of the old membership; any reader that loads the new pointer after the swap sees a fully consistent view of the new membership. No reader ever sees a half-updated state.

This is RCU (Read-Copy-Update) in its most natural form. Reads require no lock — only `rcu_read_lock()` to prevent the old css_set from being freed while in use. Writes acquire `css_set_lock`, perform the swap, then wait for an RCU grace period before freeing the old css_set. The asymmetry is intentional: the read path runs billions of times per second on a busy node; the write path runs only when containers are created, destroyed, or explicitly migrated.

## struct css_set

```c
// include/linux/cgroup-defs.h
struct css_set {
    /* The core payload: one controller-state pointer per controller */
    struct cgroup_subsys_state *subsys[CGROUP_SUBSYS_COUNT];

    refcount_t refcount;           /* how many tasks use this css_set */

    struct css_set *dom_cset;      /* css_set for default (v2) hierarchy */
    struct cgroup  *dfl_cgrp;     /* the v2 cgroup this css_set lives in */

    int              nr_tasks;
    struct list_head tasks;        /* tasks using this css_set */
    struct list_head mg_tasks;     /* tasks mid-migration */
    struct list_head task_iters;   /* ongoing cgroup.procs reads */

    struct list_head e_cset_node[CGROUP_SUBSYS_COUNT]; /* per-controller cgroup links */

    struct hlist_node hlist;       /* node in css_set_table hash table */

    struct list_head cgrp_links;   /* links to all member cgroups */

    struct list_head threaded_csets;
    struct list_head threaded_csets_node;
};
```

### subsys[]: The Core Payload

`subsys[CGROUP_SUBSYS_COUNT]` is the heart of css_set. For each controller index, it holds a pointer to the per-cgroup state struct that this controller allocated for the cgroup this css_set belongs to. If the process is in `/sys/fs/cgroup/kubepods/pod123/`, then `subsys[memory_cgrp_id]` points to the `struct mem_cgroup` for `pod123/`, and `subsys[cpu_cgrp_id]` points to the `struct task_group` for `pod123/`. If the process were to move to a different pod's cgroup, a new css_set would be constructed with different pointers in `subsys[]`, and `task->cgroups` would be atomically updated to point to the new css_set.

The combination of pointer values in `subsys[]` is the css_set's identity. Two css_sets with identical `subsys[]` arrays are semantically identical — they represent the same cgroup membership combination — and the kernel deduplicates them.

### tasks and mg_tasks: The Task Lists

`tasks` is the linked list of all `task_struct` instances currently using this css_set, connected through each task's `task_struct.cg_list` field. When you read `cgroup.procs`, the kernel walks `tasks` to find every process in the cgroup.

`mg_tasks` is the list of tasks that are currently mid-migration — they have been removed from the old css_set's `tasks` list but have not yet been added to the new css_set's `tasks` list. This in-flight list exists to solve a subtle consistency problem: without it, a process reading `cgroup.procs` during a migration could observe a task in neither cgroup (having left one but not yet joined another), which would be confusing for tools that expect every task to have a visible cgroup. Tasks in `mg_tasks` are still visible to the kernel's internal accounting — they are not in limbo from the kernel's perspective — but they are held separately until migration completes.

### e_cset_node[]: The Bidirectional Link

`e_cset_node[i]` connects this css_set to the i-th controller's cgroup's list of css_sets. It is the second edge of a bidirectional graph: the css_set knows which cgroup it belongs to (via `subsys[i]->cgroup`), and the cgroup knows which css_sets belong to it (via `cgrp.cset_links`, which `e_cset_node[i]` is linked into).

This bidirectional link is what makes "enumerate all tasks in this cgroup" efficient. To find all processes in `/sys/fs/cgroup/kubepods/pod123/`, the kernel walks the pod cgroup's `cset_links` list to find every css_set that has membership there, then walks each css_set's `tasks` list. Without this linkage, finding all tasks would require a full scan of all css_sets on the system.

### hlist: The Deduplication Key

`hlist` is the node in `css_set_table`, the global hash table keyed on the contents of `subsys[]`. This is where the real performance optimization lives.

## Deduplication: Why the Common Case Is Cheap

On a Kubernetes node with 20 pods, you might have hundreds of processes — but if all containers within a pod have the same cgroup configuration, they share the same `struct mem_cgroup`, the same `struct task_group`, and the same css_set. The css_set's `refcount` tracks how many tasks are using it. On a node with 10 identical nginx replicas all configured with the same memory and CPU limits, those 10 processes might share a single css_set with `refcount=10`, even though they live in different pods. The shared css_set points to different cgroups for different pods — each pod's `struct mem_cgroup` is distinct — but the css_set structures themselves might be deduplicated.

The deduplication happens in `find_css_set()`:

```c
// kernel/cgroup/cgroup.c (conceptual — not verbatim source)
struct css_set *find_css_set(struct css_set *oldcset, struct cgroup *target_cgrp) {
    // 1. Compute the expected subsys[] array for (oldcset + one cgroup change)
    // 2. Hash that array and search css_set_table
    // 3. If found: css_set_get() and return the existing css_set
    // 4. If not found: allocate a new css_set, populate subsys[], insert into table
}
```

The hash key is the full set of `subsys[]` pointers. Two tasks that are in exactly the same combination of cgroups — same memory cgroup, same cpu cgroup, same pids cgroup, and so on for every controller — will hash to the same bucket and find the same css_set. The refcount is incremented and no allocation happens.

This deduplication is not merely a memory optimization. By reducing the number of distinct css_sets, the kernel reduces the cost of operations that must walk css_sets — including `cgroup.procs` reads and migrations. On a node with 300 processes all running without any per-process resource limits, there may be exactly one css_set with `refcount=300`. Every one of those 300 processes shares the same pointer, and reading `task->cgroups` returns the same address.

```bash
# Observe css_set count on your system
# Each unique line in /proc/cgroups corresponds to a controller;
# the number of actual css_sets is not directly exposed, but you can infer it:
cat /proc/cgroups
#  #subsys_name  hierarchy  num_cgroups  enabled
#  memory        0          54           1
#  cpu           0          54           1
```

## Migration: Writing to cgroup.procs

The full migration path, from userspace write to kernel completion:

```
write("/sys/fs/cgroup/target/cgroup.procs", "12345\n")
  └─ cgroup_procs_write()                     # file op on cgroup.procs kernfs node
       └─ __cgroup_procs_write()
            ├─ cgroup_kn_lock_live()          # acquire cgroup_mutex
            └─ cgroup_attach_task()
                 └─ cgroup_migrate()
                      ├─ cgroup_migrate_add_task()   # move task to mg_tasks
                      ├─ [for each controller that cares]
                      │    subsys->can_attach(tset)  # pre-flight checks
                      ├─ cgroup_migrate_execute()
                      │    ├─ find_css_set(old_cset, target_cgrp)
                      │    ├─ [new css_set found or created]
                      │    └─ rcu_assign_pointer(task->cgroups, new_cset)
                      │         ^ the atomic pivot
                      ├─ [for each controller]
                      │    subsys->attach(tset)      # post-migration bookkeeping
                      └─ cgroup_migrate_finish()
                           └─ css_set_put(old_cset)  # decrement old refcount
```

The `can_attach` callbacks run before the atomic pivot. The memory controller uses this to check whether moving the task would cause an immediate OOM: if the target cgroup is already at `memory.max` and the task's current memory usage would push it over, `can_attach` returns an error and the migration is refused. This is the right place for this check — it is far better to refuse the migration than to perform it and immediately kill the task.

After `can_attach` clears all controllers, `cgroup_migrate_execute()` performs the actual work. It calls `find_css_set()` to locate or create the appropriate css_set for the new membership, then does the `rcu_assign_pointer()`. From this point forward, all new resource accounting for this task runs through the new css_set.

`css_set_put(old_cset)` decrements the old css_set's reference count. If it reaches zero — meaning no other tasks are using this particular membership combination — the css_set is removed from `css_set_table` and freed via an RCU grace period callback.

```bash
# bpftrace: trace task migrations (completion point)
bpftrace -e 'kprobe:cgroup_migrate_finish {
    printf("migration complete: pid=%d comm=%s\n", pid, comm);
}'
```

## fork() Inheritance and pids_fork()

When a process calls `fork()`, the child inherits the parent's css_set:

```
copy_process()
  └─ cgroup_fork()                      # kernel/cgroup/cgroup.c
       └─ css_set_fork(child, parent)
            ├─ child->cgroups = parent->cgroups  # same pointer, refcount++
            └─ [for each controller with a fork callback]
                 subsys->fork(child)
```

No new css_set is allocated. The parent and child momentarily share one css_set, with the refcount incremented by one. This is correct because immediately after fork, the child is in the same cgroup as the parent. containerd or runc will move the child to its container cgroup by writing to `cgroup.procs` — but that happens after fork, not during it.

The `fork()` callbacks on each controller still run, however, even though no cgroup migration is happening. This is the mechanism by which `pids_fork()` enforces `pids.max`:

```c
// kernel/cgroup/pids.c (conceptual)
static int pids_fork(struct task_struct *child)
{
    struct pids_cgroup *pids = task_css(child, pids_cgrp_id);
    s64 new_count = atomic64_add_return(1, &pids->counter);
    if (new_count > atomic64_read(&pids->limit)) {
        atomic64_dec(&pids->counter);
        return -EAGAIN;
    }
    return 0;
}
```

The counter is incremented before the check (optimistically). If the new count exceeds the limit, it is decremented again and `-EAGAIN` is returned. This propagates through `copy_process()` back to `clone()` as a failure, and the child is never created. The parent's `fork()` call returns `EAGAIN`. From the container's perspective, the system has temporarily run out of resources for new processes — which is exactly the right error for a PID limit.

## Enumerating All Tasks in a Cgroup

Reading `cgroup.procs` walks the two-level structure:

```
cgroup.cset_links
  └─ (foreach cgrp_cset_link) → get css_set
       └─ css_set.tasks
            └─ (foreach task) → output task->pid
```

The outer loop uses `cgroup.cset_links`, a list of `struct cgrp_cset_link` objects. Each link is a bridge between a specific `struct cgroup` and a specific `struct css_set` that has membership in it. The inner loop walks `css_set.tasks`.

This two-level design reflects the deduplication: a single css_set may have membership in multiple cgroups (one per controller), and a single cgroup may have multiple css_sets in it (different tasks with slightly different memberships). The `cgrp_cset_link` struct is the junction table that makes both directions walkable.

The entire walk requires holding `css_set_lock`. Because `css_set_lock` is a spinlock, this means the walk cannot sleep — it must complete atomically. On a cgroup with tens of thousands of tasks, this is a real performance concern. There is a reason that reading `cgroup.procs` on a busy Kubernetes node takes measurably longer than on an idle one.

```bash
# Read cgroup.procs — triggers the two-level walk:
cat /sys/fs/cgroup/kubepods/pod<uid>/cgroup.procs

# Count processes without printing all PIDs:
wc -l /sys/fs/cgroup/kubepods/pod<uid>/cgroup.procs

# bpftrace: trace the start of a cgroup.procs read
bpftrace -e 'kprobe:cgroup_procs_start {
    printf("procs read started: pid=%d comm=%s\n", pid, comm);
}'
```

## The Full Chain: Pod YAML to Kernel Accounting

```
Pod spec:
  resources.limits.memory: 256Mi
  resources.limits.cpu: 500m

kubelet writes:
  /sys/fs/cgroup/kubepods/pod<uid>/memory.max = 268435456
  /sys/fs/cgroup/kubepods/pod<uid>/cpu.max = 50000 100000

Container runtime (containerd) calls clone3():
  copy_process()
    └─ cgroup_fork()
         └─ child->cgroups = parent->cgroups  # containerd's cgroup initially
              refcount++

containerd writes container PID to cgroup.procs:
  cgroup_migrate()
    └─ find_css_set() → new css_set for pod<uid>/<container-id> cgroup
         └─ subsys[memory_cgrp_id] = &(pod<uid> mem_cgroup)
         └─ subsys[cpu_cgrp_id] = &(pod<uid> task_group)
    └─ rcu_assign_pointer(task->cgroups, new_css_set)

Memory allocation inside container:
  try_charge()
    └─ rcu_read_lock()
    └─ cset = rcu_dereference(task->cgroups)
    └─ mem_cgrp = cset->subsys[memory_cgrp_id]  → cast to struct mem_cgroup *
    └─ check mem_cgrp->memory.usage vs mem_cgrp->memory.max
    └─ [over limit] → OOM kill
```

Every memory allocation inside the container walks this chain: `task->cgroups` → `subsys[memory_cgrp_id]` → `struct mem_cgroup` → `page_counter.usage` vs `page_counter.max`. The entire enforcement path for the 256Mi limit in the Pod spec terminates in a comparison between two integers in a struct. The complexity of cgroups, css_sets, and controller vtables exists to get those two integers into the right place at the right time.

## Key Kernel References

| Symbol | File | Link |
|--------|------|------|
| `struct css_set` | include/linux/cgroup-defs.h | https://elixir.bootlin.com/linux/v6.9/source/include/linux/cgroup-defs.h |
| `struct cgrp_cset_link` | include/linux/cgroup-defs.h | https://elixir.bootlin.com/linux/v6.9/source/include/linux/cgroup-defs.h |
| `task_struct.cgroups` | include/linux/sched.h | https://elixir.bootlin.com/linux/v6.9/source/include/linux/sched.h |
| `find_css_set()` | kernel/cgroup/cgroup.c | https://elixir.bootlin.com/linux/v6.9/source/kernel/cgroup/cgroup.c |
| `cgroup_migrate()` | kernel/cgroup/cgroup.c | https://elixir.bootlin.com/linux/v6.9/source/kernel/cgroup/cgroup.c |
| `cgroup_migrate_execute()` | kernel/cgroup/cgroup.c | https://elixir.bootlin.com/linux/v6.9/source/kernel/cgroup/cgroup.c |
| `cgroup_fork()` / `css_set_fork()` | kernel/cgroup/cgroup.c | https://elixir.bootlin.com/linux/v6.9/source/kernel/cgroup/cgroup.c |
| `pids_fork()` | kernel/cgroup/pids.c | https://elixir.bootlin.com/linux/v6.9/source/kernel/cgroup/pids.c |
| `css_set_lock` | kernel/cgroup/cgroup.c | https://elixir.bootlin.com/linux/v6.9/source/kernel/cgroup/cgroup.c |
