# 03-b — cgroup v2 Controllers: memory, cpu, io, pids

cgroup v2 controllers are kernel subsystems that plug into the cgroup framework to track and enforce resource usage. Each controller registers a `struct cgroup_subsys` with lifecycle callbacks (`css_alloc`, `css_online`, `css_free`, `fork`, `exit`). When enabled in a cgroup, the controller allocates per-cgroup state (e.g., `struct mem_cgroup`, `struct task_group`) and hooks into the kernel paths where the resource is consumed.

## Section 1 — Controller Architecture: `struct cgroup_subsys` and `struct cgroup_subsys_state`

**struct cgroup_subsys** — the vtable every controller implements:

```c
// include/linux/cgroup-defs.h (key fields)
struct cgroup_subsys {
    struct cgroup_subsys_state *(*css_alloc)(struct cgroup_subsys_state *parent_css);
    int  (*css_online)(struct cgroup_subsys_state *css);
    void (*css_offline)(struct cgroup_subsys_state *css);
    void (*css_free)(struct cgroup_subsys_state *css);
    int  (*can_attach)(struct cgroup_taskset *tset);
    void (*attach)(struct cgroup_taskset *tset);
    int  (*fork)(struct task_struct *task);
    void (*exit)(struct task_struct *task);
    void (*release)(struct task_struct *task);
    int          id;           /* controller numeric ID */
    const char  *name;        /* "memory", "cpu", "io", "pids" */
    bool         threaded;    /* supports threaded mode */
};
```

Source: https://elixir.bootlin.com/linux/v6.9/source/include/linux/cgroup-defs.h

**struct cgroup_subsys_state** — base type embedded at offset 0 in every per-controller struct:

```c
// include/linux/cgroup-defs.h
struct cgroup_subsys_state {
    struct cgroup        *cgroup;  // the cgroup this state belongs to
    struct cgroup_subsys *ss;      // which subsystem owns this state
    refcount_t            refcnt;  // reference count
    unsigned long         flags;   // CSS_NO_REF, CSS_ONLINE, CSS_DYING, CSS_DEAD
    struct cgroup_subsys_state *parent; // parent's css for this subsystem
    struct rcu_head       rcu_head;
};
```

Source: https://elixir.bootlin.com/linux/v6.9/source/include/linux/cgroup-defs.h

Because `css` is at offset 0, a `struct mem_cgroup *` can be cast to `struct cgroup_subsys_state *` directly — this is how the cgroup core obtains the subsystem state from a controller struct.

**Enabling controllers:**

```bash
# Show available controllers at the root:
cat /sys/fs/cgroup/cgroup.controllers
# output: cpuset cpu io memory hugetlb pids rdma misc

# Enable memory and cpu for a cgroup's children:
echo "+memory +cpu" > /sys/fs/cgroup/mygroup/cgroup.subtree_control

# Verify:
cat /sys/fs/cgroup/mygroup/cgroup.controllers
```

Controllers must be enabled at each level of the hierarchy. A controller cannot be enabled in a cgroup that has tasks directly attached (internal node constraint in v2).

## Section 2 — memory controller

**Source:** `mm/memcontrol.c` — https://elixir.bootlin.com/linux/v6.9/source/mm/memcontrol.c

**Interface files (cgroup v2):**

| File | Mode | Description |
|------|------|-------------|
| `memory.current` | r | Current memory usage in bytes |
| `memory.max` | rw | Hard limit; OOM kill if exceeded |
| `memory.high` | rw | Soft limit; throttle above this |
| `memory.min` | rw | Memory protection floor (no reclaim below) |
| `memory.low` | rw | Soft protection (best-effort reclaim avoidance) |
| `memory.swap.max` | rw | Swap usage limit |
| `memory.stat` | r | Detailed breakdown: anon, file, kernel, slab, sock... |
| `memory.events` | r | Event counters: oom, oom_kill, max |
| `memory.pressure` | r | PSI pressure metrics for this cgroup |

**Key struct:**

```c
// mm/memcontrol.h (simplified — select most important fields)
struct mem_cgroup {
    struct cgroup_subsys_state css;  /* must be first — base type */

    /* accounting */
    struct page_counter memory;     /* tracks usage vs limit */
    struct page_counter swap;
    struct page_counter memsw;      /* memory+swap combined */

    /* limits */
    unsigned long       soft_limit; /* memory.high in pages */

    /* OOM */
    bool                oom_group;  /* kill whole cgroup on OOM (memory.oom.group) */
    int                 oom_kill_disable;

    /* per-NUMA stats */
    struct mem_cgroup_per_node __percpu *nodeinfo;

    /* events (memory.events) */
    struct cgroup_file  events_file;
    atomic_long_t       memory_events[MEMCG_NR_MEMORY_EVENTS];
    struct list_head    event_list;
    spinlock_t          event_list_lock;
};
```

Source: https://elixir.bootlin.com/linux/v6.9/source/mm/memcontrol.h

**struct page_counter** (the actual accounting unit):

