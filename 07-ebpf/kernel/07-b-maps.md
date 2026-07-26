# BPF Maps — Hash, Array, Ring Buffer, BTF, and bpf(2) Operations

## The Missing Ingredient: Persistent State

Classic BPF, as designed by Steven McCanne and Van Jacobson in their 1992 paper and implemented in Linux in 1997, could filter packets. That was all it could do. A BPF program ran on a packet, decided whether to pass or drop it, and terminated. There was no way to count packets, no way to build a connection table, no way to communicate results back to userspace except through the yes/no filter decision. BPF programs were stateless by design.

This was a fundamental limitation. The most interesting observability and networking tasks require state: tracking which processes are opening which files, counting bytes per connection, detecting port scans by watching connection attempts accumulate over time. Without persistent storage between invocations, a BPF program was an elaborate one-liner.

eBPF's most consequential addition was maps, introduced in Linux 3.18 (December 2014) alongside the first eBPF programs. A map is a typed key→value store allocated in kernel memory, accessed by both BPF programs and userspace via the `bpf(2)` syscall. BPF programs can read and write maps from any hook point; userspace can create them, iterate them, and read the results. A kprobe on `sys_open` can increment a counter in a hash map. A userspace tool can read that counter every second. A service mesh proxy can maintain a map of all active connections and update it on every packet. Maps transformed BPF from a packet filter into a general-purpose in-kernel computation framework with a safe userspace interface.

The kernel ships more than twenty map types today, each optimized for different access patterns. Hash maps for arbitrary key lookups. Arrays for fast integer-indexed access. Per-CPU variants for lock-free updates. Ring buffers for efficient event streaming to userspace. The BTF type system gives maps structured type information so that bpftool and debugging tools can dump map contents with field names rather than raw bytes — essential for the observability tools that Kubernetes relies on.

BPF maps are the primary mechanism for sharing state between BPF programs and between BPF programs and userspace. They are typed key→value stores created through the `bpf(2)` syscall and reference-counted as file descriptors. The kernel ships more than twenty map types; this document covers the three most important — hash, array, and ring buffer — along with the BTF type system that gives maps runtime introspection, and the bpf(2) operations used to manipulate them from userspace.

## 1. Source Locations

| File | Key Symbols | URL |
|------|-------------|-----|
| `kernel/bpf/hashtab.c` | `htab_map_alloc`, `htab_map_lookup_elem`, `htab_map_update_elem` | https://elixir.bootlin.com/linux/v6.9/source/kernel/bpf/hashtab.c |
| `kernel/bpf/arraymap.c` | `array_map_alloc`, `array_map_lookup_elem` | https://elixir.bootlin.com/linux/v6.9/source/kernel/bpf/arraymap.c |
| `kernel/bpf/ringbuf.c` | `bpf_ringbuf_alloc`, `bpf_ringbuf_reserve`, `bpf_ringbuf_commit` | https://elixir.bootlin.com/linux/v6.9/source/kernel/bpf/ringbuf.c |
| `include/linux/bpf.h` | `struct bpf_map`, `struct bpf_map_ops` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/bpf.h |
| `kernel/bpf/syscall.c` | `map_create`, `map_lookup_elem`, `map_update_elem` | https://elixir.bootlin.com/linux/v6.9/source/kernel/bpf/syscall.c |

## 2. `struct bpf_map` — Base Map Structure

Every map type embeds `struct bpf_map` as its first member, providing the common interface seen by the verifier, the syscall layer, and the reference-counting infrastructure. All type-specific logic is dispatched through the `ops` vtable.

