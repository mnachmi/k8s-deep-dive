# Chapter 08 — CPU Scheduler Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build Chapter 08 covering the Linux CPU scheduler — from `struct task_struct` scheduling fields through CFS, the per-CPU runqueue, NUMA balancing, and CPU affinity — and connect each mechanism to how Kubernetes manages CPU resources.

**Architecture:** Same structure as previous chapters: `kernel/` (3 deep-dive docs), `k8s/` (1 connection doc), `exercises/` (C + Go), `kube-inspect/` (checkpoint 08). C exercise uses `sched_setaffinity(2)` and `/proc/self/status` — libc only. Go exercise parses `/proc/<pid>/sched` and `/proc/<pid>/status` — stdlib only. kube-inspect checkpoint reads cpuset.cpus / cpuset.mems from the pod cgroup.

**Tech Stack:** Markdown, C (gcc, glibc sched.h, pthread.h), Go 1.22, Linux 6.9 kernel source via elixir.bootlin.com

## Global Constraints

- All kernel struct fields/functions must be accurate for Linux 6.9
- All elixir.bootlin.com URLs: bare format (`https://elixir.bootlin.com/linux/v6.9/source/...`), never `[text](url)`
- C compile: `gcc -Wall -Wextra -Werror -o <name> <name>.c -lpthread`
- Go: `go build ./...` + `go vet ./...` must pass
- Go module for exercises: `github.com/linux-to-k8s/<exercise-name>`, go 1.22
- No placeholder text (no TBD, TODO, etc.)
- Every kernel doc has a `## Key Kernel References` table with ≥ 5 entries (bare URLs)
- Reading order: 08-a → 08-b → 08-c → k8s-connection
- Prerequisites: Ch01 (task_struct basics), Ch03 (cgroups cpu controller)

---

## Task 1: README + `08-a-task-sched.md` — task_struct scheduling fields and sched_class

**Files:**
- Modify: `08-scheduler/README.md` (replace stub)
- Create: `08-scheduler/kernel/08-a-task-sched.md`

### `08-scheduler/README.md`

Replace the `(coming soon)` stub:

```markdown
# Chapter 08 — CPU Scheduler

The Linux CPU scheduler decides which task runs next on which CPU. It is not a single algorithm: five scheduler classes form a priority-ordered chain, each implementing a common vtable (`struct sched_class`). The Completely Fair Scheduler (CFS) handles normal processes; real-time and deadline classes serve time-sensitive workloads. This chapter follows the scheduler from the per-CPU runqueue through vruntime bookkeeping, NUMA-aware balancing, and CPU affinity — then shows how Kubernetes CPU requests, limits, and pinning map directly to these kernel primitives.

## Learning Objectives

1. Read every scheduling-relevant field in `struct task_struct` and understand what changes them
2. Understand the `sched_class` vtable and the five scheduler classes
3. Trace a context switch from `schedule()` through `pick_next_task()` to `switch_to()`
4. Understand CFS vruntime accounting and the red-black tree runqueue
5. Understand how Kubernetes CPU requests/limits map to cgroup cpu.weight / cpu.max and how static CPU pinning works

## Prerequisites

- Chapter 01 — Process Model (`struct task_struct`, clone, namespaces)
- Chapter 03 — cgroups (cpu controller, cgroup hierarchy)
- Familiarity with bpftrace (used throughout for live observation)

## Reading Order

| File | Topic |
|------|-------|
| `kernel/08-a-task-sched.md` | `struct task_struct` sched fields, `sched_class` vtable |
| `kernel/08-b-cfs.md` | CFS: `struct sched_entity`, `struct cfs_rq`, vruntime, RB tree |
| `kernel/08-c-rq.md` | `struct rq`, RT/DL schedulers, load balancing, NUMA |
| `k8s/08-k8s-connection.md` | CPU requests/limits, cpuset, topology manager, QoS |
| `exercises/sched-affinity-demo/` | C: CPU pinning with `sched_setaffinity(2)` |
| `exercises/proc-sched-reader/` | Go: parse `/proc/<pid>/sched` and `/proc/<pid>/status` |
| `kube-inspect` checkpoint 08 | CPU affinity + NUMA placement per pod |

## Scheduler Class Priority

```
stop_sched_class   ←  migration/stopper threads (highest)
  dl_sched_class   ←  SCHED_DEADLINE tasks
  rt_sched_class   ←  SCHED_FIFO / SCHED_RR tasks
  fair_sched_class ←  SCHED_NORMAL / SCHED_BATCH (CFS)
  idle_sched_class ←  idle threads (lowest)
```
```

### `08-scheduler/kernel/08-a-task-sched.md`

Full Data Structure Deep Dive. Required sections:

**1. Source Locations**

| File | Key Symbols | URL |
|------|-------------|-----|
| `include/linux/sched.h` | `struct task_struct`, `struct sched_entity`, `struct sched_rt_entity` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/sched.h |
| `kernel/sched/sched.h` | `struct sched_class`, `struct cfs_rq`, `struct rt_rq`, `struct rq` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/sched.h |
| `kernel/sched/core.c` | `schedule()`, `__schedule()`, `context_switch()`, `sched_setaffinity()` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/core.c |
| `kernel/sched/fair.c` | `enqueue_entity()`, `pick_next_entity()`, `update_curr()` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/fair.c |
| `arch/x86/entry/entry_64.S` | `__switch_to_asm` | https://elixir.bootlin.com/linux/v6.9/source/arch/x86/entry/entry_64.S |

**2. Scheduling Fields in `struct task_struct`**

Show the exact fields from `include/linux/sched.h` (Linux 6.9). Use a simplified struct snippet followed by field-by-field explanation:

```c
// include/linux/sched.h (scheduling-relevant fields, Linux 6.9)
struct task_struct {
    /* --- state --- */
    unsigned int            __state;       // TASK_RUNNING, TASK_INTERRUPTIBLE, ...

    /* --- scheduler class --- */
    int                     prio;          // dynamic priority: 0-99 RT, 100-139 CFS
    int                     static_prio;   // nice-to-prio: 120 + nice (-20..19)
    int                     normal_prio;   // derived from sched_class + static_prio
    unsigned int            rt_priority;   // RT priority 1-99 (99 = highest RT)
    const struct sched_class *sched_class; // vtable: fair/rt/dl/idle/stop
    struct sched_entity     se;            // CFS entity (embedded, not a pointer)
    struct sched_rt_entity  rt;            // RT entity
    struct sched_dl_entity  dl;            // deadline entity
    struct task_group       *sched_task_group; // cgroup task group

    /* --- CPU affinity --- */
    cpumask_t               cpus_mask;     // allowed CPUs (set by sched_setaffinity)
    int                     nr_cpus_allowed; // popcount(cpus_mask)

    /* --- NUMA --- */
    int                     numa_preferred_nid; // preferred NUMA node (-1 = none)
    struct numa_faults      *numa_faults;   // per-node fault counters (lazy alloc)
    unsigned long           numa_faults_locality[3]; // local/remote/interleave

    /* ... many more fields ... */
};
```

