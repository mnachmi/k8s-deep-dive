# 04-a — Page Tables: Linux Virtual Memory Deep Dive

## The Memory Illusion

Every process running on a modern Linux system believes it has gigabytes of contiguous memory, starting at address zero. This is a lie maintained by the hardware and the kernel together. Physical memory is shared, fragmented, partially on disk, and addressed in physical page frames that bear no relation to the virtual addresses processes use. The translation between "what the process thinks it has" and "what hardware actually knows about" happens in the page table.

The page table is not a kernel data structure in the traditional sense. It is a hardware-understood data structure that the kernel writes and the CPU's Memory Management Unit reads directly — without kernel involvement — on every memory access. When a process loads from address `0x7ffe3c4a0018`, the MMU walks the page table that `CR3` points to, finds the mapping for that virtual page, and reads from the physical frame it maps to. If there is no mapping — because the page was never faulted in, or was swapped out, or was written with wrong permissions — the MMU raises a page fault exception and the kernel's fault handler takes over.

The fault handler is where the interesting work happens. It is where copy-on-write is implemented (fork creates shared mappings marked read-only; the first write triggers a fault that copies the page). It is where demand paging happens (the kernel allocates physical pages lazily when they are actually touched, not when mmap() is called). It is where memory cgroup charging happens for containers (each container's page faults increment a counter in its cgroup's memory controller). Understanding page tables means understanding where containers actually spend their memory and why `kubectl describe pod` shows different numbers from what `free` says inside the container.

This document covers `struct mm_struct` (the per-process address space), `struct vm_area_struct` (a contiguous virtual memory region), the four-level page table hierarchy (PGD → P4D → PUD → PMD → PTE), and the page fault handler that ties them together.

## Source Locations

| File | Link | Contents |
|------|------|----------|
| `include/linux/mm_types.h` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/mm_types.h | `struct mm_struct`, `struct vm_area_struct` |
| `arch/x86/include/asm/pgtable.h` | https://elixir.bootlin.com/linux/v6.9/source/arch/x86/include/asm/pgtable.h | Page table manipulation macros |
| `arch/x86/include/asm/pgtable_types.h` | https://elixir.bootlin.com/linux/v6.9/source/arch/x86/include/asm/pgtable_types.h | PTE bit definitions |
| `arch/x86/mm/fault.c` | https://elixir.bootlin.com/linux/v6.9/source/arch/x86/mm/fault.c | `do_page_fault()` — the #PF exception handler |
| `mm/memory.c` | https://elixir.bootlin.com/linux/v6.9/source/mm/memory.c | `handle_mm_fault()`, `handle_pte_fault()`, `do_wp_page()` |

---

## `struct mm_struct` — The Virtual Address Space Descriptor

Every process has exactly one `mm_struct`. Threads within the same process share it. The kernel thread that has no user address space has `mm == NULL`. The `task_struct` holds two pointers: `task->mm` (the actual mm, NULL for kernel threads) and `task->active_mm` (the mm currently loaded into CR3, which kernel threads borrow from the last user task for lazy TLB flushing).

```c
// include/linux/mm_types.h (key fields)
struct mm_struct {
    struct {
        struct maple_tree  mm_mt;        // VMA tree (replaces rbtree since Linux 6.1)
        unsigned long      mmap_base;    // base address of mmap region (grows down from top)
        unsigned long      task_size;    // size of user virtual address space
        pgd_t             *pgd;          // Page Global Directory — CR3 points here (physical addr)
        atomic_t           mm_users;     // count of threads sharing this address space
        atomic_t           mm_count;     // count of references to mm_struct itself
        unsigned long      hiwater_rss;  // high-water mark of RSS in pages
        unsigned long      total_vm;     // total virtual memory mapped, in pages
        unsigned long      locked_vm;    // pages pinned by mlock(2)
        unsigned long      pinned_vm;    // pages pinned for DMA (not swappable)
        unsigned long      data_vm;      // pages in private data segments
        unsigned long      exec_vm;      // pages in executable mappings
        unsigned long      stack_vm;     // pages in stack mappings
        unsigned long      start_code, end_code;   // .text segment bounds
        unsigned long      start_data, end_data;   // .data/.bss segment bounds
        unsigned long      start_brk, brk;         // heap: original base and current top
        unsigned long      start_stack;             // initial stack pointer
        struct mm_rss_stat rss_stat;     // RSS broken out by type (anon, file, shmem, swap)
        spinlock_t         page_table_lock; // protects page table modifications (leaf PTEs)
        struct rw_semaphore mmap_lock;   // protects VMA tree (mm_mt) add/remove/lookup
    };
    /* ... many more fields ... */
};
```

