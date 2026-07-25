# Chapter 04 — Memory Management

Linux memory management is the largest kernel subsystem. Every container's memory isolation, every OOM kill, every HugePage allocation passes through it. Kubernetes's eviction manager, resource limits, and Topology Manager are all thin wrappers over kernel memory primitives. This chapter teaches the kernel implementation bottom-up: from physical pages to virtual address spaces to pressure metrics.

## Learning Objectives

By the end of this chapter you will be able to:

1. Navigate `/proc/vmstat` and `/proc/<pid>/smaps` and explain what every field means in terms of kernel data structures.
2. Explain the 4-level page table walk on x86_64, from the CR3 register through PGD → PUD → PMD → PTE to a physical page frame.
3. Trace an OOM kill from the moment a page allocation fails through the OOM killer's victim-selection algorithm to the delivery of SIGKILL.
4. Read PSI (Pressure Stall Information) metrics per cgroup and correlate them to in-flight eviction decisions by the Kubernetes eviction manager.
5. Understand NUMA topology — how the kernel tracks NUMA nodes, distances, and memory zones — and how Kubernetes pins workloads to NUMA nodes via the Topology Manager.

## Prerequisites

- **Chapter 01** — familiarity with `task_struct`, the `mm` pointer, and the basic process model. You will need to understand that every process has an `mm_struct` pointer (`task->mm`) that represents its virtual address space.
- **Chapter 03** — cgroups memory controller and `memory.max` enforcement. The page allocator interacts deeply with the memory cgroup subsystem to charge page allocations and trigger reclaim when a cgroup reaches its limit.

## Virtual → Physical Address Translation

Understanding page tables requires understanding how a virtual address is decoded. On x86_64 with 4-level paging (the configuration used by most Linux deployments):

```
Virtual address (48-bit canonical, x86_64):
  [63:48] sign extension  — must replicate bit 47 (canonical check)
  [47:39] PGD index (9 bits)  → Page Global Directory
  [38:30] P4D index (9 bits)  → Page 4th-level Directory  (folded on most configs)
  [29:21] PUD index (9 bits)  → Page Upper Directory
  [20:12] PMD index (9 bits)  → Page Middle Directory
  [11: 0] page offset (12 bits) → byte within 4 KiB page

CR3 register → PGD (per-process) → P4D → PUD → PMD → PTE → physical page
```

The CR3 register holds the physical address of the per-process Page Global Directory. On every context switch, the kernel writes the new process's `mm->pgd` (converted to a physical address) into CR3. The MMU then uses CR3 as the root of all address translations for that process.

P4D is folded (effectively a no-op pass-through) on configurations that do not use 5-level paging (`CONFIG_X86_5LEVEL`). 5-level paging extends the virtual address space from 48 to 57 bits by adding a real P4D level, but is not yet the default on most deployments.

Each level of the table is a 4 KiB page containing 512 8-byte entries (512 × 8 = 4096). The 9-bit index at each level selects one of those 512 entries. The entry holds the physical address of the next-level table (or the page frame itself at the leaf PTE), plus permission and status bits.

## Reading Order

Work through the files in this order. Each document builds on the previous.

| File | Topic |
|------|-------|
| `kernel/04-a-page-tables.md` | `mm_struct`, VMAs, PTE bits, page fault path, copy-on-write |
| `kernel/04-b-page-allocator.md` | Buddy allocator, zones, `alloc_pages()`, `__get_free_pages()` |
| `kernel/04-c-oom-psi.md` | OOM killer internals, PSI metrics, cgroup memory pressure |
| `kernel/04-d-numa.md` | NUMA nodes, memory policies, `mbind()`, Topology Manager |
| `k8s/04-k8s-connection.md` | How Kubernetes resource limits, eviction, and HugePages map to kernel primitives |
| `exercises/page-fault-demo/` | C program that triggers anonymous, file-backed, and CoW faults — trace them live with bpftrace |
| `exercises/psi-reader/` | Go program that reads per-cgroup PSI and correlates to Kubernetes eviction thresholds |