```c
// include/linux/bpf.h (simplified, Linux 6.9)
struct bpf_map {
    const struct bpf_map_ops *ops;  // vtable: alloc, free, lookup, update, delete, seq_show
    struct bpf_map     *inner_map_meta;  // for map-of-maps
    void               *security;
    enum bpf_map_type   map_type;        // BPF_MAP_TYPE_HASH, _ARRAY, _RINGBUF, ...
    u32                 key_size;        // in bytes
    u32                 value_size;      // in bytes
    u32                 max_entries;     // capacity
    u64                 map_flags;       // BPF_F_NO_PREALLOC, BPF_F_NUMA_NODE, ...
    int                 spin_lock_off;   // offset of bpf_spin_lock in value, -1 if none
    int                 timer_off;       // offset of bpf_timer in value, -1 if none
    u32                 id;              // global map ID
    int                 numa_node;
    u32                 btf_key_type_id;
    u32                 btf_value_type_id;
    u32                 btf_vmlinux_value_type_id;
    struct btf          *btf;
    struct mem_cgroup   *memcg;          // memory accounting
    char                name[BPF_OBJ_NAME_LEN]; // map name (from BPF_MAP_CREATE attr)
    struct mutex        freeze_mutex;
    atomic64_t          refcnt;          // reference count
    atomic64_t          usercnt;         // userspace file-descriptor count
    struct work_struct  work;
    union {
        atomic64_t      writecnt;
        struct {
            spinlock_t   lock;
            enum bpf_prog_type owner_prog_type;
            bool         owner_jited;
        };
    };
};
```

The `ops` pointer is set at map creation time based on `map_type` and never changes. The verifier uses `key_size`, `value_size`, and `max_entries` to bounds-check map accesses at load time. `refcnt` tracks kernel-internal references (BPF programs that hold the map), while `usercnt` tracks open file descriptors in userspace; the map is freed only when both reach zero.

## 3. `BPF_MAP_TYPE_HASH` — Hash Table Implementation

Hash maps store arbitrary key→value pairs using separate chaining over an array of buckets. The implementation in `kernel/bpf/hashtab.c` wraps `struct bpf_map` in a type-specific container and pre-allocates all elements at creation time by default (unless `BPF_F_NO_PREALLOC` is set) to avoid dynamic allocation on the fast path.

### `struct bpf_htab`

```c
// kernel/bpf/hashtab.c (simplified, Linux 6.9)
struct bpf_htab {
    struct bpf_map  map;           // embedded base (must be first)
    struct bucket   *buckets;      // array of [n_buckets] struct bucket
    void            *elems;        // pre-allocated element pool (if !NO_PREALLOC)
    union {
        struct pcpu_freelist freelist; // per-CPU free list of elements
        struct bpf_lru      lru;       // LRU eviction (BPF_MAP_TYPE_LRU_HASH)
    };
    struct htab_elem *extra_elems; // per-CPU spare element for update-in-place
    atomic_t         count;        // current number of entries
    u32              n_buckets;    // power-of-2 bucket count
    u32              elem_size;    // sizeof(htab_elem) + key_size + value_size
    u32              hashrnd;      // random seed for jhash
};
```

`n_buckets` is always a power of two so that the bucket index can be computed with a bitmask rather than a modulo. `elem_size` covers the fixed `htab_elem` header plus the inline key and value storage. `hashrnd` is set at allocation time from `get_random_u32()` to prevent hash-flooding attacks.

### `struct bucket`

```c
struct bucket {
    struct hlist_nulls_head head;  // linked list of htab_elem nodes
    union {
        raw_spinlock_t  raw_lock;  // used for most map types
        struct {
            spinlock_t  lock;
        };
    };
};
```

Each bucket owns a `hlist_nulls_head` (a kernel intrusive linked list that uses a NULL-sentinel with a tag bit to detect list-end without an extra branch) and a per-bucket spinlock. Reads walk the list under RCU read-side protection; writes acquire the per-bucket `raw_spinlock` to serialize concurrent updates to the same bucket without blocking readers.

### `struct htab_elem`

