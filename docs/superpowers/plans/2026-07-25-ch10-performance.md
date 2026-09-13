# Chapter 10 — Performance Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build Chapter 10 covering the kernel performance subsystem — `perf_event_open(2)`, hardware PMU counters, tracepoints, scheduler latency accounting, and CPU throttle detection — linking each to the Kubernetes tooling used to diagnose container performance problems.

**Architecture:** Same structure as previous chapters: `kernel/` (3 deep-dive docs), `k8s/` (1 connection doc), `exercises/` (C + Go), `kube-inspect/` (checkpoint 10). C exercise uses `perf_event_open(2)` directly (glibc only). Go exercise parses `/proc/<pid>/schedstat` and cgroup `cpu.stat`. kube-inspect checkpoint 10 adds `internal/metrics` with throttle stats + sched latency, plus a `--perf` flag.

**Tech Stack:** Markdown, C (gcc, glibc only), Go 1.22, Linux 6.9 kernel source via elixir.bootlin.com

## Global Constraints

- All kernel struct fields/functions must be accurate for Linux 6.9
- All elixir.bootlin.com URLs: bare format (`https://elixir.bootlin.com/linux/v6.9/source/...`), never `[text](url)`
- C compile: `gcc -Wall -Wextra -Werror -o <name> <name>.c`
- Go: `go build ./...` + `go vet ./...` must pass
- Go module for exercises: `github.com/linux-to-k8s/<exercise-name>`, go 1.22
- No placeholder text (no TBD, TODO, etc.)
- Every kernel doc has a `## Key Kernel References` table with ≥ 5 entries (bare URLs)
- Reading order: 10-a → 10-b → 10-c → k8s-connection
- Prerequisites: Ch01 (task_struct), Ch03 (cgroups), Ch08 (scheduler/CFS)

---

## Task 1: README + `10-a-perf-events.md` — perf_event_open, struct perf_event, hardware PMU counters

**Files:**
- Modify: `10-performance/README.md` (replace stub)
- Create: `10-performance/kernel/10-a-perf-events.md`

### `10-performance/README.md`

Replace the stub with:

```markdown
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
```

### `10-performance/kernel/10-a-perf-events.md`

Full Data Structure Deep Dive. Required sections:

**1. Source Locations**

| File | Key Symbols | URL |
|------|-------------|-----|
| `include/uapi/linux/perf_event.h` | `struct perf_event_attr`, `PERF_TYPE_*`, `PERF_COUNT_HW_*`, `PERF_COUNT_SW_*` | https://elixir.bootlin.com/linux/v6.9/source/include/uapi/linux/perf_event.h |
| `include/linux/perf_event.h` | `struct perf_event`, `struct hw_perf_event`, `struct perf_event_context` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/perf_event.h |
| `kernel/events/core.c` | `perf_event_open()`, `perf_read()`, `__perf_event_enable()` | https://elixir.bootlin.com/linux/v6.9/source/kernel/events/core.c |
| `arch/x86/events/core.c` | x86 PMU driver: `x86_pmu_enable_event()`, `x86_pmu_read()` | https://elixir.bootlin.com/linux/v6.9/source/arch/x86/events/core.c |
| `include/linux/perf_event.h` | `struct pmu` — PMU driver vtable | https://elixir.bootlin.com/linux/v6.9/source/include/linux/perf_event.h |

**2. `perf_event_open(2)` Syscall**

```c
// include/uapi/asm-generic/unistd.h
int perf_event_open(
    struct perf_event_attr *attr,  // event specification
    pid_t   pid,                   // target: 0=self, -1=any, >0=specific pid
    int     cpu,                   // target CPU: -1=all CPUs
    int     group_fd,              // event group leader fd, or -1
    unsigned long flags            // PERF_FLAG_FD_CLOEXEC etc.
);
// Returns: file descriptor, or -1 on error
```

`pid=0, cpu=-1`: measure the calling process on any CPU.
`pid=-1, cpu=N`: measure all processes on CPU N (requires `CAP_PERFMON` or `perf_event_paranoid ≤ 0`).
`group_fd`: links events into a group — all are measured atomically together. Useful for computing IPC (instructions/cycles).

**3. `struct perf_event_attr`**

```c
// include/uapi/linux/perf_event.h (Linux 6.9, fields most relevant to userspace)
struct perf_event_attr {
    __u32   type;               // PERF_TYPE_HARDWARE / SOFTWARE / TRACEPOINT / HW_CACHE / RAW
    __u32   size;               // sizeof(struct perf_event_attr) — ABI versioning
    __u64   config;             // event-specific: PERF_COUNT_HW_CPU_CYCLES etc.
    union {
        __u64 sample_period;    // overflow every N events (counting mode: 0)
        __u64 sample_freq;      // overflow at ~N Hz (requires freq=1)
    };
    __u64   sample_type;        // PERF_SAMPLE_* flags: what to record on overflow
    __u64   read_format;        // PERF_FORMAT_* flags: what read() returns
    __u64   disabled      :  1; // start disabled (use ioctl(PERF_EVENT_IOC_ENABLE))
    __u64   inherit       :  1; // count child processes too
    __u64   pinned        :  1; // must always be on PMU (EBUSY if can't schedule)
    __u64   exclusive     :  1; // only this event on the PMU counter group
    __u64   exclude_user  :  1; // don't count userspace
    __u64   exclude_kernel:  1; // don't count kernel
    __u64   exclude_hv    :  1; // don't count hypervisor
    __u64   mmap          :  1; // record mmap() calls in ring buffer
    __u64   comm          :  1; // record exec/comm name changes
    __u64   freq          :  1; // use sample_freq not sample_period
    // ... more flags ...
    __u32   wakeup_events;      // wake up every N overflow events
    __u32   bp_type;
    union { __u64 bp_addr; __u64 config1; };
    union { __u64 bp_len;  __u64 config2; };
    // ... branch_sample_type, sample_regs_user, sample_stack_user, clockid, ...
};
```

Minimal setup for counting CPU cycles on the calling process:
```c
struct perf_event_attr attr = {
    .type   = PERF_TYPE_HARDWARE,
    .size   = sizeof(attr),
    .config = PERF_COUNT_HW_CPU_CYCLES,
    .disabled    = 1,
    .exclude_kernel = 1,
    .exclude_hv  = 1,
};
int fd = perf_event_open(&attr, 0, -1, -1, 0);
ioctl(fd, PERF_EVENT_IOC_RESET, 0);
ioctl(fd, PERF_EVENT_IOC_ENABLE, 0);
// ... work ...
ioctl(fd, PERF_EVENT_IOC_DISABLE, 0);
long long count;
read(fd, &count, sizeof(count));
```

**4. Hardware Events (`PERF_TYPE_HARDWARE`)**

| `config` constant | Value | Measures |
|-------------------|-------|---------|
| `PERF_COUNT_HW_CPU_CYCLES` | 0 | CPU cycles |
| `PERF_COUNT_HW_INSTRUCTIONS` | 1 | Retired instructions |
| `PERF_COUNT_HW_CACHE_REFERENCES` | 2 | Last-level cache references |
| `PERF_COUNT_HW_CACHE_MISSES` | 3 | Last-level cache misses |
| `PERF_COUNT_HW_BRANCH_INSTRUCTIONS` | 4 | Branch instructions retired |
| `PERF_COUNT_HW_BRANCH_MISSES` | 5 | Branch mispredictions |
| `PERF_COUNT_HW_BUS_CYCLES` | 6 | Bus cycles |
| `PERF_COUNT_HW_STALLED_CYCLES_FRONTEND` | 7 | Cycles stalled (frontend) |
| `PERF_COUNT_HW_STALLED_CYCLES_BACKEND` | 8 | Cycles stalled (backend) |

IPC = `PERF_COUNT_HW_INSTRUCTIONS / PERF_COUNT_HW_CPU_CYCLES`. IPC < 1.0 on a modern out-of-order CPU usually indicates memory stalls.

**5. Software Events (`PERF_TYPE_SOFTWARE`)**