Field-by-field explanations:

- `__state`: bitmask set by `set_current_state()`. Key values: `TASK_RUNNING` (0) — on a runqueue or currently executing; `TASK_INTERRUPTIBLE` (1) — sleeping, wakes on signal or event; `TASK_UNINTERRUPTIBLE` (2) — sleeping, ignores signals (disk I/O); `__TASK_STOPPED` (4) — stopped by SIGSTOP; `EXIT_ZOMBIE` (32) — exited, waiting for parent `wait4()`.

- `prio`: the effective priority used by the scheduler. For CFS tasks (nice -20 to 19), `prio` ranges 100–139. For RT tasks (SCHED_FIFO/SCHED_RR), `prio` ranges 0–99 (lower = higher priority on the RT scale, but the scheduler treats RT as numerically lower = higher urgency).

- `static_prio`: computed as `120 + nice`. Changed only by `setpriority(2)` / `nice(2)`.

- `normal_prio`: `static_prio` for non-RT tasks; `MAX_RT_PRIO - 1 - rt_priority` for RT tasks. Never changed after task creation except when the task switches scheduling policies.

- `sched_class`: pointer to one of five vtable singletons (address comparison determines precedence): `stop_sched_class` → `dl_sched_class` → `rt_sched_class` → `fair_sched_class` → `idle_sched_class`.

- `se`, `rt`, `dl`: embedded structs, one per scheduling class. Only the class that owns the task has an active entity on a runqueue at any time.

- `cpus_mask`: a `cpumask_t` (bitmap over `NR_CPUS` bits). Modified only by `__set_cpus_allowed_ptr()` in `kernel/sched/core.c`. The kernel never migrates a task to a CPU outside this mask.

**3. `struct sched_class` Vtable**

```c
// kernel/sched/sched.h (simplified, Linux 6.9)
struct sched_class {
    void (*enqueue_task)    (struct rq *rq, struct task_struct *p, int flags);
    void (*dequeue_task)    (struct rq *rq, struct task_struct *p, int flags);
    void (*yield_task)      (struct rq *rq);
    struct task_struct *(*pick_next_task)(struct rq *rq);
    void (*put_prev_task)   (struct rq *rq, struct task_struct *p);
    void (*set_curr_task)   (struct rq *rq);
    void (*task_tick)       (struct rq *rq, struct task_struct *p, int queued);
    void (*task_fork)       (struct task_struct *p);
    void (*switched_to)     (struct rq *rq, struct task_struct *p);
    void (*prio_changed)    (struct rq *rq, struct task_struct *p, int oldprio);
    unsigned int (*get_rr_interval)(struct rq *rq, struct task_struct *p);
};
```

The five singletons are declared in `kernel/sched/sched.h` and defined across `kernel/sched/fair.c`, `rt.c`, `deadline.c`, `idle.c`, `stop_task.c`.

**4. Context Switch Path**

```
tick interrupt → scheduler_tick() → task_tick() on sched_class
                                   → set_tsk_need_resched(curr)

return-to-userspace or explicit sleep → schedule()
  └─ __schedule(SM_NONE)
       ├─ pick_next_task(rq, prev)        // sched_class->pick_next_task()
       │    └─ for CFS: pick_next_entity() — leftmost rb node by vruntime
       └─ context_switch(rq, prev, next)
            ├─ switch_mm_irqs_off()       // load next->mm page tables (CR3 on x86)
            └─ switch_to(prev, next, prev)
                 └─ __switch_to_asm       // arch/x86/entry/entry_64.S
                      ├─ save callee-saved regs (rbx, r12-r15, rbp) on prev stack
                      ├─ mov rsp → prev->thread.sp
                      ├─ mov next->thread.sp → rsp
                      └─ restore callee-saved regs from next stack → ret
```

After `__switch_to_asm` returns, execution continues in the context of `next` (at whatever point it was last preempted).

**5. CPU Affinity Syscall Path**

`sched_setaffinity(pid, cpusetsize, user_mask_ptr)` → `kernel/sched/core.c`:
1. Copies the user cpumask into kernel space.
2. Intersects with `cpu_active_mask` and the task's cgroup cpuset constraint.
3. Calls `__set_cpus_allowed_ptr(p, new_mask, SCA_USER)`.
4. If the current CPU is no longer in `new_mask`, `stop_one_cpu()` migrates the task to a CPU in the new mask.

**6. Live Observation**

```bash
# Show scheduler class and priority for all processes
ps -eo pid,cls,rtprio,pri,ni,comm | head -20

# Watch context switch rate per task (from /proc/<pid>/status)
watch -n1 "grep -E 'voluntary|nonvoluntary' /proc/\$(pgrep -n nginx)/status"

# bpftrace: trace every context switch and log prev/next
bpftrace -e '
tracepoint:sched:sched_switch {
    printf("%-16s %-6d -> %-16s %-6d cpu=%d\n",
           args->prev_comm, args->prev_pid,
           args->next_comm, args->next_pid,
           cpu);
}'

# bpftrace: show NUMA migrations
bpftrace -e 'tracepoint:sched:sched_migrate_task { printf("%s pid=%d cpu %d->%d\n", comm, pid, args->orig_cpu, args->dest_cpu); }'
```

**7. Key Kernel References**

| Symbol | File | URL |
|--------|------|-----|
| `struct task_struct` | `include/linux/sched.h` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/sched.h |
| `struct sched_class` | `kernel/sched/sched.h` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/sched.h |
| `__schedule()` | `kernel/sched/core.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/core.c |
| `context_switch()` | `kernel/sched/core.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/core.c |
| `__switch_to_asm` | `arch/x86/entry/entry_64.S` | https://elixir.bootlin.com/linux/v6.9/source/arch/x86/entry/entry_64.S |
| `sched_setaffinity()` | `kernel/sched/core.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/core.c |
| `fair_sched_class` | `kernel/sched/fair.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/fair.c |

- [ ] **Step 1: Read the existing ch08 README stub**

```bash
cat 08-scheduler/README.md
```

- [ ] **Step 2: Write `08-scheduler/README.md`** — replace stub with full chapter intro as specified above

- [ ] **Step 3: Write `08-scheduler/kernel/08-a-task-sched.md`** — all 7 sections exactly as specified above

- [ ] **Step 4: Verify no placeholder text, all URLs are bare v6.9, ≥ 5 Key References**

- [ ] **Step 5: Commit**