```c
struct htab_elem {
    union {
        struct hlist_nulls_node hash_node; // intrusive list node
        struct {
            void                *padding;
            union {
                struct bpf_htab *htab;
                struct pcpu_freelist_node fnode;
                struct htab_elem *batch_flink;
            };
        };
    };
    union {
        struct rcu_head rcu;
        struct bpf_lru_node lru_node;
    };
    u32             hash;          // cached jhash result for this key
    char            key[] __aligned(8); // key bytes immediately follow; value bytes follow key
};
```

The key is stored inline at `elem->key`, and the value is stored immediately after the key (at `elem->key + round_up(key_size, 8)`). This avoids a second allocation and improves cache locality. The `hash` field caches the `jhash()` result so that bucket-chain walks can compare the hash before calling `memcmp()` on the full key.

### Lookup Path

`htab_map_lookup_elem` (the `ops->map_lookup_elem` callback) calls `__htab_map_lookup_elem`, which:

1. Computes `hash = jhash(key, map->key_size, htab->hashrnd)`.
2. Selects `bucket = &htab->buckets[hash & (htab->n_buckets - 1)]`.
3. Enters an RCU read-side critical section (`rcu_read_lock()`).
4. Walks `bucket->head` using `hlist_nulls_for_each_entry_rcu`, comparing `elem->hash == hash` first, then `memcmp(elem->key, key, key_size) == 0`.
5. Returns a pointer to the value region (`elem->key + round_up(key_size, 8)`) or `NULL`.

Writes (update, delete) acquire `bucket->raw_lock` first, then re-walk the chain to find or insert the element. After modification the old element is freed via `call_rcu()` so readers that are still mid-walk can finish safely.

## 4. `BPF_MAP_TYPE_ARRAY` — Fixed-Size Array Map

Array maps provide O(1) lookup indexed by a `u32` key in the range `[0, max_entries)`. There are no buckets, no hashing, and no locking on the read path:

```c
// kernel/bpf/arraymap.c (simplified)
struct bpf_array {
    struct bpf_map  map;
    u32             elem_size;    // round_up(value_size, 8)
    u32             index_mask;   // max_entries - 1 (only when power-of-2)
    struct bpf_array_aux *aux;
    union {
        char value[0] __aligned(8);  // value data region
        void *ptrs[0] __aligned(8);  // for pointer-typed maps
    };
};
```

`array_map_lookup_elem` validates that `*(u32 *)key < array->map.max_entries` and returns `array->value + (u64)index * array->elem_size`. Because the key is an index into a pre-allocated slab, the kernel never needs to allocate or free entries at runtime — making this the best choice for per-CPU counters, static configuration tables read by BPF programs, and tail-call program arrays.

Per-CPU variants (`BPF_MAP_TYPE_PERCPU_ARRAY`) store `nr_cpus` copies of each value, accessed with `this_cpu_ptr`. Atomic updates to scalar values use `cmpxchg`; larger values require a `bpf_spin_lock` embedded in the value struct.

## 5. `BPF_MAP_TYPE_RINGBUF` — Event Streaming

The ring buffer map was introduced in Linux 5.8 as a high-throughput, low-overhead replacement for `BPF_MAP_TYPE_PERF_EVENT_ARRAY`. It supports multiple producers (BPF programs running on any CPU) and a single consumer (userspace polling via `ring_buffer__poll()` from libbpf). The `struct bpf_ringbuf` contains a `spinlock_t` that serializes concurrent producers in `bpf_ringbuf_reserve` via `spin_lock_irqsave()`; the consumer path reads `consumer_pos` without a lock.

### `struct bpf_ringbuf`

```c
// kernel/bpf/ringbuf.c (simplified)
struct bpf_ringbuf {
    wait_queue_head_t   waitq;
    struct irq_work     work;
    u64                 mask;              // size - 1 (size is power-of-2)
    struct page         **pages;
    int                 nr_pages;
    spinlock_t          spinlock ____cacheline_aligned_in_smp;
    atomic_t            busy ____cacheline_aligned_in_smp;
    /* shared with userspace via mmap: */
    unsigned long       consumer_pos __aligned(PAGE_SIZE);
    unsigned long       producer_pos __aligned(PAGE_SIZE);
    char                data[] __aligned(PAGE_SIZE);
};
```

