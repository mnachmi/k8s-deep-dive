# 08-b — CFS: `struct sched_entity`, `struct cfs_rq`, vruntime

## Source Locations

| File | Key Symbols | URL |
|------|-------------|-----|
| `kernel/sched/fair.c` | `update_curr()`, `enqueue_entity()`, `pick_next_entity()`, `dequeue_entity()` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/fair.c |
| `kernel/sched/sched.h` | `struct cfs_rq`, `struct cfs_bandwidth` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/sched.h |
| `include/linux/sched.h` | `struct sched_entity`, `struct load_weight` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/sched.h |
| `kernel/sched/pelt.c` | `update_load_avg()`, per-entity load tracking | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/pelt.c |
| `kernel/sched/autogroup.c` | `autogroup_task_group()` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/autogroup.c |

## `struct sched_entity`

```c
// include/linux/sched.h (simplified, Linux 6.9)
struct sched_entity {
    struct load_weight      load;           // weight + inv_weight
    struct rb_node          run_node;       // position in cfs_rq->tasks_timeline
    struct list_head        group_node;     // membership in cfs_rq->entities
    unsigned int            on_rq;          // 1 if currently enqueued
    u64                     exec_start;     // rq->clock_task at last update_curr()
    u64                     sum_exec_runtime; // total CPU time (ns)
    u64                     vruntime;       // virtual runtime (ns, weight-scaled)
    u64                     prev_sum_exec_runtime; // snapshot at last sleep/dequeue
    u64                     nr_migrations;  // times this entity was CPU-migrated
    struct sched_avg        avg;            // PELT load average (util_avg, load_avg)
};
```

Key fields:

- `load.weight`: derived from nice value via `sched_prio_to_weight[]` table. nice=0 → weight=1024 (`NICE_0_LOAD`). nice=-1 → 1277; nice=+1 → 820. Each step is roughly 1.25×.

- `vruntime`: the central quantity CFS sorts by. Updated in `update_curr()`:
  ```
  delta_exec = rq->clock_task - se->exec_start
  vruntime  += delta_exec × NICE_0_LOAD / se->load.weight
  ```
  Heavy tasks (high weight) accumulate vruntime slowly → run longer before preemption. Light tasks accumulate quickly → preempted sooner per real-time nanosecond.

- `on_rq`: set to 1 by `enqueue_entity()`, cleared by `dequeue_entity()`.

## `struct cfs_rq`

```c
// kernel/sched/sched.h (simplified, Linux 6.9)
struct cfs_rq {
    struct load_weight          load;           // aggregate weight of all entities
    unsigned int                nr_running;     // number of runnable entities
    unsigned int                h_nr_running;   // includes group entities (hierarchical)
    u64                         exec_clock;     // total exec time on this rq
    u64                         min_vruntime;   // floor for new entities (monotone)
    struct rb_root_cached       tasks_timeline; // RB tree; leftmost = min vruntime
    struct sched_entity         *curr;          // currently executing entity
    struct sched_entity         *next;          // wakeup preemption candidate
    struct sched_entity         *skip;          // skip buddy (yield_to)
    struct sched_avg            avg;            // PELT aggregate load
    struct cfs_bandwidth        *tg_cfs_bandwidth; // throttle state (when cpu.max set)
    int                         throttled;      // 1 if cgroup bandwidth quota exhausted
    int                         throttle_count;
    struct list_head            throttled_list; // link when throttled
};
```

Key operation: `pick_next_entity()` calls `__pick_first_entity()` which reads `tasks_timeline.rb_leftmost` — O(1) access to the task with minimum vruntime.

## `min_vruntime` and Fairness

`min_vruntime` is the CFS floor: it monotonically increases as `update_min_vruntime()` advances it to `min(curr->vruntime, leftmost->vruntime)`. When a task wakes from sleep, `place_entity()` sets its `vruntime = max(vruntime, cfs_rq->min_vruntime - sched_latency_ns/2)` — preventing a long-sleeping task from immediately starving all others by running with stale vruntime.

## CFS Bandwidth (`cpu.max` / `cpu.cfs_quota_us`)

When a cgroup has a CPU quota set, `struct cfs_bandwidth` tracks consumption:

```c
// kernel/sched/sched.h (simplified)
struct cfs_bandwidth {
    raw_spinlock_t  lock;
    ktime_t         period;        // cpu.cfs_period_us (default 100 ms)
    u64             quota;         // cpu.cfs_quota_us in ns (-1 = unlimited)
    u64             runtime;       // remaining quota this period
    s64             hierarchical_quota;
    u8              idle;
    struct hrtimer  period_timer;  // fires each period to refill runtime
    struct hrtimer  slack_timer;   // deferred refill
    struct list_head throttled_cfs_rq; // all throttled cfs_rqs in this group
};
```

When a task in the group runs, `account_cfs_rq_runtime()` debits `cfs_b->runtime`. At zero, `throttle_cfs_rq()` removes all the group's entities from their per-CPU runqueues. `sched_cfs_period_timer()` fires at the end of each period and calls `distribute_cfs_runtime()` to refill and unthrottle.

## Load Balancing

`load_balance()` is called from `run_rebalance_domains()` on each scheduler tick. It walks the `sched_domain` hierarchy (SMT → LLC → NUMA node) looking for the busiest CPU in each domain. If the imbalance exceeds a threshold, `move_tasks()` migrates the heaviest migratable task to the idle CPU. Migration respects `cpus_mask` and `numa_preferred_nid`.

## Live Observation

```bash
# Show per-task scheduler statistics (vruntime, nr_switches, etc.)
cat /proc/$(pgrep -n nginx)/sched

# bpftrace: log vruntime of picked tasks
bpftrace -e '
tracepoint:sched:sched_switch {
    printf("next=%s vruntime=%llu\n", args->next_comm, args->next_prio);
}'

# Watch CFS throttle events (when cpu.max is set on a pod)
bpftrace -e 'tracepoint:sched:sched_cfs_throttle_max_vruntime { printf("throttle: cgroup=%u\n", args->cgroup_id); }'

# Check per-cgroup CPU stats (cgroup v2)
cat /sys/fs/cgroup/kubepods.slice/kubepods-pod<uid>.slice/cpu.stat
```

## Key Kernel References

| Symbol | File | URL |
|--------|------|-----|
| `struct sched_entity` | `include/linux/sched.h` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/sched.h |
| `struct cfs_rq` | `kernel/sched/sched.h` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/sched.h |
| `update_curr()` | `kernel/sched/fair.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/fair.c |
| `pick_next_entity()` | `kernel/sched/fair.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/fair.c |
| `throttle_cfs_rq()` | `kernel/sched/fair.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/fair.c |
| `struct cfs_bandwidth` | `kernel/sched/sched.h` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/sched.h |
| `sched_prio_to_weight[]` | `kernel/sched/core.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/core.c |
