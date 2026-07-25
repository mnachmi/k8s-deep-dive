# oom-score-demo

Demonstrates `/proc/<pid>/oom_score_adj` — the per-process knob that biases the kernel OOM killer — and shows how the resulting `/proc/<pid>/oom_score` changes.

## What It Shows

| Part | Demonstrates |
|------|-------------|
| Part a | Current process OOM score and adj |
| Part b | kubelet's QoS adj values (-998, 500, 1000) |
| Part c | Three children with Guaranteed/Burstable/BestEffort adj; each child's resulting oom_score |
| Top-5 | Highest oom_score processes (OOM kill candidates) |

## Build and Run

```
make
sudo ./oom_score_demo   # root required to write oom_score_adj
```

## Kernel Path

```
write /proc/<pid>/oom_score_adj
  → fs/proc/base.c:proc_oom_score_adj_write()
    → task->signal->oom_score_adj = value

read /proc/<pid>/oom_score
  → fs/proc/base.c:proc_oom_score_read()
    → oom_badness(task, totalpages) — normalized to 0-2000
```

Source:
https://elixir.bootlin.com/linux/v6.9/source/fs/proc/base.c
https://elixir.bootlin.com/linux/v6.9/source/mm/oom_kill.c

## Exercises

a) Change the Burstable adj formula to match kubelet:
   `adj = min(max(2, 1000 - (1000 * rss_pages / total_pages)), 999)`.
   Use `/proc/<pid>/status` Vm fields to estimate rss_pages.

b) Add a Part d that allocates increasing amounts of memory in a loop and
   observes how oom_score rises as the process's RSS grows.

c) Read `/proc/sys/kernel/panic_on_oom` and `/proc/sys/vm/overcommit_memory`.
   Print their current values and explain what each setting means for
   OOM behavior.
