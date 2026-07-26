# NUMA — Non-Uniform Memory Access: Linux Kernel Internals

## The Topology Beneath the Abstraction

For most of computing history, programmers treated memory as a flat array. A pointer was a pointer; reading from address 0x1000 took the same time as reading from address 0x1000000. This model was true for decades, and then it became a convenient lie.

Multi-socket server hardware broke it. When AMD introduced the HyperTransport interconnect with the Opteron in 2003 and Intel followed with the QuickPath Interconnect in the Nehalem architecture in 2008, they eliminated the shared memory bus that had constrained single-socket performance. Each processor now had its own memory controller, attached to its own bank of DRAM. A processor could reach its local memory in 60-80 nanoseconds. Reaching memory attached to a different socket required crossing the inter-socket interconnect — 120-200 nanoseconds. The same physical address could take twice as long to access depending on which CPU was doing the asking.

The Linux kernel had to model this topology. The data structure is `struct pglist_data` — one instance per NUMA node — containing `struct zone` arrays for DMA, Normal, and Highmem regions. The allocator tries to satisfy requests from the calling CPU's local node first; when local memory is exhausted or the task's memory policy says otherwise, it falls over to remote nodes and pays the latency penalty. The policy interface — `mbind(2)`, `set_mempolicy(2)`, `move_pages(2)` — gives userspace control over which nodes memory comes from and where existing allocations migrate.

For Kubernetes, NUMA topology is the difference between a latency-sensitive pod running at 1ms tail latency and 3ms tail latency on identical hardware. The CPU Manager and Topology Manager features in kubelet exist precisely to co-locate a pod's CPUs and memory on the same NUMA node, eliminating cross-socket penalty. A pod scheduled without topology awareness may interleave its memory across nodes, running each memory access through the interconnect — invisible in CPU utilization metrics but devastating in application latency measurements.

## Source Locations

| File | Link | Contents |
|------|------|----------|
| `include/linux/mmzone.h` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/mmzone.h | `struct pglist_data`, `struct zone`, `struct free_area`, zone watermarks |
| `include/linux/mempolicy.h` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/mempolicy.h | `struct mempolicy`, MPOL_* constants |
| `mm/numa_balancing.c` | https://elixir.bootlin.com/linux/v6.9/source/mm/numa_balancing.c | Automatic NUMA balancing — `task_numa_fault()`, `task_numa_work()` |
| `mm/migrate.c` | https://elixir.bootlin.com/linux/v6.9/source/mm/migrate.c | `migrate_pages()` — physical page migration between NUMA nodes |

---

## What NUMA Is

On single-socket systems all RAM is equidistant from all CPUs. On multi-socket systems — the norm for server hardware — each CPU socket has a bank of RAM physically attached to it via its own memory controller. Accesses to that local RAM are fast; accesses to RAM on a different socket travel over an inter-socket interconnect (Intel UPI, AMD Infinity Fabric, or similar), adding latency.

**Typical numbers on a 2-socket x86 server:**

| Access type | Latency | Bandwidth |
|-------------|---------|-----------|
| Local DRAM (same socket) | ~80–100 ns | ~50–100 GB/s per socket |
| Remote DRAM (cross-socket) | ~150–200 ns | ~20–40 GB/s (bottlenecked by interconnect) |

The kernel calls each socket's memory domain a **NUMA node**. Node IDs are assigned by the firmware (ACPI SRAT table) and range from 0 upward. The kernel learns the topology at boot and represents it in a set of data structures that control every subsequent memory allocation.

**Why this matters operationally:**

- A process running on CPUs in node 0 but allocating pages from node 1 pays a ~2× latency penalty on every cache miss.
- Bandwidth-hungry workloads (databases, ML training, video processing) can halve their effective memory bandwidth if pages are on the wrong node.
- For latency-sensitive services (trading systems, real-time audio/video), a single remote access in a hot path is measurable.

