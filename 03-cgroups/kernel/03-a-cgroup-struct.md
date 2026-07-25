# struct cgroup — Kernel Deep Dive

## Source Location

| File | Link |
|------|------|
| `include/linux/cgroup-defs.h` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/cgroup-defs.h |
| `kernel/cgroup/cgroup.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/cgroup/cgroup.c |

`struct cgroup` is defined in `include/linux/cgroup-defs.h`. The implementation — creation, migration, destruction, file operations — lives in `kernel/cgroup/cgroup.c`.

## cgroup v1 vs v2: Why the Unified Hierarchy

**cgroup v1** mounted each controller at its own path:

```
/sys/fs/cgroup/memory/   (mounted with -t cgroup -o memory)
/sys/fs/cgroup/cpu/
/sys/fs/cgroup/cpuacct/
/sys/fs/cgroup/blkio/
/sys/fs/cgroup/pids/
```

This created serious consistency problems: a process could be in different positions in the memory hierarchy and the cpu hierarchy. There was no atomic way to move a task across all controllers. Each controller had subtly different semantics and file names.

**cgroup v2** (unified hierarchy) uses a single mount:

```
/sys/fs/cgroup/          (mounted with -t cgroup2)
```

All controllers share one hierarchy. A process is always at the same level in every controller's view. Controllers are enabled per-directory by writing to `cgroup.subtree_control`. The kernel enforces the "no internal tasks" rule — a cgroup with controllers enabled cannot also have tasks (tasks must live in leaf cgroups).

To verify which mode your system uses:

```bash
mount | grep cgroup
# cgroup v2 only: "cgroup2 on /sys/fs/cgroup type cgroup2"
# mixed:          also shows "cgroup on /sys/fs/cgroup/... type cgroup"
```

Force pure v2 with the kernel parameter `cgroup_no_v1=all`. Modern systemd (248+) defaults to v2 when the kernel supports it. Kubernetes requires cgroup v2 for proper QoS enforcement and PSI (Pressure Stall Information) — it became the default in Kubernetes 1.25.

## struct cgroup (Key Fields)

```c
// include/linux/cgroup-defs.h (key fields — struct is much larger in source)
struct cgroup {
    /* kernfs backing — the filesystem directory */
    struct kernfs_node        *kn;           // directory node in cgroupfs
    struct cgroup_file         procs_file;   // cgroup.procs file
    struct cgroup_file         events_file;  // cgroup.events file

    /* hierarchy navigation */
    struct cgroup_root        *root;         // which cgroupfs hierarchy this belongs to
    struct cgroup             *parent;       // parent cgroup (NULL for hierarchy root)
    u64                        id;           // unique 64-bit ID (visible in cgroup.id file)
    int                        level;        // depth from root (root=0, children=1, ...)
    int                        max_depth;    // deepest allowed subtree depth
    int                        nr_descendants; // count of all descendant cgroups

    /* reference counting */
    struct percpu_ref           self;        // reference to this cgroup's lifetime
    atomic_t                    online_cnt;  // processes online in this cgroup

    /* subsystem state — one pointer per controller */
    struct cgroup_subsys_state __rcu *subsys[CGROUP_SUBSYS_COUNT];

    /* task membership */
    struct list_head            cset_links;  // css_sets that reference this cgroup
    int                         nr_tasks;    // tasks directly in this cgroup (not children)
    int                         nr_populated_csets; // non-empty css_sets

    /* resource pressure */
    struct psi_group            psi;         // PSI (Pressure Stall Information) data

    /* BPF programs attached to this cgroup */
    struct cgroup_bpf           bpf;