`consumer_pos` and `producer_pos` are each page-aligned so that the kernel can map them into userspace as separate read-only and read-write pages respectively — the consumer updates `consumer_pos` to acknowledge records; the kernel updates `producer_pos` after committing a record.

### Producer / Consumer Design

The producer path (`bpf_ringbuf_reserve`):

1. Acquires `spinlock_t` with `spin_lock_irqsave()` (or `spin_trylock_irqsave()` from NMI context).
2. Checks that `producer_pos - consumer_pos < mask + 1` (ring not full). Returns `NULL` if full, then releases the lock.
3. Advances `producer_pos` by `round_up(size + BPF_RINGBUF_HDR_SZ, 8)` to claim the slot, then releases the lock with `spin_unlock_irqrestore()`.
4. Writes the record header (length + `BPF_RINGBUF_BUSY_BIT`) into `data[old_pos & mask]`.
5. Returns a pointer to the record body for the BPF program to fill.

`bpf_ringbuf_submit` marks the record header as committed (clears the `BPF_RINGBUF_BUSY_BIT`) and wakes `waitq` via `irq_work` to notify the consumer.

The consumer polls by comparing its local copy of `consumer_pos` against `producer_pos`. Because both positions are memory-mapped, no syscall is required for the consumer to detect new records.

### BPF Program Usage

```c
// In BPF C (compiled with clang -g -O2 -target bpf):
struct event *e = bpf_ringbuf_reserve(&rb, sizeof(*e), 0);
if (!e) return 0;  // ringbuf full — discard
e->pid = bpf_get_current_pid_tgid() >> 32;
bpf_ringbuf_submit(e, 0);
```

`bpf_ringbuf_discard(e, 0)` can be called instead of `submit` to release the reserved slot without publishing the record.

## 6. BTF (BPF Type Format)

BTF is a compact binary encoding of C type information, stored in the `.BTF` section of BPF ELF objects and embedded in the running kernel image at `/sys/kernel/btf/vmlinux`. It serves three roles:

### CO-RE (Compile Once, Run Everywhere)

Kernel data structures shift between releases — fields are added, reordered, or removed. A BPF program compiled against one kernel's headers would use hardcoded field offsets that break on a different kernel.

CO-RE eliminates this fragility:

1. `clang` compiles with `-g` and wraps field accesses in `__builtin_preserve_access_index()`. The compiler records each access as a CO-RE relocation in the `.BTF.ext` section, tagging it with the BTF type ID and field name rather than a raw offset.
2. At load time, `libbpf` reads the running kernel's BTF from `/sys/kernel/btf/vmlinux` and resolves each relocation by looking up the actual field offset in the running kernel's type information.
3. `libbpf` patches the BPF bytecode instructions' immediate values with the resolved offsets before passing the program to the kernel.
4. If a required field does not exist in the running kernel's BTF, `libbpf` rejects the program at load time with a descriptive error.

```c
// In BPF C (compiled with clang -g -O2 -target bpf):
#include "vmlinux.h"   // generated by: bpftool btf dump file /sys/kernel/btf/vmlinux format c

// BPF_CORE_READ expands to __builtin_preserve_access_index; offset patched at load time
pid_t pid = BPF_CORE_READ(task, pid);
```

### Verifier Type Checking

When a BPF program holds a BTF-annotated pointer (e.g., a `struct task_struct *` obtained via a kfunc), the verifier uses BTF to verify that field accesses stay within bounds and that pointer casts are legal — stricter than the generic register-range checks applied to untyped pointers.

### Map Pretty-Printing

When a map is created with `btf_key_type_id` and `btf_value_type_id` set, `bpftool map dump id <id>` uses BTF to format the binary data as C-like structs rather than raw hex.

