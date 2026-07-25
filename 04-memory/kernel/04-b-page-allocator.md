# Physical Page Allocator, Slab, and LRU — Linux Memory Internals

## Source Locations

| File | Link | Contents |
|------|------|----------|
| `include/linux/mm_types.h` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/mm_types.h | `struct page` — physical page descriptor |
| `mm/page_alloc.c` | https://elixir.bootlin.com/linux/v6.9/source/mm/page_alloc.c | Buddy allocator — `alloc_pages()`, zone management |
| `include/linux/gfp_types.h` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/gfp_types.h | GFP flag definitions |
| `mm/slub.c` | https://elixir.bootlin.com/linux/v6.9/source/mm/slub.c | SLUB allocator — `kmem_cache_alloc()`, per-CPU slabs |
| `include/linux/slub_def.h` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/slub_def.h | `struct kmem_cache`, `struct kmem_cache_cpu` |
| `mm/vmscan.c` | https://elixir.bootlin.com/linux/v6.9/source/mm/vmscan.c | Page reclaim — `kswapd`, `shrink_node()`, `shrink_inactive_list()` |

---

## `struct page` — The Physical Page Descriptor

Every physical page frame tracked by the kernel has a corresponding `struct page` in the `mem_map` array. This struct is the most memory-dense data structure in the kernel: it must fit as much information as possible into as few bytes as possible, because the overhead is multiplied by the total number of physical pages.

```c
// include/linux/mm_types.h (key fields — simplified for clarity)
struct page {
    unsigned long flags;        // PG_locked, PG_dirty, PG_uptodate, PG_lru, ...

    union {
        /* ── Page cache and anonymous pages ── */
        struct {
            struct list_head lru;        // LRU list linkage (active or inactive list)
            struct address_space *mapping; // page cache owner, or anon_vma for anon pages
            pgoff_t index;               // offset within the mapping (in pages)
            unsigned long private;       // private data (e.g., buffer_heads for block I/O)
        };

        /* ── Slab allocator (SLUB) ── */
        struct {
            struct list_head slab_list;   // partial/full slab list linkage
            struct kmem_cache *slab_cache; // which cache owns this slab
            void *freelist;               // pointer to first free object in slab
            union {
                void        *s_mem;       // first object in the slab
                unsigned long counters;   // inuse + objects + frozen packed into one word
            };
        };

        /* ── Page table pages ── */
        struct {
            unsigned long _pt_pad_1;
            pgtable_t pmd_huge_pte;       // used by x86 huge page PMD tracking
            unsigned long _pt_pad_2;
            union {
                struct mm_struct *pt_mm;        // owning mm for top-level page tables
                atomic_t pt_frag_refcount;      // fragment refcount for page table pages
            };
            spinlock_t *ptl;              // page table lock pointer
        };
    };

    atomic_t _refcount;   // reference count; 0 = free, >0 = in use
    atomic_t _mapcount;   // number of PTEs mapping this page (-1 = not mapped into any PTE)
};
```