    /* controller enablement */
    u16                         subtree_control;  // controllers enabled for children
    u16                         subtree_ss_mask;  // controllers with tasks in subtree
};
```

### Field-by-Field Explanation

**`kn` — `struct kernfs_node *`**

The kernfs node that backs this cgroup's directory on the filesystem. Every file operation on files inside a cgroup directory (`memory.max`, `cgroup.procs`, `cpu.max`) goes through this node's registered file operations. When you `open()` `/sys/fs/cgroup/kubepods/pod123/memory.max`, the VFS layer resolves the path through kernfs, reaches this `kn`, and dispatches to the memory controller's `seq_show` or `write` function. The kernfs node also holds the directory's inode metadata (permissions, timestamps).

**`procs_file`, `events_file` — `struct cgroup_file`**

Wrapper structs around specific kernfs files within the cgroup directory. `procs_file` backs `cgroup.procs` — writing a PID here triggers `cgroup_procs_write()` which migrates the process. `events_file` backs `cgroup.events` — it reports `populated` (1 if any tasks are in the subtree) and `frozen`. These are kept as embedded structs so the kernel can trigger a kernfs notification (`kernfs_notify()`) to wake up `poll()` waiters when the state changes.

**`root` — `struct cgroup_root *`**

Points to the hierarchy root this cgroup belongs to. In cgroup v2 there is exactly one root — `cgrp_dfl_root` (the default root). In cgroup v1 each controller had its own `cgroup_root`. This pointer lets any code path quickly determine which subsystems are active (`root->subsys_mask`) and reach the root cgroup (`root->cgrp`).

**`parent` — `struct cgroup *`**

The parent cgroup in the tree. NULL only for the root cgroup. Used during `mkdir` to inherit controller settings, during `rmdir` to detach, and during resource limit inheritance walks. The root cgroup's `parent` being NULL is the sentinel that prevents `cgroup_rmdir()` from attempting to delete the root.

**`id` — `u64`**

A unique 64-bit identifier assigned at cgroup creation time (via `idr_alloc()`). Visible in userspace via `cat /sys/fs/cgroup/some/path/cgroup.id`. Used in BPF programs (`bpf_get_current_cgroup_id()` returns this value), in audit logs, and in systemd's tracking. Stable for the lifetime of the cgroup — not reused until the cgroup is destroyed and a new one is created.

**`level` — `int`**

Depth from the root. Root cgroup is level 0, its direct children are level 1, grandchildren level 2, and so on. Kubernetes pod cgroups typically sit at level 2 or 3 (`kubepods/besteffort/pod<uid>`) and container cgroups at level 3 or 4. Used to enforce `max_depth` limits and by tools like `systemd-cgls` to indent the tree display.

**`max_depth` — `int`**

Maximum allowed depth of the subtree rooted at this cgroup. Written via `cgroup.max.depth`. Default is `INT_MAX` (unlimited). Kubernetes does not typically restrict this, but it can be used to prevent runaway nesting.

**`nr_descendants` — `int`**

Count of all cgroups in this cgroup's subtree, not counting itself. Updated atomically when child cgroups are created or destroyed. Used to enforce `cgroup.max.descendants` limits and for the `populated` event in `cgroup.events`.

**`self` — `struct percpu_ref`**

The primary reference counter for this cgroup's lifetime. `percpu_ref` uses per-CPU counters to avoid cache-line bouncing under high concurrency — each CPU increments its own slot, and the count is summed only when transitioning to atomic mode (during teardown). The cgroup is not freed until this reference drops to zero. Code that needs to keep a cgroup alive (BPF, iterators, etc.) calls `css_get()` which ultimately increments this ref.

**`online_cnt` — `atomic_t`**

Tracks how many processes are currently running (online) in this cgroup's subtree. Combined with `nr_descendants` to compute the `populated` state for `cgroup.events`.

**`subsys[CGROUP_SUBSYS_COUNT]` — `struct cgroup_subsys_state __rcu *`**

The critical array that links a cgroup to each active controller's per-cgroup state. `CGROUP_SUBSYS_COUNT` is the compile-time count of all compiled-in controllers. For each enabled controller, this slot holds a pointer to the controller-specific struct (e.g., `struct mem_cgroup` for the memory controller, `struct task_group` for the cpu controller). The `__rcu` annotation means readers must use `rcu_dereference()`. The common header `struct cgroup_subsys_state` at the start of each controller struct holds the back-pointer to this `struct cgroup`.

**`cset_links` — `struct list_head`**

A doubly-linked list head connecting this cgroup to all `css_set` structs that reference it. A `css_set` represents one unique combination of cgroup memberships for a set of tasks. When a task's cgroup membership changes (via `cgroup.procs` write), a new `css_set` is found or created, and this list is updated. To enumerate all tasks in a cgroup, the kernel walks `cset_links` to find all `css_set` instances, then walks each `css_set`'s task list.

**`nr_tasks` — `int`**

Number of tasks directly in this cgroup (not counting descendants). A cgroup can only be rmdir'd when this is 0 and `nr_descendants` is also 0 (the cgroup is fully empty).

**`nr_populated_csets` — `int`**

Count of `css_set` instances associated with this cgroup that have at least one task. Used to efficiently compute whether the cgroup subtree is `populated` without walking all css_sets.

**`psi` — `struct psi_group`**

Per-cgroup Pressure Stall Information. Tracks how long tasks in this cgroup stall waiting for memory, CPU, or I/O. Each `psi_group` accumulates time-weighted stall percentages. Available via `memory.pressure`, `cpu.pressure`, and `io.pressure` files. Kubernetes uses PSI data to make more accurate eviction decisions and to implement the `MemoryPressure` node condition.

**`bpf` — `struct cgroup_bpf`**

Holds BPF programs attached to this cgroup at each hook point (`BPF_CGROUP_INET_INGRESS`, `BPF_CGROUP_SOCK_OPS`, `BPF_LSM_CGROUP`, etc.). BPF programs attached to a parent cgroup are inherited by children via `effective` program arrays. This is the mechanism behind Kubernetes network policy enforcement via Cilium and cgroup-based socket filtering.

**`subtree_control` — `u16`**

Bitmask of controllers enabled for this cgroup's children. Written by userspace via `cgroup.subtree_control` (e.g., `echo "+memory +cpu" > cgroup.subtree_control`). A controller bit set here means child cgroups of this cgroup will have that controller's files (e.g., `memory.max`) available. The kubelet enables `memory`, `cpu`, `io`, and `pids` controllers on the `kubepods/` directory so all pod cgroups inherit them.

**`subtree_ss_mask` — `u16`**

Bitmask of controllers that have tasks somewhere in this cgroup's subtree. Maintained by the kernel as tasks migrate in and out. Used during controller enable/disable validation — you cannot disable a controller if tasks in the subtree are using it.

## struct cgroup_root

```c
// include/linux/cgroup-defs.h (simplified)
struct cgroup_root {
    struct kernfs_root    *kf_root;     // the kernfs filesystem root
    unsigned int           subsys_mask; // bitmask of subsystems attached
    int                    hierarchy_id;
    struct cgroup          cgrp;        // the root cgroup (embedded, not a pointer)
    atomic_t               nr_cgrps;   // total cgroup count in this hierarchy
    struct list_head       root_list;  // list of all roots
    unsigned int           flags;      // CGRP_ROOT_* flags
    char                   name[MAX_CGROUP_ROOT_NAMELEN]; // hierarchy name
};
```

The `cgrp` field is embedded (not a pointer) — the root cgroup is part of the root struct itself. This means `&cgrp_dfl_root.cgrp` is the cgroup v2 root cgroup.

For cgroup v2 there is exactly one `cgroup_root` instance — `cgrp_dfl_root` — defined at the top of `kernel/cgroup/cgroup.c`:

```c
// kernel/cgroup/cgroup.c
struct cgroup_root cgrp_dfl_root = { .cgrp.self.flags = CSS_NO_REF };
```

All subsystems active in v2 are attached to this single root. In v1 mode, each controller mount created its own `cgroup_root` instance, and `root_list` linked them all together. The `hierarchy_id` for `cgrp_dfl_root` is always 0.

## Lifecycle

A cgroup's life follows three phases: creation, use, and destruction.

```
mkdir /sys/fs/cgroup/mygroup      ->  kernel_mkdir()
  └─ cgroup_mkdir()               kernel/cgroup/cgroup.c
       ├─ cgroup_create()         allocates struct cgroup + all subsys states
       │    ├─ kzalloc(cgroup)
       │    ├─ kernfs_create_dir() creates the kernfs directory node
       │    └─ for each controller: css_alloc() + css_online()
       └─ cgroup_apply_control()  propagate controller inheritance