```bash
git add 08-scheduler/README.md 08-scheduler/kernel/08-a-task-sched.md
git commit -m "docs(ch08): README + 08-a task_struct scheduling fields and sched_class vtable"
```

---

## Task 2: `08-b-cfs.md` — CFS: struct sched_entity, struct cfs_rq, vruntime

**Files:**
- Create: `08-scheduler/kernel/08-b-cfs.md`

Full Data Structure Deep Dive. Required sections:

**1. Source Locations**

| File | Key Symbols | URL |
|------|-------------|-----|
| `kernel/sched/fair.c` | `update_curr()`, `enqueue_entity()`, `pick_next_entity()`, `dequeue_entity()` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/fair.c |
| `kernel/sched/sched.h` | `struct cfs_rq`, `struct cfs_bandwidth` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/sched.h |
| `include/linux/sched.h` | `struct sched_entity`, `struct load_weight` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/sched.h |
| `kernel/sched/pelt.c` | `update_load_avg()`, per-entity load tracking | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/pelt.c |
| `kernel/sched/autogroup.c` | `autogroup_task_group()` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/autogroup.c |

**2. `struct sched_entity`**

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

**3. `struct cfs_rq`**

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

**4. `min_vruntime` and Fairness**

`min_vruntime` is the CFS floor: it monotonically increases as `update_min_vruntime()` advances it to `min(curr->vruntime, leftmost->vruntime)`. When a task wakes from sleep, `place_entity()` sets its `vruntime = max(vruntime, cfs_rq->min_vruntime - sched_latency_ns/2)` — preventing a long-sleeping task from immediately starving all others by running with stale vruntime.

**5. CFS Bandwidth (cpu.max / cpu.cfs_quota_us)**

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

**6. Load Balancing**

`load_balance()` is called from `run_rebalance_domains()` on each scheduler tick. It walks the `sched_domain` hierarchy (SMT → LLC → NUMA node) looking for the busiest CPU in each domain. If the imbalance exceeds a threshold, `move_tasks()` migrates the heaviest migratable task to the idle CPU. Migration respects `cpus_mask` and `numa_preferred_nid`.

**7. Live Observation**

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

**8. Key Kernel References**

| Symbol | File | URL |
|--------|------|-----|
| `struct sched_entity` | `include/linux/sched.h` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/sched.h |
| `struct cfs_rq` | `kernel/sched/sched.h` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/sched.h |
| `update_curr()` | `kernel/sched/fair.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/fair.c |
| `pick_next_entity()` | `kernel/sched/fair.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/fair.c |
| `throttle_cfs_rq()` | `kernel/sched/fair.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/fair.c |
| `struct cfs_bandwidth` | `kernel/sched/sched.h` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/sched.h |
| `sched_prio_to_weight[]` | `kernel/sched/core.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/core.c |

- [ ] **Step 1: Write `08-scheduler/kernel/08-b-cfs.md`** — all 8 sections exactly as specified above

- [ ] **Step 2: Verify no placeholder text, all URLs bare v6.9, ≥ 5 Key References**

- [ ] **Step 3: Commit**

```bash
git add 08-scheduler/kernel/08-b-cfs.md
git commit -m "docs(ch08): 08-b CFS sched_entity, cfs_rq, vruntime, bandwidth throttle"
```

---

## Task 3: `08-c-rq.md` — struct rq, RT/DL schedulers, sched_domain, NUMA

**Files:**
- Create: `08-scheduler/kernel/08-c-rq.md`

Full Data Structure Deep Dive. Required sections:

**1. Source Locations**

| File | Key Symbols | URL |
|------|-------------|-----|
| `kernel/sched/sched.h` | `struct rq`, `struct rt_rq`, `struct dl_rq`, `struct sched_domain` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/sched.h |
| `kernel/sched/rt.c` | `pick_next_task_rt()`, `enqueue_task_rt()`, `dequeue_task_rt()` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/rt.c |
| `kernel/sched/deadline.c` | `pick_next_task_dl()`, `enqueue_task_dl()` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/deadline.c |
| `kernel/sched/topology.c` | `build_sched_domains()`, `sched_domain_topology` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/topology.c |
| `kernel/sched/numa_balancing.c` | `task_numa_fault()`, `task_numa_migrate()` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/numa_balancing.c |

**2. `struct rq` — per-CPU runqueue**

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

**3. RT Scheduler**

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

**4. Deadline Scheduler (SCHED_DEADLINE)**

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

**5. Scheduling Domains**

`struct sched_domain` forms a hierarchy from innermost (SMT siblings) to outermost (NUMA nodes). Built by `build_sched_domains()`:

```
sched_domain_topology (global array):
  SD_TOPOLOGY_LEVEL_SMT   → spans HT siblings (same physical core)
  SD_TOPOLOGY_LEVEL_MC    → spans cores (same LLC)
  SD_TOPOLOGY_LEVEL_NUMA  → spans NUMA nodes
```

Load balancing runs at each level: `load_balance()` starts at the current CPU's innermost domain and walks up to the root. Each domain has `imbalance_pct`, `cache_nice_tries`, and `flags` controlling when migration is attempted. The `SD_NUMA` flag marks the NUMA boundary; `SD_BALANCE_FORK`/`SD_BALANCE_EXEC` control when newly created/exec'd tasks are balanced.

**6. NUMA Balancing**

When `/proc/sys/kernel/numa_balancing = 1` (default), the kernel periodically unmaps a task's pages (PROT_NONE scan via `task_numa_work()`). The resulting page faults call `task_numa_fault()`:

1. Increments `p->numa_faults[node][cpunode][rw]` — tracking which NUMA node caused the fault.
2. If remote faults dominate, `numa_migrate_preferred()` sets `p->numa_preferred_nid` to the node with most faults.
3. `task_numa_migrate()` moves the task to a CPU on the preferred node (respecting `cpus_mask` and load balance).

The scan period (`numa_scan_period`) adapts: if migrations are improving locality, the period shortens; if the task is already local, it lengthens to reduce overhead.

**7. Live Observation**

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

**8. Key Kernel References**

