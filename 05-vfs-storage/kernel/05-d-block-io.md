# 05-d: Block I/O — struct bio, Writeback, and blk-mq

## The Slowest Layer

The disk is slow. This is the oldest truth in systems programming, and everything in the Linux block I/O layer exists to mitigate it. A memory access takes 60-100 nanoseconds. A random read from a spinning hard disk takes 3-10 milliseconds — a factor of 50,000 slower. Even a modern NVMe SSD, the fastest persistent storage available, takes 50-100 microseconds per random read — still 1,000 times slower than DRAM. The kernel cannot make storage faster, but it can hide the latency through batching, reordering, and asynchrony.

The original Linux block layer was a single-queue design: one request queue per block device, protected by a single spinlock. This worked adequately in the 1990s when disk throughput was the bottleneck and CPUs were single-core. As SSDs replaced spinning disks and storage devices gained the ability to process hundreds of thousands of IOPS, the single queue became the bottleneck. The spinlock became so contended that CPUs were spending more time waiting for the lock than actually issuing I/O. Jens Axboe rewrote the block layer as blk-mq — multi-queue block I/O — merged in Linux 3.13 (2014). blk-mq eliminates the global spinlock by giving each CPU core its own software queue; hardware queues aggregate these per-CPU queues and submit directly to the device.

The fundamental kernel block I/O object is `struct bio`. A bio is a single I/O request: a target device, a starting sector, and a set of memory pages (the `bi_io_vec` array) to read into or write from. Filesystems and the page cache construct bio objects and submit them to the block layer. The block layer then schedules, merges, and eventually dispatches them to the device driver. writeback is the process that flushes dirty page cache pages to disk — turning a `write(2)` that returned instantly (because it wrote to the page cache) into actual persistent storage. Understanding the writeback lifecycle is essential for diagnosing latency spikes in containers: when the dirty ratio limit is hit, the writing process blocks in the kernel waiting for writeback to make room.

For Kubernetes, persistent storage means a PVC translates to a block device or filesystem mounted into the container's namespace. Every container log write, every emptyDir write, every PersistentVolume access goes through the block I/O stack. Throttling I/O at the cgroup level (`io.max`) means setting limits on how many bytes per second and how many IOPS a container's bio submissions can consume — enforced by the blk-cgroup controller sitting inside the block layer.

## Source Locations

| File | URL |
|------|-----|
| `include/linux/bio.h` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/bio.h |
| `block/blk-core.c` | https://elixir.bootlin.com/linux/v6.9/source/block/blk-core.c |
| `mm/page-writeback.c` | https://elixir.bootlin.com/linux/v6.9/source/mm/page-writeback.c |
| `block/blk-mq.c` | https://elixir.bootlin.com/linux/v6.9/source/block/blk-mq.c |

---

## struct bio — The Block I/O Unit

A `bio` (block I/O) is the fundamental unit of I/O in the Linux block layer. Every read and write that reaches a block device is represented as a `bio`. It carries a scatter-gather list of memory pages, the target device, the byte range (expressed in 512-byte sectors), and a completion callback.

```c
// include/linux/bio.h (key fields)
struct bio {
    struct bio          *bi_next;       // linked list of bios (for splitting/chaining)
    struct block_device *bi_bdev;       // target block device
    blk_opf_t           bi_opf;         // REQ_OP_READ, REQ_OP_WRITE, REQ_OP_FLUSH, REQ_OP_DISCARD
    unsigned short      bi_flags;
    unsigned short      bi_ioprio;      // I/O priority class
    blk_status_t        bi_status;      // BLK_STS_OK, BLK_STS_IOERR, BLK_STS_TIMEOUT...
    atomic_t            __bi_remaining; // countdown for chained bios
    struct bvec_iter    bi_iter;        // {sector, bi_size, bi_idx, bi_bvec_done}
    bio_end_io_t       *bi_end_io;      // completion callback
    void               *bi_private;     // private data for bi_end_io
    struct blkcg_gq    *bi_blkg;        // blkcg group (for io controller accounting)
    struct bio_vec     *bi_io_vec;      // scatter-gather list of {page, offset, len}
    unsigned short      bi_vcnt;        // number of bio_vecs used
    unsigned short      bi_max_vecs;    // capacity of bi_io_vec array
    struct bio_vec      bi_inline_vecs[]; // inline vecs for small bios (avoids separate alloc)
};
```

Source: https://elixir.bootlin.com/linux/v6.9/source/include/linux/bio.h

### Field-by-Field Explanation

**`bi_next`** — When the block layer needs to split a bio that is too large for the hardware (e.g., exceeds `max_sectors`), it chains the split pieces together using this pointer. The driver sees them as a linked list.

**`bi_bdev`** — The target block device. For example, `/dev/sda`, `/dev/nvme0n1`, or a device-mapper device. The block layer uses this to look up the request queue.

**`bi_opf`** — Encodes both the operation type and modifier flags. The low bits hold the operation code; higher bits hold request flags. Key values:

| Flag | Value | Meaning |
|------|-------|---------|
| `REQ_OP_READ` | 0 | Read sectors from device |
| `REQ_OP_WRITE` | 1 | Write sectors to device |
| `REQ_OP_FLUSH` | 2 | Flush the device write cache (fsync boundary) |
| `REQ_OP_DISCARD` | 3 | TRIM — tell SSD to discard blocks (reclaim space for garbage collection) |
| `REQ_SYNC` | flag | Do not batch this I/O; submit immediately (used for metadata, not data) |
| `REQ_META` | flag | Filesystem metadata I/O — elevated priority over user data |
| `REQ_FUA` | flag | Force Unit Access — write must reach stable storage before completion (bypasses write cache) |

**`bi_ioprio`** — The I/O priority class for this bio. Maps to the cgroup io.weight/io.latency settings from Chapter 03. Higher-priority classes see lower latency under congestion.

**`bi_status`** — Set by the device driver upon completion. `BLK_STS_OK` on success; `BLK_STS_IOERR` for device errors; `BLK_STS_TIMEOUT` when the request exceeds the driver's timeout threshold.

**`__bi_remaining`** — An atomic counter used when a bio is cloned or split into sub-bios. The parent bio's `bi_end_io` is only called when this counter reaches zero — that is, when all sub-bios complete.

**`bi_iter`** — Tracks the current position within the bio's data:
```c
struct bvec_iter {
    sector_t bi_sector;      // current sector on the device
    unsigned int bi_size;    // remaining bytes to transfer
    unsigned int bi_idx;     // current index into bi_io_vec
    unsigned int bi_bvec_done; // bytes completed in current bvec
};
```
As the bio moves through the block layer (partial completions, retries), `bi_iter` is advanced in place rather than modifying the original bio_vec array.

**`bi_io_vec` and `struct bio_vec`** — The scatter-gather list. Each entry describes a contiguous region within a single memory page:
```c
struct bio_vec {
    struct page  *bv_page;    // the page holding the data
    unsigned int  bv_len;     // number of bytes in this segment
    unsigned int  bv_offset;  // byte offset within the page
};
```
A single bio can aggregate data from multiple non-contiguous pages. The hardware DMA engine uses this list directly — there is no copy into a contiguous bounce buffer (on capable hardware). This is what makes page cache I/O zero-copy: the kernel hands the DMA engine a list of page frame numbers, and the device writes directly into or reads directly from those pages.

**`bi_vcnt` / `bi_max_vecs`** — `bi_vcnt` is the number of `bio_vec` entries currently in use; `bi_max_vecs` is the total capacity allocated. Small I/Os use `bi_inline_vecs` (embedded in the struct) to avoid a separate allocation.

**`bi_blkg`** — Links this bio to a block cgroup group (`blkcg_gq`). This is the hook that lets the kernel's I/O controller (cgroup v2 `io.weight`, `io.max`) account for and throttle I/O on a per-cgroup (and thus per-pod) basis. When the block layer dispatches a bio, it checks this pointer to apply bandwidth and IOPS limits.

**`bi_end_io`** — A function pointer called when the I/O completes. Invoked from interrupt context (for physical devices) or from a softirq (for device-mapper, loop, etc.). The callback must be fast and non-blocking. Filesystems use this to unlock pages and wake up waiting processes.

**`bi_private`** — Opaque pointer for the `bi_end_io` callback. Filesystems typically store a pointer to the writeback control structure or the page here.

---

## Page Writeback

Source: https://elixir.bootlin.com/linux/v6.9/source/mm/page-writeback.c

When a process writes to a file, the kernel marks the corresponding page cache pages as **dirty** and returns to userspace immediately — the write to disk is deferred. This is the write-back cache. Dirty pages accumulate until writeback pressure forces them to disk.

### Writeback Sysctls

| Sysctl | Default | Meaning |
|--------|---------|---------|
| `vm.dirty_background_ratio` | 10% | Background writeback threads wake when dirty pages exceed this percentage of total RAM |
| `vm.dirty_ratio` | 20% | Writing processes are throttled (put to sleep) when dirty pages exceed this percentage |
| `vm.dirty_expire_centisecs` | 3000 (30 s) | A dirty page older than this age must be written out on the next writeback pass |
| `vm.dirty_writeback_centisecs` | 500 (5 s) | How often per-backing-device writeback threads wake to check for work |

The two-threshold design is intentional: `dirty_background_ratio` keeps writeback steady in the background without stalling writers; `dirty_ratio` is the emergency brake that prevents unbounded dirty page accumulation from exhausting memory.

### Writeback Call Path

```
dirty page threshold exceeded (vm.dirty_background_ratio)
  └─ balance_dirty_pages_ratelimited()        # mm/page-writeback.c
       └─ wakeup_flusher_threads()
            └─ wb_start_writeback()           # mm/backing-dev.c (via work queue)
                 └─ writeback_inodes_wb()
                      └─ writeback_single_inode()
                           └─ do_writepages() → mapping->a_ops->writepages()
                                └─ [ext4] ext4_writepages() → submit_bio()
```