echo $PID > cgroup.procs          ->  cgroup_procs_write()
  └─ cgroup_attach_task()
       └─ cgroup_migrate()        find or create new css_set, swap task->cgroups

rmdir /sys/fs/cgroup/mygroup      ->  cgroup_rmdir()
  └─ cgroup_destroy_locked()      (only succeeds when cgroup.procs is empty)
       └─ css_kill_scheduling()   async teardown of subsystem states
            └─ css_free_rwork_fn() -> kfree(cgrp)
```

**Creation** (`cgroup_mkdir`): The kernel allocates a `struct cgroup`, links it into the parent's child list, creates a kernfs directory node for it, and calls each active controller's `css_alloc()` callback to allocate the per-cgroup controller state. Then `css_online()` is called to make the controller state active. If any `css_alloc()` fails, all previously allocated controller states are freed.

**Task attachment** (`cgroup_procs_write`): Writing a PID to `cgroup.procs` triggers `cgroup_procs_write()`, which resolves the task, acquires `cgroup_mutex`, validates the move (permissions, frozen state, delegation constraints), then calls `cgroup_migrate()`. Migration finds or creates a new `css_set` representing the task's new cgroup membership combination and atomically replaces `task->cgroups` under `css_set_lock` + RCU.

**Destruction** (`cgroup_rmdir`): `rmdir` only succeeds when the cgroup has no tasks (`cgroup.procs` is empty) and no live descendants. The kernel calls `cgroup_destroy_locked()`, which marks the cgroup offline, schedules asynchronous teardown of each controller's state via `css_kill_scheduling()`, and eventually calls the controller's `css_free()` callback. The `struct cgroup` itself is freed via a work queue item (`css_free_rwork_fn`) after all RCU readers have completed.

## Locking Discipline

Four synchronization mechanisms protect the cgroup subsystem:

**`cgroup_mutex` (mutex)**

The top-level lock for structural changes. Held during `cgroup_mkdir()`, `cgroup_rmdir()`, `cgroup_apply_control()` (enabling/disabling controllers), and any operation that changes the hierarchy shape. This is the outer lock — if you need both `cgroup_mutex` and `css_set_lock`, acquire `cgroup_mutex` first.

```c
// example from kernel/cgroup/cgroup.c
static int cgroup_mkdir(struct kernfs_node *parent_kn, const char *name, umode_t mode)
{
    mutex_lock(&cgroup_mutex);
    // ... create the cgroup ...
    mutex_unlock(&cgroup_mutex);
}
```

**`css_set_lock` (spinlock)**

Protects all `css_set` membership data: the `task_struct->cgroups` pointer, `css_set->tasks` list, and `css_set` reference counts. Held during task migration and when walking tasks in a cgroup. Must be acquired with IRQs disabled (`spin_lock_irq`) because it can be taken from interrupt context during task death.

**`kernfs_mutex` (mutex, inside kernfs)**

Protects kernfs directory entries and their lifecycle. Mostly an internal kernfs concern — cgroup code doesn't acquire it directly, but `cgroup_mkdir()` calls `kernfs_create_dir()` which acquires it internally. The ordering constraint: `cgroup_mutex` -> `kernfs_mutex`.

**RCU (read-copy-update)**

`task_struct->cgroups` is RCU-protected. This means:
- **Readers** (e.g., code looking up a task's cgroup) use `rcu_read_lock()` + `rcu_dereference(task->cgroups)` — zero overhead, no cache line bounce.
- **Writers** (task migration) hold `css_set_lock`, do the pointer swap with `rcu_assign_pointer()`, then call `css_set_put()` on the old `css_set` after a grace period.

This design allows the extremely hot path of "what cgroup is this task in?" to be lock-free.

## Object Graph

The kernel maintains a web of cross-references between tasks, css_sets, controller states, and cgroups:

```
task_struct
  └─ cgroups ──────────────────────────────► struct css_set
                                               ├─ subsys[0] ──► struct mem_cgroup (CSS)
                                               │                    └─ cgroup ──────► struct cgroup
                                               │                                          └─ subsys[0] (same mem_cgroup)
                                               ├─ subsys[1] ──► struct task_group (CSS, for cpu)
                                               │                    └─ cgroup ──────► struct cgroup
                                               └─ subsys[N] ──► struct pids_cgroup (CSS)
                                                                    └─ cgroup ──────► struct cgroup