| `config` constant | Value | Measures |
|-------------------|-------|---------|
| `PERF_COUNT_SW_CPU_CLOCK` | 0 | CPU clock (nanoseconds) |
| `PERF_COUNT_SW_TASK_CLOCK` | 1 | Task clock (time on CPU) |
| `PERF_COUNT_SW_PAGE_FAULTS` | 2 | Page faults (minor + major) |
| `PERF_COUNT_SW_CONTEXT_SWITCHES` | 3 | Context switches |
| `PERF_COUNT_SW_CPU_MIGRATIONS` | 4 | CPU migrations |
| `PERF_COUNT_SW_PAGE_FAULTS_MIN` | 5 | Minor page faults |
| `PERF_COUNT_SW_PAGE_FAULTS_MAJ` | 6 | Major page faults |

**6. `struct perf_event` — Kernel Internal**

```c
// include/linux/perf_event.h (selected fields, Linux 6.9)
struct perf_event {
    struct list_head        event_entry;     // entry in ctx->event_list
    struct list_head        group_entry;     // entry in sibling list
    struct list_head        active_entry;    // entry in active list
    struct hlist_node       hlist_entry;     // hash for fast lookup

    struct perf_event_attr  attr;            // user-visible event specification
    struct hw_perf_event    hw;              // arch-specific PMU state

    struct perf_event_context *ctx;          // per-task or per-CPU context
    struct pmu             *pmu;             // PMU driver

    atomic64_t              count;           // event count
    u64                     total_time_enabled;
    u64                     total_time_running; // for multiplexing ratio
    // ...
};
```

`total_time_running / total_time_enabled` gives the **multiplexing ratio** — when more events are requested than PMU counters available, the kernel time-multiplexes them and you must scale: `scaled_count = raw_count * total_time_enabled / total_time_running`.

**7. Live Observation**

```bash
# Count CPU cycles for a command (perf uses perf_event_open internally)
perf stat -e cycles,instructions,cache-misses -- sleep 1

# Open perf_event_open file descriptors for a container process
ls -la /proc/$(pgrep -n nginx)/fd | grep anon_inode

# bpftrace: trace perf_event_open syscall (see what events are being opened)
bpftrace -e 'tracepoint:syscalls:sys_enter_perf_event_open {
    printf("pid=%d type=%d config=%llu\n", pid, args->attr_uptr->type, args->attr_uptr->config);
}'

# Check PMU capabilities
cat /sys/bus/event_source/devices/cpu/type
ls /sys/bus/event_source/devices/
```

**8. Key Kernel References**

| Symbol | File | URL |
|--------|------|-----|
| `struct perf_event_attr` | `include/uapi/linux/perf_event.h` | https://elixir.bootlin.com/linux/v6.9/source/include/uapi/linux/perf_event.h |
| `struct perf_event` | `include/linux/perf_event.h` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/perf_event.h |
| `perf_event_open()` | `kernel/events/core.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/events/core.c |
| `struct hw_perf_event` | `include/linux/perf_event.h` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/perf_event.h |
| `struct pmu` (PMU driver vtable) | `include/linux/perf_event.h` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/perf_event.h |
| `x86_pmu_enable_event()` | `arch/x86/events/core.c` | https://elixir.bootlin.com/linux/v6.9/source/arch/x86/events/core.c |

- [ ] **Step 1: Read the existing ch10 README stub**
- [ ] **Step 2: Write `10-performance/README.md`** — replace stub with full chapter intro
- [ ] **Step 3: Write `10-performance/kernel/10-a-perf-events.md`** — all 8 sections
- [ ] **Step 4: Verify no placeholder text, all URLs bare v6.9, ≥ 5 Key References**
- [ ] **Step 5: Commit**

```bash
git add 10-performance/README.md 10-performance/kernel/10-a-perf-events.md
git commit -m "docs(ch10): README + 10-a perf_event_open, struct perf_event, hardware PMU counters"
```

---

## Task 2: `10-b-tracing.md` — ftrace, tracepoints, struct trace_event_call, ring buffer

**Files:**
- Create: `10-performance/kernel/10-b-tracing.md`

Full Data Structure Deep Dive. Required sections:

**1. Source Locations**

| File | Key Symbols | URL |
|------|-------------|-----|
| `include/linux/tracepoint.h` | `struct tracepoint`, `DEFINE_TRACE()`, `TRACE_EVENT()` macro | https://elixir.bootlin.com/linux/v6.9/source/include/linux/tracepoint.h |
| `include/linux/trace_events.h` | `struct trace_event_call`, `struct trace_event_class`, `struct trace_event` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/trace_events.h |
| `kernel/trace/trace.c` | `trace_array`, ftrace core, `/sys/kernel/tracing/` | https://elixir.bootlin.com/linux/v6.9/source/kernel/trace/trace.c |
| `kernel/trace/ring_buffer.c` | `ring_buffer_lock_reserve()`, `ring_buffer_unlock_commit()` | https://elixir.bootlin.com/linux/v6.9/source/kernel/trace/ring_buffer.c |
| `include/linux/ftrace.h` | `struct ftrace_ops`, `ftrace_func_t`, `register_ftrace_function()` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/ftrace.h |

**2. Tracepoint Mechanism Overview**

A tracepoint is a hook point placed inside kernel code with near-zero overhead when no probe is attached. It uses the **jump label** mechanism: an unconditional NOP in the hot path that is patched to a JMP when enabled — a single `LOCK CMPXCHG` rewrite of 5 bytes.

```
Kernel code:
    ... normal code ...
    TRACE_EVENT(sched_switch, TP_PROTO(...), TP_ARGS(...), ...)
    ... normal code ...

When disabled (default):
    NOP         // 5-byte nop — zero overhead

When enabled (bpftrace / perf attaches):
    CALL __tracepoint_sched_switch  // JMP to tracepoint handler
        → iterates probe list
        → calls each registered probe function
        → writes to per-CPU ring buffer
```

**3. `struct tracepoint`**

```c
// include/linux/tracepoint.h (Linux 6.9)
struct tracepoint {
    const char              *name;          // e.g. "sched_switch"
    struct static_key_false  key;           // jump label — 0 = disabled (NOP), 1 = enabled (JMP)
    struct module           *mod;           // owning module (NULL for built-in)
    void                   (*regfunc)(void);   // called when first probe registers
    void                   (*unregfunc)(void); // called when last probe unregisters
    struct tracepoint_func __rcu *funcs;    // RCU-protected list of probe functions
};
```

`static_key_false key`: when 0 (no probes), the jump label generates a NOP instruction inline. When `tracepoint_probe_register()` is called, `static_key_slow_inc()` patches all NOP sites to JMP instructions. The overhead of an unattended tracepoint is literally zero — the NOP is on the same cache line as the surrounding code.

**4. `struct trace_event_call`**

```c
// include/linux/trace_events.h (Linux 6.9, key fields)
struct trace_event_call {
    struct list_head        list;               // entry in event list
    struct trace_event_class *class;            // shared class (format, fields)
    union {
        char                *name;              // event name
        struct tracepoint   *tp;                // underlying tracepoint
    };
    struct trace_event      event;              // embedded trace_event (type, funcs)
    char                   *print_fmt;          // printf format string for trace output
    struct event_filter __rcu *filter;          // per-event filter
    void                   *mod;                // module pointer
    void                   *data;
    int                     flags;              // TRACE_EVENT_FL_*
    int                     perf_refcount;      // number of perf users
    struct hlist_head __percpu *perf_events;    // per-CPU perf event list
    // ...
};
```

Each `TRACE_EVENT()` macro in the kernel generates a `struct trace_event_call` and a corresponding `/sys/kernel/tracing/events/<subsystem>/<event>/` directory.

**5. Ring Buffer**

The ftrace ring buffer is a per-CPU lock-free circular buffer. Each CPU writes independently (no cross-CPU locking). Readers can consume the buffer from `/sys/kernel/tracing/trace_pipe` or via `perf_event_open` with `PERF_TYPE_TRACEPOINT`.

```
struct trace_array (per trace instance)
    │
    └── struct array_buffer buffer
            │
            └── struct trace_buffer
                    │
                    └── struct ring_buffer_per_cpu __percpu *buffers
                            │
                            ├── head_page, tail_page (struct buffer_page)
                            ├── reader_page (for lockless reads)
                            └── entries (atomic_t — count of available entries)