The kernel does not hide NUMA — it exposes the topology, provides policy knobs, and (optionally) automatically migrates pages to reduce remote access. Understanding the data structures behind these mechanisms is the first step to diagnosing and fixing NUMA-induced performance problems.

---

## `struct pglist_data` — One Per NUMA Node

Source: https://elixir.bootlin.com/linux/v6.9/source/include/linux/mmzone.h

The kernel allocates one `pg_data_t` (typedef for `struct pglist_data`) per NUMA node. On UMA (single-socket) systems there is exactly one node: `NODE_DATA(0)`. All allocation and reclaim paths index into `NODE_DATA(nid)` to get the relevant node state.

```c
// include/linux/mmzone.h (key fields)
typedef struct pglist_data {
    struct zone     node_zones[MAX_NR_ZONES];     // DMA, DMA32, Normal, Movable
    struct zonelist node_zonelists[MAX_ZONELISTS]; // fallback order for allocations
    int             nr_zones;                      // number of populated zones on this node
    unsigned long   node_start_pfn;               // first Page Frame Number of this node
    unsigned long   node_present_pages;            // usable pages (excluding holes)
    unsigned long   node_spanned_pages;            // total PFN span including memory holes
    int             node_id;                       // NUMA node ID (0, 1, 2, ...)
    wait_queue_head_t kswapd_wait;                 // kswapd sleeps here waiting for work
    struct task_struct *kswapd;                    // kswapd kernel thread for this node
    struct per_cpu_pageset __percpu *pageset;      // per-CPU page caches (pcp lists)
} pg_data_t;
```

**Field-by-field explanation:**

`node_zones[MAX_NR_ZONES]` — An array of `struct zone` objects, one per zone type. `MAX_NR_ZONES` is typically 4 on x86_64:
- `ZONE_DMA` (index 0): physical addresses 0–16 MiB. Legacy ISA DMA constraint. Nearly obsolete on modern hardware.
- `ZONE_DMA32` (index 1): physical addresses 0–4 GiB. Required by 32-bit PCI devices that can only DMA within a 32-bit address range.
- `ZONE_NORMAL` (index 2): all remaining directly-mapped physical memory. The vast majority of RAM on modern servers lives here.
- `ZONE_MOVABLE` (index 3): pages that can be migrated, used for memory hot-plug and balloon drivers.

`node_zonelists[MAX_ZONELISTS]` — The **fallback zone list**. When an allocation cannot be satisfied from the preferred zone (e.g., `ZONE_NORMAL` is exhausted), the allocator walks this list to find memory in a different zone or a different NUMA node. The first element (`ZONELIST_FALLBACK`) is a priority-ordered list of all zones on all nodes. The allocator prefers local-node zones first, then falls back to remote nodes in topology order (nearest first, then farther). This is the mechanism behind **NUMA fallback**: the system keeps functioning even if one node runs out of memory, but the caller pays the cross-node access cost.

`node_start_pfn` — The Page Frame Number (physical address divided by page size) of the first page on this node. Used for address-to-node lookups: `pfn_to_nid(pfn)` checks whether a PFN falls within a node's range.

`node_present_pages` — The number of pages that are actually usable on this node. Excludes memory holes (gaps in the physical address map reported by firmware).

`node_spanned_pages` — The full PFN range of the node, including holes. `node_spanned_pages >= node_present_pages` always.

`node_id` — The NUMA node index. `NODE_DATA(nid)->node_id == nid` always holds.

`kswapd` / `kswapd_wait` — Each NUMA node runs its own `kswapd` kernel thread, named `kswapd0`, `kswapd1`, etc. When a zone's free pages fall below the **low watermark**, the allocator wakes `kswapd` via `wakeup_kswapd()`. The thread sleeps on `kswapd_wait` until woken, then calls `balance_pgdat()` to reclaim pages on this node. Having per-node kswapd threads avoids cross-node reclaim pressure: kswapd for node 0 reclaims pages from node 0, not node 1.