```

The key insight is `struct cgroup_subsys_state` (abbreviated `css`). Every controller-specific struct begins with an embedded `css`:

```c
struct mem_cgroup {
    struct cgroup_subsys_state css;   // MUST be first
    // ... memory controller fields ...
};

struct cgroup_subsys_state {
    struct cgroup        *cgroup;     // back-pointer to the cgroup
    struct cgroup_subsys *ss;         // which controller this belongs to
    struct percpu_ref     refcnt;     // reference count
    // ...
};
```

Because `css` is always first, a `struct cgroup_subsys_state *` pointer can be safely cast to/from `struct mem_cgroup *` using `container_of`. This is how `cgroup->subsys[mem_cgroup_id]` returns a `struct cgroup_subsys_state *` that is really the start of a `struct mem_cgroup`.

A `css_set` represents a unique combination of (memory cgroup, cpu cgroup, pids cgroup, ...) that a set of tasks shares. Multiple tasks with identical cgroup membership share a single `css_set`, reducing memory overhead. When one task moves to a different cgroup, it gets a new `css_set` (potentially shared with other tasks that already have that combination).

## Live Observation

```bash
# Find a process's cgroup path
cat /proc/$PID/cgroup
# Output for cgroup v2: "0::<path>"
# Example: 0::/kubepods/besteffort/pod6abc1234-.../abc123container...