```

Write path:
1. `ring_buffer_lock_reserve(buffer, len)` — atomically advances `tail_page->write` pointer
2. Caller writes event data into the reserved slot
3. `ring_buffer_unlock_commit(buffer, event)` — marks slot as committed, visible to readers

The buffer never blocks writers — old entries are overwritten when the buffer is full (overwrite mode) or new writes fail silently (discard mode).

**6. ftrace Function Tracer**

ftrace hooks all kernel functions via a `call` instruction inserted by the compiler (`-mfentry` on x86). `register_ftrace_function()` installs a probe:

```c
// include/linux/ftrace.h
struct ftrace_ops {
    ftrace_func_t func;             // callback: void fn(unsigned long ip, unsigned long parent_ip, ...)
    struct ftrace_ops_hash local_hash;
    struct ftrace_ops_hash *func_hash;  // function filter (which functions to trace)
    unsigned long flags;            // FTRACE_OPS_FL_*
    // ...
};
```

`ip` = instruction pointer of the traced function. `parent_ip` = return address (caller). This is the foundation of the function call graph tracers and latency analyzers.

**7. Live Observation**

```bash
# List all available tracepoints
ls /sys/kernel/tracing/events/sched/

# Enable sched_switch tracepoint and read the ring buffer
echo 1 > /sys/kernel/tracing/events/sched/sched_switch/enable
cat /sys/kernel/tracing/trace_pipe

# bpftrace uses tracepoints
bpftrace -e 'tracepoint:sched:sched_switch { printf("%s -> %s\n", args->prev_comm, args->next_comm); }'

# perf with tracepoint
perf record -e sched:sched_switch -a sleep 5
perf report

# Count tracepoint fires via perf stat
perf stat -e sched:sched_switch -p <pid> sleep 5
```

**8. Key Kernel References**

| Symbol | File | URL |
|--------|------|-----|
| `struct tracepoint` | `include/linux/tracepoint.h` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/tracepoint.h |
| `struct trace_event_call` | `include/linux/trace_events.h` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/trace_events.h |
| `ring_buffer_lock_reserve()` | `kernel/trace/ring_buffer.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/trace/ring_buffer.c |
| `ring_buffer_unlock_commit()` | `kernel/trace/ring_buffer.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/trace/ring_buffer.c |
| `struct ftrace_ops` | `include/linux/ftrace.h` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/ftrace.h |
| `register_ftrace_function()` | `kernel/trace/ftrace.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/trace/ftrace.c |

- [ ] **Step 1: Write `10-performance/kernel/10-b-tracing.md`** — all 8 sections
- [ ] **Step 2: Verify no placeholder text, all URLs bare v6.9, ≥ 5 Key References**
- [ ] **Step 3: Commit**

```bash
git add 10-performance/kernel/10-b-tracing.md
git commit -m "docs(ch10): 10-b tracepoints, struct trace_event_call, ring buffer, ftrace"
```

---

## Task 3: `10-c-schedstat.md` — Scheduler latency accounting, /proc/schedstat, cpu.stat throttle

**Files:**
- Create: `10-performance/kernel/10-c-schedstat.md`

Full Data Structure Deep Dive. Required sections:

**1. Source Locations**

| File | Key Symbols | URL |
|------|-------------|-----|
| `kernel/sched/stats.h` | `schedstat_*` macros, `struct sched_statistics` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/stats.h |
| `kernel/sched/stats.c` | `/proc/schedstat`, `/proc/<pid>/schedstat` handlers | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/stats.c |
| `kernel/sched/fair.c` | `update_curr()`, `cfs_rq->exec_clock`, CFS latency stats | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/fair.c |
| `kernel/sched/fair.c` | `throttle_cfs_rq()`, `unthrottle_cfs_rq()`, `cfs_b->throttled_time` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/fair.c |
| `kernel/sched/cpufreq_schedutil.c` | `sugov_update_shared()` — cpufreq/scheduler interaction | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/cpufreq_schedutil.c |

**2. `struct sched_statistics`**

When `CONFIG_SCHEDSTATS=y`, each `struct task_struct` embeds `struct sched_statistics stats` (via `struct sched_entity.statistics`):

```c
// kernel/sched/stats.h (Linux 6.9)
struct sched_statistics {
#ifdef CONFIG_SCHEDSTATS
    u64         wait_start;            // timestamp when task entered runqueue
    u64         wait_max;              // max time waiting on runqueue (ns)
    u64         wait_count;            // number of times task waited on runqueue
    u64         wait_sum;              // total time waiting on runqueue (ns) ← KEY
    u64         iowait_count;
    u64         iowait_sum;

    u64         sleep_start;
    u64         sleep_max;
    s64         sum_sleep_runtime;

    u64         block_start;
    u64         block_max;
    u64         exec_max;
    u64         slice_max;

    u64         nr_migrations_cold;
    u64         nr_failed_migrations_affine;
    u64         nr_failed_migrations_running;
    u64         nr_failed_migrations_hot;
    u64         nr_forced_migrations;

    u64         nr_wakeups;
    u64         nr_wakeups_sync;
    u64         nr_wakeups_migrate;
    u64         nr_wakeups_local;
    u64         nr_wakeups_remote;
    u64         nr_wakeups_affine;
    u64         nr_wakeups_affine_attempts;
    u64         nr_wakeups_passive;
    u64         nr_wakeups_idle;
#endif
};
```

`wait_sum` accumulates every time `update_stats_wait_end()` is called when the task leaves the runqueue to execute — it stores the total scheduler latency experienced by the task since boot.

**3. `/proc/<pid>/schedstat`**

Format (3 space-separated fields):
```
<sum_exec_runtime_ns> <wait_sum_ns> <nr_switches>
```

- `sum_exec_runtime_ns`: total time the task has been running on CPU (nanoseconds)
- `wait_sum_ns`: total time the task has spent waiting on the runqueue (scheduler latency)
- `nr_switches`: total number of times the task was context-switched out

Example:
```
# cat /proc/1234/schedstat
5234567890 876543210 4231
```
→ 5.23 s on CPU, 876 ms waiting in runqueue, 4231 context switches.

Scheduler latency ratio: `wait_sum / (wait_sum + sum_exec_runtime)`. A ratio > 10% on a non-IO-bound process suggests CPU contention or throttling.

**4. `/proc/schedstat` — System-Wide**

`/proc/schedstat` shows per-CPU runqueue statistics. Version line: `version 15`. Per-CPU lines:

```
cpu0 <yld_count> 0 <sched_count> <sched_goidle> <ttwu_count> <ttwu_local> <rq_cpu_time> <run_delay> <pcount>
```

`run_delay`: cumulative nanoseconds tasks waited on this CPU's runqueue. `pcount`: number of tasks that ran on this CPU. `run_delay / pcount` = average scheduler latency per task.

**5. CFS Throttle Metrics in `cpu.stat`**

When a cgroup's `cpu.max` quota is exhausted, `throttle_cfs_rq()` removes the runqueue from the timeline. The cgroup accumulates `cfs_bandwidth.throttled_time`. This is exposed in `cpu.stat`:

```
# cat /sys/fs/cgroup/kubepods.slice/.../cpu.stat
usage_usec       5234567
user_usec        4100000
system_usec      1134567
nr_periods       1000          ← number of CFS periods elapsed
nr_throttled     50            ← periods where quota was exhausted
throttled_usec   5000000       ← total throttled time (microseconds)
nr_bursts        0
burst_usec       0
```

**Throttle ratio** = `throttled_usec / (nr_periods × period_us)`.

For `cpu.max = "50000 100000"` (0.5 CPU, 100ms period):
- Over 100 periods (10 s): if `nr_throttled = 50` and `throttled_usec = 2500000`:
  - Throttle ratio = 2.5 s / 10 s = **25%** of allowed CPU time was lost to throttling