```c
struct page_counter {
    atomic_long_t   usage;   /* current usage in pages */
    long            max;     /* limit in pages (LONG_MAX = unlimited) */
    long            min;     /* protection floor */
    long            low;     /* soft protection */
    long            high;    /* soft limit (memory.high) */
    /* failcnt, watermarks ... */
};
```

When `usage > max`, OOM is triggered. When `usage > high`, processes are throttled with `schedule()` calls to force memory reclaim.

**memory.high enforcement (throttling) path:**

```
page allocation → charge_memcg() → mem_cgroup_charge()
  └─ try_charge()
       └─ [usage > high] → mem_cgroup_handle_over_high()
            └─ schedule()  /* force the task to yield, triggering reclaim */
```

**memory.max enforcement (OOM kill) path:**

```
page allocation → try_charge()
  └─ [usage > max, reclaim fails] → mem_cgroup_out_of_memory()
       └─ out_of_memory(&oc)
            └─ oom_kill_process() → SIGKILL to the process
```

**Live observation:**

```bash
# Current usage for a pod:
cat /sys/fs/cgroup/kubepods/pod<uid>/memory.current

# Full stats breakdown:
cat /sys/fs/cgroup/kubepods/pod<uid>/memory.stat

# Watch for OOM events:
cat /sys/fs/cgroup/kubepods/pod<uid>/memory.events
# oom N — times limit was hit; oom_kill N — times OOM killer fired

# bpftrace: trace OOM kills inside cgroups:
bpftrace -e 'kprobe:mem_cgroup_out_of_memory {
    printf("OOM: pid=%d comm=%s\n", pid, comm);
}'
```

## Section 3 — cpu controller

**Source:** `kernel/sched/fair.c` — https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/fair.c

**Interface files (cgroup v2):**

| File | Mode | Description |
|------|------|-------------|
| `cpu.stat` | r | usage_usec, user_usec, system_usec, nr_periods, nr_throttled, throttled_usec |
| `cpu.max` | rw | `<quota> <period>` in µs; `100000 100000` = 1 CPU; `max 100000` = unlimited |
| `cpu.weight` | rw | CFS weight (1–10000, default 100) — relative CPU share |
| `cpu.weight.nice` | rw | nice-value alias for cpu.weight |
| `cpu.pressure` | r | PSI pressure for this cgroup |

**Key struct:**

```c
// kernel/sched/sched.h (simplified)
struct task_group {
    struct cgroup_subsys_state css;  /* must be first */

    /* CFS bandwidth control (cpu.max) */
    struct cfs_bandwidth    cfs_bandwidth;  /* quota/period/runtime tracking */

    /* per-CPU scheduling entities */
    struct sched_entity   **se;      /* one per CPU */
    struct cfs_rq         **cfs_rq; /* per-CPU runqueue slice */

    unsigned long           shares;  /* cpu.weight → shares */

    /* RT scheduling (not used for containers typically) */
    struct sched_rt_entity **rt_se;
    struct rt_rq           **rt_rq;
};
```

Source: https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/sched.h

**struct cfs_bandwidth** — the CFS bandwidth controller state:

```c
struct cfs_bandwidth {
    raw_spinlock_t  lock;
    ktime_t         period;        /* period duration (cpu.max period) */
    u64             quota;         /* quota per period in nanoseconds */
    u64             runtime;       /* remaining runtime this period */
    s64             hierarchical_quota;
    u8              idle;          /* is bandwidth currently idle? */
    u8              period_active;
    struct hrtimer  period_timer;  /* fires at end of period to replenish */
    struct hrtimer  slack_timer;   /* coalesces small replenishments */
};
```

**Throttling path:**

```
scheduler tick → scheduler_tick() → task_tick_fair() → entity_tick()
  └─ update_curr() → account_cfs_rq_runtime()
       └─ [runtime ≤ 0] → throttle_cfs_rq()
            └─ dequeue_entity() from runqueue → task cannot run

period_timer fires → do_sched_cfs_period_timer()
  └─ refill cfs_bandwidth.runtime = quota
       └─ unthrottle_cfs_rq() → re-enqueue tasks
```

**`cpu.max` format explained:**

```bash
echo "200000 100000" > cpu.max   # 200ms quota in 100ms period = 2 CPUs worth
echo "50000 100000" > cpu.max    # 50ms quota in 100ms period = 0.5 CPU (500m)
echo "max 100000" > cpu.max      # unlimited
```

Kubernetes maps `resources.limits.cpu: 500m` → `50000 100000`.

**Live observation:**

```bash
# See quota for a pod:
cat /sys/fs/cgroup/kubepods/pod<uid>/cpu.max

# Check if the pod is being throttled:
cat /sys/fs/cgroup/kubepods/pod<uid>/cpu.stat
# nr_throttled > 0 means the pod hit its CPU limit

# bpftrace: trace CFS throttling
bpftrace -e 'kprobe:throttle_cfs_rq {
    printf("throttled: pid=%d comm=%s\n", pid, comm);
}'
```

