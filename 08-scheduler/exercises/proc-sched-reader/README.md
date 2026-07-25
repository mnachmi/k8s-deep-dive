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