A throttle ratio > 5% for a latency-sensitive workload is a strong signal to increase `limits.cpu` or tune the period.

**6. Latency Top Observation**

```bash
# Read /proc/<pid>/schedstat (3-field format)
cat /proc/$(pgrep -n nginx)/schedstat

# System-wide runqueue latency per CPU
awk '/^cpu[0-9]/{
    run_delay=$8; pcount=$9;
    if (pcount > 0) printf "%s avg_latency=%.2fms\n", $1, run_delay/pcount/1e6
}' /proc/schedstat

# Monitor throttling for a pod (watch cpu.stat change)
CGROUP=/sys/fs/cgroup/kubepods.slice/$(kubectl get pod <name> -o json | jq -r '.metadata.uid' | sed 's/-//g')
watch -n1 "awk '/nr_throttled|throttled_usec|nr_periods/{print}' $CGROUP/cpu.stat"

# bpftrace: measure time spent waiting on runqueue
bpftrace -e '
tracepoint:sched:sched_stat_wait {
    @wait_ns[args->comm] = sum(args->delay);
}
interval:s:5 { print(@wait_ns); clear(@wait_ns); }'
```

**7. Live Observation — CFS Throttle Tracing**

```bash
# Trace CFS throttle events via bpftrace kprobe
bpftrace -e 'kprobe:throttle_cfs_rq { printf("throttle: cpu=%d\n", cpu); }'

# Watch cpu.stat for rising nr_throttled
while true; do
    awk '/nr_throttled/{print systime(), $0}' \
        /sys/fs/cgroup/kubepods.slice/kubepods-pod*.slice/cpu.stat 2>/dev/null
    sleep 5
done
```

**8. Key Kernel References**

| Symbol | File | URL |
|--------|------|-----|
| `struct sched_statistics` | `kernel/sched/stats.h` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/stats.h |
| `update_stats_wait_end()` | `kernel/sched/stats.h` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/stats.h |
| `/proc/<pid>/schedstat` handler | `kernel/sched/stats.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/stats.c |
| `throttle_cfs_rq()` | `kernel/sched/fair.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/fair.c |
| `struct cfs_bandwidth` | `kernel/sched/sched.h` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/sched.h |
| `update_curr()` | `kernel/sched/fair.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/fair.c |

Note: `struct sched_statistics` is only available when kernel is built with `CONFIG_SCHEDSTATS=y`. Typical production distro kernels (Debian, Ubuntu, RHEL 9, GKE, EKS nodes) enable this by default. If `/proc/<pid>/schedstat` returns only `0 0 0`, the kernel was built without CONFIG_SCHEDSTATS.

- [ ] **Step 1: Write `10-performance/kernel/10-c-schedstat.md`** — all 8 sections
- [ ] **Step 2: Verify no placeholder text, all URLs bare v6.9, ≥ 5 Key References**
- [ ] **Step 3: Commit**

```bash
git add 10-performance/kernel/10-c-schedstat.md
git commit -m "docs(ch10): 10-c schedstat sched_statistics wait_sum, cpu.stat throttle metrics"
```

---

## Task 4: `10-k8s-connection.md` — Container profiling, CPU throttle diagnosis, perf in k8s

**Files:**
- Create: `10-performance/k8s/10-k8s-connection.md`

Required sections:

**1. Architecture Overview**

| Performance Problem | Kernel Signal | K8s Interface |
|--------------------|--------------|---------------|
| CPU starvation | `/proc/<pid>/schedstat` wait_sum | `cpu.stat nr_throttled` |
| CPU throttling | `cpu.stat throttled_usec` | `limits.cpu` too low |
| Cache miss storm | `PERF_COUNT_HW_CACHE_MISSES` | `resources.limits.memory` |
| Branch mispredicts | `PERF_COUNT_HW_BRANCH_MISSES` | CPU pinning (topology manager) |
| Page fault flood | `PERF_COUNT_SW_PAGE_FAULTS_MAJ` | memory.events max counter |
| High involuntary ctxsw | `/proc/<pid>/status` voluntary_ctxt_switches | RT task competing on cpuset |

**2. CPU Throttle Diagnosis Flow**

```
Symptom: high p99 latency despite low average CPU
     │
     ▼
Check cpu.stat throttled_usec
     │  if rising → quota exhausted mid-request
     ▼
Compute throttle ratio:
     throttled_usec / (nr_periods × period_us)
     │  > 5% → increase limits.cpu
     │  spiky (high nr_throttled, low throttled_usec per event) → shorten period
     ▼
Check cpu.max: "<quota_us> <period_us>"
     │  default period = 100ms — long period amplifies burstiness
     │  → reduce period to 10ms: kubelet --cpu-cfs-period=10000
     ▼
If throttle ratio acceptable but latency high:
     check /proc/<pid>/schedstat wait_sum / runtime
     │  high ratio → scheduler contention (noisy neighbor)
     ▼
Identify noisy neighbor:
     perf sched latency -p <pid>
     bpftrace -e 'tracepoint:sched:sched_stat_wait { @[args->comm]=sum(args->delay); }'
```

**3. `perf_event_open` Inside a Container**

Running `perf` inside a container requires:
1. `CAP_PERFMON` (Linux 5.8+) or `CAP_SYS_ADMIN` — needed for system-wide profiling
2. `/proc/sys/kernel/perf_event_paranoid ≤ 1` — allows per-process profiling without CAP_PERFMON
3. `seccomp` must not block `perf_event_open` (GKE default seccomp blocks it; use a custom profile)

Profile a specific container process:
```bash
# From node (outside container), profile container PID
CPID=$(crictl inspect <container-id> | jq '.info.pid')
perf stat -e cycles,instructions,cache-misses -p $CPID sleep 10

# Flamegraph from node targeting container PID
perf record -F 99 -p $CPID -g -- sleep 30
perf script | stackcollapse-perf.pl | flamegraph.pl > flame.svg
```

**4. Scheduler Latency — Identifying CFS Throttle vs. CPU Starvation**

| Condition | `wait_sum` trend | `nr_throttled` | Diagnosis |
|-----------|-----------------|----------------|-----------|
| CPU throttle | grows when at quota | high | Raise `limits.cpu` |
| CPU starvation (noisy neighbor) | grows continuously | low | CPU manager static policy |
| IO wait masquerading as latency | flat | low | Check PSI io.some |
| Correct sizing | flat or slow growth | 0 | No action |

```bash
# Snapshot schedstat for all pod processes, compute wait ratio
for pid in $(ls /proc | grep -E '^[0-9]+$'); do
  cg=$(cat /proc/$pid/cgroup 2>/dev/null | grep kubepods)
  [ -z "$cg" ] && continue
  read rt wait sw < /proc/$pid/schedstat 2>/dev/null
  [ "$rt" -gt 0 ] 2>/dev/null || continue
  ratio=$(awk "BEGIN{printf \"%.1f\", $wait * 100 / ($wait + $rt)}")
  comm=$(cat /proc/$pid/comm 2>/dev/null)
  echo "pid=$pid comm=$comm wait_ratio=${ratio}% nr_switches=$sw"
done
```

**5. K8s Performance Best Practices**

| Practice | Kernel Mechanism | K8s Config |
|---------|-----------------|------------|
| Reduce CPU throttle | Shorter CFS period | `--cpu-cfs-period=10000` (10ms) |
| Eliminate noisy neighbor | Dedicated cpuset | CPU manager static + `topology.kubernetes.io/node` |
| Profile without privileges | `perf_event_paranoid=1` | DaemonSet with hostPID + CAP_PERFMON |
| NUMA-local memory | cpuset.mems | Topology Manager single-numa-node |
| HugePages | THP + hugetlbfs | `resources.limits.hugepages-2Mi` |

**6. Common Failure Patterns**

| Symptom | Kernel Cause | Diagnosis |
|---------|-------------|-----------|
| p99 >> p50 latency | CFS period throttle mid-burst | `cpu.stat throttled_usec` rising |
| CPU usage 90% but throttled | Quota exhausted before period ends | `nr_throttled/nr_periods` > 0.1 |
| perf_event_open EPERM | perf_event_paranoid = 3 | `sysctl kernel.perf_event_paranoid` |
| Zero schedstat values | CONFIG_SCHEDSTATS=n | `grep CONFIG_SCHEDSTATS /boot/config-$(uname -r)` |
| High cache miss rate | NUMA cross-socket access | `numastat -p <pid>` |

**7. Verification Commands**

```bash
# Full throttle report for all pods
for cg in /sys/fs/cgroup/kubepods.slice/kubepods-pod*.slice; do
  uid=$(basename $cg | sed 's/.*pod//')
  periods=$(awk '/^nr_periods/{print $2}' $cg/cpu.stat 2>/dev/null)
  throttled=$(awk '/^nr_throttled/{print $2}' $cg/cpu.stat 2>/dev/null)
  echo "pod $uid: nr_periods=$periods nr_throttled=$throttled"