## Section 4 — io controller

**Source:** `block/blk-cgroup.c` — https://elixir.bootlin.com/linux/v6.9/source/block/blk-cgroup.c

**Interface files (cgroup v2):**

| File | Mode | Description |
|------|------|-------------|
| `io.stat` | r | Per-device: rbytes, wbytes, rios, wios, dbytes, dios (discard) |
| `io.max` | rw | Hard limit: `<major:minor> rbps=N wbps=N riops=N wiops=N` |
| `io.weight` | rw | Relative I/O weight (1–10000) for BFQ scheduler |
| `io.latency` | rw | Target latency (ms) per device for latency-based throttling |
| `io.pressure` | r | PSI pressure for I/O |

**Key struct: `struct blkcg`** in `include/linux/blk-cgroup.h`:

```c
struct blkcg {
    struct cgroup_subsys_state  css;
    spinlock_t                  lock;
    struct radix_tree_root      blkg_tree;   /* per-device blkcg_gq structs */
    struct blkcg_gq __rcu      *blkg_hint;  /* last used blkg */
    struct hlist_head           blkg_list;  /* all blkgs in this blkcg */
    struct blkcg_policy_data   *cpd[BLKCG_MAX_POLS]; /* per-policy data */
    struct list_head            all_blkcgs_node;
};
```

Source: https://elixir.bootlin.com/linux/v6.9/source/include/linux/blk-cgroup.h

The io controller intercepts I/O at `submit_bio()` and applies throttling via the block layer. BFQ (Budget Fair Queueing) and mq-deadline schedulers both support cgroup weights.

**Live observation:**

```bash
# I/O stats for a cgroup (major:minor = device number):
cat /sys/fs/cgroup/kubepods/pod<uid>/io.stat

# Set an I/O bandwidth limit (requires device major:minor from lsblk):
lsblk -no MAJ:MIN /dev/sda
echo "8:0 rbps=10485760 wbps=10485760" > /sys/fs/cgroup/mygroup/io.max
```

**Note for Kubernetes:** io.max is not commonly used in Kubernetes (no direct Pod spec field). However, io.weight is used when multiple pods compete for the same disk.

## Section 5 — pids controller

**Source:** `kernel/cgroup/pids.c` — https://elixir.bootlin.com/linux/v6.9/source/kernel/cgroup/pids.c

**Interface files (cgroup v2):**

| File | Mode | Description |
|------|------|-------------|
| `pids.current` | r | Current number of processes + threads |
| `pids.max` | rw | Limit; `fork()` returns EAGAIN if exceeded |
| `pids.events` | r | `max` counter: how many times the limit was hit |

**Key struct:**

```c
// kernel/cgroup/pids.c
struct pids_cgroup {
    struct cgroup_subsys_state  css;

    /*
     * Use 64-bit types so that we can safely represent "max" as
     * PIDS_MAX = (int64_t)INT64_MAX.
     */
    atomic64_t  counter;      /* current count (pids.current) */
    atomic64_t  limit;        /* pids.max */
    local_t     events_limit; /* pids.events.max counter */
};
```

Source: https://elixir.bootlin.com/linux/v6.9/source/kernel/cgroup/pids.c

**Enforcement path:**

```
clone()/fork() → copy_process()
  └─ pids_fork()                     # cgroup/pids.c
       └─ pids_try_charge()
            └─ [counter >= limit] → return -EAGAIN
                 → clone() returns EAGAIN to userspace
                 → bash/shell: "fork: retry: Resource temporarily unavailable"
```

**Live observation:**

```bash
# Current process count for a pod:
cat /sys/fs/cgroup/kubepods/pod<uid>/pids.current

# PID limit (Kubernetes sets this from spec.containers[].resources):
cat /sys/fs/cgroup/kubepods/pod<uid>/pids.max

# Check if the limit was hit:
cat /sys/fs/cgroup/kubepods/pod<uid>/pids.events
# max N — how many times fork was rejected
```

**Kubernetes context:** The default PID limit per pod is controlled by `--pod-max-pids` kubelet flag (default: -1 = unlimited). Set `PodPidsLimit` feature gate + kubelet config to enforce per-pod PID limits.

## Section 6 — Connecting the Dots: resource limits → kernel path

| Pod spec | File written | Value format | Kernel enforcement |
|----------|-------------|--------------|-------------------|
| `limits.memory: 256Mi` | `memory.max` | `268435456` | OOM kill via `mem_cgroup_out_of_memory()` |
| `requests.memory: 128Mi` | `memory.min` | `134217728` | Reclaim protection in mm/vmscan.c |
| `limits.cpu: 1` | `cpu.max` | `100000 100000` | CFS throttle via `throttle_cfs_rq()` |
| `requests.cpu: 500m` | `cpu.weight` | `51` (formula) | CFS fair scheduling via `shares` |
| (kubelet flag) | `pids.max` | integer | `pids_fork()` returns EAGAIN |

The cpu.weight formula: `max(2, min(262144, (1024 * milliCPU) / 1000))`
