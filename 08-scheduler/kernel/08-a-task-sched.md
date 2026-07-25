# 08-a — `task_struct` Scheduling Fields and `sched_class` Vtable

## Source Locations

| File | Key Symbols | URL |
|------|-------------|-----|
| `include/linux/sched.h` | `struct task_struct`, `struct sched_entity`, `struct sched_rt_entity` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/sched.h |
| `kernel/sched/sched.h` | `struct sched_class`, `struct cfs_rq`, `struct rt_rq`, `struct rq` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/sched.h |
| `kernel/sched/core.c` | `schedule()`, `__schedule()`, `context_switch()`, `sched_setaffinity()` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/core.c |
| `kernel/sched/fair.c` | `enqueue_entity()`, `pick_next_entity()`, `update_curr()` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/fair.c |
| `arch/x86/entry/entry_64.S` | `__switch_to_asm` | https://elixir.bootlin.com/linux/v6.9/source/arch/x86/entry/entry_64.S |

## Scheduling Fields in `struct task_struct`

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

### Field-by-Field Explanations

- `__state`: bitmask set by `set_current_state()`. Key values: `TASK_RUNNING` (0) — on a runqueue or currently executing; `TASK_INTERRUPTIBLE` (1) — sleeping, wakes on signal or event; `TASK_UNINTERRUPTIBLE` (2) — sleeping, ignores signals (disk I/O); `__TASK_STOPPED` (4) — stopped by SIGSTOP; `EXIT_ZOMBIE` (32) — exited, waiting for parent `wait4()`.

- `prio`: the effective priority used by the scheduler. For CFS tasks (nice -20 to 19), `prio` ranges 100–139. For RT tasks (SCHED_FIFO/SCHED_RR), `prio` ranges 0–99 (lower = higher priority on the RT scale, but the scheduler treats RT as numerically lower = higher urgency).

- `static_prio`: computed as `120 + nice`. Changed only by `setpriority(2)` / `nice(2)`.

- `normal_prio`: `static_prio` for non-RT tasks; `MAX_RT_PRIO - 1 - rt_priority` for RT tasks. Never changed after task creation except when the task switches scheduling policies.

- `sched_class`: pointer to one of five vtable singletons (address comparison determines precedence): `stop_sched_class` → `dl_sched_class` → `rt_sched_class` → `fair_sched_class` → `idle_sched_class`.

- `se`, `rt`, `dl`: embedded structs, one per scheduling class. Only the class that owns the task has an active entity on a runqueue at any time.

- `cpus_mask`: a `cpumask_t` (bitmap over `NR_CPUS` bits). Modified only by `__set_cpus_allowed_ptr()` in `kernel/sched/core.c`. The kernel never migrates a task to a CPU outside this mask.

## `struct sched_class` Vtable

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

## Context Switch Path

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

## CPU Affinity Syscall Path

`sched_setaffinity(pid, cpusetsize, user_mask_ptr)` → `kernel/sched/core.c`:
1. Copies the user cpumask into kernel space.
2. Intersects with `cpu_active_mask` and the task's cgroup cpuset constraint.
3. Calls `__set_cpus_allowed_ptr(p, new_mask, SCA_USER)`.
4. If the current CPU is no longer in `new_mask`, `stop_one_cpu()` migrates the task to a CPU in the new mask.

## Live Observation

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

## Key Kernel References

| Symbol | File | URL |
|--------|------|-----|
| `struct task_struct` | `include/linux/sched.h` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/sched.h |
| `struct sched_class` | `kernel/sched/sched.h` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/sched.h |
| `__schedule()` | `kernel/sched/core.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/core.c |
| `context_switch()` | `kernel/sched/core.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/core.c |
| `__switch_to_asm` | `arch/x86/entry/entry_64.S` | https://elixir.bootlin.com/linux/v6.9/source/arch/x86/entry/entry_64.S |
| `sched_setaffinity()` | `kernel/sched/core.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/core.c |
| `fair_sched_class` | `kernel/sched/fair.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/fair.c |
