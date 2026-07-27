# 10-a — `perf_event_open`, `struct perf_event`, and Hardware PMU Counters

## Measuring What Actually Happens

Profiling a program on a modern system is hard. The processor executes instructions out of order, speculatively, across multiple cores. A program that appears to spend 30ms in `malloc()` according to `strace` might actually be stalling in the L3 cache. A pod that reports 100% CPU usage in Kubernetes metrics might be spending half that time waiting for memory bandwidth. The nanosecond-accurate numbers that userspace profilers produce measure elapsed time — they cannot tell you *why* the time is being spent.

Hardware Performance Monitoring Units (PMUs) can. Every modern CPU has a set of performance counters — hardware registers that count specific micro-architectural events: instruction retirements, branch mispredictions, cache misses, TLB misses, memory bandwidth consumed, cycles spent stalled waiting for data from DRAM. These counters measure what the CPU physically does, independent of the clock. If a function takes twice as long as expected because of cache misses, the cache miss counter will tell you exactly how many misses occurred.

The Linux perf subsystem, introduced in 2009, exposes hardware PMU counters through a unified kernel interface. The `perf_event_open(2)` syscall creates a file descriptor that monitors either a specific process or a specific CPU for a specific event. The `perf` command-line tool, bpftrace, and profilers like flamegraph generators all use this interface. For Kubernetes, it is the foundation for identifying which pods are causing cache pollution on shared nodes, for understanding why latency spikes correlate with specific CPU-level events, and for capacity planning based on actual resource consumption rather than requested limits.

## 1. Source Locations

| File | Key Symbols | URL |
|------|-------------|-----|
| `include/uapi/linux/perf_event.h` | `struct perf_event_attr`, `PERF_TYPE_*`, `PERF_COUNT_HW_*`, `PERF_COUNT_SW_*` | https://elixir.bootlin.com/linux/v6.9/source/include/uapi/linux/perf_event.h |
| `include/linux/perf_event.h` | `struct perf_event`, `struct hw_perf_event`, `struct perf_event_context` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/perf_event.h |
| `kernel/events/core.c` | `perf_event_open()`, `perf_read()`, `__perf_event_enable()` | https://elixir.bootlin.com/linux/v6.9/source/kernel/events/core.c |
| `arch/x86/events/core.c` | x86 PMU driver: `x86_pmu_enable_event()`, `x86_pmu_read()` | https://elixir.bootlin.com/linux/v6.9/source/arch/x86/events/core.c |
| `include/linux/perf_event.h` | `struct pmu` — PMU driver vtable | https://elixir.bootlin.com/linux/v6.9/source/include/linux/perf_event.h |

## 2. `perf_event_open(2)` Syscall

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
`pid=-1, cpu=N`: measure all processes on CPU N (requires `CAP_PERFMON` or `perf_event_paranoid <= 0`).
`group_fd`: links events into a group — all are measured atomically together. Useful for computing IPC (instructions/cycles).

## 3. `struct perf_event_attr`

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

## 4. Hardware Events (`PERF_TYPE_HARDWARE`)

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

## 5. Software Events (`PERF_TYPE_SOFTWARE`)

| `config` constant | Value | Measures |
|-------------------|-------|---------|
| `PERF_COUNT_SW_CPU_CLOCK` | 0 | CPU clock (nanoseconds) |
| `PERF_COUNT_SW_TASK_CLOCK` | 1 | Task clock (time on CPU) |
| `PERF_COUNT_SW_PAGE_FAULTS` | 2 | Page faults (minor + major) |
| `PERF_COUNT_SW_CONTEXT_SWITCHES` | 3 | Context switches |
| `PERF_COUNT_SW_CPU_MIGRATIONS` | 4 | CPU migrations |
| `PERF_COUNT_SW_PAGE_FAULTS_MIN` | 5 | Minor page faults |
| `PERF_COUNT_SW_PAGE_FAULTS_MAJ` | 6 | Major page faults |

