# Chapter 08 — CPU Scheduler

A latency-sensitive service deployed to Kubernetes starts experiencing 200ms p99 response times that correlate with nothing visible: no CPU saturation, no memory pressure, no network errors. Profiling the application shows normal execution time. The node has 40% idle CPU. The issue is invisible from every Kubernetes metric and every application profiling tool. The issue is CPU throttling.

The CFS bandwidth controller is enforcing a quota. The pod's `limits.cpu: 200m` maps to `cpu.max = 20000 100000` — the cgroup may consume 20ms of CPU time per 100ms scheduling period. The service handles requests at variable rates; a burst of requests exhausts the 20ms quota in the first few milliseconds of the period, and the remaining 80-95ms of the period are spent paused. During that pause, client requests are queuing, TCP connections are timing out, and the application is producing nothing. The node's idle CPU is irrelevant — the bandwidth controller is not a node-level resource; it is a per-cgroup absolute limit enforced by the CFS scheduler regardless of available capacity.

This is the class of problem that is impossible to diagnose without understanding the Linux scheduler. The symptoms live in `cpu.stat`. The root cause lives in the CFS bandwidth controller. The solution involves understanding the relationship between `requests.cpu`, `limits.cpu`, scheduling weight, and quota — distinctions that matter only once you understand what vruntime and the bandwidth controller actually do.

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