`pageset` — Per-CPU page caches (PCP lists). Each CPU maintains a small cache of free pages for its local allocation path. When a CPU allocates a single page, it takes from its PCP list without acquiring the zone lock. This reduces contention on the central buddy allocator lock. Pages cycle between the PCP list and the buddy allocator in batches.

---

## `struct zone` — Zone State and Watermarks

Source: https://elixir.bootlin.com/linux/v6.9/source/include/linux/mmzone.h

Each `pg_data_t` contains an array of `struct zone`. A zone is the unit at which watermarks, free lists, and reclaim policy are applied.

```c
// include/linux/mmzone.h (key fields)
struct zone {
    unsigned long   _watermark[NR_WMARK];          // min, low, high — free page thresholds
    unsigned long   watermark_boost;               // boost added during fragmentation events
    long            lowmem_reserve[MAX_NR_ZONES];  // pages held in reserve for lower zones
    struct free_area free_area[MAX_ORDER];          // buddy allocator free lists per order
    unsigned long   flags;                         // ZONE_RECLAIM_ACTIVE, ZONE_WRITEBACK, etc.
    spinlock_t      lock;                          // protects free_area lists
    const char     *name;                          // "DMA", "DMA32", "Normal", "Movable"
    atomic_long_t   vm_stat[NR_VM_ZONE_STAT_ITEMS]; // per-zone vmstat counters
};
```

### Zone Watermarks

The three watermarks (`NR_WMARK` = 3) control the kernel's response to memory pressure. They are stored in `_watermark[]` and read via `low_wmark_pages(zone)` etc.:

| Watermark | Index | Meaning |
|-----------|-------|---------|
| `WMARK_MIN` | 0 | Emergency reserve. Below this, only `GFP_ATOMIC` (interrupt context) and `__GFP_HIGH` allocations are permitted. `PF_MEMALLOC` tasks (kswapd, direct reclaim) can also dip into this reserve. |
| `WMARK_LOW` | 1 | kswapd wake threshold. When free pages drop below this level, `wakeup_kswapd()` is called. Background reclaim starts asynchronously. |
| `WMARK_HIGH` | 2 | kswapd stop threshold. kswapd continues reclaiming until free pages reach this level, then goes back to sleep. |

**Operational implications:**

- Normal allocations succeed as long as free pages are above `WMARK_LOW`. Between `WMARK_LOW` and `WMARK_MIN`, kswapd is running but allocations still succeed (direct reclaim may be triggered under heavy load).
- When free pages fall below `WMARK_MIN`, the allocator enters the slow path and triggers **direct reclaim**: the allocating task itself reclaims pages synchronously before its allocation can proceed. This is a latency-amplifier — application threads stall inside the kernel for tens of milliseconds.
- Watermark values are set by `__setup_per_zone_wmarks()` based on the total amount of RAM, tunable via `/proc/sys/vm/watermark_scale_factor`.

`free_area[MAX_ORDER]` — The buddy allocator's free lists, one per allocation order (0 = single page, MAX_ORDER-1 = 2^(MAX_ORDER-1) contiguous pages). The buddy system splits and merges blocks of physically contiguous pages to satisfy allocations of arbitrary power-of-two sizes.

`vm_stat[]` — Per-zone counters for vmstat items: `NR_FREE_PAGES`, `NR_ANON_PAGES`, `NR_FILE_PAGES`, `NR_SLAB_RECLAIMABLE`, etc. These are the raw counts behind `/proc/vmstat` and `/proc/zoneinfo`.

---

## `struct mempolicy` — Per-Process NUMA Policy

Source: https://elixir.bootlin.com/linux/v6.9/source/include/linux/mempolicy.h