### Field-by-Field Explanation

**`mm_mt` — `struct maple_tree`**

The maple tree replaced the interval red-black tree (`mm->mmap` + `mm->mm_rb`) in Linux 6.1 as the primary data structure for tracking VMAs. A maple tree is an order-N B-tree optimized for RCU-safe range queries. Given any virtual address, a lookup in `mm_mt` finds the VMA covering that address in O(log n) time. The kernel walks this tree constantly — on every page fault, every `mmap()`, every `munmap()`. Before 6.1 the tree held `struct vm_area_struct *` values directly; in 6.1+ it uses a `vma_tree` wrapper. Reading `/proc/<pid>/maps` walks the entire `mm_mt` in order.

**`mmap_base` — `unsigned long`**

The base address of the mmap region. By default the kernel maps anonymous regions and file maps downward from near the top of the user address space. `mmap_base` is computed at `exec` time from `task_size` minus a random offset (ASLR). When a process calls `mmap(NULL, ...)`, the kernel searches for a free gap starting from `mmap_base` and working downward.

**`pgd` — `pgd_t *`**

A virtual-space pointer to the process's Page Global Directory. This is the root of the 4-level page table tree. On context switch, the kernel converts this to a physical address (using `__pa()`) and writes it into CR3. The PGD itself is a 4 KiB page containing 512 8-byte entries. The top half of the PGD entries (kernel mappings) are shared across all processes — only the bottom half (user mappings) is per-process.

**`mm_users` vs `mm_count`**

These two counters serve different purposes and are easy to confuse.

`mm_users` (atomic_t) counts the number of users of the address space — effectively, the number of threads (`task_struct` instances) whose `mm` pointer points at this `mm_struct`. A fresh process has `mm_users = 1`. After `pthread_create()`, the new thread shares the same `mm_struct`, raising `mm_users` to 2. When a thread exits (`do_exit()`), it calls `mmput()` which decrements `mm_users`. When `mm_users` reaches zero, the address space is torn down: all VMAs are unmapped, page tables are freed, and memory mappings are destroyed.

`mm_count` (atomic_t) counts references to the `mm_struct` object itself. It includes the `mm_users` reference (the address space holds itself alive as long as there are users), plus additional references from kernel code that needs to keep the struct alive without being a full user — for example, `ptrace(2)`, kernel threads doing lazy TLB work, and certain perf subsystem paths. When `mm_count` reaches zero, `__mmdrop()` frees the `mm_struct` allocation itself. The sequence is always: `mm_users → 0` first (tears down the address space), then `mm_count → 0` (frees the struct).

**`hiwater_rss` — `unsigned long`**

The high-water mark of Resident Set Size, in pages. Updated by `update_hiwater_rss()` whenever RSS rises. Exposed in `/proc/<pid>/status` as `VmPeak` (for virtual) and read by tools like `ps` and `top`. The kernel tracks this separately because RSS can decrease (pages can be reclaimed) but you still want to know the peak.

**`total_vm` — `unsigned long`**

The total number of pages covered by all VMAs, whether those pages are physically present or not. This is the virtual memory size (`VmSize` in `/proc/<pid>/status`). A process can have a huge `total_vm` — for example, a process that `mmap()`s a 1 GiB file maps 262144 pages into `total_vm` — but have a small RSS if most pages have not been faulted in yet. The kernel uses `total_vm` against `RLIMIT_AS` to enforce address space limits.

**`locked_vm` — `unsigned long`**

Pages locked in RAM by `mlock(2)` or `mlockall(2)`, in page units. These pages are marked `VM_LOCKED` in their VMA's `vm_flags` and are excluded from the page reclaim LRU. The kernel enforces `RLIMIT_MEMLOCK` against `locked_vm`. Container runtimes typically set a high `RLIMIT_MEMLOCK` for privileged containers.

**`rss_stat` — `struct mm_rss_stat`**