| Symbol | File | URL |
|--------|------|-----|
| `struct rq` | `kernel/sched/sched.h` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/sched.h |
| `struct rt_rq` | `kernel/sched/sched.h` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/sched.h |
| `struct sched_dl_entity` | `include/linux/sched.h` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/sched.h |
| `pick_next_task_rt()` | `kernel/sched/rt.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/rt.c |
| `task_numa_fault()` | `kernel/sched/numa_balancing.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/numa_balancing.c |
| `build_sched_domains()` | `kernel/sched/topology.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/topology.c |
| `struct sched_domain` | `include/linux/sched/sd_flags.h` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/sched/sd_flags.h |

- [ ] **Step 1: Write `08-scheduler/kernel/08-c-rq.md`** — all 8 sections exactly as specified above

- [ ] **Step 2: Verify no placeholder text, all URLs bare v6.9, ≥ 5 Key References**

- [ ] **Step 3: Commit**

```bash
git add 08-scheduler/kernel/08-c-rq.md
git commit -m "docs(ch08): 08-c struct rq, RT/DL schedulers, sched_domain, NUMA balancing"
```

---

## Task 4: `08-k8s-connection.md` — CPU requests/limits, cpuset, topology manager

**Files:**
- Create: `08-scheduler/k8s/08-k8s-connection.md`

Required sections:

**1. Architecture Overview**

Table mapping Kubernetes CPU concepts to kernel primitives:

| K8s Concept | Kernel Primitive | File / Interface |
|-------------|-----------------|------------------|
| CPU request (`resources.requests.cpu`) | `cpu.weight` (cgroup v2) / `cpu.shares` (v1) | `/sys/fs/cgroup/.../cpu.weight` |
| CPU limit (`resources.limits.cpu`) | `cpu.max` (cgroup v2) / `cpu.cfs_quota_us` (v1) | `/sys/fs/cgroup/.../cpu.max` |
| CPU pinning (static policy) | `cpuset.cpus` (cgroup cpuset) | `/sys/fs/cgroup/.../cpuset.cpus` |
| NUMA affinity | `cpuset.mems` (cgroup cpuset) | `/sys/fs/cgroup/.../cpuset.mems` |
| QoS Guaranteed | highest `cpu.weight` (10000) + static cpuset eligible | kubelet CPU manager |
| QoS BestEffort | lowest `cpu.weight` (1) | kubelet CPU manager |

**2. CPU Requests → `cpu.weight`**

In cgroup v2, `cpu.weight` ranges 1–10000 (default 100). Kubelet sets it as:

```
cpu.weight = max(1, min(10000, milliCPU × 10 / 1000))
# For 250m: weight = max(1, 250×10/1000) = 2  (rounds down)
# For 1000m: weight = 10
# For 4000m: weight = 40
```

This maps directly to `task_group->shares`, which feeds into each CFS entity's `load.weight` via `calc_group_shares()`. A container requesting 4× more CPU than another gets roughly 4× the CFS weight → 4× the runtime when both are runnable.

Cgroup v1 equivalent: `cpu.shares` (default 1024). Kubelet computes `shares = max(2, milliCPU × 1024 / 1000)`.

**3. CPU Limits → `cpu.max` / CFS Bandwidth**

`cpu.max` format: `<quota_us> <period_us>`. For `limits.cpu: "0.5"`, kubelet writes:
```
50000 100000
```
meaning the container may use 50ms of CPU per 100ms period. The kernel enforces via `struct cfs_bandwidth`:
- `cfs_b->quota = 50000000 ns` (50 ms)
- `cfs_b->period = 100000000 ns` (100 ms)
- `throttle_cfs_rq()` removes the runqueue at quota exhaustion
- `sched_cfs_period_timer()` fires each period and calls `distribute_cfs_runtime()` to unthrottle

Diagnosis: throttled pods show `throttled_usec` growing in `cpu.stat`:
```bash
cat /sys/fs/cgroup/kubepods.slice/kubepods-pod<uid>.slice/cpu.stat
```
Fields: `usage_usec`, `user_usec`, `system_usec`, `nr_throttled`, `throttled_usec`, `nr_bursts`, `burst_usec`.

**4. CPU Manager — Static Policy**

When kubelet runs with `--cpu-manager-policy=static`, Guaranteed pods with integer CPU requests get exclusive CPUs:

1. Kubelet maintains a CPU pool (all CPUs minus `--reserved-cpus` and system overhead).
2. On pod admission, it allocates `floor(cpu_request)` exclusive CPUs.
3. Writes them to `cpuset.cpus`: e.g., `0-3` for a 4-CPU Guaranteed pod.
4. Also writes `cpuset.mems` to the corresponding NUMA nodes.
5. Remaining BestEffort / Burstable pods share the "default" cpuset (all non-exclusive CPUs).

The kernel enforces via `cpuset_can_attach()` + `cpuset_attach()` in `kernel/cgroup/cpuset.c`: tasks in the cpuset cgroup can only be scheduled on CPUs in `cpuset.cpus`.

**5. Topology Manager**

`--topology-manager-policy` aligns CPU, memory, and device (GPU/RDMA NIC) NUMA hints:

| Policy | Behavior |
|--------|----------|
| `none` (default) | No topology alignment |
| `best-effort` | Prefer aligned NUMA, admit even if impossible |
| `restricted` | Require aligned NUMA; reject pod if impossible |
| `single-numa-node` | All resources must be on same NUMA node |

Hint providers: CPU Manager, Memory Manager (`--memory-manager-policy`), Device Manager. Each returns a `TopologyHint` (a bitmask of NUMA nodes that can satisfy the request). The Topology Manager intersects all hints to find the best affinity.

**6. QoS Classes**

| QoS Class | Condition | cpu.weight | cpuset eligible |
|-----------|-----------|-----------|-----------------|
| `Guaranteed` | All containers: requests == limits | 10 per milliCPU | Yes (static policy) |
| `Burstable` | At least one container: requests < limits | proportional to requests | No |
| `BestEffort` | No requests or limits set | 2 (minimum) | No |

The OOM killer in the kernel also uses these classes: BestEffort processes are killed first (oom_score_adj ≈ 1000), Burstable next, Guaranteed last (oom_score_adj = -998 for Guaranteed).

**7. Common Failure Patterns**

| Symptom | Kernel Cause | Fix |
|---------|-------------|-----|
| Pod throttled at < limit | CFS period too short (burst spikes exceed quota) | Increase `--cpu-cfs-period` or add `cpu.burst` |
| Noisy neighbor spikes | BestEffort pod on shared cpuset | Use CPU manager static policy for latency-sensitive pods |
| NUMA remote memory faults | Container cpuset spans NUMA nodes | Set `--topology-manager-policy=single-numa-node` |
| High involuntary context switches | RT task competing with CFS | Pin RT workload to dedicated CPUs via cpuset |
| `cpu.stat` shows `nr_throttled` > 0 | Quota exhausted mid-burst | Raise `limits.cpu` or enable `cpuCFSBurst` feature |
| Latency spike every 100ms | CFS period expiry during burst | Diagnose with `bpftrace tracepoint:sched:sched_cfs_throttle_max_vruntime` |

**8. Verification Commands**

```bash
# Show cpu.weight and cpu.max for a pod
PODUID=<uid>
CGROUP=/sys/fs/cgroup/kubepods.slice
find $CGROUP -name "pod${PODUID}.slice" | head -1 | xargs -I{} sh -c \
  'echo weight=$(cat {}/cpu.weight); echo max=$(cat {}/cpu.max)'

