# 13-b — ARM64 Memory: Weak Ordering, `TTBR0`/`TTBR1`, and ASID vs PCID

## The Race Condition That Only Fires on ARM

In 2013, a team porting a high-performance lock-free queue implementation from x86 to ARM found that their code crashed immediately. The code had no explicit memory barriers. It had worked on x86 for two years. The reason it worked on x86 had nothing to do with the code being correct — it worked because x86 enforces Total Store Order (TSO), which guarantees that stores become visible to other processors in program order. ARM does not.

ARM64 uses a **weakly ordered memory model**: the CPU may reorder memory accesses as long as single-thread program semantics are preserved. Two stores to two different addresses may become visible to another CPU in reverse order. A store followed by a load to a different address may have the load complete first from another CPU's perspective. This is permitted by the ARMv8 architecture.

The x86 equivalent is a `LOCK`-prefixed instruction or an `MFENCE`. On ARM64 the equivalent is a `dsb sy` (Data Synchronization Barrier, full system) or `dmb ish` (Data Memory Barrier, inner shareable domain). Code that omits barriers and relies on x86 TSO is correct on x86, subtly wrong on ARM64, and the bugs manifest as rare intermittent failures under load.

This matters for Kubernetes on RPi5 because the kernel, container runtime, and application code all make memory ordering assumptions. The Linux kernel abstracts most of this — `atomic_t` operations, spinlocks, and RCU all include the necessary barriers in their architecture-specific implementations. But user-space code that uses `std::atomic<T>` with `memory_order_relaxed` and expects TSO semantics will fail on ARM.

## Source Locations

| File | Key Symbols | URL |
|------|-------------|-----|
| `arch/arm64/include/asm/barrier.h` | `dsb()`, `dmb()`, `isb()`, `smp_mb()`, `smp_rmb()`, `smp_wmb()` | https://elixir.bootlin.com/linux/v6.9/source/arch/arm64/include/asm/barrier.h |
| `arch/arm64/mm/proc.S` | `TTBR0_EL1`/`TTBR1_EL1` setup, ASID handling | https://elixir.bootlin.com/linux/v6.9/source/arch/arm64/mm/proc.S |
| `arch/arm64/include/asm/pgtable.h` | ARM64 page table flags and entry manipulation | https://elixir.bootlin.com/linux/v6.9/source/arch/arm64/include/asm/pgtable.h |
| `arch/arm64/mm/context.c` | ASID allocation, `check_and_switch_context()` | https://elixir.bootlin.com/linux/v6.9/source/arch/arm64/mm/context.c |

## 1. The ARM64 Memory Model: RVWMO

ARM64 implements the Relaxed Virtual Memory Ordering (RVWMO) model defined in the ARM Architecture Reference Manual. The key properties:

- **Stores are observable out of order:** CPU0 writing A=1 then B=1, CPU1 reading B=1 does NOT guarantee it sees A=1.
- **Loads can complete before earlier stores:** speculative loads can complete while the pipeline is still processing earlier stores.
- **Same-address ordering is preserved:** reads and writes to the same address always appear in program order.
- **Dependent loads are ordered:** if you read a pointer and then dereference it, the dereference sees what the pointer points to (data-dependency ordering).

x86 TSO guarantees that all stores become visible in program order to all processors (Total Store Order). It does NOT prevent load reordering (loads can be hoisted before earlier stores to different addresses), but in practice x86 code that would fail under RVWMO almost always also fails under TSO.

## 2. Memory Barriers on ARM64

```c
// arch/arm64/include/asm/barrier.h

// dsb sy — Data Synchronization Barrier, full system
// Ensures all memory accesses before the barrier are visible to
// all observers before any accesses after the barrier begin.
#define dsb(opt) __asm__ __volatile__("dsb " #opt : : : "memory")

// dmb ish — Data Memory Barrier, inner shareable
// Ensures ordering between memory accesses within the inner shareable
// domain (all CPUs on the chip) — cheaper than dsb sy.
#define dmb(opt) __asm__ __volatile__("dmb " #opt : : : "memory")

// isb — Instruction Synchronization Barrier
// Flushes the CPU pipeline. Required after writing system registers
// that affect instruction fetch (like VBAR_EL1 after patching).
#define isb() __asm__ __volatile__("isb" : : : "memory")

// Linux kernel SMP primitives (use these in portable kernel code):
#define smp_mb()    dmb(ish)    // full barrier (read + write ordering)
#define smp_rmb()   dmb(ishld)  // load barrier (load-load ordering)
#define smp_wmb()   dmb(ishst)  // store barrier (store-store ordering)
```

**Where barriers matter for Kubernetes workloads:**

| Scenario | Required barrier | x86 behavior | ARM64 without barrier |
|----------|-----------------|--------------|----------------------|
| Producer/consumer ring buffer | `smp_wmb()` after write, `smp_rmb()` before read | Works (TSO) | Reads stale data |
| Lock-free stack push | `smp_mb()` after CAS | Works (TSO LOCK prefix) | ABA problem |
| Publishing a struct pointer | `smp_wmb()` before publishing ptr | Works (TSO) | Reader sees half-written struct |

The virtio ring buffer (Chapter 12-c) uses `vring_desc` entries — the Linux virtio driver correctly inserts barriers at all ring transitions. If you write your own ring buffer in a pod and skip barriers, it fails on ARM.

## 3. ARM64 Page Tables: TTBR0 and TTBR1

Chapter 04 described x86 page tables: CR3 points to the process page table (user address space). The kernel has a fixed mapping in the upper half of the 64-bit address space, also accessible via CR3 (with KPTI, the kernel has its own CR3).