Tracks Resident Set Size broken out by page type. The four counters are:
- `MM_FILEPAGES` — pages from file-backed mappings currently in RAM
- `MM_ANONPAGES` — anonymous pages (heap, stack, private maps) currently in RAM
- `MM_SWAPENTS` — swap entries (pages currently swapped out)
- `MM_SHMEMPAGES` — shared memory (`tmpfs`/`shmem`) pages in RAM

These are per-CPU counters batched to avoid false sharing. The sum of `MM_FILEPAGES + MM_ANONPAGES + MM_SHMEMPAGES` gives RSS; `MM_SWAPENTS` gives the swap usage. Both are visible in `/proc/<pid>/status` as `VmRSS` and `VmSwap`.

**`page_table_lock` — `spinlock_t`**

A spinlock protecting modifications to the page tables (leaf PTEs). Held when installing or removing PTEs, when doing page table TLB shootdowns, and in the CoW path. For most normal fault-handling paths in recent kernels, the lock is per-VMA or per-page-table-page rather than this global lock, but `page_table_lock` is still used as a fallback for cases where finer-grained locking is not available.

**`mmap_lock` — `struct rw_semaphore`**

The most frequently contested lock in `mm_struct`. It protects the VMA tree (`mm_mt`) and must be held whenever VMAs are added, removed, modified, or looked up for write. `mmap(2)`, `munmap(2)`, `mprotect(2)`, and `mremap(2)` all take the write side. Page fault handlers take the read side (they look up the VMA for a faulting address). Under very high concurrency (thousands of threads faulting simultaneously) `mmap_lock` contention is the dominant bottleneck. The kernel community has ongoing work to replace it with finer-grained locking.

---

## `struct vm_area_struct` — A Single Mapped Region

A VMA describes one contiguous range of a process's virtual address space with uniform permissions and backing. Every segment visible in `/proc/<pid>/maps` is one VMA. A typical process has dozens of VMAs; a Java or Go process with many dynamically loaded libraries can have hundreds.

```c
// include/linux/mm_types.h (key fields)
struct vm_area_struct {
    unsigned long   vm_start;     // first byte of VMA (inclusive)
    unsigned long   vm_end;       // first byte past end of VMA (exclusive)
    struct mm_struct *vm_mm;      // back-pointer to the owning mm_struct
    pgprot_t        vm_page_prot; // hardware page protection bits for this VMA
    unsigned long   vm_flags;     // VM_READ, VM_WRITE, VM_EXEC, VM_SHARED, ...
    const struct vm_operations_struct *vm_ops; // fault/open/close callbacks
    unsigned long   vm_pgoff;     // page offset into the file (for file mappings)
    struct file    *vm_file;      // the mapped file (NULL for anonymous mappings)
    /* VMA tree membership managed by maple_tree in mm_mt */
};
```

### Field-by-Field Explanation

**`vm_start` and `vm_end`**

The virtual address range. `vm_start` is inclusive, `vm_end` is exclusive (following the standard C half-open interval convention). The VMA length is always `vm_end - vm_start`, which is always a multiple of `PAGE_SIZE`. No two VMAs in the same `mm_struct` overlap — the maple tree enforces this.

**`vm_mm`**

Back-pointer to the owning `mm_struct`. This allows code that has a VMA pointer (e.g., a page fault handler that has looked up the VMA in the maple tree) to reach the `mm_struct` without an additional argument.

**`vm_page_prot`**

The hardware-level page protection bits derived from `vm_flags`. This is what actually goes into PTEs when the page is mapped. The translation from `vm_flags` to `vm_page_prot` happens in `vm_get_page_prot()`. The distinction matters because `vm_flags` operates at the VMA level (semantic) while `vm_page_prot` operates at the PTE level (hardware).

**`vm_flags` — VMA Permission and Attribute Bits**

A bitmask controlling the behavior of every page within this VMA. The most important bits:

| Flag | Value | Meaning |
|------|-------|---------|
| `VM_READ` | `0x00000001` | Pages are readable from userspace |
| `VM_WRITE` | `0x00000002` | Pages are writable from userspace |
| `VM_EXEC` | `0x00000004` | Pages contain executable code (maps to NX bit in PTE) |
| `VM_SHARED` | `0x00000008` | Mapping is shared (`MAP_SHARED`); writes visible to other mappings of the same file |
| `VM_MAYWRITE` | `0x00000020` | The VMA _can_ become writable (used with `mprotect()` permission checks) |
| `VM_LOCKED` | `0x00002000` | Pages are `mlock()`d — excluded from page reclaim LRU |
| `VM_HUGETLB` | `0x00400000` | This VMA uses HugeTLB huge pages (2 MiB or 1 GiB on x86_64) |
| `VM_PFNMAP` | `0x00000400` | Raw PFN mapping for device memory — no `struct page` backing; typical for `/dev/mem` and VFIO |

