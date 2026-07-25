# 08-c — `struct rq`, RT/DL Schedulers, `sched_domain`, NUMA Balancing

## Source Locations

| File | Key Symbols | URL |
|------|-------------|-----|
| `kernel/sched/sched.h` | `struct rq`, `struct rt_rq`, `struct dl_rq`, `struct sched_domain` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/sched.h |
| `kernel/sched/rt.c` | `pick_next_task_rt()`, `enqueue_task_rt()`, `dequeue_task_rt()` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/rt.c |
| `kernel/sched/deadline.c` | `pick_next_task_dl()`, `enqueue_task_dl()` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/deadline.c |
| `kernel/sched/topology.c` | `build_sched_domains()`, `sched_domain_topology` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/topology.c |
| `kernel/sched/numa_balancing.c` | `task_numa_fault()`, `task_numa_migrate()` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/numa_balancing.c |

## `struct rq` — per-CPU Runqueue

There is exactly one `struct rq` per online CPU, embedded in `runqueues` (a per-CPU variable). It is the central scheduler data structure:

```c
// kernel/sched/sched.h (simplified, Linux 6.9)
struct rq {
    raw_spinlock_t      __lock;         // protects this rq
    unsigned int        nr_running;     // total runnable tasks (all classes)
    unsigned long       last_load_update_tick;
    u64                 nr_switches;    // total context switches on this CPU

    struct cfs_rq       cfs;            // CFS runqueue (embedded)
    struct rt_rq        rt;             // RT runqueue (embedded)
    struct dl_rq        dl;             // deadline runqueue (embedded)

    struct task_struct  *curr;          // currently executing task
    struct task_struct  *idle;          // this CPU's idle task
    struct task_struct  *stop;          // migration/stopper thread

    u64                 clock;          // rq local monotonic clock (ns)
    u64                 clock_task;     // excludes idle and irq time
    u64                 clock_pelt;     // PELT clock

    int                 cpu;            // which CPU
    int                 online;         // 1 if CPU is online

    struct sched_domain __rcu *sd;      // top of sched_domain hierarchy

    unsigned long       cpu_capacity;   // CPU capacity (1024 = full speed)
    unsigned long       cpu_capacity_orig;

    struct llist_head   wake_list;      // cross-CPU wake-up queue
    struct mm_struct    *prev_mm;       // prev task mm (for deferred TLB flush)
};
```

Key field: `rq->lock` (now `rq->__lock`) must be held for any modification to `nr_running`, `curr`, or enqueue/dequeue operations. `task_rq_lock()` acquires both the rq lock and disables IRQs.

## RT Scheduler

SCHED_FIFO and SCHED_RR tasks use `rt_sched_class`. The RT runqueue contains a priority array:

```c
// kernel/sched/sched.h (simplified)
struct rt_rq {
    struct rt_prio_array    active;         // 100 priority queues (0-99)
    unsigned int            rt_nr_running;
    unsigned int            rr_nr_running;  // SCHED_RR tasks only
    struct rt_bandwidth     *rt_bandwidth;  // per-cgroup RT bandwidth
    int                     overloaded;     // > 1 RT task on this CPU
    struct plist_head       pushable_tasks; // tasks eligible for push migration
    /* ... */
};
```

`struct rt_prio_array` contains a 100-bit bitmap and 100 `list_head` queues. `pick_next_task_rt()` finds the highest-priority (lowest index) non-empty queue in O(1) using `sched_find_first_bit()`. SCHED_FIFO tasks run to completion (or until preempted by higher RT/DL); SCHED_RR tasks have a time slice (`get_rr_interval_rt()` returns `RR_TIMESLICE`).

## Deadline Scheduler (SCHED_DEADLINE)

SCHED_DEADLINE (EDF — Earliest Deadline First) takes three parameters: `runtime` (execution budget per period), `deadline` (relative deadline), `period`. Implemented in `struct sched_dl_entity`:

```c
// include/linux/sched.h (simplified)
struct sched_dl_entity {
    struct rb_node      rb_node;        // position in dl_rq red-black tree
    u64                 dl_runtime;     // budget in ns per period
    u64                 dl_deadline;    // absolute deadline (ns, from rq->clock)
    u64                 dl_period;      // replenishment period in ns
    u64                 dl_bw;          // dl_runtime / dl_period (bandwidth fraction)
    unsigned int        dl_throttled:1; // overrun: suspended until next period
    unsigned int        dl_yielded:1;
    /* ... */
};
```

`pick_next_task_dl()` selects the leftmost node in `dl_rq->root` (earliest absolute deadline). An admission test in `dl_overflow()` rejects `sched_setattr()` calls that would exceed total CPU bandwidth.

## Scheduling Domains

`struct sched_domain` forms a hierarchy from innermost (SMT siblings) to outermost (NUMA nodes). Built by `build_sched_domains()`:

```
sched_domain_topology (global array):
  SD_TOPOLOGY_LEVEL_SMT   → spans HT siblings (same physical core)
  SD_TOPOLOGY_LEVEL_MC    → spans cores (same LLC)
  SD_TOPOLOGY_LEVEL_NUMA  → spans NUMA nodes
```

Load balancing runs at each level: `load_balance()` starts at the current CPU's innermost domain and walks up to the root. Each domain has `imbalance_pct`, `cache_nice_tries`, and `flags` controlling when migration is attempted. The `SD_NUMA` flag marks the NUMA boundary; `SD_BALANCE_FORK`/`SD_BALANCE_EXEC` control when newly created/exec'd tasks are balanced.

## NUMA Balancing

When `/proc/sys/kernel/numa_balancing = 1` (default), the kernel periodically unmaps a task's pages (PROT_NONE scan via `task_numa_work()`). The resulting page faults call `task_numa_fault()`:

1. Increments `p->numa_faults[node][cpunode][rw]` — tracking which NUMA node caused the fault.
2. If remote faults dominate, `numa_migrate_preferred()` sets `p->numa_preferred_nid` to the node with most faults.
3. `task_numa_migrate()` moves the task to a CPU on the preferred node (respecting `cpus_mask` and load balance).

The scan period (`numa_scan_period`) adapts: if migrations are improving locality, the period shortens; if the task is already local, it lengthens to reduce overhead.

## Live Observation

```bash
# Show per-CPU runqueue statistics
cat /proc/schedstat       # format: cpu<N> yld_count ... run_delay ...

# Show scheduling domain topology
cat /proc/sys/kernel/sched_domain/cpu0/domain0/name

# bpftrace: trace RT task preemptions
bpftrace -e '
tracepoint:sched:sched_wakeup {
    if (args->prio < 100) {  // RT priority
        printf("RT wakeup: %s prio=%d cpu=%d\n", args->comm, args->prio, cpu);
    }
}'

# NUMA migration tracking
cat /proc/$(pgrep -n nginx)/numa_maps | head -5
numastat -p nginx
```

## Key Kernel References

| Symbol | File | URL |
|--------|------|-----|
| `struct rq` | `kernel/sched/sched.h` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/sched.h |
| `struct rt_rq` | `kernel/sched/sched.h` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/sched.h |
| `struct sched_dl_entity` | `include/linux/sched.h` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/sched.h |
| `pick_next_task_rt()` | `kernel/sched/rt.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/rt.c |
| `task_numa_fault()` | `kernel/sched/numa_balancing.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/numa_balancing.c |
| `build_sched_domains()` | `kernel/sched/topology.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/topology.c |
| `struct sched_domain` | `include/linux/sched/sd_flags.h` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/sched/sd_flags.h |
