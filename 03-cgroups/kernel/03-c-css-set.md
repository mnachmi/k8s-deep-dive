# 03-C — struct css_set: Efficient Process-to-Cgroup Membership

## Introduction

A process belongs to exactly one cgroup per controller. With 12+ controllers available in a typical Linux system (memory, cpu, pids, blkio, cpuset, and so on), a naive design would store one `cgroup *` pointer per controller directly in `task_struct`. That approach has a painful atomicity problem: every task migration would require updating all those pointers at once, with no simple way to guarantee consistency if a reader races the writer halfway through. The kernel's solution is `struct css_set`: a single struct that groups all per-controller state pointers for one unique combination of cgroup memberships. Moving a task then reduces to a single pointer swap — `task->cgroups` — instead of a dozen. Because most processes on a node share identical cgroup memberships (all processes in the same Kubernetes pod land in the same cgroup path), `css_set` instances are deduplicated via a global hash table keyed on the full combination of controller pointers. On a lightly-configured node with 300 processes all running without resource limits, there may be just one `css_set` with `refcount=300`, not 300 separate structs.

## Section 1 — struct css_set

```c
// include/linux/cgroup-defs.h
struct css_set {
    /*
     * Set of subsystem states, one per subsystem.
     * This is the set a process belongs to when it's in this css_set.
     */
    struct cgroup_subsys_state *subsys[CGROUP_SUBSYS_COUNT];

    /* reference count — how many tasks use this css_set */
    refcount_t      refcount;

    /* default hierarchy pointer */
    struct css_set *dom_cset;    // css_set for the default (v2) hierarchy
    struct cgroup  *dfl_cgrp;   // cgroup in the default hierarchy

    /* task list */
    int             nr_tasks;    // number of tasks in this set
    struct list_head tasks;      // list of tasks via task_struct.cg_list
    struct list_head mg_tasks;   // tasks mid-migration (being moved)
    struct list_head task_iters; // ongoing task iterators (for cgroup.procs reads)

    /* per-subsystem list nodes */
    struct list_head e_cset_node[CGROUP_SUBSYS_COUNT]; // list in each cgroup's cset_links

    /* hash table node for css_set_table */
    struct hlist_node hlist;

    /* links to all member cgroups */
    struct list_head cgrp_links; // list of cgrp_cset_link structs

    /* threaded css_sets under this one */
    struct list_head threaded_csets;
    struct list_head threaded_csets_node;
};
```

Source: https://elixir.bootlin.com/linux/v6.9/source/include/linux/cgroup-defs.h

Field-by-field explanation:

- `subsys[CGROUP_SUBSYS_COUNT]`: the core payload. For each enabled controller (indexed by its controller ID), this array holds a pointer to the per-cgroup state struct allocated by that controller — `struct mem_cgroup *`, `struct task_group *`, `struct pids_cgroup *`, and so on. The controller's `css_alloc()` callback created each of these objects when the cgroup was first created.

- `refcount`: atomic reference count. Incremented when a task joins this css_set (via `css_set_get()`), decremented when a task leaves (via `css_set_put()`). When the count drops to zero, all memory associated with the css_set is freed and it is removed from the hash table.

- `dfl_cgrp`: for cgroup v2, points to the single cgroup this css_set lives in on the unified hierarchy. On a pure v2 system this is the primary cgroup for the process.

- `tasks`: the linked list of all `task_struct` instances currently using this css_set. Each task participates via `task_struct.cg_list`. This is the list iterated when reading `cgroup.procs`.

- `mg_tasks`: tasks that are currently mid-migration — they have been detached from their old css_set but have not yet completed the move to the new one. Keeping them on a separate list prevents `cgroup.procs` reads from seeing a task twice or not at all during migration.

- `e_cset_node[i]`: the list node that hooks this css_set into the i-th controller's cgroup's list of css_sets. This bidirectional linkage is how the kernel answers "give me all tasks in this memory cgroup": walk the cgroup's css_set list via `e_cset_node`, then for each css_set walk its `tasks` list.

- `hlist`: the node in `css_set_table`, the global hash table keyed on the combination of `subsys[]` pointers. This enables the kernel to find an existing css_set matching a desired membership combination in O(1) average time instead of allocating a new one every time.