`VM_MAYWRITE` deserves special attention: a VMA can have `VM_READ | VM_EXEC` without `VM_WRITE` (e.g., a code segment mapped from an ELF binary), but still have `VM_MAYWRITE` set so that `mprotect(PROT_WRITE)` can succeed later. Without `VM_MAYWRITE`, `mprotect()` returns `EACCES`. This prevents a readonly file-backed VMA from being made writable by a confined process.

**`vm_ops` — `struct vm_operations_struct *`**

A table of callbacks invoked by the kernel for events on this VMA. The most important operation is `fault()`, called by the page fault handler when a page within this VMA is not present. For anonymous VMAs `vm_ops` is NULL (the fault handler detects this and runs the anonymous allocation path directly). For file-backed VMAs, `vm_ops->fault` reads the appropriate page from the page cache. For special VMAs (device memory, DAX files), `vm_ops->fault` may map hardware memory directly.

**`vm_pgoff`**

For file-backed mappings, the page offset of this VMA within the file. If you `mmap(fd, offset=4096, length=8192)`, the resulting VMA has `vm_pgoff = 1` (4096 / 4096). Page fault handlers use `vm_pgoff + (fault_addr - vm_start) / PAGE_SIZE` to compute which page of the file to read.

**`vm_file`**

A pointer to the `struct file` for file-backed mappings, NULL for anonymous mappings. The file's page cache (`file->f_mapping`, an `address_space`) is where the kernel looks for pages before reading from disk. The VMA's `vm_ops->fault` typically calls `filemap_fault()`, which looks up the page in `file->f_mapping->i_pages` (a `xarray`) and reads it from disk on a cache miss.

---

## Page Table Entry (PTE) Bits on x86_64

Each leaf PTE is an 8-byte value encoding both the physical page frame number and a set of hardware-interpreted status and permission bits. The MMU reads these bits on every memory access.

```
Bit  0: Present      — 1 = page is in RAM; 0 = not present → triggers #PF exception
Bit  1: Writable     — 1 = page can be written; 0 = write triggers #PF (used for CoW)
Bit  2: User         — 1 = accessible from CPL3 (userspace); 0 = kernel-only
Bit  3: PWT          — Page-level Write-Through cache policy
Bit  4: PCD          — Page-level Cache Disable
Bit  5: Accessed     — hardware sets to 1 on any access (read or write); used by LRU
Bit  6: Dirty        — hardware sets to 1 on write; used for writeback decisions
Bit  7: PSE/PAT      — at PMD level: 1 = this is a 2 MiB huge page (Page Size Extension)
                        at PTE level: selects PAT entry for memory type
Bits 8-11: available for software use (kernel uses for pte_special, pte_soft_dirty, etc.)
Bits 12-51: Physical Page Frame Number (PFN) — shifted left 12 gives the physical address
Bit 62: Protection Key — MPK (Memory Protection Keys) index (if CR4.PKE=1)
Bit 63: NX           — No-Execute: 1 = instruction fetch from this page causes #PF
```

