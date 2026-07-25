# page-fault-demo — demand paging and madvise in C

A C program that allocates memory in five different ways and measures page faults and RSS growth at each step, making the kernel's demand-paging machinery directly observable from userspace.

## What It Demonstrates

- **Demand paging**: `malloc` reserves virtual address space but triggers zero page faults until the memory is written — the kernel maps physical pages lazily
- **Anonymous mmap**: `mmap(MAP_ANONYMOUS)` follows the same demand-paging path; pages are faulted in on first touch, not at `mmap` time
- **Transparent Huge Pages (THP)**: `MADV_HUGEPAGE` hints to the kernel that 2 MiB pages are preferred; `khugepaged` may promote 4 KiB pages into a 2 MiB page after the region is touched
- **Page release via MADV_DONTNEED**: instructs the kernel to unmap and reclaim physical pages while keeping virtual address space intact; RSS drops immediately; re-touching the region faults in fresh zero pages again
- **Fault counting via `/proc/self/stat`**: field 10 (`minflt`) increments for every demand-paging fault served from RAM (no disk I/O required)

## Build and Run

```bash
make run
```

No root required. Runs on any Linux with `/proc` mounted (standard everywhere).

## Expected Output

```
=== Page Fault Demo ===
Allocation size: 64 MiB

[1] malloc 64 MiB (no touch)
  after malloc (no touch)                 faults=3       RSS delta=68 KiB
[2] touch all pages (memset)
  after memset (all pages touched)        faults=16384   RSS delta=65536 KiB
[3] mmap(MAP_ANONYMOUS) 64 MiB + touch
  after mmap anon + touch                 faults=16384   RSS delta=65536 KiB
[4] mmap + MADV_HUGEPAGE (THP hint) + touch
  after mmap + MADV_HUGEPAGE + touch      faults=16384   RSS delta=65536 KiB
[5] mmap + touch + MADV_DONTNEED (release pages) + re-touch
  RSS after touch:         67272 KiB
  RSS after MADV_DONTNEED: 1736 KiB
  Pages released: ~65536 KiB
  re-touch after MADV_DONTNEED            faults=16384   RSS delta=65536 KiB

Note: minor faults = demand paging from RAM (zero-fill, CoW)
      major faults = disk reads (swap-in, file read)
      check /proc/self/stat fields 10 (minflt) and 12 (majflt)
```

Key numbers: 64 MiB / 4096 bytes per page = 16,384 pages, so each full-touch demo produces ~16,384 minor faults and ~65,536 KiB RSS growth. Demo 1 shows near-zero faults because `malloc` only adjusts `brk` metadata — no physical pages are allocated.

## Kernel Paths

**Demo 1 — malloc (no touch)**
`malloc` calls `brk(2)` or `mmap(2)` to expand the process heap. The kernel updates `mm->brk` and the VMA tree (`vm_area_struct`) but does not allocate any physical pages. The page table entries remain absent. No page fault handler is invoked.

**Demo 2 — memset after malloc**
Each first write to a 4 KiB page that has no PTE triggers a page fault. The CPU delivers a `#PF` exception; the kernel fault handler (`handle_mm_fault` in `mm/memory.c`) walks the VMA tree, identifies an anonymous mapping with no backing page, and calls `do_anonymous_page()`. That function allocates a zeroed page from the page allocator, installs a PTE, and returns. This repeats 16,384 times for 64 MiB — one fault per 4 KiB page.

**Demo 3 — mmap anonymous + touch**
`mmap(MAP_ANONYMOUS)` creates a new VMA but again installs no PTEs. The page-fault path on first write is identical to Demo 2: `handle_mm_fault` → `do_anonymous_page()` in `mm/memory.c`. The observable fault count is the same as Demo 2.

**Demo 4 — MADV_HUGEPAGE + touch**
`madvise(MADV_HUGEPAGE)` sets the `VM_HUGEPAGE` flag on the VMA (`do_madvise` → `madvise_vma_behavior` in `mm/madvise.c`). The initial page faults still go through `do_anonymous_page()` at 4 KiB granularity. After the region is touched, the `khugepaged` kernel thread scans for eligible VMAs and, when 512 contiguous 4 KiB pages are present, calls `collapse_huge_page()` in `mm/khugepaged.c` to promote them into a single 2 MiB huge page. This promotion happens asynchronously; the fault count during the touch loop reflects 4 KiB pages regardless.

**Demo 5 — MADV_DONTNEED + re-touch**
`madvise(MADV_DONTNEED)` invokes `do_madvise` → `madvise_dontneed_single_vma` → `zap_page_range_single` in `mm/madvise.c`. `zap_page_range_single` walks the page tables and calls `zap_pte_range`, which clears each PTE and releases the backing physical page back to the page allocator — all without removing the VMA. RSS drops by the full 64 MiB because every physical page is freed. When the region is touched again, each access faults through `do_anonymous_page()` exactly as in Demo 2, producing another 16,384 minor faults.

## Exercises

a. **Copy-on-Write faults**: add a `fork()` after the `memset` in Demo 2. In both parent and child, read `get_minor_faults()` before and after writing a single byte to a shared page. Observe that each write in either process produces a CoW fault (the kernel copies the page via `do_wp_page()` in `mm/memory.c` before giving the writer an exclusive copy).

b. **Verify THP promotion**: after Demo 4, read `/proc/self/smaps` and `grep -A5 "page_fault_demo" /proc/self/smaps` or inspect `/proc/<pid>/smaps_rollup`. Look for `AnonHugePages` being non-zero. Note that promotion may take a few hundred milliseconds after `khugepaged` next runs; add a `sleep(2)` before reading smaps if necessary.

c. **Touch granularity vs fault count**: modify Demo 3 to use a 2 MiB stride (`i += 2 * 1024 * 1024`) instead of 4 KiB. Count faults with `get_minor_faults()`. Observe that you get only 32 faults (one per 2 MiB touched page) but RSS grows only by the pages actually faulted in — the remaining pages in each 2 MiB block stay unmapped until first touch.

## K8s Connection

`MADV_DONTNEED` is the mechanism by which Go's memory scavenger (`runtime.(*mheap).scavenge`, called after GC) and glibc `malloc` (for allocations above `MMAP_THRESHOLD`, typically 128 KiB) return physical pages to the kernel after freeing heap memory. This is why a Go container's `memory.current` (the cgroup v2 counter) drops measurably after a garbage collection cycle — the virtual address space reported by `VmSize` does not shrink, but physical pages are released via `MADV_DONTNEED`, and the kernel removes them from the cgroup's charge. Operators who see a container's memory "spike then drop" after a GC are observing exactly the behavior demonstrated in Demo 5.