Every process can have a NUMA memory policy that controls which nodes its pages are allocated from. The policy is stored in `task_struct->mempolicy` and referenced from `mm_struct` as well. VMAs can also carry their own policy via `vm_area_struct->vm_policy`, which overrides the task policy for that range.

```c
// include/linux/mempolicy.h
struct mempolicy {
    atomic_t         refcnt;    // reference count — shared between task and VMAs
    unsigned short   mode;      // MPOL_DEFAULT, MPOL_BIND, MPOL_PREFERRED, MPOL_INTERLEAVE
    unsigned short   flags;     // MPOL_F_NODE, MPOL_F_ADDR, MPOL_F_RELATIVE_NODES, ...
    nodemask_t       nodes;     // bitmask of allowed/preferred NUMA nodes
};
```

### Policy Modes

**`MPOL_DEFAULT`** — Inherit policy from parent task (or use system default if no parent policy). For most processes this means: allocate from the node local to the CPU the task is currently running on. This is the correct default for the vast majority of workloads.

**`MPOL_BIND`** — Hard binding. The allocator is restricted to the nodes listed in `nodes`. If none of those nodes has free memory, the allocation fails with `ENOMEM` rather than falling back to a remote node. Use this when you need strict NUMA isolation and can guarantee the working set fits within the bound nodes.

**`MPOL_PREFERRED`** — Soft preference. The allocator tries the preferred node first, but falls back to other nodes if the preferred node is exhausted. Unlike `MPOL_BIND`, this never fails due to a single node being full. The `nodes` mask specifies the preferred node; if more than one node is listed, the first is used.

**`MPOL_INTERLEAVE`** — Round-robin allocation across all nodes in `nodes`. Page N is allocated from node `N % popcount(nodes)`. This spreads memory usage evenly across nodes, which is optimal for workloads that are memory-bandwidth-limited rather than latency-sensitive: databases doing large sequential scans, machine learning data loading, etc. TLB locality is sacrificed for aggregate bandwidth.

### Syscall Interface

| Syscall | Purpose |
|---------|---------|
| `mbind(2)` | Set NUMA policy for a specific VMA address range. Optionally migrate existing pages immediately (`MPOL_MF_MOVE`). |
| `set_mempolicy(2)` | Set the thread-wide default policy (`task_struct->mempolicy`). Affects all future allocations not covered by a VMA policy. |
| `get_mempolicy(2)` | Query the current effective policy for a thread or a specific address. |
| `move_pages(2)` | Manually migrate specific pages to specified nodes. Does not change policy. |

---

## Automatic NUMA Balancing

Source: https://elixir.bootlin.com/linux/v6.9/source/mm/numa_balancing.c

When `kernel.numa_balancing=1` (the default on multi-node systems), the kernel automatically detects and corrects NUMA imbalances without requiring any application changes. The mechanism works through a deliberate fault loop:

**How it works:**

1. `task_numa_work()` is scheduled periodically via `task_work_add()` for each task. It scans a chunk of the task's VMAs and calls `change_prot_numa()` to mark PTEs with the NUMA hint bit (`_PAGE_PROTNONE`) — making the pages appear inaccessible to hardware.

2. On the next access to a marked page, the CPU raises a page fault. The fault handler calls `do_numa_page()` → `task_numa_fault()`.

3. `task_numa_fault()` determines whether the access was **local** (CPU's NUMA node == page's NUMA node) or **remote**. It accumulates statistics in `task_struct->numa_faults[]` to track the pattern over time.

4. If the statistics show a persistent pattern of remote access, `numa_migrate_prep()` and `migrate_pages()` are called to physically move the pages to the node where the task is running. The buddy allocator on the destination node provides new page frames; the page table is updated; the old frames are returned to the source node's free list.

5. The scheduler's NUMA balancing logic (`task_numa_placement()`) may also migrate the task itself to a node where most of its pages already reside — a pull rather than push strategy.

**Tuning knobs:**