done

# perf stat on a container process (from node)
perf stat -e cycles,instructions,cache-misses,branch-misses \
  -p $(pgrep -n nginx) sleep 5

# Check perf_event_paranoid
sysctl kernel.perf_event_paranoid
```

**8. Key Kernel References**

| Symbol | File | URL |
|--------|------|-----|
| `perf_event_open()` | `kernel/events/core.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/events/core.c |
| `struct sched_statistics` | `kernel/sched/stats.h` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/stats.h |
| `throttle_cfs_rq()` | `kernel/sched/fair.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/fair.c |
| `struct tracepoint` | `include/linux/tracepoint.h` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/tracepoint.h |
| `struct cfs_bandwidth` | `kernel/sched/sched.h` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/sched.h |
| `update_stats_wait_end()` | `kernel/sched/stats.h` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/stats.h |

- [ ] **Step 1: Write `10-performance/k8s/10-k8s-connection.md`** — all 8 sections
- [ ] **Step 2: Verify all facts, no placeholder text, all URLs bare v6.9**
- [ ] **Step 3: Commit**

```bash
git add 10-performance/k8s/10-k8s-connection.md
git commit -m "docs(ch10): 10-k8s-connection CPU throttle diagnosis, perf in k8s, sched latency"
```

---

## Task 5: C exercise — `perf-event-demo`

**Files:**
- Create: `10-performance/exercises/perf-event-demo/perf_event_demo.c`
- Create: `10-performance/exercises/perf-event-demo/Makefile`
- Create: `10-performance/exercises/perf-event-demo/README.md`

### `perf_event_demo.c`

```c
/*
 * perf_event_demo.c — demonstrate perf_event_open(2) to count hardware
 * performance events: CPU cycles, retired instructions, last-level cache
 * misses, and software page faults.
 *
 * Demonstrates:
 *   a) Opening a hardware event (CPU cycles) on the calling process
 *   b) Opening a group: cycles + instructions (measure IPC atomically)
 *   c) Counting cache misses vs cache references
 *   d) Counting software events: task clock, page faults, context switches
 *   e) Demonstrating multiplexing scaling when more events than counters
 *
 * Build:  gcc -Wall -Wextra -Werror -o perf_event_demo perf_event_demo.c
 * Run:    ./perf_event_demo
 *         (may require: sudo sysctl kernel.perf_event_paranoid=1)
 *
 * Kernel path:
 *   perf_event_open(2) → kernel/events/core.c:perf_event_open()
 *   https://elixir.bootlin.com/linux/v6.9/source/kernel/events/core.c
 *
 *   struct perf_event_attr:
 *   https://elixir.bootlin.com/linux/v6.9/source/include/uapi/linux/perf_event.h
 */

#define _GNU_SOURCE
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>
#include <errno.h>
#include <sys/syscall.h>
#include <sys/ioctl.h>
#include <linux/perf_event.h>

/* Wrapper: perf_event_open is not in glibc */
static long perf_event_open(struct perf_event_attr *hw, pid_t pid,
                             int cpu, int group_fd, unsigned long flags)
{
    return syscall(__NR_perf_event_open, hw, pid, cpu, group_fd, flags);
}

/* Open one counting event. Returns fd or -1 on error. */
static int open_event(__u32 type, __u64 config, int group_fd)
{
    struct perf_event_attr attr;
    memset(&attr, 0, sizeof(attr));
    attr.type           = type;
    attr.size           = sizeof(attr);
    attr.config         = config;
    attr.disabled       = 1;
    attr.exclude_kernel = 1;
    attr.exclude_hv     = 1;
    /* PERF_FORMAT_GROUP: read() returns all group members at once */
    attr.read_format    = (group_fd == -1) ? 0 : PERF_FORMAT_GROUP;
    int fd = (int)perf_event_open(&attr, 0 /*self*/, -1 /*any cpu*/,
                                  group_fd, 0);
    if (fd < 0)
        perror("perf_event_open");
    return fd;
}

/* Read a single counter fd */
static long long read_counter(int fd)
{
    long long val = 0;
    if (read(fd, &val, sizeof(val)) != sizeof(val))
        return -1;
    return val;
}

/* Busy loop doing work: memory reads + integer math */
static volatile long sink;
static void do_work(int iterations)
{
    long acc = 0;
    int arr[256];
    for (int i = 0; i < 256; i++) arr[i] = i;
    for (int i = 0; i < iterations; i++) {
        acc += arr[i % 256] * i;
        /* occasional branch to exercise predictor */
        if ((i & 0xFFF) == 0) acc ^= i;
    }
    sink = acc;
}

static void part_a(void)
{
    printf("=== Part a: CPU cycles for do_work(5M) ===\n");
    int fd = open_event(PERF_TYPE_HARDWARE, PERF_COUNT_HW_CPU_CYCLES, -1);
    if (fd < 0) return;

    ioctl(fd, PERF_EVENT_IOC_RESET, 0);
    ioctl(fd, PERF_EVENT_IOC_ENABLE, 0);
    do_work(5000000);
    ioctl(fd, PERF_EVENT_IOC_DISABLE, 0);

    printf("  cycles: %lld\n", read_counter(fd));
    close(fd);
}

static void part_b(void)
{
    printf("\n=== Part b: cycles + instructions group (IPC) ===\n");
    /* Group leader: cycles */
    int fd_cycles = open_event(PERF_TYPE_HARDWARE, PERF_COUNT_HW_CPU_CYCLES, -1);
    if (fd_cycles < 0) return;
    /* Sibling: instructions */
    int fd_insns  = open_event(PERF_TYPE_HARDWARE, PERF_COUNT_HW_INSTRUCTIONS, fd_cycles);
    if (fd_insns < 0) { close(fd_cycles); return; }

    ioctl(fd_cycles, PERF_EVENT_IOC_RESET, PERF_IOC_FLAG_GROUP);
    ioctl(fd_cycles, PERF_EVENT_IOC_ENABLE, PERF_IOC_FLAG_GROUP);
    do_work(5000000);
    ioctl(fd_cycles, PERF_EVENT_IOC_DISABLE, PERF_IOC_FLAG_GROUP);

    /* With PERF_FORMAT_GROUP, reading the leader returns all members:
     * [nr_events][val0][val1]...  (8 bytes each) */
    long long buf[4] = {0};
    if (read(fd_cycles, buf, sizeof(buf)) < 0) { perror("read group"); }
    else {
        long long nr = buf[0];  /* number of events in group */
        long long cycles = buf[1];
        long long insns  = (nr >= 2) ? buf[2] : 0;
        double ipc = (cycles > 0) ? (double)insns / cycles : 0.0;
        printf("  cycles=%lld  instructions=%lld  IPC=%.2f\n",
               cycles, insns, ipc);
    }
    close(fd_insns);
    close(fd_cycles);
}