### bpftool BTF Commands

```bash
# Dump the running kernel's full type information:
bpftool btf dump file /sys/kernel/btf/vmlinux format raw | head -40

# Pretty-print a loaded BPF map using its BTF annotations:
bpftool map dump id <id>

# Load a BPF program (libbpf resolves CO-RE relocations automatically):
bpftool prog load prog.bpf.o /sys/fs/bpf/test_prog

# Generate vmlinux.h (one-time, per kernel version):
bpftool btf dump file /sys/kernel/btf/vmlinux format c > vmlinux.h
```

## 7. bpf(2) Map Operations

All map lifecycle operations go through the `bpf(2)` syscall (`sys_bpf` in `kernel/bpf/syscall.c`) with a `union bpf_attr` argument. The kernel dispatches on the `cmd` field.

### `BPF_MAP_CREATE`

`map_create()` validates the attribute, calls `find_and_alloc_map()` to dispatch to the type's `ops->map_alloc()`, and returns a file descriptor backed by an anonymous inode. The file descriptor keeps the map's `usercnt` elevated.

### `BPF_MAP_UPDATE_ELEM`

`map_update_elem()` copies the key and value from userspace with `copy_from_user`, then calls `map->ops->map_update_elem(map, key, value, flags)`. For hash maps this acquires the per-bucket spinlock and either updates an existing element in place or inserts a new one from the freelist.

### `BPF_MAP_LOOKUP_ELEM`

`map_lookup_elem()` calls `map->ops->map_lookup_elem(map, key)` to obtain a pointer to the value, then copies it to userspace with `copy_to_user`.

### C Userspace Example

```c
#include <linux/bpf.h>
#include <sys/syscall.h>
#include <unistd.h>

// Create a hash map (key: u32, value: u64, capacity: 1024):
union bpf_attr attr = {
    .map_type    = BPF_MAP_TYPE_HASH,
    .key_size    = sizeof(u32),
    .value_size  = sizeof(u64),
    .max_entries = 1024,
};
int map_fd = syscall(SYS_bpf, BPF_MAP_CREATE, &attr, sizeof(attr));

// Update: key=1, value=42:
u32 key = 1;
u64 val = 42;
attr = (union bpf_attr){
    .map_fd = map_fd,
    .key    = (u64)(uintptr_t)&key,
    .value  = (u64)(uintptr_t)&val,
    .flags  = BPF_ANY,   // BPF_ANY = insert or update
};
syscall(SYS_bpf, BPF_MAP_UPDATE_ELEM, &attr, sizeof(attr));

// Lookup:
u64 result;
attr = (union bpf_attr){
    .map_fd = map_fd,
    .key    = (u64)(uintptr_t)&key,
    .value  = (u64)(uintptr_t)&result,
};
syscall(SYS_bpf, BPF_MAP_LOOKUP_ELEM, &attr, sizeof(attr));
```

`BPF_MAP_DELETE_ELEM` removes a key; `BPF_MAP_GET_NEXT_KEY` iterates over all keys. `BPF_OBJ_PIN` persists the map fd to the BPF filesystem at `/sys/fs/bpf/` so it survives the creating process's exit.

## 8. Key Kernel References

| Symbol | File | URL |
|--------|------|-----|
| `struct bpf_map` | `include/linux/bpf.h` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/bpf.h |
| `struct bpf_htab` | `kernel/bpf/hashtab.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/bpf/hashtab.c |
| `htab_map_lookup_elem` | `kernel/bpf/hashtab.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/bpf/hashtab.c |
| `struct bpf_ringbuf` | `kernel/bpf/ringbuf.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/bpf/ringbuf.c |
| `map_create` | `kernel/bpf/syscall.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/bpf/syscall.c |
| `array_map_lookup_elem` | `kernel/bpf/arraymap.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/bpf/arraymap.c |