# List all throttled pods
find /sys/fs/cgroup/kubepods.slice -name cpu.stat | xargs grep -l 'nr_throttled [^0]'

# Show CPU pinning for a Guaranteed pod
cat /sys/fs/cgroup/kubepods.slice/kubepods-guaranteed-pod${PODUID}.slice/cpuset.cpus
cat /sys/fs/cgroup/kubepods.slice/kubepods-guaranteed-pod${PODUID}.slice/cpuset.mems
```

**9. Key Kernel References**

| Symbol | File | URL |
|--------|------|-----|
| `throttle_cfs_rq()` | `kernel/sched/fair.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/fair.c |
| `struct cfs_bandwidth` | `kernel/sched/sched.h` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/sched.h |
| `cpuset_can_attach()` | `kernel/cgroup/cpuset.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/cgroup/cpuset.c |
| `cpu_shares_write_u64()` (v1 shares) | `kernel/sched/core.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/core.c |
| `tg_set_cfs_bandwidth()` | `kernel/sched/fair.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/fair.c |
| `calc_group_shares()` | `kernel/sched/fair.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/fair.c |

- [ ] **Step 1: Write `08-scheduler/k8s/08-k8s-connection.md`** — all 9 sections as specified above

- [ ] **Step 2: Verify all facts against Linux 6.9, no placeholder text**

- [ ] **Step 3: Commit**

```bash
git add 08-scheduler/k8s/08-k8s-connection.md
git commit -m "docs(ch08): 08-k8s-connection CPU requests/limits, cpuset, topology manager, QoS"
```

---

## Task 5: C exercise — `sched-affinity-demo`

**Files:**
- Create: `08-scheduler/exercises/sched-affinity-demo/sched_affinity_demo.c`
- Create: `08-scheduler/exercises/sched-affinity-demo/Makefile`
- Create: `08-scheduler/exercises/sched-affinity-demo/README.md`

### `sched_affinity_demo.c`

A self-contained C program (no dependencies beyond libc + pthreads) that demonstrates CPU affinity and its effect on scheduling:

```c
/*
 * sched_affinity_demo.c — demonstrate sched_setaffinity(2) and observe
 * the kernel scheduler obeying cpus_mask.
 *
 * Demonstrates:
 *   a) Pinning the main process to CPU 0 with sched_setaffinity(2)
 *   b) Spawning two threads: one pinned to CPU 0, one pinned to CPU 1
 *   c) Reading /proc/self/status to verify Cpus_allowed_list
 *   d) Printing nice-to-weight table for nice values -5 to +5
 *   e) Printing the current task's scheduling policy and priority
 *
 * Build:  gcc -Wall -Wextra -Werror -o sched_affinity_demo sched_affinity_demo.c -lpthread
 * Run:    ./sched_affinity_demo
 *
 * Kernel path:
 *   sched_setaffinity(2) -> kernel/sched/core.c:sched_setaffinity()
 *   https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/core.c
 */

#define _GNU_SOURCE
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>
#include <pthread.h>
#include <sched.h>
#include <errno.h>
#include <sys/syscall.h>
#include <sys/types.h>

/* Kernel nice-to-weight table (sched_prio_to_weight[], kernel/sched/core.c)
 * Index 0 = nice -20, index 39 = nice +19.
 * Only show nice -5..+5 (indices 15..25). */
static const int nice_to_weight[] = {
    /* nice -5 */ 3121,
    /* nice -4 */ 2501,
    /* nice -3 */ 1991,
    /* nice -2 */ 1586,
    /* nice -1 */ 1277,
    /* nice  0 */ 1024,
    /* nice +1 */  820,
    /* nice +2 */  655,
    /* nice +3 */  526,
    /* nice +4 */  423,
    /* nice +5 */  335,
};

static void print_cpus_allowed(const char *label)
{
    char buf[256];
    FILE *f = fopen("/proc/self/status", "r");
    if (!f) { perror("fopen /proc/self/status"); return; }
    while (fgets(buf, sizeof(buf), f)) {
        if (strncmp(buf, "Cpus_allowed_list:", 18) == 0) {
            printf("  [%s] Cpus_allowed_list: %s", label, buf + 18);
            break;
        }
    }
    fclose(f);
}

static void pin_to_cpu(int cpu)
{
    cpu_set_t set;
    CPU_ZERO(&set);
    CPU_SET(cpu, &set);
    if (sched_setaffinity(0, sizeof(set), &set) != 0) {
        perror("sched_setaffinity");
        exit(1);
    }
}

struct thread_arg { int cpu; int id; };

static void *thread_fn(void *arg)
{
    struct thread_arg *a = arg;
    pin_to_cpu(a->cpu);
    /* sched_getcpu() reads the VDSO clock_gettime-style fast path on x86 */
    int actual = sched_getcpu();
    printf("  Thread %d: pinned to CPU %d, actually running on CPU %d\n",
           a->id, a->cpu, actual);
    print_cpus_allowed("thread");
    return NULL;
}

int main(void)
{
    int nprocs = (int)sysconf(_SC_NPROCESSORS_ONLN);
    printf("Online CPUs: %d\n\n", nprocs);

    /* --- Part 1: pin main to CPU 0 --- */
    printf("=== Part 1: pin main to CPU 0 ===\n");
    pin_to_cpu(0);
    printf("  Running on CPU %d\n", sched_getcpu());
    print_cpus_allowed("main");

    /* --- Part 2: two threads on separate CPUs --- */
    printf("\n=== Part 2: two threads, CPU 0 and CPU %d ===\n",
           nprocs > 1 ? 1 : 0);
    pthread_t t1, t2;
    struct thread_arg a1 = {0, 1};
    struct thread_arg a2 = {nprocs > 1 ? 1 : 0, 2};
    pthread_create(&t1, NULL, thread_fn, &a1);
    pthread_create(&t2, NULL, thread_fn, &a2);
    pthread_join(t1, NULL);
    pthread_join(t2, NULL);

    /* --- Part 3: nice-to-weight table --- */
    printf("\n=== Part 3: nice → CFS weight (kernel/sched/core.c sched_prio_to_weight) ===\n");
    for (int i = 0; i <= 10; i++) {
        printf("  nice %+3d → weight %5d\n", i - 5, nice_to_weight[i]);
    }

    /* --- Part 4: current scheduling policy --- */
    printf("\n=== Part 4: current scheduling policy ===\n");
    int policy = sched_getscheduler(0);
    struct sched_param param;
    sched_getparam(0, &param);
    const char *policy_name =
        policy == SCHED_OTHER   ? "SCHED_OTHER (CFS)" :
        policy == SCHED_FIFO    ? "SCHED_FIFO (RT)" :
        policy == SCHED_RR      ? "SCHED_RR (RT)" :
        policy == SCHED_BATCH   ? "SCHED_BATCH" :
        policy == SCHED_IDLE    ? "SCHED_IDLE" :
        policy == SCHED_DEADLINE ? "SCHED_DEADLINE" : "UNKNOWN";
    printf("  policy=%s sched_priority=%d\n", policy_name, param.sched_priority);
    printf("  nice=%d\n", getpriority(PRIO_PROCESS, 0));

    return 0;
}
```

