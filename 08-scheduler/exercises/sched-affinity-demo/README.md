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
