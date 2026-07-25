# Chapter 10 Final Review Fixes

Date: 2026-07-25

## Fix 1 — CPU starvation K8s Interface (10-k8s-connection.md, Section 1 table)

**File:** `10-performance/k8s/10-k8s-connection.md`

- **Before:** K8s Interface column for "CPU starvation" = `cpu.stat nr_throttled`
- **After:** `CPU manager static policy (cpuset isolation)`
- **Reason:** `cpu.stat nr_throttled` is the throttle signal (a diagnostic metric), not the remedy for CPU starvation. The correct K8s interface for eliminating noisy-neighbor CPU starvation is the CPU manager static policy, which provides cpuset isolation for Guaranteed QoS pods.

## Fix 2 — Involuntary context switches kernel field (10-k8s-connection.md, Section 1 table)

**File:** `10-performance/k8s/10-k8s-connection.md`

- **Before:** Kernel Signal column for "High involuntary context switches" = `voluntary_ctxt_switches`
- **After:** `nonvoluntary_ctxt_switches`
- **Reason:** The voluntary and involuntary fields in `/proc/<pid>/status` are distinct. High *involuntary* context switches signal preemption by competing tasks; the correct field is `nonvoluntary_ctxt_switches`.

## Fix 3 — struct sched_statistics source file (10-k8s-connection.md, Section 8 table)

**File:** `10-performance/k8s/10-k8s-connection.md`

- **Before:** `kernel/sched/stats.h` / `https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/stats.h`
- **After:** `include/linux/sched.h` / `https://elixir.bootlin.com/linux/v6.9/source/include/linux/sched.h`
- **Reason:** `struct sched_statistics` is defined in `include/linux/sched.h` (embedded in `struct sched_entity`), not in `kernel/sched/stats.h`. The stats.h file contains inline helper functions that operate on the struct, not the struct definition itself.

## Fix 4 — /proc/<pid>/schedstat kernel source (main.go and README.md)

**Files:** `10-performance/exercises/sched-latency-reader/main.go`, `10-performance/exercises/sched-latency-reader/README.md`

- **Before:** `/proc/<pid>/schedstat` attributed to `kernel/sched/stats.c`
- **After:** `fs/proc/base.c` — `proc_pid_schedstat()`
- **Reason:** `kernel/sched/stats.c` implements the system-wide `/proc/schedstat` (aggregate per-CPU stats). The per-process `/proc/<pid>/schedstat` is implemented by `proc_pid_schedstat()` in `fs/proc/base.c`. Three locations updated in main.go (package comment block + runtime printf), one in README.md Kernel Paths table, one in README.md Sources URL list.