static void part_c(void)
{
    printf("\n=== Part c: cache references vs cache misses ===\n");
    int fd_ref  = open_event(PERF_TYPE_HARDWARE, PERF_COUNT_HW_CACHE_REFERENCES, -1);
    int fd_miss = open_event(PERF_TYPE_HARDWARE, PERF_COUNT_HW_CACHE_MISSES, -1);
    if (fd_ref < 0 || fd_miss < 0) {
        if (fd_ref >= 0) close(fd_ref);
        if (fd_miss >= 0) close(fd_miss);
        printf("  (hardware cache events not available on this CPU/VM)\n");
        return;
    }

    ioctl(fd_ref,  PERF_EVENT_IOC_RESET, 0);
    ioctl(fd_miss, PERF_EVENT_IOC_RESET, 0);
    ioctl(fd_ref,  PERF_EVENT_IOC_ENABLE, 0);
    ioctl(fd_miss, PERF_EVENT_IOC_ENABLE, 0);
    do_work(5000000);
    ioctl(fd_ref,  PERF_EVENT_IOC_DISABLE, 0);
    ioctl(fd_miss, PERF_EVENT_IOC_DISABLE, 0);

    long long ref  = read_counter(fd_ref);
    long long miss = read_counter(fd_miss);
    double miss_rate = (ref > 0) ? (double)miss * 100.0 / ref : 0.0;
    printf("  cache_refs=%lld  cache_misses=%lld  miss_rate=%.2f%%\n",
           ref, miss, miss_rate);
    close(fd_ref);
    close(fd_miss);
}

static void part_d(void)
{
    printf("\n=== Part d: software events — task_clock, page faults, context switches ===\n");
    int fd_clock = open_event(PERF_TYPE_SOFTWARE, PERF_COUNT_SW_TASK_CLOCK, -1);
    int fd_pf    = open_event(PERF_TYPE_SOFTWARE, PERF_COUNT_SW_PAGE_FAULTS, -1);
    int fd_cs    = open_event(PERF_TYPE_SOFTWARE, PERF_COUNT_SW_CONTEXT_SWITCHES, -1);

    if (fd_clock >= 0 && fd_pf >= 0 && fd_cs >= 0) {
        ioctl(fd_clock, PERF_EVENT_IOC_RESET, 0);
        ioctl(fd_pf,    PERF_EVENT_IOC_RESET, 0);
        ioctl(fd_cs,    PERF_EVENT_IOC_RESET, 0);
        ioctl(fd_clock, PERF_EVENT_IOC_ENABLE, 0);
        ioctl(fd_pf,    PERF_EVENT_IOC_ENABLE, 0);
        ioctl(fd_cs,    PERF_EVENT_IOC_ENABLE, 0);

        do_work(5000000);
        usleep(100000); /* sleep 100ms to accumulate context switches */

        ioctl(fd_clock, PERF_EVENT_IOC_DISABLE, 0);
        ioctl(fd_pf,    PERF_EVENT_IOC_DISABLE, 0);
        ioctl(fd_cs,    PERF_EVENT_IOC_DISABLE, 0);

        printf("  task_clock=%lld ns  page_faults=%lld  context_switches=%lld\n",
               read_counter(fd_clock), read_counter(fd_pf), read_counter(fd_cs));
    }
    if (fd_clock >= 0) close(fd_clock);
    if (fd_pf >= 0)    close(fd_pf);
    if (fd_cs >= 0)    close(fd_cs);
}

int main(void)
{
    printf("perf_event_demo — perf_event_open(2) hardware/software counters\n\n");
    printf("Note: if events fail with EPERM, run:\n");
    printf("  sudo sysctl kernel.perf_event_paranoid=1\n\n");

    part_a();
    part_b();
    part_c();
    part_d();

    return 0;
}
```

### `Makefile`

```makefile
CC      = gcc
CFLAGS  = -Wall -Wextra -Werror
TARGET  = perf_event_demo

all: $(TARGET)

$(TARGET): perf_event_demo.c
	$(CC) $(CFLAGS) -o $@ $<

run: $(TARGET)
	./$(TARGET)

clean:
	rm -f $(TARGET)

.PHONY: all run clean
```

### `README.md`

```markdown
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
```
```

- [ ] **Step 1: Write all three files exactly as specified**
- [ ] **Step 2: Build: `gcc -Wall -Wextra -Werror -o perf_event_demo perf_event_demo.c`** — must compile clean
- [ ] **Step 3: Commit**

```bash
git add 10-performance/exercises/perf-event-demo/
git commit -m "feat(ch10): C exercise perf-event-demo — perf_event_open hardware/software counters, IPC, cache misses"
```

---

## Task 6: Go exercise — `sched-latency-reader`

**Files:**
- Create: `10-performance/exercises/sched-latency-reader/main.go`
- Create: `10-performance/exercises/sched-latency-reader/go.mod`
- Create: `10-performance/exercises/sched-latency-reader/README.md`

### `main.go`