Source: [arch/x86/include/asm/pgtable_types.h](https://elixir.bootlin.com/linux/v6.9/source/arch/x86/include/asm/pgtable_types.h)

The key insight is that bit 0 (Present) is what drives demand paging. The kernel maps virtual addresses into VMAs without installing PTEs. When userspace first accesses the address, the hardware finds no PTE with Present=1 and raises a #PF exception. The kernel's fault handler then allocates a page and installs the PTE with Present=1. This is demand paging: physical memory is only committed when actually accessed.

Bit 1 (Writable) is the CoW bit. After `fork()`, the kernel marks all writable pages in both parent and child PTEs with Writable=0 (even though `vm_flags` still has `VM_WRITE`). The first write attempt raises a #PF, the handler detects the CoW condition, allocates a new page, copies the content, and installs a fresh PTE with Writable=1 for the writing process only.

Bit 5 (Accessed) and Bit 6 (Dirty) are written by the hardware MMU without any software intervention. The page reclaim subsystem (`mm/vmscan.c`) reads the Accessed bit to implement a clock-based LRU approximation: pages not recently accessed are candidates for reclaim. The writeback subsystem reads the Dirty bit to find pages that must be flushed to disk before the physical frame can be reused.

---

## Page Fault Handling — The Demand Paging Path

The page fault handler is the heart of demand paging. Every first access to a memory region, every stack growth, every mmap'd file page read, and every swap-in flows through this path.

```
CPU: access to virtual address with no valid PTE
  → #PF hardware exception
  → exc_page_fault()                   # arch/x86/mm/fault.c (entry point from IDT)
       └─ do_user_addr_fault()         # arch/x86/mm/fault.c
            ├─ mmap_read_lock(mm)      # acquire mmap_lock read side
            ├─ find_vma(mm, addr)      # look up VMA in mm_mt
            ├─ [addr not in any VMA] → SIGSEGV
            └─ handle_mm_fault()       # mm/memory.c — architecture-independent path
                 ├─ [huge page VMA?] → hugetlb_fault() or create_huge_pmd()
                 └─ handle_pte_fault() # mm/memory.c
                      ├─ [PTE not present, anonymous VMA] → do_anonymous_page()
                      │    └─ alloc_zeroed_user_highpage_movable()
                      │         └─ set_pte_at()   # install PTE with Present=1
                      ├─ [PTE not present, file-backed VMA] → do_fault()
                      │    └─ __do_fault() → vm_ops->fault() → filemap_fault()
                      │         ├─ [page in page cache] → set_pte_at()
                      │         └─ [page not cached] → read from disk → set_pte_at()
                      └─ [PTE present but Writable=0 on write] → do_wp_page()
                           # CoW path — see next section
```

### Fault Type 1: Anonymous Page Fault

An anonymous mapping has no file backing. Heap pages (allocated by `brk(2)` or `mmap(MAP_ANONYMOUS)`), stack pages, and private mappings after modification are all anonymous.

When the fault fires, `handle_pte_fault()` sees an absent PTE and a VMA with `vm_file == NULL`. It calls `do_anonymous_page()`:

1. `alloc_zeroed_user_highpage_movable()` — allocates a fresh 4 KiB page, zeroed (required by POSIX to prevent information leaks between processes). The allocator prefers `ZONE_HIGHMEM` or `ZONE_MOVABLE` pages for user allocations.
2. `set_pte_at()` — installs the PTE with `Present=1`, `User=1`, and `Writable=1` (if the VMA is writable). The PTE's PFN field is set to the new page's frame number.
3. `update_mmu_cache()` — optional arch hook to update software TLB structures.

The process resumes at the faulting instruction, which now succeeds.

### Fault Type 2: File-Backed Page Fault

A file-backed VMA (`vm_file != NULL`, created by `mmap(fd, MAP_SHARED|MAP_PRIVATE)`) backs pages from a file's page cache.

`handle_pte_fault()` calls `do_fault()`, which calls `vm_ops->fault()`. For regular files this is `filemap_fault()` in `mm/filemap.c`:

1. Look up `vm_pgoff + page_index` in the file's `address_space` (`file->f_mapping`).
2. If the page is in the page cache (`find_get_page()` succeeds): map it with `set_pte_at()` directly.
3. If not in cache: call `page_cache_read()` to read from the block device via `address_space->a_ops->readpage()`. This is the major page fault — it involves I/O and blocks the faulting process until the page arrives.

A major fault increments `pgmajfault` in `/proc/vmstat` and `/proc/<pid>/stat`.

### Fault Type 3: Swap Fault

If the PTE is "not present" but its value is non-zero, it contains a swap entry: a encoding of the swap device and the page's slot within it. This is a page that was once present but was swapped out by the reclaimer.

`handle_pte_fault()` calls `do_swap_page()`:

1. Decode the swap entry (`swp_type()`, `swp_offset()`) to identify the swap device and slot.
2. Check the swap cache (`swapcache_get_page()`) — pages being swapped in are temporarily in a swap cache to handle concurrent faults for the same page.
3. Read the page from swap via `swap_readpage()`.
4. Install the PTE with Present=1, removing the swap entry.
5. Decrement the swap map reference count (`swap_free()`).

---

## Copy-on-Write (CoW) — Efficient `fork()`

Copy-on-Write is the mechanism that makes `fork()` cheap. Instead of copying the entire address space at fork time, the kernel shares pages between parent and child, deferring the actual copy until a write occurs.

### The Fork Path

When `fork()` calls `copy_mm()` → `dup_mm()` → `copy_page_range()`:

1. A new `mm_struct` is allocated for the child.
2. `copy_page_range()` walks the parent's page tables and copies each PTE entry into the child's page table — but marks both parent's and child's PTEs as Writable=0, even for VMAs with `VM_WRITE`.
3. Each shared page's reference count is incremented (`get_page()`).

At this point, parent and child have independent page tables that point to the same physical pages. Both see the correct data. Neither can write without triggering a fault.

### The Write Fault Path

The first write to a shared page by either parent or child raises a #PF because Writable=0 in the PTE. `handle_pte_fault()` detects this as a write-protection fault (PTE present but not writable, VMA has `VM_WRITE`):

```
CoW write fault → #PF (Writable=0 in PTE, Present=1)
  → handle_pte_fault()
       └─ do_wp_page()                    # mm/memory.c
            ├─ [page_count(page) == 1]    # only one reference — safe to reuse
            │    └─ wp_page_reuse()       # just set Writable=1 in PTE, no copy needed
            └─ [page_count(page) > 1]     # shared — must copy
                 └─ wp_page_copy()
                      ├─ alloc_page()     # allocate new page
                      ├─ copy_user_highpage(new, old, addr, vma)  # copy content
                      ├─ set_pte_at()     # install new PTE (Writable=1, new PFN)
                      └─ put_page(old)    # drop reference on old page
```

The optimization in `wp_page_reuse()` handles the case where the "last" reference is this process itself — the other party (parent or child) has already done its own CoW copy. In that case no data movement is needed; the kernel simply sets Writable=1 in the existing PTE.

This design has a critical performance consequence for containers: a container that calls `fork()` and immediately `exec()`s a child (the `posix_spawn` / shell pipeline pattern) pays almost no memory cost for the fork, because `exec()` tears down the shared address space before any CoW copy is triggered.

---

## Live Observation

### Viewing the VMA Layout of a Process

```bash
# One VMA per line: start-end perms offset dev:inode path
# perms: r=read, w=write, x=exec, s=shared, p=private (CoW)
cat /proc/$PID/maps
```

Example output for a simple C program:
```
55a3c8000000-55a3c8001000 r--p 00000000 08:01 1234567  /usr/bin/cat  # .rodata
55a3c8001000-55a3c8002000 r-xp 00001000 08:01 1234567  /usr/bin/cat  # .text
55a3c8002000-55a3c8003000 r--p 00002000 08:01 1234567  /usr/bin/cat  # .rodata
55a3c8003000-55a3c8004000 r--p 00002000 08:01 1234567  /usr/bin/cat  # .data (CoW)
55a3c8004000-55a3c8005000 rw-p 00003000 08:01 1234567  /usr/bin/cat  # .bss
55a3c9e00000-55a3ca021000 rw-p 00000000 00:00 0        [heap]
7f8b1e000000-7f8b1e021000 rw-p 00000000 00:00 0        [anon]
7ffca3a00000-7ffca3c00000 rw-p 00000000 00:00 0        [stack]
```

```bash
# Detailed per-VMA stats — Size, Rss, Pss, Anonymous, Swap, etc.
cat /proc/$PID/smaps
```

Key fields in `smaps`:
- `Size`: virtual size of the VMA in KiB
- `Rss`: resident pages (physically present) in KiB
- `Pss`: Proportional Set Size — RSS divided by the number of processes sharing each page
- `Shared_Clean`, `Shared_Dirty`: shared pages (not modified / modified)
- `Private_Clean`, `Private_Dirty`: private pages (CoW or anonymous)
- `Anonymous`: pages with no file backing
- `Swap`: pages currently swapped out

```bash
# Compact summary of all VMAs with PSS
cat /proc/$PID/smaps_rollup
```

### System-Wide Page Statistics

```bash
# Page fault and swap counters — all values are cumulative since boot
cat /proc/vmstat | grep -E "pgfault|pgmajfault|pswpin|pswpout|pgsteal|pgscan"
```

Key counters:
- `pgfault` — total minor page faults (anonymous or page cache hit)
- `pgmajfault` — major page faults (required disk I/O)
- `pswpin` / `pswpout` — pages swapped in / out
- `pgsteal_kswapd` — pages reclaimed by kswapd
- `pgscan_kswapd` — pages scanned by kswapd LRU

### bpftrace Probes

```bash
# Count page faults per process (minor + major combined)
bpftrace -e 'kprobe:handle_mm_fault {
    @faults[comm] = count();
}'

# Count CoW faults per process
bpftrace -e 'kprobe:do_wp_page {
    @cow_faults[comm] = count();
}'

# Count anonymous page allocations per process
bpftrace -e 'kprobe:do_anonymous_page {
    @anon_faults[comm] = count();
}'

# Measure time spent in page fault handler (latency histogram per process)
bpftrace -e '
kprobe:handle_mm_fault { @start[tid] = nsecs; }
kretprobe:handle_mm_fault
/@start[tid]/
{
    @fault_ns[comm] = hist(nsecs - @start[tid]);
    delete(@start[tid]);
}'

# Trace swap-in events (major fault from swap)
bpftrace -e 'kprobe:do_swap_page {
    printf("swap-in: pid=%d comm=%s\n", pid, comm);
}'
```

---

## Key Kernel References

| Symbol | File | Link | Purpose |
|--------|------|------|---------|
| `struct mm_struct` | `include/linux/mm_types.h` | [link](https://elixir.bootlin.com/linux/v6.9/source/include/linux/mm_types.h) | Virtual address space descriptor |
| `struct vm_area_struct` | `include/linux/mm_types.h` | [link](https://elixir.bootlin.com/linux/v6.9/source/include/linux/mm_types.h) | Single mapped region descriptor |
| `pgd_t`, `pud_t`, `pmd_t`, `pte_t` | `arch/x86/include/asm/pgtable_types.h` | [link](https://elixir.bootlin.com/linux/v6.9/source/arch/x86/include/asm/pgtable_types.h) | Page table entry types and bit definitions |
| `exc_page_fault()` | `arch/x86/mm/fault.c` | [link](https://elixir.bootlin.com/linux/v6.9/source/arch/x86/mm/fault.c) | x86 #PF exception entry point |
| `do_user_addr_fault()` | `arch/x86/mm/fault.c` | [link](https://elixir.bootlin.com/linux/v6.9/source/arch/x86/mm/fault.c) | User-space fault dispatcher |
| `handle_mm_fault()` | `mm/memory.c` | [link](https://elixir.bootlin.com/linux/v6.9/source/mm/memory.c) | Architecture-independent fault top-level |
| `handle_pte_fault()` | `mm/memory.c` | [link](https://elixir.bootlin.com/linux/v6.9/source/mm/memory.c) | Fault type classifier (anon / file / swap / CoW) |
| `do_anonymous_page()` | `mm/memory.c` | [link](https://elixir.bootlin.com/linux/v6.9/source/mm/memory.c) | Allocate and map an anonymous page |
| `do_fault()` | `mm/memory.c` | [link](https://elixir.bootlin.com/linux/v6.9/source/mm/memory.c) | File-backed fault dispatcher |
| `do_swap_page()` | `mm/memory.c` | [link](https://elixir.bootlin.com/linux/v6.9/source/mm/memory.c) | Swap-in a page from the swap device |
| `do_wp_page()` | `mm/memory.c` | [link](https://elixir.bootlin.com/linux/v6.9/source/mm/memory.c) | Copy-on-Write write-protection fault handler |
| `filemap_fault()` | `mm/filemap.c` | [link](https://elixir.bootlin.com/linux/v6.9/source/mm/filemap.c) | Read a page from the file page cache |
| `copy_page_range()` | `mm/memory.c` | [link](https://elixir.bootlin.com/linux/v6.9/source/mm/memory.c) | Fork: copy page tables and set CoW bits |
| `find_vma()` | `mm/mmap.c` | [link](https://elixir.bootlin.com/linux/v6.9/source/mm/mmap.c) | Look up a VMA by virtual address in mm_mt |
| `mmput()` | `kernel/fork.c` | [link](https://elixir.bootlin.com/linux/v6.9/source/kernel/fork.c) | Decrement mm_users; tear down address space at zero |
| `__mmdrop()` | `kernel/fork.c` | [link](https://elixir.bootlin.com/linux/v6.9/source/kernel/fork.c) | Free mm_struct when mm_count reaches zero |