```
/proc/sys/kernel/numa_balancing          # 0=off, 1=on
/proc/sys/kernel/numa_balancing_scan_size_mb    # chunk size per scan (default 256 MiB)
/proc/sys/kernel/numa_balancing_scan_period_min_ms  # minimum scan interval
/proc/sys/kernel/numa_balancing_scan_period_max_ms  # maximum scan interval (backs off if already balanced)
```

**Trade-offs:** NUMA balancing adds fault overhead during the scan/fault phase. For workloads with already-optimal NUMA placement (e.g., pinned with `numactl`), disable it with `kernel.numa_balancing=0` to avoid the PTE-marking and fault overhead.

---

## Live Observation

```bash
# Show NUMA topology: nodes, CPUs per node, memory per node, distances:
numactl --hardware

# Show current shell process's NUMA policy:
numactl --show

# Per-node memory statistics (numa_hit, numa_miss, local_node, other_node, ...):
numastat

# Per-process NUMA allocation breakdown (which nodes a process's pages live on):
numastat -p <pid>

# Free pages per order per zone per node (buddy allocator state):
cat /proc/buddyinfo

# Per-VMA NUMA placement for a process:
cat /proc/$PID/numa_maps | head -20
# Output format: <vma_start> <policy> [N0=<pages_on_node0>] [N1=<pages_on_node1>]
# A high ratio of N1 pages on a process running on node 0 indicates NUMA imbalance.

# Zone watermark levels (free, min, low, high for each zone):
cat /proc/zoneinfo | grep -A5 "Node 0, zone   Normal"

# NUMA balancing fault statistics for a process:
cat /proc/$PID/sched | grep -i numa

# bpftrace: trace NUMA page faults (task accessing remote memory)
# arg0=task_struct*, arg1=vma*, arg2=nid (node where page resides), arg3=flags
bpftrace -e 'kprobe:task_numa_fault {
    printf("numa fault: pid=%d comm=%s node=%d\n", pid, comm, arg2);
}'

# bpftrace: observe NUMA page migrations
# migrate_pages(pagelist, new_get_page, put_new_page, private, ...)
bpftrace -e 'kprobe:migrate_pages {
    printf("migrate_pages: pid=%d comm=%s\n", pid, comm);
}'
```

### Reading `numa_maps`

```
7f3a00000000 default file=/usr/lib/libc.so.6 mapped=342 N0=342
7f3a14000000 default anon=1 dirty=1 N0=1024 N1=890
```

Line 1: the libc mapping lives entirely on node 0 (N0=342). Good — the process is running on node 0.

Line 2: anonymous (heap/stack) memory is split — 1024 pages on node 0, 890 pages on node 1. This indicates ~46% remote memory access for this VMA. If the process is latency-sensitive and consistently running on node 0 CPUs, the 890 node-1 pages will cause remote DRAM accesses. NUMA balancing should eventually migrate them, or you can force migration with `numactl --membind 0 <cmd>` and `move_pages(2)`.

---

## Key Kernel References

| Symbol | File | Link |
|--------|------|------|
| `pg_data_t` / `struct pglist_data` | `include/linux/mmzone.h` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/mmzone.h |
| `struct zone` | `include/linux/mmzone.h` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/mmzone.h |
| `struct mempolicy` | `include/linux/mempolicy.h` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/mempolicy.h |
| `task_numa_fault()` | `mm/numa_balancing.c` | https://elixir.bootlin.com/linux/v6.9/source/mm/numa_balancing.c |
| `numa_migrate_prep()` | `mm/numa_balancing.c` | https://elixir.bootlin.com/linux/v6.9/source/mm/numa_balancing.c |
| `migrate_pages()` | `mm/migrate.c` | https://elixir.bootlin.com/linux/v6.9/source/mm/migrate.c |
| `task_numa_work()` | `mm/numa_balancing.c` | https://elixir.bootlin.com/linux/v6.9/source/mm/numa_balancing.c |