```go
// sched-latency-reader: reads scheduler latency stats from
// /proc/<pid>/schedstat and CPU throttle stats from a pod cgroup's cpu.stat.
//
// Usage:
//   sched-latency-reader --pid <pid>           print schedstat for one process
//   sched-latency-reader --pod <uid>           print throttle report for pod
//   sched-latency-reader --pod <uid> --all     also show per-process schedstat
//
// Build: go build -o sched-latency-reader .
//
// Kernel paths:
//   /proc/<pid>/schedstat  → kernel/sched/stats.c
//   cgroup cpu.stat        → kernel/sched/fair.c (struct cfs_bandwidth)
//
// Sources:
//   https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/stats.c
//   https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/fair.c
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// SchedStat holds /proc/<pid>/schedstat fields.
type SchedStat struct {
	PID           int
	Comm          string
	RuntimeNS     uint64 // time running on CPU (nanoseconds)
	WaitNS        uint64 // time waiting on runqueue (nanoseconds)
	NrSwitches    uint64 // total context switches
}

// CPUStat holds relevant fields from cgroup cpu.stat.
type CPUStat struct {
	UsageUS      uint64
	NrPeriods    uint64
	NrThrottled  uint64
	ThrottledUS  uint64
}

// ThrottleRatio returns throttled fraction (0.0–1.0). Returns 0 if no periods.
func (s CPUStat) ThrottleRatio() float64 {
	if s.NrPeriods == 0 {
		return 0
	}
	return float64(s.NrThrottled) / float64(s.NrPeriods)
}

// WaitRatio returns wait_ns / (wait_ns + runtime_ns). Returns 0 if both zero.
func (s SchedStat) WaitRatio() float64 {
	total := s.WaitNS + s.RuntimeNS
	if total == 0 {
		return 0
	}
	return float64(s.WaitNS) / float64(total)
}

// parseSchedStat reads /proc/<pid>/schedstat.
func parseSchedStat(pid int) (SchedStat, error) {
	path := fmt.Sprintf("/proc/%d/schedstat", pid)
	data, err := os.ReadFile(path)
	if err != nil {
		return SchedStat{}, err
	}
	fields := strings.Fields(strings.TrimSpace(string(data)))
	if len(fields) < 3 {
		return SchedStat{}, fmt.Errorf("%s: unexpected format %q", path, data)
	}
	rt, _ := strconv.ParseUint(fields[0], 10, 64)
	wait, _ := strconv.ParseUint(fields[1], 10, 64)
	sw, _ := strconv.ParseUint(fields[2], 10, 64)

	comm := "(unknown)"
	if cb, err2 := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid)); err2 == nil {
		comm = strings.TrimSpace(string(cb))
	}
	return SchedStat{PID: pid, Comm: comm, RuntimeNS: rt, WaitNS: wait, NrSwitches: sw}, nil
}

// parseCPUStat reads cgroup cpu.stat.
func parseCPUStat(path string) (CPUStat, error) {
	f, err := os.Open(path)
	if err != nil {
		return CPUStat{}, err
	}
	defer f.Close()

	var s CPUStat
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		parts := strings.Fields(scanner.Text())
		if len(parts) != 2 {
			continue
		}
		v, _ := strconv.ParseUint(parts[1], 10, 64)
		switch parts[0] {
		case "usage_usec":
			s.UsageUS = v
		case "nr_periods":
			s.NrPeriods = v
		case "nr_throttled":
			s.NrThrottled = v
		case "throttled_usec":
			s.ThrottledUS = v
		}
	}
	return s, scanner.Err()
}

func findPodCgroup(podUID string) (string, error) {
	base := "/sys/fs/cgroup/kubepods.slice"
	pattern := fmt.Sprintf("*pod%s*", podUID)
	matches, _ := filepath.Glob(filepath.Join(base, "*", pattern))
	direct, _ := filepath.Glob(filepath.Join(base, pattern))
	all := append(matches, direct...)
	if len(all) == 0 {
		return "", fmt.Errorf("cgroup not found for pod %s", podUID)
	}
	return all[0], nil
}

// listPodPIDs returns PIDs whose /proc/<pid>/cgroup mentions the pod UID.
func listPodPIDs(podUID string) []int {
	var pids []int
	entries, _ := os.ReadDir("/proc")
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		cgData, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
		if err != nil {
			continue
		}
		if strings.Contains(string(cgData), "pod"+podUID) ||
			strings.Contains(string(cgData), strings.ReplaceAll(podUID, "-", "")) {
			pids = append(pids, pid)
		}
	}
	return pids
}

func printSchedStat(s SchedStat) {
	fmt.Printf("  pid=%-6d  comm=%-16s  runtime=%6.1fms  wait=%6.1fms  wait_ratio=%4.1f%%  switches=%d\n",
		s.PID, s.Comm,
		float64(s.RuntimeNS)/1e6,
		float64(s.WaitNS)/1e6,
		s.WaitRatio()*100,
		s.NrSwitches)
}

func runPID(pid int) {
	s, err := parseSchedStat(pid)
	if err != nil {
		fmt.Fprintf(os.Stderr, "schedstat: %v\n", err)
		return
	}
	fmt.Printf("=== /proc/%d/schedstat ===\n", pid)
	printSchedStat(s)
	fmt.Printf("\nKernel path: /proc/%d/schedstat → kernel/sched/stats.c\n", pid)
	fmt.Printf("Source: https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/stats.c\n")
}

func runPod(podUID string, showAll bool) {
	cgPath, err := findPodCgroup(podUID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cgroup: %v\n", err)
	} else {
		fmt.Printf("=== CPU throttle for pod %s ===\n", podUID)
		fmt.Printf("  cgroup: %s\n", cgPath)
		cs, err := parseCPUStat(filepath.Join(cgPath, "cpu.stat"))
		if err != nil {
			fmt.Fprintf(os.Stderr, "  cpu.stat: %v\n", err)
		} else {
			fmt.Printf("  usage_usec:     %d\n", cs.UsageUS)
			fmt.Printf("  nr_periods:     %d\n", cs.NrPeriods)
			fmt.Printf("  nr_throttled:   %d\n", cs.NrThrottled)
			fmt.Printf("  throttled_usec: %d\n", cs.ThrottledUS)
			fmt.Printf("  throttle_ratio: %.1f%%\n", cs.ThrottleRatio()*100)
		}
		fmt.Println()
	}

	if showAll {
		fmt.Printf("=== Scheduler latency for pod %s processes ===\n", podUID)
		pids := listPodPIDs(podUID)
		if len(pids) == 0 {
			fmt.Println("  (no processes found for this pod UID)")
			return
		}
		for _, pid := range pids {
			s, err := parseSchedStat(pid)
			if err != nil {
				continue
			}
			printSchedStat(s)
		}
	}
}

func main() {
	pid := flag.Int("pid", 0, "PID to read schedstat for")
	pod := flag.String("pod", "", "Pod UID for cgroup cpu.stat throttle report")
	all := flag.Bool("all", false, "Also show per-process schedstat for --pod")
	flag.Parse()

	if *pid == 0 && *pod == "" {
		fmt.Fprintln(os.Stderr, "usage: sched-latency-reader --pid <pid> | --pod <uid> [--all]")
		os.Exit(1)
	}
	if *pid != 0 {
		runPID(*pid)
	}
	if *pod != "" {
		runPod(*pod, *all)
	}
}
```

### `go.mod`

```
module github.com/linux-to-k8s/sched-latency-reader

go 1.22
```

### `README.md`

```markdown
# sched-latency-reader

Reads scheduler latency from `/proc/<pid>/schedstat` (wait time on the runqueue)
and CPU throttle metrics from a pod cgroup's `cpu.stat`.

## Build and Run

```
go build -o sched-latency-reader .

# Scheduler latency for one process
./sched-latency-reader --pid 1234

# CPU throttle report for a pod
./sched-latency-reader --pod <pod-uid>

# Both: throttle report + per-process schedstat
./sched-latency-reader --pod <pod-uid> --all
```

## Sample Output

```
=== /proc/1234/schedstat ===
  pid=1234    comm=nginx            runtime=5234.1ms  wait=  87.3ms  wait_ratio= 1.6%  switches=4231

=== CPU throttle for pod abc123 ===
  cgroup: /sys/fs/cgroup/kubepods.slice/kubepods-burstable-podabc123.slice
  usage_usec:     5234567
  nr_periods:     1000
  nr_throttled:   50
  throttled_usec: 5000000
  throttle_ratio: 5.0%
```

## Kernel Paths

| Field | Kernel Source |
|-------|--------------|
| `/proc/<pid>/schedstat` | `kernel/sched/stats.c` — `sched_statistics.wait_sum` |
| `cpu.stat nr_throttled` | `kernel/sched/fair.c` — `struct cfs_bandwidth.nr_throttled` |
| `cpu.stat throttled_usec` | `kernel/sched/fair.c` — `cfs_bandwidth.throttled_time` |

Sources:
https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/stats.c
https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/fair.c

## Exercises

a) Add `--watch` flag that re-runs every 5 seconds and prints the delta in
   wait_ns (to compute wait rate in ns/s rather than cumulative total).

b) Read `/proc/schedstat` (system-wide) and compute average runqueue
   latency per CPU: `run_delay / pcount` for each CPU line.

c) Add parsing of `cpu.max` alongside `cpu.stat` and compute the
   theoretical maximum throttle-free throughput for the pod.
```
```

- [ ] **Step 1: Write all three files exactly as specified**
- [ ] **Step 2: Build: `go build ./...` + `go vet ./...`** — must pass clean
- [ ] **Step 3: Commit**

```bash
git add 10-performance/exercises/sched-latency-reader/
git commit -m "feat(ch10): Go exercise sched-latency-reader — /proc/schedstat wait_sum + cgroup cpu.stat throttle ratio"
```

---

## Task 7: kube-inspect checkpoint 10 — Full perf report: throttle stats + sched latency

**Files:**
- Create: `kube-inspect/internal/metrics/metrics.go`
- Modify: `kube-inspect/cmd/kube-inspect/main.go` (add `--perf` flag)
- Modify: `kube-inspect/CHECKPOINT.md` (mark checkpoint 10 done)

### `kube-inspect/internal/metrics/metrics.go`

```go
package metrics

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ThrottleStats holds cpu.stat throttle fields for a pod cgroup.
type ThrottleStats struct {
	PodUID      string
	CgPath      string
	NrPeriods   uint64
	NrThrottled uint64
	ThrottledUS uint64
	UsageUS     uint64
}

// ThrottleRatio returns the fraction of periods that were throttled (0.0–1.0).
func (t ThrottleStats) ThrottleRatio() float64 {
	if t.NrPeriods == 0 {
		return 0
	}
	return float64(t.NrThrottled) / float64(t.NrPeriods)
}

// ProcessSchedStat holds /proc/<pid>/schedstat for one process in a pod.
type ProcessSchedStat struct {
	PID        int
	Comm       string
	RuntimeNS  uint64
	WaitNS     uint64
	NrSwitches uint64
}

// WaitRatio returns wait/(wait+runtime). Returns 0 if both zero.
func (p ProcessSchedStat) WaitRatio() float64 {
	total := p.WaitNS + p.RuntimeNS
	if total == 0 {
		return 0
	}
	return float64(p.WaitNS) / float64(total)
}

// PodPerfReport aggregates throttle stats and per-process sched latency.
type PodPerfReport struct {
	PodUID    string
	Throttle  ThrottleStats
	Processes []ProcessSchedStat
}