# Navigate to the cgroup directory
CGROUP=$(cat /proc/$PID/cgroup | sed 's/0:://')
ls /sys/fs/cgroup$CGROUP

# Read a cgroup's unique ID
cat /sys/fs/cgroup$CGROUP/cgroup.id

# List all processes in a cgroup (not recursive)
cat /sys/fs/cgroup$CGROUP/cgroup.procs

# List all processes including descendants
find /sys/fs/cgroup$CGROUP -name cgroup.procs -exec cat {} +

# See which controllers are available at the root
cat /sys/fs/cgroup/cgroup.controllers
# Example output: cpuset cpu io memory hugetlb pids rdma misc

# See which controllers are enabled in a specific cgroup's children
cat /sys/fs/cgroup$CGROUP/cgroup.subtree_control

# Check memory usage and limits
cat /sys/fs/cgroup$CGROUP/memory.current   # current usage in bytes
cat /sys/fs/cgroup$CGROUP/memory.max       # limit ("max" means unlimited)
cat /sys/fs/cgroup$CGROUP/memory.stat      # detailed breakdown

# Check CPU quota
cat /sys/fs/cgroup$CGROUP/cpu.max
# Output format: "<quota_us> <period_us>"
# Example: "100000 100000" means 100% of one CPU per 100ms period
# Example: "max 100000" means no limit

# Check CPU throttling statistics
cat /sys/fs/cgroup$CGROUP/cpu.stat
# Key fields: usage_usec, user_usec, system_usec, nr_periods, nr_throttled, throttled_usec

# Read PSI (Pressure Stall Information) for memory
cat /sys/fs/cgroup$CGROUP/memory.pressure

# Tree view using systemd tooling
systemd-cgls
systemd-cgls /kubepods.slice    # scoped to pod cgroups

# Tree view without systemd
find /sys/fs/cgroup -maxdepth 4 -name cgroup.procs | sort

# bpftrace: trace cgroup creation (every mkdir on cgroupfs)
bpftrace -e 'kprobe:cgroup_mkdir {
    printf("pid=%d comm=%s creating new cgroup\n", pid, comm);
}'

# bpftrace: trace process migration between cgroups
bpftrace -e 'kprobe:cgroup_migrate_finish {
    printf("pid=%d comm=%s migrated to new cgroup\n", pid, comm);
}'

# bpftrace: trace OOM kills within cgroups
bpftrace -e 'kprobe:mem_cgroup_out_of_memory {
    printf("OOM kill triggered: pid=%d comm=%s\n", pid, comm);
}'

# bpftrace: print cgroup ID for every fork
bpftrace -e 'tracepoint:sched:sched_process_fork {
    printf("fork: parent_pid=%d child_pid=%d cgroup_id=%llu\n",
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