ARM64 splits this more cleanly into two registers:

```
TTBR0_EL1 — userspace page table base register
  Controls translations for addresses 0x0000_0000_0000_0000 → 0x0000_FFFF_FFFF_FFFF
  Points to the current process's PGD (Page Global Directory)
  Swapped on every context switch (like CR3 on x86)

TTBR1_EL1 — kernel page table base register
  Controls translations for addresses 0xFFFF_0000_0000_0000 → 0xFFFF_FFFF_FFFF_FFFF
  Points to the kernel's PGD — never changes (unlike x86 KPTI)
  Set once at kernel init, all CPUs share the same TTBR1_EL1 value
```

The split is architecturally enforced: address bit 55 selects which register is used for translation. User addresses (bit 55 = 0) use TTBR0_EL1. Kernel addresses (bit 55 = 1) use TTBR1_EL1. This is why ARM64 Linux does not need KPTI (Kernel Page Table Isolation) — the hardware already provides address space separation at the MMU level. The x86 Meltdown vulnerability required KPTI precisely because x86 does not have this separation.

## 4. ASID: Address Space ID (ARM64's PCID)

Chapter 00 covered x86 PCID (Process Context ID): a 12-bit TLB tag that allows TLB entries from different processes to coexist, eliminating the TLB flush on every context switch.

ARM64 has the equivalent: ASID (Address Space ID), a 16-bit field in the TTBR0_EL1 register (stored in bits [63:48]).

```c
// arch/arm64/mm/context.c — ASID allocation
static u64 new_context(struct mm_struct *mm)
{
    static atomic64_t asid_generation = ATOMIC64_INIT(ASID_FIRST_VERSION);
    u64 asid = 0;
    u64 generation = atomic64_read(&asid_generation);

    // Try to reuse the existing ASID if generation matches
    if (mm->context.id != 0) {
        u64 newasid = generation | (mm->context.id & ~ASID_MASK);
        if (check_update_reserved_asid(mm->context.id, newasid))
            return newasid;
    }

    // Allocate a new ASID (cycles through the 16-bit space)
    asid = find_next_zero_bit(asid_map, NUM_USER_ASIDS, cur_idx);
    ...
}
```

When the ASID space is exhausted (2^16 = 65536 ASIDs), the kernel increments the generation counter and flushes the TLB — identical in concept to x86 PCID rollover behavior.

The ASID is written into TTBR0_EL1 on context switch:

```asm
// arch/arm64/mm/proc.S
// TTBR0_EL1 = (ASID << 48) | pgd_phys
msr ttbr0_el1, x0    // x0 = asid:pgd combination
isb                   // ISB required after TTBR write
```

## 5. ARM64 Page Table Format

ARM64 uses a 4-level page table (identical depth to x86 Skylake+), but with slightly different terminology:

```
ARM64                    x86-64
────────────────────     ────────────────────
PGD (bits [47:39])   =   PGD (bits [47:39])
PUD (bits [38:30])   =   P4D/PUD (bits [38:30])
PMD (bits [29:21])   =   PMD (bits [29:21])
PTE (bits [20:12])   =   PTE (bits [20:12])
Page offset [11:0]       Page offset [11:0]
```

Page table entry format differs — ARM64 uses "block" entries (equivalent of x86 huge pages) at PUD and PMD levels. A PUD-level block entry maps 1GB (same as x86 1GB huge page). A PMD-level block entry maps 2MB (same as x86 2MB huge page).

## 6. Live Observation on RPi5

```bash
# Verify page size (ARM64 Linux uses 4KB pages by default)
getconf PAGE_SIZE
# → 4096

# Verify address space split
# User addresses:
cat /proc/self/maps | head -5
# → starts at 0x000000... (TTBR0 range)

# Kernel addresses:
# Visible in /proc/kallsyms (if not kptr_restrict)
sudo cat /proc/kallsyms | grep " T " | head -5
# → ffff800008... (TTBR1 range, bit 55 = 1)

# Memory ordering: demonstrate barrier requirement
# (This is theoretical — Linux kernel handles it; use this for learning)
# The kernel's rcu_dereference() on ARM64 uses smp_read_barrier_depends()
# which compiles to a dmb(ish) on weak memory architectures

# Check ASID size support
# 16-bit ASID support in ARMv8.1+ (RPi5 BCM2712 is ARMv8.2-A):
grep -i asid /proc/cpuinfo
# Or check ID_AA64MMFR0_EL1 register (requires kernel module or arm64_sysinfo exercise)

# Large system register dump (requires arm64-sysinfo exercise from this chapter):
./arm64_sysinfo
```

## 7. Key Kernel References

| Symbol | File | URL |
|--------|------|-----|
| `smp_mb()`, `dsb()`, `dmb()` | `arch/arm64/include/asm/barrier.h` | https://elixir.bootlin.com/linux/v6.9/source/arch/arm64/include/asm/barrier.h |
| `TTBR0_EL1`/`TTBR1_EL1` write | `arch/arm64/mm/proc.S` | https://elixir.bootlin.com/linux/v6.9/source/arch/arm64/mm/proc.S |
| ASID allocation | `arch/arm64/mm/context.c` | https://elixir.bootlin.com/linux/v6.9/source/arch/arm64/mm/context.c |
| ARM64 pgtable flags | `arch/arm64/include/asm/pgtable.h` | https://elixir.bootlin.com/linux/v6.9/source/arch/arm64/include/asm/pgtable.h |