- `cgrp_links`: a list of `cgrp_cset_link` structs, one for every cgroup that this css_set has membership in. In a pure v2 system with a single hierarchy this is typically one entry; in a mixed v1/v2 system it can be one per controller.

## Section 2 — How task_struct Links to css_set

```c
// include/linux/sched.h (relevant fields)
struct task_struct {
    /* ... */
    struct css_set __rcu    *cgroups;    // RCU-protected pointer to css_set
    struct list_head         cg_list;   // node in css_set.tasks or css_set.mg_tasks
    /* ... */
};
```

Source: https://elixir.bootlin.com/linux/v6.9/source/include/linux/sched.h

Reading `task->cgroups` requires holding the RCU read lock and dereferencing through `rcu_dereference(task->cgroups)`. RCU allows lockless reads because the pointer swap during migration is a single atomic store, and old readers are protected until the next RCU grace period.

Writing `task->cgroups` — i.e., migrating a task — requires acquiring `css_set_lock` (a spinlock) before swapping the pointer, and then waiting for an RCU grace period before freeing the old css_set. This ensures that any reader that loaded the old pointer before the swap can finish safely.

The `css_set_lock` spinlock (defined in `kernel/cgroup/cgroup.c`) protects:

- `task->cgroups` pointer modifications
- `css_set.tasks` list modifications
- `css_set.refcount` changes
- `css_set_table` hash table modifications

## Section 3 — The css_set Hash Table: Deduplication

The key optimization: when a new task is created or a task migrates to a different cgroup, the kernel must find or create a `css_set` matching the resulting combination of cgroup memberships. Allocating a fresh struct every time would negate the sharing benefit entirely.

```c
// kernel/cgroup/cgroup.c (conceptual — not exact source)
// Global hash table: keyed on the full subsys[] pointer array
static struct hlist_head css_set_table[CSS_SET_TABLE_SIZE];
static DEFINE_SPINLOCK(css_set_lock);

// Finding an existing css_set:
struct css_set *find_css_set(struct css_set *oldcset, struct cgroup *cgrp) {
    // 1. Compute hash from (oldcset + target cgroup) combination
    // 2. Walk css_set_table bucket, compare subsys[] arrays
    // 3. If found: css_set_get() and return it
    // 4. If not found: allocate new css_set, populate subsys[], insert into table
}
```

Source: https://elixir.bootlin.com/linux/v6.9/source/kernel/cgroup/cgroup.c

Concrete example: on a node with 300 processes all in the same cgroup configuration and no per-process resource limits applied, there is likely ONE `css_set` with `refcount=300`, not 300 separate structs. On a Kubernetes node with 20 pods, you would expect roughly 20–40 css_sets — one per distinct cgroup membership combination. Each pod introduces a new cgroup path (and therefore a new `subsys[]` combination), but all containers within the same pod that land in the same cgroup share a single css_set.

## Section 4 — Task Migration: Writing to cgroup.procs

Step-by-step trace of what happens when a PID is written to `cgroup.procs`:

```
write("cgroup.procs", "12345\n")
  └─ cgroup_procs_write()                     # kernel/cgroup/cgroup.c
       └─ __cgroup_procs_write()
            ├─ cgroup_kn_lock_live()          # acquire cgroup_mutex for target cgroup
            └─ cgroup_attach_task()
                 └─ cgroup_migrate()
                      ├─ cgroup_migrate_add_task()  # add task to migration set (mg_tasks)
                      ├─ [for each subsystem that cares]
                      │    subsys->can_attach(tset)  # pre-check (e.g., memory can check limits)
                      ├─ cgroup_migrate_execute()
                      │    ├─ [for each migrating task]
                      │    │    task_css_set_replace(task, new_cset)  # swap task->cgroups
                      │    └─ [for each subsystem]
                      │         subsys->attach(tset)    # e.g., mem_cgroup_attach()
                      └─ cgroup_migrate_finish()
                           └─ css_set_put(old_cset)  # decrement refcount on old css_set
```