`balance_dirty_pages_ratelimited()` is called by every buffered write path. If the system is under pressure (dirty pages above `dirty_background_ratio`), it wakes the per-bdi (backing device info) flusher workqueue threads. When pressure reaches `dirty_ratio`, it additionally throttles the calling process.

Each filesystem implements `address_space_operations.writepages()`, which allocates bios, populates their `bio_vec` lists from dirty pages, and calls `submit_bio()` to hand them to the block layer.

### Observing Writeback via /proc/vmstat

```bash
grep -E "nr_dirty|nr_writeback|pgpgout|wb_writeback" /proc/vmstat
```

| Field | Meaning |
|-------|---------|
| `nr_dirty` | Pages currently dirty (in page cache, not yet written) |
| `nr_writeback` | Pages currently being written to disk |
| `pgpgout` | Total pages written out (monotonic counter) |
| `wb_writeback` | Writeback work items processed by flusher threads |

> **Note:** `nr_unstable` (NFS unstable writes — written to server but not yet committed) was removed in Linux 5.14 when the NFS unstable-write path was eliminated. It no longer appears in `/proc/vmstat` on Linux 6.9.

---

## blk-mq: Multi-Queue Block Layer

Source: https://elixir.bootlin.com/linux/v6.9/source/block/blk-mq.c

Since Linux 5.0, the block layer exclusively uses **blk-mq** (multi-queue). The old single-queue path (`blk_queue_bio`) is gone. blk-mq maps each CPU to a software staging queue and maps each hardware queue lane (NVMe has 32+, SATA has 1) to a hardware dispatch queue.

### Queue Architecture

**Software queue (`struct blk_mq_ctx`, per-CPU):** When a bio arrives via `submit_bio()`, it is initially staged in the calling CPU's software queue. This eliminates contention on a global lock.

**Hardware queue (`struct blk_mq_hw_ctx`):** Requests are dispatched from the software queue into one of the device's hardware queues. NVMe controllers expose many hardware queues (one per CPU on high-end drives); SATA/SAS controllers expose one. The scheduler (elevator) runs at this boundary to reorder requests for efficiency.

### Submission Path

```
submit_bio()
  └─ blk_mq_submit_bio()                     # block/blk-mq.c
       ├─ blk_mq_get_tag()                    # allocate a request tag from hardware queue
       └─ blk_mq_run_hw_queue()               # dispatch tagged request to device driver
```

### I/O Schedulers

Configured per device via `/sys/block/<dev>/queue/scheduler`:

| Scheduler | Use Case |
|-----------|----------|
| `none` | Pass-through for NVMe SSDs — the device has its own internal queuing |
| `mq-deadline` | Default for rotating disks — bounds worst-case latency |
| `bfq` | Budget Fair Queuing — proportional bandwidth allocation (useful for mixed workloads) |
| `kyber` | Latency-targeted for fast SSDs — targets separate read and write latency budgets |

---

## Live Observation

```bash
# Current I/O scheduler:
cat /sys/block/sda/queue/scheduler

# Per-device I/O stats (reads, writes, merges, time in queue):
cat /proc/diskstats
iostat -x 1

# Dirty page state:
grep -E "nr_dirty|nr_writeback|pgpgout" /proc/vmstat

# I/O pressure (PSI):
cat /proc/pressure/io

# bpftrace: trace block I/O submissions (op type, size, command)
bpftrace -e 'tracepoint:block:block_rq_issue {
    printf("blk: pid=%-6d comm=%-16s dev=%s op=%s size=%u\n",
           pid, comm, str(args->name), str(args->rwbs), args->nr_sector * 512);
}'

# bpftrace: trace I/O completions
bpftrace -e 'tracepoint:block:block_rq_complete {
    printf("blk done: dev=%s op=%s err=%d\n",
           str(args->name), str(args->rwbs), args->error);
}'

# bpftrace: count write I/Os per command (aggregate over 5 seconds)
bpftrace -e 'tracepoint:block:block_rq_issue /str(args->rwbs) == "W"/ {
    @writes[comm] = count();
} interval:s:5 { print(@writes); clear(@writes); }'
```

The `rwbs` field in block tracepoints is a string encoding the operation: `R` = read, `W` = write, `D` = discard, `F` = flush, `S` = sync, `M` = metadata.

---

## Key References

| Symbol | File | URL |
|--------|------|-----|
| `struct bio` | `include/linux/bio.h` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/bio.h |
| `struct bio_vec` | `include/linux/bvec.h` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/bvec.h |
| `submit_bio` | `block/blk-core.c` | https://elixir.bootlin.com/linux/v6.9/source/block/blk-core.c |
| `blk_mq_submit_bio` | `block/blk-mq.c` | https://elixir.bootlin.com/linux/v6.9/source/block/blk-mq.c |
| `balance_dirty_pages_ratelimited` | `mm/page-writeback.c` | https://elixir.bootlin.com/linux/v6.9/source/mm/page-writeback.c |
| `writeback_inodes_wb` | `fs/fs-writeback.c` | https://elixir.bootlin.com/linux/v6.9/source/fs/fs-writeback.c |