### `Makefile`

```makefile
CC      = gcc
CFLAGS  = -Wall -Wextra -Werror
LDFLAGS = -lpthread
TARGET  = sched_affinity_demo

all: $(TARGET)

$(TARGET): sched_affinity_demo.c
	$(CC) $(CFLAGS) -o $@ $< $(LDFLAGS)

run: $(TARGET)
	./$(TARGET)

clean:
	rm -f $(TARGET)

.PHONY: all run clean
```

### `README.md`

```markdown
# sched-affinity-demo

Demonstrates `sched_setaffinity(2)` — the Linux syscall that sets `task_struct.cpus_mask` and causes the scheduler to restrict a task to specific CPUs.

## What It Shows

| Part | Demonstrates |
|------|-------------|
| Part 1 | Pin main process to CPU 0; verify via `/proc/self/status` |
| Part 2 | Two threads on different CPUs; `sched_getcpu()` confirms placement |
| Part 3 | nice-to-weight table from `sched_prio_to_weight[]` in `kernel/sched/core.c` |
| Part 4 | Current scheduling policy and priority via `sched_getscheduler(2)` |

## Build and Run

```
make
./sched_affinity_demo
```

## Kernel Path

```
sched_setaffinity(2)
  → kernel/sched/core.c:sched_setaffinity()
    → __set_cpus_allowed_ptr(p, new_mask, SCA_USER)
      → stop_one_cpu() if current CPU no longer in mask
```

Source:
https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/core.c

## Exercises

a) Change Part 1 to pin to the last CPU instead of CPU 0.  Verify with `sched_getcpu()`.

b) Add a Part 5 that forks a child, pins it to CPU 0, and then reads the child's
   `Cpus_allowed_list` from `/proc/<child_pid>/status`.

c) Add SCHED_FIFO support: call `sched_setscheduler(0, SCHED_FIFO, &param)` with
   `param.sched_priority = 50`.  Observe that `sched_getscheduler(0)` now returns
   `SCHED_FIFO`.  (Requires CAP_SYS_NICE or running as root.)
```
```

- [ ] **Step 1: Write `08-scheduler/exercises/sched-affinity-demo/sched_affinity_demo.c`** exactly as above

- [ ] **Step 2: Write `08-scheduler/exercises/sched-affinity-demo/Makefile`** exactly as above

- [ ] **Step 3: Write `08-scheduler/exercises/sched-affinity-demo/README.md`** exactly as above

- [ ] **Step 4: Build and verify**

```bash
cd 08-scheduler/exercises/sched-affinity-demo
gcc -Wall -Wextra -Werror -o sched_affinity_demo sched_affinity_demo.c -lpthread
```
Expected: compiles with no warnings or errors.

- [ ] **Step 5: Commit**

```bash
cd ../../..
git add 08-scheduler/exercises/sched-affinity-demo/
git commit -m "feat(ch08): C exercise sched-affinity-demo — sched_setaffinity + nice-to-weight"
```

---

## Task 6: Go exercise — `proc-sched-reader`

**Files:**
- Create: `08-scheduler/exercises/proc-sched-reader/main.go`
- Create: `08-scheduler/exercises/proc-sched-reader/go.mod`
- Create: `08-scheduler/exercises/proc-sched-reader/README.md`

### `main.go`

A standalone Go program that parses `/proc/<pid>/sched` and `/proc/<pid>/status` to extract scheduling information — no external dependencies:

```go
// proc-sched-reader: parses /proc/<pid>/sched and /proc/<pid>/status
// to extract CPU affinity, NUMA affinity, and CFS scheduling statistics.
//
// Usage: proc-sched-reader <pid> [pid...]
// Build: go build -o proc-sched-reader .
//
// Kernel paths:
//   /proc/<pid>/sched     — kernel/sched/debug.c:proc_sched_show_task()
//   /proc/<pid>/status    — fs/proc/array.c:task_status()
//
// Source:
//   https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/debug.c
//   https://elixir.bootlin.com/linux/v6.9/source/fs/proc/array.c
package main

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// SchedInfo holds fields parsed from /proc/<pid>/sched.
type SchedInfo struct {
	PID                    int
	Comm                   string
	VrunTime               float64 // se.vruntime (ns)
	SumExecRuntime         float64 // se.sum_exec_runtime (ns)
	NrVoluntarySwitches    int
	NrInvoluntarySwitches  int
	PrioValue              int     // prio (dynamic priority)
	PolicyValue            int     // sched policy number
}

// StatusInfo holds scheduling-relevant fields from /proc/<pid>/status.
type StatusInfo struct {
	PID              int
	Name             string
	CpusAllowedList  string // e.g. "0-3" or "0,2,4-7"
	MemsAllowedList  string // NUMA nodes, e.g. "0-1"
	VoluntaryCtxt    int
	NonvoluntaryCtxt int
}

// parseSchedFile reads /proc/<pid>/sched and returns a SchedInfo.
// The file has a header line "task_name (pid, #threads: N)" then
// key-value lines: "field.name  :   value".
func parseSchedFile(pid int) (SchedInfo, error) {
	path := fmt.Sprintf("/proc/%d/sched", pid)
	f, err := os.Open(path)
	if err != nil {
		return SchedInfo{}, err
	}
	defer f.Close()

	info := SchedInfo{PID: pid}
	scanner := bufio.NewScanner(f)
	firstLine := true
	for scanner.Scan() {
		line := scanner.Text()
		if firstLine {
			// header: "nginx (1234, #threads: 1)"
			firstLine = false
			if idx := strings.Index(line, " ("); idx > 0 {
				info.Comm = line[:idx]
			}
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])
		switch key {
		case "se.vruntime":
			info.VrunTime, _ = strconv.ParseFloat(val, 64)
		case "se.sum_exec_runtime":
			info.SumExecRuntime, _ = strconv.ParseFloat(val, 64)
		case "nr_voluntary_switches":
			info.NrVoluntarySwitches, _ = strconv.Atoi(val)
		case "nr_involuntary_switches":
			info.NrInvoluntarySwitches, _ = strconv.Atoi(val)
		case "prio":
			info.PrioValue, _ = strconv.Atoi(val)
		case "policy":
			info.PolicyValue, _ = strconv.Atoi(val)
		}
	}
	return info, scanner.Err()
}