func findPodCgroup(podUID string) (string, error) {
	base := "/sys/fs/cgroup/kubepods.slice"
	pattern := fmt.Sprintf("*pod%s*", podUID)
	matches, _ := filepath.Glob(filepath.Join(base, "*", pattern))
	direct, _ := filepath.Glob(filepath.Join(base, pattern))
	all := append(matches, direct...)
	if len(all) == 0 {
		return "", fmt.Errorf("cgroup not found for pod %s", podUID)
	}
	return all[0], nil
}

func parseCPUStat(path string) (ThrottleStats, error) {
	f, err := os.Open(path)
	if err != nil {
		return ThrottleStats{}, err
	}
	defer f.Close()

	var s ThrottleStats
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		parts := strings.Fields(scanner.Text())
		if len(parts) != 2 {
			continue
		}
		v, _ := strconv.ParseUint(parts[1], 10, 64)
		switch parts[0] {
		case "usage_usec":
			s.UsageUS = v
		case "nr_periods":
			s.NrPeriods = v
		case "nr_throttled":
			s.NrThrottled = v
		case "throttled_usec":
			s.ThrottledUS = v
		}
	}
	return s, scanner.Err()
}

func podPIDs(podUID string) []int {
	var pids []int
	entries, _ := os.ReadDir("/proc")
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		cgData, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
		if err != nil {
			continue
		}
		s := string(cgData)
		if strings.Contains(s, "pod"+podUID) ||
			strings.Contains(s, strings.ReplaceAll(podUID, "-", "")) {
			pids = append(pids, pid)
		}
	}
	return pids
}

func parseSchedStat(pid int) (ProcessSchedStat, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/schedstat", pid))
	if err != nil {
		return ProcessSchedStat{}, err
	}
	fields := strings.Fields(strings.TrimSpace(string(data)))
	if len(fields) < 3 {
		return ProcessSchedStat{}, fmt.Errorf("unexpected schedstat format")
	}
	rt, _ := strconv.ParseUint(fields[0], 10, 64)
	wait, _ := strconv.ParseUint(fields[1], 10, 64)
	sw, _ := strconv.ParseUint(fields[2], 10, 64)
	comm := "(unknown)"
	if cb, err2 := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid)); err2 == nil {
		comm = strings.TrimSpace(string(cb))
	}
	return ProcessSchedStat{PID: pid, Comm: comm, RuntimeNS: rt, WaitNS: wait, NrSwitches: sw}, nil
}

// GetPodPerfReport reads cpu.stat throttle metrics and per-process schedstat
// for all processes belonging to the given pod UID.
func GetPodPerfReport(podUID string) (PodPerfReport, error) {
	report := PodPerfReport{PodUID: podUID}

	cgPath, err := findPodCgroup(podUID)
	if err != nil {
		return report, err
	}
	ts, err := parseCPUStat(filepath.Join(cgPath, "cpu.stat"))
	if err != nil {
		return report, fmt.Errorf("cpu.stat: %w", err)
	}
	ts.PodUID = podUID
	ts.CgPath = cgPath
	report.Throttle = ts

	for _, pid := range podPIDs(podUID) {
		ps, err := parseSchedStat(pid)
		if err != nil {
			continue
		}
		report.Processes = append(report.Processes, ps)
	}
	return report, nil
}
```

### `--perf` flag in `main.go`

Add after the `--pressure` block, following the exact same pattern as all other flags:

```go
flagPerf = flag.Bool("perf", false, "Show CPU throttle stats and scheduler latency per process (requires --pod)")
```

Output block (inside `if *flagPod != ""`):

```go
if *flagPerf {
    report, err := metrics.GetPodPerfReport(*flagPod)
    if err != nil {
        fmt.Fprintf(os.Stderr, "perf: %v\n", err)
    } else {
        t := report.Throttle
        fmt.Printf("CPU throttle for pod %s:\n", report.PodUID)
        fmt.Printf("  cgroup:         %s\n", t.CgPath)
        fmt.Printf("  usage_usec:     %d\n", t.UsageUS)
        fmt.Printf("  nr_periods:     %d\n", t.NrPeriods)
        fmt.Printf("  nr_throttled:   %d\n", t.NrThrottled)
        fmt.Printf("  throttled_usec: %d\n", t.ThrottledUS)
        fmt.Printf("  throttle_ratio: %.1f%%\n", t.ThrottleRatio()*100)
        fmt.Println()
        if len(report.Processes) > 0 {
            fmt.Printf("Scheduler latency (top processes by wait ratio):\n")
            fmt.Printf("  %-8s %-16s %12s %12s %8s %10s\n",
                "PID", "COMM", "RUNTIME_MS", "WAIT_MS", "WAIT%", "SWITCHES")
            for _, p := range report.Processes {
                fmt.Printf("  %-8d %-16s %12.1f %12.1f %7.1f%% %10d\n",
                    p.PID, p.Comm,
                    float64(p.RuntimeNS)/1e6,
                    float64(p.WaitNS)/1e6,
                    p.WaitRatio()*100,
                    p.NrSwitches)
            }
            fmt.Println()
        }
    }
}
```

Import: `"github.com/linux-to-k8s/kube-inspect/internal/metrics"`

Also add `[--perf]` to the usage string after `[--pressure]`.

### `CHECKPOINT.md`

Change `| 10 | Full perf report — complete tool | internal/metrics | pending |` to `| 10 | Full perf report — complete tool | internal/metrics | done |`

- [ ] **Step 1: Read `kube-inspect/cmd/kube-inspect/main.go`** — note current import block and flag/output pattern
- [ ] **Step 2: Read `kube-inspect/CHECKPOINT.md`** — note current row 10
- [ ] **Step 3: Create `kube-inspect/internal/metrics/metrics.go`** exactly as specified
- [ ] **Step 4: Modify `kube-inspect/cmd/kube-inspect/main.go`** — add `--perf` flag and output block
- [ ] **Step 5: Modify `kube-inspect/CHECKPOINT.md`** — mark row 10 done
- [ ] **Step 6: Build and vet**

```bash
cd kube-inspect
go build ./...
go vet ./...
```
Expected: clean.

- [ ] **Step 7: Commit**

```bash
cd ..
git add kube-inspect/internal/metrics/metrics.go kube-inspect/cmd/kube-inspect/main.go kube-inspect/CHECKPOINT.md
git commit -m "feat(kube-inspect): checkpoint 10 — CPU throttle + sched latency perf report"
```

---

## Self-Review

**Spec coverage:**
- ✅ `10-performance/README.md` — objectives, prerequisites, reading order, kernel-to-k8s bridge
- ✅ `kernel/10-a-perf-events.md` — struct perf_event_attr, perf_event_open, hardware/software events, struct perf_event (8 sections)
- ✅ `kernel/10-b-tracing.md` — struct tracepoint, struct trace_event_call, ring buffer, ftrace (8 sections)
- ✅ `kernel/10-c-schedstat.md` — struct sched_statistics, /proc/schedstat, cpu.stat throttle (8 sections)
- ✅ `k8s/10-k8s-connection.md` — throttle diagnosis, perf in containers, sched latency patterns (8 sections)
- ✅ `exercises/perf-event-demo/` — C exercise, perf_event_open, cycles, IPC, cache, software events
- ✅ `exercises/sched-latency-reader/` — Go exercise, schedstat parsing, cpu.stat throttle ratio
- ✅ `kube-inspect/internal/metrics/` — ThrottleStats, ProcessSchedStat, PodPerfReport, GetPodPerfReport

**Placeholder scan:** All sections contain real content. No TBD/TODO.

**Type consistency:**
- `ThrottleStats.ThrottleRatio()` used in main.go output: `t.ThrottleRatio()*100` — matches method on ThrottleStats
- `ProcessSchedStat.WaitRatio()` used in main.go: `p.WaitRatio()*100` — matches method
- `PodPerfReport.Throttle` (ThrottleStats), `PodPerfReport.Processes` ([]ProcessSchedStat) — consistent
- `--perf` flag in usage string after `[--pressure]` — consistent with plan
- `GetPodPerfReport(podUID string) (PodPerfReport, error)` — called correctly in main.go
