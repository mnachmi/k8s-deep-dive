# Exercise: arm64-sysinfo

Read ARM64 system registers and CPU identification information from a Raspberry Pi 5 or any ARM64 Linux system. Demonstrates the `mrs` instruction, cache topology, PMU register layout, and exception level model.

## Build and Run

```bash
# Must be compiled for ARM64 (aarch64) — either on the RPi5 or cross-compiled
gcc -O2 -Wall -march=armv8-a -o arm64_sysinfo arm64_sysinfo.c

# Run (most info accessible without root)
./arm64_sysinfo

# Full PMU access requires running perf first (to enable PMUSERENR_EL0):
sudo sysctl -w kernel.perf_event_paranoid=1
perf stat -e cycles -- sleep 1
./arm64_sysinfo
```

## Expected Output

```
ARM64 System Information
──────────────────────────────────────────────────────────
Kernel : Linux 6.8.0-1013-raspi aarch64
Node   : rpi5-01
──────────────────────────────────────────────────────────
CPU Identification (from /proc/cpuinfo):
  Hardware     : BCM2835
  Model        : Raspberry Pi 5 Model B Rev 1.0
  CPU implementer  : 0x41
  CPU architecture : 8
  CPU part     : 0xd0b
  CPU revision : 0
  Implementer decoded : ARM Limited
  CPU part decoded    : Cortex-A76

Cache Topology (from CTR_EL0 and /sys):
  CTR_EL0             : 0x84448004
  L1 I-cache line     : 64 bytes
  L1 D-cache line     : 64 bytes
  L1 Instruction cache : 64K
  L1 Data cache       : 64K
  L2 Unified cache    : 512K

ARM PMU (Performance Monitoring Unit):
  PMU type (perf id)  : 8 (armv8_pmuv3)
  Standard events     :
    cpu-cycles                     (ARM code: r11) CPU_CYCLES — cycle counter
    instructions                   (ARM code: r08) INST_RETIRED — instructions executed
    cache-references               (ARM code: r04) L1D_CACHE — L1 data cache accesses
    cache-misses                   (ARM code: r03) L1D_CACHE_REFILL — L1 miss
    ...

Memory System:
  Page size           : 4096 bytes
  TTBR split          : TTBR0_EL1 (user 0x0000...) / TTBR1_EL1 (kernel 0xFFFF...)
  VA bits             : 48 (256 TB per half, ARMv8.0-A)
  Memory model        : RVWMO (weakly ordered — barriers needed for SMP)
  ASID width          : 16-bit (Cortex-A76, ARMv8.2-A)
  MemTotal:           8167744 kB
  MemAvailable:       6891032 kB

Exception Levels:
  EL0 (user)          : current process (this program)
  EL1 (kernel)        : Linux kernel (active)
  EL2 (hypervisor)    : KVM available (/dev/kvm present)
  EL3 (secure mon)    : TrustZone firmware (not accessible from Linux)
  SVC instruction     : EL0 → EL1 trap (system call)
```

## What the `mrs` Instruction Does

`mrs x0, ctr_el0` — Move from System Register to general-purpose register. This is the ARM64 equivalent of x86's `RDMSR` (read MSR) but for architectural system registers. Unlike x86 MSRs (which require Ring 0 for most registers), ARM64 defines specific registers as EL0-accessible. `CTR_EL0` (Cache Type Register) is one of them — user processes can read it without a syscall.

Registers accessible at EL0 (without privilege):
- `CTR_EL0` — cache line sizes, cache type
- `PMCCNTR_EL0` — cycle counter (if PMUSERENR_EL0.EN=1)
- `PMXEVCNTR_EL0` — event counters (if PMUSERENR_EL0.EN=1)
- `DCZID_EL0` — data cache zero ID register
- `FPCR`/`FPSR` — floating point control/status

EL1-only registers (kernel-accessible only):
- `MIDR_EL1` — main ID register (CPU part, variant, revision)
- `MPIDR_EL1` — multiprocessor affinity register
- `TTBR0_EL1`/`TTBR1_EL1` — page table base registers
- `VBAR_EL1` — exception vector table base
- `PMCR_EL0` — PMU control (paradoxically named but often EL1-managed)

## Kernel Source Connections

| Register | Purpose | Linux source |
|----------|---------|-------------|
| `CTR_EL0` | Cache line sizes | `arch/arm64/include/asm/cacheflush.h` |
| `PMCR_EL0` | PMU control | `arch/arm64/include/asm/perf_event.h` |
| `PMCCNTR_EL0` | Cycle counter | `arch/arm64/kernel/perf_event.c` |
| `TTBR0_EL1` | User page tables | `arch/arm64/mm/proc.S` |
| `VBAR_EL1` | Exception vectors | `arch/arm64/kernel/head.S` |

## Go to Chapter

- `kernel/13-a-arm64-syscall.md` — `SVC` instruction, EL levels, `VBAR_EL1`
- `kernel/13-b-arm64-memory.md` — `TTBR0`/`TTBR1`, ASID, weak memory model
- `kernel/13-c-arm64-pmu.md` — ARM PMU registers and `perf_event_open`
