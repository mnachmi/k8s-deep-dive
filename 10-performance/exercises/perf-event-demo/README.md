# perf-event-demo

Demonstrates `perf_event_open(2)` — the kernel syscall that powers `perf stat`,
`bpftrace`, and Flame Graphs — to directly count hardware and software events.

## What It Shows

| Part | Events | What you learn |
|------|--------|---------------|
| a | CPU cycles | Raw PMU counter access |
| b | Cycles + instructions group | IPC (instructions per cycle) |
| c | Cache references + misses | LLC miss rate |
| d | Task clock, page faults, ctxsw | Software event counting |

## Build and Run

```
make
./perf_event_demo
# If EPERM: sudo sysctl kernel.perf_event_paranoid=1
```

## Kernel Path

```
perf_event_open(2)
  → kernel/events/core.c:perf_event_open()
    → perf_event_alloc() — creates struct perf_event
    → pmu->event_init() — arch PMU driver programs the hardware counter
    → anon_inode_getfd() — returns fd backed by the perf_event

read(fd)
  → perf_read() → __perf_read()
    → pmu->read() — reads MSR (Model Specific Register) into event->count
    → copies count to userspace
```

Source:
https://elixir.bootlin.com/linux/v6.9/source/kernel/events/core.c
https://elixir.bootlin.com/linux/v6.9/source/include/uapi/linux/perf_event.h

## Exercises

a) Open `PERF_COUNT_HW_BRANCH_MISSES` alongside cycles and compute the
   branch misprediction rate. Hint: use a group with 3 members.

b) Modify `do_work()` to cause more cache misses by accessing a large array
   in random order (stride = prime number × cache line size). Observe the
   change in cache miss rate from Part c.

c) Add `read_format = PERF_FORMAT_TOTAL_TIME_ENABLED | PERF_FORMAT_TOTAL_TIME_RUNNING`
   to count with multiplexing scaling. Print the scale factor.