// parseStatusFile reads scheduling-relevant fields from /proc/<pid>/status.
func parseStatusFile(pid int) (StatusInfo, error) {
	path := fmt.Sprintf("/proc/%d/status", pid)
	f, err := os.Open(path)
	if err != nil {
		return StatusInfo{}, err
	}
	defer f.Close()

	info := StatusInfo{PID: pid}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])
		switch key {
		case "Name":
			info.Name = val
		case "Cpus_allowed_list":
			info.CpusAllowedList = val
		case "Mems_allowed_list":
			info.MemsAllowedList = val
		case "voluntary_ctxt_switches":
			info.VoluntaryCtxt, _ = strconv.Atoi(val)
		case "nonvoluntary_ctxt_switches":
			info.NonvoluntaryCtxt, _ = strconv.Atoi(val)
		}
	}
	return info, scanner.Err()
}

func policyName(policy int) string {
	switch policy {
	case 0:
		return "SCHED_NORMAL"
	case 1:
		return "SCHED_FIFO"
	case 2:
		return "SCHED_RR"
	case 3:
		return "SCHED_BATCH"
	case 5:
		return "SCHED_IDLE"
	case 6:
		return "SCHED_DEADLINE"
	default:
		return fmt.Sprintf("UNKNOWN(%d)", policy)
	}
}

func printPID(pid int) {
	sched, err := parseSchedFile(pid)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pid %d: %v\n", pid, err)
		return
	}
	status, err := parseStatusFile(pid)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pid %d: %v\n", pid, err)
		return
	}

	fmt.Printf("PID %d  (%s)\n", pid, sched.Comm)
	fmt.Printf("  Policy         : %s\n", policyName(sched.PolicyValue))
	fmt.Printf("  Prio (dynamic) : %d  (100-139=CFS, 0-99=RT)\n", sched.PrioValue)
	fmt.Printf("  vruntime       : %.3f ms\n", sched.VrunTime/1e6)
	fmt.Printf("  sum_exec       : %.3f ms\n", sched.SumExecRuntime/1e6)
	fmt.Printf("  voluntary sw   : %d\n", sched.NrVoluntarySwitches)
	fmt.Printf("  involuntary sw : %d\n", sched.NrInvoluntarySwitches)
	fmt.Printf("  CPUs allowed   : %s\n", status.CpusAllowedList)
	fmt.Printf("  NUMA nodes     : %s\n", status.MemsAllowedList)
	fmt.Println()
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "usage: %s <pid> [pid...]\n", os.Args[0])
		os.Exit(1)
	}
	for _, arg := range os.Args[1:] {
		pid, err := strconv.Atoi(arg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "invalid pid %q: %v\n", arg, err)
			continue
		}
		printPID(pid)
	}
}
```

### `go.mod`

```
module github.com/linux-to-k8s/proc-sched-reader

go 1.22
```

### `README.md`

```markdown
# proc-sched-reader

Parses `/proc/<pid>/sched` and `/proc/<pid>/status` to show CPU affinity,
NUMA affinity, and CFS scheduling statistics for any process.

## Build and Run

```
go build -o proc-sched-reader .
./proc-sched-reader 1 $$
```

## Sample Output

```
PID 1  (systemd)
  Policy         : SCHED_NORMAL
  Prio (dynamic) : 120  (100-139=CFS, 0-99=RT)
  vruntime       : 2847301.442 ms
  sum_exec       : 1234.567 ms
  voluntary sw   : 98321
  involuntary sw : 42
  CPUs allowed   : 0-7
  NUMA nodes     : 0
```

## Kernel Paths

| File | Symbol |
|------|--------|
| `kernel/sched/debug.c` | `proc_sched_show_task()` — writes `/proc/<pid>/sched` |
| `fs/proc/array.c` | `task_status()` — writes Cpus_allowed_list, Mems_allowed_list |

Sources:
https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/debug.c
https://elixir.bootlin.com/linux/v6.9/source/fs/proc/array.c

## Exercises

a) Extend the tool to accept `-all` and iterate over all PIDs in `/proc/`.

b) Add a `--watch` flag that re-prints every second and shows the delta
   in voluntary/involuntary switches.

c) Read `/proc/<pid>/numa_maps` and report how many anonymous pages are
   remote vs. local for NUMA-aware analysis.
```
```

- [ ] **Step 1: Write all three files** exactly as specified above

- [ ] **Step 2: Build and vet**

```bash
cd 08-scheduler/exercises/proc-sched-reader
go build ./...
go vet ./...
```
Expected: clean.

- [ ] **Step 3: Commit**

```bash
cd ../../..
git add 08-scheduler/exercises/proc-sched-reader/
git commit -m "feat(ch08): Go exercise proc-sched-reader — /proc/<pid>/sched + status parser"
```

---

## Task 7: kube-inspect checkpoint 08 — CPU affinity + NUMA placement per pod

**Files:**
- Create: `kube-inspect/internal/sched/sched.go`
- Modify: `kube-inspect/cmd/kube-inspect/main.go` (add `--sched` flag)
- Modify: `kube-inspect/CHECKPOINT.md` (mark checkpoint 08 done)

### `kube-inspect/internal/sched/sched.go`

New package `sched` that reads cpuset and CPU/NUMA affinity from the pod's cgroup v2 path and from each process's `/proc/<pid>/status`:

```go
package sched

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/linux-to-k8s/kube-inspect/internal/proc"
)

// PodSchedInfo holds CPU and NUMA affinity for a pod.
type PodSchedInfo struct {
	PodUID         string
	CgroupCPUs     string // cpuset.cpus from the pod's cgroup v2 path (empty if not found)
	CgroupMems     string // cpuset.mems from the pod's cgroup v2 path (empty if not found)
	CPUWeight      string // cpu.weight (empty if not found)
	CPUMax         string // cpu.max (empty if not found)
	ProcessAffinities []ProcessAffinity
}

// ProcessAffinity holds per-process affinity from /proc/<pid>/status.
type ProcessAffinity struct {
	PID             int
	Comm            string
	CpusAllowedList string
	MemsAllowedList string
}

// cgroupPath returns the cgroup v2 path for the given pod UID.
// It searches under /sys/fs/cgroup/kubepods.slice for a directory
// matching *pod<uid>*.
func cgroupPath(podUID string) (string, error) {
	base := "/sys/fs/cgroup/kubepods.slice"
	pattern := fmt.Sprintf("*pod%s*", podUID)
	matches, err := filepath.Glob(filepath.Join(base, "*", pattern))
	if err != nil {
		return "", err
	}
	// Also try direct path (no QoS subdirectory) for some kubelet configurations.
	direct, _ := filepath.Glob(filepath.Join(base, pattern))
	matches = append(matches, direct...)
	if len(matches) == 0 {
		return "", nil
	}
	return matches[0], nil
}

// readCgroupFile reads a single-line cgroup file, returning "" if missing.
func readCgroupFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// parseStatusAffinity reads Cpus_allowed_list and Mems_allowed_list from
// /proc/<pid>/status, along with the process name.
func parseStatusAffinity(pid int) ProcessAffinity {
	path := fmt.Sprintf("/proc/%d/status", pid)
	f, err := os.Open(path)
	if err != nil {
		return ProcessAffinity{PID: pid}
	}
	defer f.Close()

	pa := ProcessAffinity{PID: pid}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		parts := strings.SplitN(scanner.Text(), ":", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])
		switch key {
		case "Name":
			pa.Comm = val
		case "Cpus_allowed_list":
			pa.CpusAllowedList = val
		case "Mems_allowed_list":
			pa.MemsAllowedList = val
		}
	}
	return pa
}

// GetPodSchedInfo returns CPU/NUMA affinity for all processes in the pod
// plus the pod-level cgroup cpuset and cpu.weight / cpu.max.
func GetPodSchedInfo(podUID string) (PodSchedInfo, error) {
	info := PodSchedInfo{PodUID: podUID}

	cgPath, err := cgroupPath(podUID)
	if err != nil {
		return info, fmt.Errorf("cgroup lookup: %w", err)
	}
	if cgPath != "" {
		info.CgroupCPUs = readCgroupFile(filepath.Join(cgPath, "cpuset.cpus"))
		info.CgroupMems = readCgroupFile(filepath.Join(cgPath, "cpuset.mems"))
		info.CPUWeight = readCgroupFile(filepath.Join(cgPath, "cpu.weight"))
		info.CPUMax = readCgroupFile(filepath.Join(cgPath, "cpu.max"))
	}

	processes, err := proc.ListPodProcesses(podUID)
	if err != nil {
		return info, fmt.Errorf("listing processes: %w", err)
	}

	seen := make(map[string]bool)
	for _, p := range processes {
		pa := parseStatusAffinity(p.PID)
		key := pa.CpusAllowedList + "|" + pa.MemsAllowedList
		if seen[key] {
			continue
		}
		seen[key] = true
		info.ProcessAffinities = append(info.ProcessAffinities, pa)
	}

	return info, nil
}
```

### `--sched` flag in `main.go`

Add a `--sched` flag following the exact pattern of `--ebpf` and `--netns`:

```go
flagSched = flag.Bool("sched", false, "Show CPU/NUMA affinity and cgroup cpu.weight/cpu.max for the pod")
```

In the pod-mode block, after the `--ebpf` block:

```go
if *flagSched {
    info, err := sched.GetPodSchedInfo(*flagPod)
    if err != nil {
        fmt.Fprintf(os.Stderr, "sched: %v\n", err)
    } else {
        fmt.Printf("Scheduler info for pod %s:\n\n", info.PodUID)
        if info.CgroupCPUs != "" {
            fmt.Printf("  cgroup cpuset.cpus  : %s\n", info.CgroupCPUs)
            fmt.Printf("  cgroup cpuset.mems  : %s\n", info.CgroupMems)
            fmt.Printf("  cgroup cpu.weight   : %s\n", info.CPUWeight)
            fmt.Printf("  cgroup cpu.max      : %s\n", info.CPUMax)
        } else {
            fmt.Printf("  (cgroup path not found for pod %s)\n", info.PodUID)
        }
        if len(info.ProcessAffinities) > 0 {
            fmt.Printf("\n  Process affinities (deduplicated):\n")
            for _, pa := range info.ProcessAffinities {
                fmt.Printf("    PID=%-6d  comm=%-16s  cpus=%-10s  mems=%s\n",
                    pa.PID, pa.Comm, pa.CpusAllowedList, pa.MemsAllowedList)
            }
        }
        fmt.Println()
    }
}
```

Import: `"github.com/linux-to-k8s/kube-inspect/internal/sched"`

### `CHECKPOINT.md`

Change `| 08 | CPU affinity + NUMA placement | internal/cgroup | pending |` to `| 08 | CPU affinity + NUMA placement | internal/sched | done |`

- [ ] **Step 1: Read existing `kube-inspect/cmd/kube-inspect/main.go`** for import block and flag/output pattern

- [ ] **Step 2: Create `kube-inspect/internal/sched/sched.go`** exactly as specified above

- [ ] **Step 3: Modify `kube-inspect/cmd/kube-inspect/main.go`** — add `--sched` flag and output block

- [ ] **Step 4: Modify `kube-inspect/CHECKPOINT.md`** — mark checkpoint 08 done

- [ ] **Step 5: Build and vet**

```bash
cd kube-inspect
go build ./...
go vet ./...
```
Expected: clean.

- [ ] **Step 6: Commit**

```bash
cd ..
git add kube-inspect/internal/sched/sched.go kube-inspect/cmd/kube-inspect/main.go kube-inspect/CHECKPOINT.md
git commit -m "feat(kube-inspect): checkpoint 08 — CPU affinity + NUMA placement per pod"
```

---

## Self-Review

**Spec coverage:**
- ✅ `08-scheduler/README.md` — objectives, prerequisites, reading order, scheduler class diagram
- ✅ `kernel/08-a-task-sched.md` — `struct task_struct` sched fields, `sched_class` vtable, context switch, affinity path (7 sections)
- ✅ `kernel/08-b-cfs.md` — `struct sched_entity`, `struct cfs_rq`, vruntime formula, bandwidth throttle (8 sections)
- ✅ `kernel/08-c-rq.md` — `struct rq`, RT/DL schedulers, sched_domain, NUMA balancing (8 sections)
- ✅ `k8s/08-k8s-connection.md` — CPU requests/limits, cpuset, topology manager, QoS, failure patterns (9 sections)
- ✅ `exercises/sched-affinity-demo/` — C exercise, `sched_setaffinity`, nice-to-weight table, pthreads
- ✅ `exercises/proc-sched-reader/` — Go exercise, `/proc/<pid>/sched` + `/proc/<pid>/status`, stdlib only
- ✅ `kube-inspect/internal/sched/sched.go` — `GetPodSchedInfo`, cgroup cpuset, per-process affinity

**Placeholder scan:** All sections contain real content. No TBD/TODO.

**Type consistency:**
- `proc.ListPodProcesses(podUID)` returns `[]proc.Process` with `.PID int` — used correctly in Task 7
- `PodSchedInfo` fields are `string` for cgroup values (handles "max" literal in `cpu.max`) — consistent
- `kube-inspect` main.go `--sched` block uses `info.CgroupCPUs`, `info.CgroupMems`, `info.CPUWeight`, `info.CPUMax`, `info.ProcessAffinities[i].PID/.Comm/.CpusAllowedList/.MemsAllowedList` — all match struct fields exactly