After migration, `task->cgroups` points to the new css_set. The old css_set's refcount is decremented; if it reaches zero, it is removed from `css_set_table` and freed. The `can_attach()` callbacks run before any state is changed, giving controllers an opportunity to reject the migration (for example, if a memory controller would immediately OOM under the target cgroup's limits).

## Section 5 — Enumerating All Tasks in a Cgroup

When you `cat /sys/fs/cgroup/kubepods/pod<uid>/cgroup.procs`, the kernel iterates:

```
cgroup.cset_links → (foreach css_set in this cgroup)
  css_set.tasks → (foreach task in this css_set)
    yield task->pid
```

The outer iteration walks the cgroup's list of `cgrp_cset_link` structures, each of which points back to a css_set that has membership in this cgroup. The inner iteration then walks `css_set.tasks`. Reading `cgroup.procs` requires holding `css_set_lock` — the task lists are protected by it — which is why reading this file has non-trivial overhead on cgroups with many tasks.

## Section 6 — fork() and css_set Inheritance

When a process forks, the child inherits the parent's css_set:

```
copy_process()
  └─ cgroup_fork()                    # kernel/cgroup/cgroup.c
       └─ css_set_fork(child, parent) # child gets same css_set, refcount++
            └─ [for each controller]
                 subsys->fork(child)  # e.g., pids_fork() checks pids.max
```

The key invariant: immediately after `fork()`, parent and child share one css_set with an incremented refcount. No new css_set is allocated. However, each controller's `fork()` callback runs and can enforce limits. `pids_fork()` is the enforcement point for `pids.max` — it atomically increments the pid counter for the cgroup and returns `-EAGAIN` if the new count would exceed `pids.max`, which propagates back to `clone()` as `EAGAIN`. This is how the Kubernetes `pids` limit in a pod's cgroup prevents fork bombs even though the process is not yet in a different cgroup.

## Section 7 — Live Observation

```bash
# Check a specific process's cgroup membership across all controllers:
cat /proc/$PID/cgroup
# In v2: only one line "0::<path>"
# In v1 (if mixed): multiple lines like "7:memory:/kubepods/pod..."

# Show per-subsystem statistics: hierarchy_id, num_cgroups, enabled:
cat /proc/cgroups

# Count processes in a cgroup (reads via css_set.tasks iteration):
wc -l /sys/fs/cgroup/kubepods/pod<uid>/cgroup.procs

# bpftrace: trace css_set changes (task migrations):
bpftrace -e 'kprobe:cgroup_migrate_finish {
    printf("migration: pid=%d comm=%s\n", pid, comm);
}'

# bpftrace: trace pids_fork enforcement (catching pids.max hits):
bpftrace -e 'kretprobe:pids_try_charge {
    if (retval < 0) {
        printf("pids.max hit: pid=%d comm=%s\n", pid, comm);
    }
}'
```

## Section 8 — Summary: The Full Chain

From userspace Pod spec to kernel data structures:

```
Pod spec:
  resources.limits.memory: 256Mi
  resources.limits.cpu: 500m

kubelet writes:
  /sys/fs/cgroup/kubepods/pod<uid>/memory.max = 268435456
  /sys/fs/cgroup/kubepods/pod<uid>/cpu.max = 50000 100000

Container process starts → containerd calls clone3():
  copy_process()
    └─ cgroup_fork()
         └─ css_set_fork() → child->cgroups = parent->cgroups (same css_set, refcount++)

containerd writes container PID to cgroup.procs:
  cgroup_migrate()
    └─ find_css_set() → new css_set for pod<uid>/<container-id> cgroup
         └─ task->cgroups = new_cset (now uses mem_cgroup + task_group from pod cgroup)

Memory allocation in container:
  try_charge() → checks mem_cgroup.memory.usage vs mem_cgroup.memory.max
  └─ [if over limit] → OOM kill
```

Every allocation in the container now flows through `mem_cgroup_charge()`, which walks `task->cgroups->subsys[memory_cgrp_id]` to reach the `struct mem_cgroup` that holds the 256 MiB limit. The journey from a Pod YAML field to a kernel enforcement check passes entirely through `task->cgroups` and the `css_set` it points to.
