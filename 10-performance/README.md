# Chapter 10 — Performance

Linux performance analysis starts in the kernel. The PMU (Performance Monitoring Unit) hardware counter, the tracepoint mechanism, and the scheduler's own latency accounting all produce the raw data that tools like `perf`, `bpftrace`, and Kubernetes dashboard metrics consume. This chapter traces each mechanism from its kernel data structures to the userspace interface, then shows how to apply them to diagnose container performance problems — CPU throttling, scheduler latency, cache misses, and memory bandwidth pressure.

## Learning Objectives

1. Understand `perf_event_open(2)` and `struct perf_event_attr` — how hardware PMU counters are programmed
2. Understand kernel tracepoints: `struct tracepoint`, `struct trace_event_call`, and the ring buffer
3. Understand scheduler latency accounting: `/proc/<pid>/schedstat`, `wait_sum`, CFS latency stats
4. Diagnose CPU throttling via `cpu.stat`: `nr_throttled`, `throttled_usec`, `nr_periods`
5. Profile a container process using `perf_event_open(2)` and read its scheduler wait time

## Prerequisites

- Chapter 01 — Process Model (task_struct, scheduling basics)
- Chapter 03 — cgroups (cgroup v2, cpu.max, cpu.stat)
- Chapter 08 — Scheduler (CFS, vruntime, struct cfs_rq)

## Reading Order

| File | Topic |
|------|-------|
| `kernel/10-a-perf-events.md` | perf_event_open, struct perf_event, hardware PMU, software counters |
| `kernel/10-b-tracing.md` | ftrace, tracepoints, struct trace_event_call, ring buffer |
| `kernel/10-c-schedstat.md` | Scheduler latency: /proc/schedstat, wait_sum, cpu.stat throttle metrics |
| `k8s/10-k8s-connection.md` | Container profiling, CPU throttle diagnosis, perf in k8s |
| `exercises/perf-event-demo/` | C: perf_event_open for cycles, instructions, cache misses |
| `exercises/sched-latency-reader/` | Go: /proc/<pid>/schedstat + cgroup cpu.stat throttle ratio |
| `kube-inspect` checkpoint 10 | Full perf report: throttle stats + sched latency per pod |

## Kernel to K8s Bridge

```
Hardware PMU
    │ PMU counter overflow → interrupt → perf_event handler
    ▼
struct perf_event (kernel/events/core.c)
    │ read(fd) → count value
    ▼
userspace: perf_event_open(2) fd

Kernel tracepoints
    │ TRACE_EVENT() → struct trace_event_call
    │ enabled: jump label → ring buffer write
    ▼
/sys/kernel/tracing/events/   bpftrace kprobe/tracepoint

Scheduler latency
    │ task waits on rq → schedstat.wait_sum
    ▼
/proc/<pid>/schedstat          cpu.stat throttled_usec
```
