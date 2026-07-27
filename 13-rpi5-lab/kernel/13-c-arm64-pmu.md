# 13-c — ARM64 PMU: Performance Monitoring Registers and `perf_event_open` on ARMv8

## Same Interface, Different Hardware

Chapter 10 introduced the Linux performance monitoring infrastructure: `perf_event_open(2)` as the stable kernel interface, perf and bpftrace as the user-facing tools, hardware performance counters as the mechanism that makes it fast. The chapter was written from an x86 perspective — PMU events are read from x86 MSRs via `RDMSR`, the precise events are described by Intel or AMD architecture manuals, and the Linux perf subsystem abstracts the hardware away.

On the Raspberry Pi 5, the hardware is different. The BCM2712 SoC uses ARM Cortex-A76 cores implementing ARMv8.2-A. The PMU is the ARM PMU — accessed not through MSRs but through ARM system registers read with the `mrs` (Move from System Register) instruction. The event numbers are defined by the ARM Architecture Reference Manual. The perf API is identical.

This is one of the clearest examples in the course of Linux abstraction working correctly. `perf stat -e cache-misses ./my_program` works on x86 and ARM64 with the same command and produces comparable output. Underneath, on x86 the kernel reads `IA32_PMC0` (MSR 0x0C1); on ARM64 the kernel reads `PMEVCNTR0_EL0` (PMEVCNTRn_EL0). The abstraction is complete. The hardware difference is hidden.

Understanding the ARM64 PMU hardware lets you read raw system registers from bpftrace or C programs, write architecture-specific perf analysis tools for your RPi5 cluster, and understand why certain x86 perf events have no ARM64 equivalent (and vice versa).

## Source Locations

| File | Key Symbols | URL |
|------|-------------|-----|
| `arch/arm64/kernel/perf_event.c` | ARM64 PMU driver, event→hardware mapping | https://elixir.bootlin.com/linux/v6.9/source/arch/arm64/kernel/perf_event.c |
| `arch/arm64/include/asm/perf_event.h` | ARM64 PMU register definitions, `ARMV8_PMUV3_*` | https://elixir.bootlin.com/linux/v6.9/source/arch/arm64/include/asm/perf_event.h |
| `kernel/events/core.c` | `perf_event_open()` — architecture-independent core | https://elixir.bootlin.com/linux/v6.9/source/kernel/events/core.c |
| `arch/arm64/kernel/perf_callchain.c` | ARM64 stack unwinding for perf | https://elixir.bootlin.com/linux/v6.9/source/arch/arm64/kernel/perf_callchain.c |

## 1. ARM PMU System Registers

ARM64 PMU registers are system registers accessed with `mrs`/`msr` instructions. Key registers:

| Register | Access | Description |
|----------|--------|-------------|
| `PMCR_EL0` | RW | PMU control: enable, event counter count, reset |
| `PMCNTENSET_EL0` | WO | Enable specific event counters |
| `PMCNTENCLR_EL0` | WO | Disable specific event counters |
| `PMOVSR_EL0` | RW | Overflow status register |
| `PMSELR_EL0` | RW | Select which counter `PMXEVTYPER`/`PMXEVCNTR` access |
| `PMXEVTYPER_EL0` | RW | Set event type for selected counter |
| `PMXEVCNTR_EL0` | RW | Read/write selected event counter value |
| `PMEVCNTR<n>_EL0` | RW | Direct access to counter n (n=0..30) |
| `PMEVTYPER<n>_EL0` | RW | Direct event type for counter n |
| `PMCCNTR_EL0` | RW | Cycle counter (64-bit) |
| `PMCCFILTR_EL0` | RW | Cycle counter filter (EL0/EL1/EL2/EL3) |

**Reading the cycle counter from user space (with PMU user access enabled):**

```c
// Read PMCCNTR_EL0 — cycle counter
// Works if PMUSERENR_EL0.EN bit is set by the kernel
static inline uint64_t arm64_read_cycle_counter(void)
{
    uint64_t val;
    asm volatile("mrs %0, pmccntr_el0" : "=r"(val));
    return val;
}
```

The Linux kernel enables user-space PMU access via `PMUSERENR_EL0` in the perf driver. When `perf_event_open()` creates a CPU cycle counter event, the kernel sets the bit; the application can then read `PMCCNTR_EL0` directly without a syscall on each read — the same optimization that x86 uses with `RDTSC`.

## 2. ARM PMU Event Numbers

ARM defines a standard set of event numbers in the ARM Architecture Reference Manual (PMUv3). These are supported on all ARMv8 implementations including Cortex-A76:

| Event | Code | Description | x86 equivalent |
|-------|------|-------------|----------------|
| `SW_INCR` | 0x00 | Software increment | (no equivalent) |
| `L1I_CACHE_REFILL` | 0x01 | L1 instruction cache refill | `L1-icache-load-misses` |
| `L1I_TLB_REFILL` | 0x02 | L1 instruction TLB refill | `iTLB-load-misses` |
| `L1D_CACHE_REFILL` | 0x03 | L1 data cache refill | `L1-dcache-load-misses` |
| `L1D_CACHE` | 0x04 | L1 data cache access | `L1-dcache-loads` |
| `L1D_TLB_REFILL` | 0x05 | L1 data TLB refill | `dTLB-load-misses` |
| `MEM_ACCESS` | 0x13 | Data memory access | (approximate: `mem-loads`) |
| `L1D_CACHE_WB` | 0x15 | L1 data cache writeback | (no direct equivalent) |
| `L2D_CACHE` | 0x16 | L2 data cache access | `LLC-loads` |
| `L2D_CACHE_REFILL` | 0x17 | L2 data cache refill | `LLC-load-misses` |
| `BUS_ACCESS` | 0x19 | Bus access | (no equivalent) |
| `INST_RETIRED` | 0x08 | Instructions architecturally executed | `instructions` |
| `EXC_TAKEN` | 0x09 | Exceptions taken | (no direct equivalent) |
| `CPU_CYCLES` | 0x11 | CPU cycles | `cycles` |
| `BR_PRED` | 0x12 | Predictable branch speculatively executed | `branch-instructions` |
| `STALL_FRONTEND` | 0x23 | No ops from frontend (fetch stall) | (approximate) |
| `STALL_BACKEND` | 0x24 | No ops from backend (execution stall) | (approximate) |

## 3. `perf` on ARM64: Same Commands, Different Hardware

```bash
# CPU cycles and instructions (works identically on x86 and ARM64)
perf stat -e cycles,instructions,cache-references,cache-misses -- sleep 1

# On ARM64, perf maps "cycles" → CPU_CYCLES (0x11), "cache-misses" → L1D_CACHE_REFILL (0x03)
# On x86, perf maps "cycles" → IA32_FIXED_CTR1, "cache-misses" → event 0x2E umask 0x41

# ARM64-specific: frontend and backend stalls (useful for Cortex-A76 analysis)
perf stat -e r23,r24 -- ./my_workload
# r23 = STALL_FRONTEND (0x23), r24 = STALL_BACKEND (0x24)

# Raw ARM64 event access (hex event code):
perf stat -e r0003 -- ./my_workload
# r0003 = L1D_CACHE_REFILL

# System-wide profiling on the RPi5 cluster:
perf record -g -a -- sleep 10
perf report

# Verify perf works with Kubernetes workloads:
# (run while a pod is executing)
kubectl exec -it mypod -- sh -c 'while true; do cat /dev/null; done' &
perf top -a -e cycles
```

## 4. ARM64 PMU in bpftrace

bpftrace accesses hardware PMU events through the same `perf_event_open()` kernel interface:

```bash
# Count cache misses per process name (ARM64-compatible)
bpftrace -e 'hardware:cache-misses:1000 { @[comm] = count(); }'

# Profile CPU cycles on all CPUs (works on ARM64)
bpftrace -e 'hardware:cpu-cycles:100000 { @[kstack] = count(); } interval:s:10 { print(@); clear(@); exit(); }'

# ARM64-specific: count SVC (system call trap) exceptions
# This is a tracepoint, not a PMU event, but shows ARM64-specific counting:
bpftrace -e 'tracepoint:exceptions:arm64_break_hook { @brks++ } interval:s:1 { print(@brks); }'

# L1D cache miss rate via bpftrace (ARM64 raw event):
bpftrace -e '
  hardware:L1-dcache-load-misses:1000 { @misses++ }
  hardware:L1-dcache-loads:1000 { @loads++ }
  interval:s:5 {
    printf("L1D miss rate: %.2f%%\n", (float)@misses/(float)@loads*100);
    clear(@misses); clear(@loads);
  }'
```

## 5. Cortex-A76 Specifics (BCM2712)

The Raspberry Pi 5 uses the BCM2712 SoC with 4× Cortex-A76 cores at 2.4GHz. The Cortex-A76:

- 4-wide decode (instructions decoded per cycle)
- 8-wide out-of-order execution
- L1: 64KB instruction + 64KB data per core
- L2: 512KB per core (unified)
- L3: 2MB shared (equivalent of x86 LLC)
- Out-of-order window: 128 μops in-flight
- Branch predictor: hybrid indirect/direct, 6KB BTB

**Performance characterization:** For Kubernetes workloads on RPi5, the bottleneck is typically L2/L3 cache bandwidth and memory latency, not compute throughput. Each Cortex-A76 can execute ~4 instructions per cycle but LPDDR4X has ~60ns latency (vs ~40ns DDR5 in a server). A pod doing in-memory hash table operations will show high `L2D_CACHE_REFILL` counts.

```bash
# Read CPU identification on RPi5
# MIDR_EL1: Main ID Register — contains implementer, part number, revision
cat /proc/cpuinfo | grep -E "CPU part|CPU implementer|CPU architecture"
# CPU implementer : 0x41  (ARM Ltd)
# CPU architecture: 8
# CPU part        : 0xd0b  (Cortex-A76)
```

## 6. Key Kernel References

| Symbol | File | URL |
|--------|------|-----|
| ARM64 PMU driver | `arch/arm64/kernel/perf_event.c` | https://elixir.bootlin.com/linux/v6.9/source/arch/arm64/kernel/perf_event.c |
| `ARMV8_PMUV3_PERFCTR_*` | `arch/arm64/include/asm/perf_event.h` | https://elixir.bootlin.com/linux/v6.9/source/arch/arm64/include/asm/perf_event.h |
| `perf_event_open()` core | `kernel/events/core.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/events/core.c |