## 6. `struct perf_event` — Kernel Internal

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

## 7. Live Observation

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

## 8. Key Kernel References

| Symbol | File | URL |
|--------|------|-----|
| `struct perf_event_attr` | `include/uapi/linux/perf_event.h` | https://elixir.bootlin.com/linux/v6.9/source/include/uapi/linux/perf_event.h |
| `struct perf_event` | `include/linux/perf_event.h` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/perf_event.h |
| `perf_event_open()` | `kernel/events/core.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/events/core.c |
| `struct hw_perf_event` | `include/linux/perf_event.h` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/perf_event.h |
| `struct pmu` (PMU driver vtable) | `include/linux/perf_event.h` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/perf_event.h |
| `x86_pmu_enable_event()` | `arch/x86/events/core.c` | https://elixir.bootlin.com/linux/v6.9/source/arch/x86/events/core.c |

---

## On ARM64: ARM PMUv3, `mrs` Instruction, Same `perf_event_open` API

The `perf_event_open(2)` system call, `struct perf_event_attr`, and everything from the kernel's `kernel/events/core.c` described above are architecture-independent. What changes on ARM64 (Raspberry Pi 5) is the hardware implementation underneath.

**x86 PMU: MSR-based.** x86 hardware performance counters are accessed via Model Specific Registers. `RDMSR` reads them; `WRMSR` writes them. The Linux x86 PMU driver (`arch/x86/events/core.c`, `arch/x86/events/intel/core.c`) issues `RDMSR`/`WRMSR` instructions in Ring 0 to program and read counters. Event codes differ between Intel and AMD and across CPU generations (Intel vs AMD event codes for LLC misses are completely different numbers).

**ARM64 PMU: System registers, `mrs` instruction.** ARM64 accesses performance monitoring through a set of architecturally-defined system registers read with `mrs` (Move from System Register). Key registers:

| Register | Description | x86 equivalent |
|----------|-------------|----------------|
| `PMCCNTR_EL0` | 64-bit cycle counter | `IA32_TIME_STAMP_COUNTER` (RDTSC) |
| `PMCR_EL0` | PMU control: enable, reset, counter count | `IA32_PERF_GLOBAL_CTRL` |
| `PMEVCNTR<n>_EL0` | Event counter n (up to 31) | `IA32_PMC<n>` (MSR 0x0C1+n) |
| `PMEVTYPER<n>_EL0` | Event type for counter n | `IA32_PERFEVTSEL<n>` (MSR 0x186+n) |

**User-space PMU access:** On both architectures, the kernel sets a flag allowing user-mode applications to read PMU counters directly without a syscall — eliminating the overhead of reading a counter. On x86 this is done via `RDPMC` instruction (enabled by CR4.PCE). On ARM64, the kernel sets `PMUSERENR_EL0.EN=1`, enabling direct `mrs pmccntr_el0` from EL0. After `perf_event_open()` creates a cycle counter event, the application can read the cycle counter via `mrs` at near-zero overhead — no system call, no mode switch.

**ARM64 PMU event codes (ARMv8 architectural, Cortex-A76):**
- `CPU_CYCLES` = 0x11 (equivalent: `perf stat -e cycles`)
- `INST_RETIRED` = 0x08 (equivalent: `perf stat -e instructions`)
- `L1D_CACHE_REFILL` = 0x03 (equivalent: `perf stat -e cache-misses`)
- `STALL_FRONTEND` = 0x23, `STALL_BACKEND` = 0x24 (no direct x86 equivalent)

**Same perf commands work:** `perf stat -e cycles,instructions,cache-misses` runs identically on x86 and ARM64. The Linux perf tool maps symbolic event names to architecture-specific codes at runtime. `bpftrace` hardware probe events (`hardware:cpu-cycles:1000`) work the same way.

**Full ARM64 coverage:** `kernel/13-c-arm64-pmu.md` — full register table, Cortex-A76 event codes, bpftrace on ARM64, user-mode PMU access via `mrs`.