Source: [include/linux/mm_types.h](https://elixir.bootlin.com/linux/v6.9/source/include/linux/mm_types.h)

### Memory Cost of `struct page`

On x86_64, `struct page` is exactly **64 bytes**. The cost scales linearly with installed RAM:

| RAM | Pages (4 KiB each) | `mem_map` size |
|-----|---------------------|----------------|
| 4 GiB | 1,048,576 | 64 MiB |
| 16 GiB | 4,194,304 | 256 MiB |
| 128 GiB | 33,554,432 | 2 GiB |
| 1 TiB | 268,435,456 | 16 GiB |

A server with 1 TiB of RAM devotes 16 GiB purely to page descriptors — 1.6% of total RAM. This is why the struct is so aggressively packed and why the union trick is essential.

### Why the Union Exists

A physical page frame can serve exactly one purpose at any given time: it is either a page-cache or anonymous page, a slab holding kernel objects, or a page table page. These three roles need completely different metadata. Rather than allocating separate structs (which would require pointer indirection and worsen cache behavior), the kernel overlays all three sets of fields in the same 64 bytes. The `flags` field encodes the current role via `PG_slab`, and high-level allocators enforce the invariant that the right union member is accessed.

### `flags` Field — Individual Bits

The `flags` field is an `unsigned long` (64 bits on x86_64). The kernel defines named constants for each bit in `include/linux/page-flags.h`. The most important bits are:

| Flag | Meaning |
|------|---------|
| `PG_locked` | Page is locked for I/O — other accesses must wait. Set during disk read/write. |
| `PG_dirty` | Page has been written to but not yet flushed to backing store. Writeback will clear it. |
| `PG_uptodate` | Page contents are valid and consistent with the backing file. Set after a successful read. |
| `PG_lru` | Page is on one of the LRU lists (active or inactive). Used by the reclaimer. |
| `PG_active` | Page is on the active LRU list. Cleared when the page is demoted to inactive. |
| `PG_referenced` | Page has been accessed recently. Used as a second-chance bit before demotion. |
| `PG_reclaim` | Page has been selected for reclaim and is being written back. |
| `PG_slab` | Page is owned by the slab allocator (SLUB). Distinguishes slab pages from page-cache pages. |
| `PG_head` | This page is the head of a compound page (e.g., a THP or HugeTLB page). |
| `PG_tail` | This page is a tail page within a compound page — it is not independently usable. |

### `_refcount` and `_mapcount`

`_refcount` (atomic_t) tracks how many kernel subsystems hold a reference to the page. The buddy allocator initialises it to 1 on allocation. When it drops to 0, the page is returned to the free pool. Code that wants to keep a page alive calls `get_page()` (which increments `_refcount`) and releases with `put_page()`. The page cache, slab, and file I/O paths all participate in refcounting.

`_mapcount` (atomic_t) counts how many PTEs across all processes currently point at this page. It starts at -1 (meaning: no PTEs, not mapped anywhere). Each PTE install increments it; each PTE removal decrements it. When `_mapcount == -1`, the page has no user-space mappings and the reclaimer can free it without needing to unmap anything. When `_mapcount > 0`, the reclaimer must first walk the reverse-mapping structure (`anon_vma` or the page cache's `i_mmap` interval tree) to remove all PTEs before freeing.

---

## The Buddy Allocator

The buddy allocator is the foundational physical memory allocator in Linux. It manages all free pages and is the source from which all higher-level allocators (slab, vmalloc, direct mappings) ultimately draw memory.

Source: [mm/page_alloc.c](https://elixir.bootlin.com/linux/v6.9/source/mm/page_alloc.c)

### Orders and Block Sizes

The buddy system divides memory into power-of-two blocks called "orders". Order N means a contiguous block of 2^N pages:

| Order | Pages | Size |
|-------|-------|------|
| 0 | 1 | 4 KiB |
| 1 | 2 | 8 KiB |
| 2 | 4 | 16 KiB |
| 3 | 8 | 32 KiB |
| 4 | 16 | 64 KiB |
| 5 | 32 | 128 KiB |
| 6 | 64 | 256 KiB |
| 7 | 128 | 512 KiB |
| 8 | 256 | 1 MiB |
| 9 | 512 | 2 MiB |
| 10 | 1024 | 4 MiB |

Each memory zone (DMA, DMA32, Normal, Highmem) maintains 11 free lists, one per order.

### `/proc/buddyinfo`

`/proc/buddyinfo` shows how many free blocks of each order exist per zone:

```
Node 0, zone      DMA:  1  1  0  0  0  0  0  0  0  0  0
Node 0, zone    DMA32: 63 48 24 13  6  3  2  1  1  0  0
Node 0, zone   Normal: 120 45 23 12  8  4  2  1  1  0  0
                order:   0   1   2   3  4  5  6  7  8  9 10
```

Each number is a count of free blocks at that order. The Normal zone in this example has 120 free order-0 blocks (480 KiB total) and 1 free order-8 block (1 MiB). When the counts at high orders are near zero, the system suffers from fragmentation: plenty of total free memory may exist in small disconnected chunks, but large contiguous allocations will fail.

### Allocation: `alloc_pages(gfp_flags, order)`

```
alloc_pages(GFP_KERNEL, 0)          // allocate one 4 KiB page
  └─ __alloc_pages()                 // mm/page_alloc.c
       ├─ get_page_from_freelist()   // fast path: check per-zone free lists
       │    └─ [found order-N block where N >= requested order]
       │         └─ expand():        // split the larger block, return buddies to lower lists
       │              // e.g., order-3 block → satisfy order-0, return order-1, order-2 free blocks
       └─ __alloc_pages_slowpath()   // slow path: reclaim, compaction, OOM
            ├─ wake_all_kswapds()    // ask kswapd to reclaim
            ├─ try_to_free_pages()   // direct reclaim if needed
            └─ page_alloc_extfrag_bypass() // steal from other migratetype freelist
```

When a larger block is split to satisfy a smaller request, the unused halves ("buddies") are placed on the appropriate lower-order free list. When two adjacent same-order buddies are both free, they coalesce back into a higher-order block. This coalescing is what makes the buddy system efficient at reducing fragmentation over time.

### GFP Flags

GFP (Get Free Pages) flags control how `alloc_pages()` behaves when memory is tight. They are defined in [include/linux/gfp_types.h](https://elixir.bootlin.com/linux/v6.9/source/include/linux/gfp_types.h):

| Flag | Meaning |
|------|---------|
| `GFP_KERNEL` | Standard allocation for kernel code. May sleep, may trigger page reclaim, may call the OOM killer. The correct default for most kernel subsystems. |
| `GFP_ATOMIC` | Cannot sleep; must not block. Used from interrupt handlers, spinlock-held contexts, and NMI context. Falls back to memory reserves rather than waiting for reclaim. |
| `GFP_USER` | Allocate on behalf of a user process. Uses `ZONE_NORMAL` or `ZONE_HIGHMEM`, marks the page as movable for memory compaction. |
| `GFP_DMA` | Restrict allocation to `ZONE_DMA` (the low 16 MiB of physical memory). Required for devices that can only DMA into the first 16 MiB. Rare on modern hardware. |
| `GFP_HIGHUSER` | Like `GFP_USER` but additionally marks pages as `MIGRATE_MOVABLE`. Used by the page fault handler for anonymous user pages — movable pages can be relocated during compaction to create larger contiguous free regions. |
| `__GFP_ZERO` | Clear the allocated page(s) before returning. Combined with other flags: `alloc_pages(GFP_KERNEL | __GFP_ZERO, 0)`. Required for user-facing pages to prevent information leaks. |

Flags compose via bitwise OR. `GFP_KERNEL` is itself a composite of `__GFP_RECLAIM | __GFP_IO | __GFP_FS`, meaning: reclaim is allowed, block I/O is allowed, filesystem operations are allowed. `GFP_ATOMIC` clears all of these, which is why atomic allocations are more likely to fail under memory pressure.

---

## The Slab Allocator (SLUB)

The buddy allocator is efficient for page-granularity allocations but wasteful for the small, frequently-allocated kernel objects that dominate kernel memory usage: `task_struct`, `inode`, `dentry`, `sk_buff`, `file`, `vm_area_struct`. These range from tens to hundreds of bytes. Allocating a full page (4 KiB) for each would waste most of it.

SLUB (the default slab allocator since Linux 2.6.23) solves this by carving pages into fixed-size objects and recycling them through per-CPU free lists.

Source: [mm/slub.c](https://elixir.bootlin.com/linux/v6.9/source/mm/slub.c)

### `struct kmem_cache` — A Cache of Fixed-Size Objects

Each distinct object type has its own `struct kmem_cache`. The kernel creates dedicated caches for types like `task_struct_cachep`, `inode_cache`, and `dentry`. `kmalloc()` uses a set of generic size-class caches (kmalloc-8, kmalloc-16, ..., kmalloc-8192).

```c
// include/linux/slub_def.h (key fields)
struct kmem_cache {
    struct kmem_cache_cpu __percpu *cpu_slab; // per-CPU slab (the fast path)
    unsigned long  flags;          // SLAB_HWCACHE_ALIGN, SLAB_RECLAIM_ACCOUNT, ...
    unsigned long  min_partial;    // minimum partial slabs to keep on the node list
    unsigned int   size;           // allocated size per object (includes SLUB metadata)
    unsigned int   object_size;    // actual requested object size (no metadata)
    unsigned int   offset;         // byte offset of the free-pointer within a free object
    unsigned int   cpu_partial;    // max partial slabs to keep on the per-CPU partial list
    struct kmem_cache_order_objects oo;  // preferred slab order + objects per slab (packed)
    gfp_t          allocflags;     // GFP flags used when requesting pages from buddy
    int            refcount;       // number of kmalloc size-class aliases to this cache
    void (*ctor)(void *);          // constructor called on each new object (can be NULL)
    const char    *name;           // name shown in /proc/slabinfo and slabtop
    struct list_head list;         // global list of all kmem_cache instances
};
```

Source: [include/linux/slub_def.h](https://elixir.bootlin.com/linux/v6.9/source/include/linux/slub_def.h)

**Key field explanations:**

`cpu_slab` is a `__percpu` pointer to a `struct kmem_cache_cpu` per logical CPU. Each `kmem_cache_cpu` holds a pointer to the active slab page and a `freelist` pointer to the next free object in that slab. Allocations from the fast path read and write only this per-CPU structure, requiring no locking at all.

`size` vs `object_size`: `object_size` is what the caller requested; `size` includes any alignment padding, SLUB redzone (when `CONFIG_SLUB_DEBUG` is enabled), and the free-pointer. The free-pointer is embedded in each free object at `offset` bytes from its start, forming a singly-linked list through the slab's free objects.

`oo` encodes two values in one word: the preferred page order for slab pages (a few bits) and the number of objects that fit in a slab of that order. Choosing a larger order reduces per-slab overhead but increases internal fragmentation.

`ctor` is an optional constructor invoked once when a new object is carved out of a fresh slab page. It is used by subsystems that maintain invariants across object lifetimes (e.g., pre-initialising spinlocks). Since SLUB recycles objects, the constructor is called on the first use of a fresh page, not on every allocation.

### SLUB Allocation Path

```
kmalloc(size, gfp)
  └─ kmem_cache_alloc(cache, gfp)           // mm/slub.c
       └─ slab_alloc_node()
            │
            ├─ [FAST PATH — no lock, runs in most cases]
            │    read cpu_slab->freelist     // per-CPU freelist pointer
            │    if freelist != NULL:
            │      object = freelist
            │      freelist = object->next   // advance freelist via embedded pointer
            │      return object             // done — no spinlock, no atomic op
            │
            └─ [SLOW PATH — freelist empty or CPU slab not active]
                 ___slab_alloc()
                   ├─ [per-CPU partial list has a slab]
                   │    → move slab from cpu_partial to cpu_slab, retry fast path
                   ├─ [node partial list has a slab]
                   │    → get_partial(): find a partially-used slab on the NUMA node
                   └─ [no partial slabs anywhere]
                        → new_slab(): alloc_pages(cache->allocflags, cache->oo.order)
                             → carve objects into freelist, set cpu_slab, retry fast path
```

The fast path is the common case: it reads and updates two pointers (the freelist head and the next pointer inside the returned object) with no lock and no atomic operation, relying on per-CPU access to avoid races. The slow path holds a node-level lock for the duration of getting a partial slab or allocating a new one from the buddy allocator.

### Live Observation — SLUB

```bash
# All slab caches: name, active objects, total objects, object size
cat /proc/slabinfo

# Interactive sorted view (sort by cache size by default)
slabtop -s c

# Per-cache stats and configuration (requires SLUB or SLUB_DEBUG)
ls /sys/kernel/slab/
cat /sys/kernel/slab/kmalloc-256/objects_partial
cat /sys/kernel/slab/dentry/object_size
```

`/proc/slabinfo` columns: `name`, `active_objs` (currently allocated), `num_objs` (total capacity across all slabs), `objsize` (bytes per object), `objperslab`, `pagesperslab`, and tuning parameters. The delta between `num_objs` and `active_objs` quantifies internal fragmentation.

---

## LRU Lists and Page Reclaim

The kernel must decide which physical pages to evict when memory runs low. The LRU (Least Recently Used) subsystem tracks how recently pages have been accessed and is the primary mechanism for selecting eviction candidates.

### Two LRU Lists Per Zone

Each memory zone maintains two LRU lists:

- **Inactive list**: newly faulted-in pages start here, and pages being considered for eviction live here.
- **Active list**: pages that have been accessed a second time are promoted here, indicating they are hot and should be kept in memory.

The split prevents a single large sequential scan (e.g., reading a big file) from evicting all the hot pages in the cache: the file pages enter the inactive list and can be reclaimed before they ever reach the active list.

### Page Movement Between Lists

```
              [first fault] → inactive list
                                │
                     [accessed again while on inactive]
                                │
                                ▼
                           active list
                                │
                    [not recently accessed — second-chance]
                                │
                                ▼
                           inactive list
                                │
                      [reclaim selects this page]
                                │
                ┌───────────────┴─────────────────┐
                ▼                                  ▼
       [anonymous page]                   [file page, clean]
       swap_out → swap device             delete from page cache → free
```

**Promotion** (inactive → active): when the hardware Accessed bit (bit 5 of the PTE) is set and the kernel's `mark_page_accessed()` is called, a page on the inactive list is promoted to the active list. The kernel clears the Accessed bit after checking it, so access must happen again within the next scan interval to keep a page active.

**Demotion** (active → inactive): the active list has a maximum size target (roughly half of all LRU pages). When it grows beyond the target, `shrink_active_list()` checks pages at the tail of the active list: pages whose Accessed bit is clear are moved to the inactive list. Pages whose Accessed bit is set have it cleared and are rotated to the head of the active list (second-chance policy).

### kswapd and the Reclaim Path

`kswapd` is a per-NUMA-node kernel thread that wakes when the number of free pages in any zone falls below the `pages_low` watermark. It runs asynchronously in the background to refill free memory before processes are forced into direct reclaim.

```
kswapd (kernel thread, per NUMA node)
  └─ balance_pgdat()                      // mm/vmscan.c
       └─ kswapd_shrink_node()
            └─ shrink_node()              // mm/vmscan.c
                 └─ shrink_lruvec()       // process LRU lists of one zone
                      ├─ shrink_active_list()   // demote cold pages active → inactive
                      └─ shrink_inactive_list() // reclaim pages from inactive list
                           ├─ [anonymous page, swap enabled]
                           │    → pageout() → swap_writepage()
                           │         → write to swap device → PTE replaced with swap entry
                           └─ [file-backed page, clean]
                                → delete_from_page_cache()
                                     → free page back to buddy allocator
```

When kswapd cannot keep up, or when `alloc_pages()` fails its fast path, the calling process itself runs **direct reclaim** — it executes the same `shrink_node()` path synchronously, blocking the allocation until enough pages are freed. Direct reclaim causes visible latency spikes and is observable via `pgscan_direct` in `/proc/vmstat`.

If reclaim fails to find enough pages, the OOM killer is invoked (covered in the next chapter).

### `/proc/vmstat` Reclaim Counters

All values in `/proc/vmstat` are cumulative since boot:

| Counter | Meaning |
|---------|---------|
| `pgsteal_kswapd` | Pages successfully reclaimed by kswapd (asynchronous background reclaim). |
| `pgsteal_direct` | Pages successfully reclaimed by a process in direct reclaim (synchronous — adds latency). |
| `pgscan_kswapd` | Pages scanned by kswapd's LRU walk. High values relative to `pgsteal_kswapd` mean low reclaim efficiency. |
| `pgscan_direct` | Pages scanned during direct reclaim. A sustained high rate indicates severe memory pressure. |
| `pgmajfault` | Major page faults — faults that required I/O (either reading from disk or reading from swap). Each increment means a process stalled waiting for a disk read. |

The ratio `pgsteal / pgscan` is the reclaim efficiency. An efficiency below 50% means the kernel is scanning many pages without being able to reclaim them, often because most pages are anonymous (requiring swap) and swap is slow or disabled.

---

## Live Observation

```bash
# Buddy allocator state — free pages per order per zone
cat /proc/buddyinfo

# Slab usage sorted by total cache size (largest first)
slabtop -s c

# Watch page reclaim activity in real time (values are cumulative — watch the delta)
watch -n 1 'grep -E "pgsteal|pgscan|pgfault" /proc/vmstat'

# bpftrace: count kmalloc calls grouped by requested size
# (useful for finding which sizes dominate kernel allocation pressure)
bpftrace -e 'kprobe:kmalloc { @[arg1] = count(); }'

# bpftrace: trace page reclaim — show which process triggers shrink_inactive_list
# (kswapd triggers this in background; user processes trigger it in direct reclaim)
bpftrace -e 'kprobe:shrink_inactive_list {
    printf("reclaim: pid=%d comm=%s\n", pid, comm);
}'
```

### Interpreting the Output

When `slabtop` shows a single cache consuming gigabytes (commonly `dentry` or `inode_cache`), it usually indicates a workload that opens many unique file paths. The kernel does not eagerly shrink slab caches; they grow until memory pressure triggers `shrink_slab()`, which calls each cache's `shrinker` callback.

When `pgsteal_direct` is climbing rapidly in `/proc/vmstat`, processes are being forced into synchronous reclaim — a sign that kswapd is not keeping up. Investigate with `sar -B 1` or `vmstat 1` to observe the reclaim rate vs fault rate balance.

Fragmentation is visible in `/proc/buddyinfo`: if order-9 and order-10 counts are consistently zero while order-0 and order-1 counts are high, THP allocation will fail and hugepage-backed workloads will fall back to base pages.

---

## Key Kernel References

| Symbol | File | Link | Purpose |
|--------|------|------|---------|
| `struct page` | `include/linux/mm_types.h` | [link](https://elixir.bootlin.com/linux/v6.9/source/include/linux/mm_types.h) | Physical page frame descriptor — 64 bytes per page |
| `struct kmem_cache` | `include/linux/slub_def.h` | [link](https://elixir.bootlin.com/linux/v6.9/source/include/linux/slub_def.h) | SLUB cache descriptor — one instance per object type |
| `alloc_pages()` | `mm/page_alloc.c` | [link](https://elixir.bootlin.com/linux/v6.9/source/mm/page_alloc.c) | Buddy allocator entry point — allocate 2^order contiguous pages |
| `kmem_cache_alloc()` | `mm/slub.c` | [link](https://elixir.bootlin.com/linux/v6.9/source/mm/slub.c) | SLUB fast-path object allocation |
| `shrink_inactive_list()` | `mm/vmscan.c` | [link](https://elixir.bootlin.com/linux/v6.9/source/mm/vmscan.c) | Core LRU reclaim — selects and frees inactive pages |
