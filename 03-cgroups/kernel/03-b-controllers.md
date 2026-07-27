# cgroup v2 Controllers — Kernel Deep Dive

## Source Locations

| File | Link |
|------|------|
| `include/linux/cgroup-defs.h` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/cgroup-defs.h |
| `mm/memcontrol.c` | https://elixir.bootlin.com/linux/v6.9/source/mm/memcontrol.c |
| `kernel/sched/fair.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/fair.c |
| `block/blk-cgroup.c` | https://elixir.bootlin.com/linux/v6.9/source/block/blk-cgroup.c |
| `kernel/cgroup/pids.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/cgroup/pids.c |

## The Plugin Architecture

The kernel's core resource management paths — memory allocation, scheduler tick, block I/O submission — were written long before cgroups existed. They could not be modified to hard-code knowledge of every controller. Instead, each controller registers itself through a vtable — `struct cgroup_subsys` — and the cgroup framework calls the vtable's callbacks at the right moments. This is not a novel design; it is the same callback-table pattern used by the VFS (`struct file_operations`), the network stack (`struct proto_ops`), and the block layer (`struct blk_mq_ops`). The value of the pattern is that the core paths remain unaware of which controllers are compiled in. The entire memory controller can be disabled at build time and the allocator is none the wiser.

### struct cgroup_subsys — The Controller Vtable

```c
// include/linux/cgroup-defs.h
struct cgroup_subsys {
    /* lifecycle */
    struct cgroup_subsys_state *(*css_alloc)(struct cgroup_subsys_state *parent_css);
    int  (*css_online)(struct cgroup_subsys_state *css);
    void (*css_offline)(struct cgroup_subsys_state *css);
    void (*css_free)(struct cgroup_subsys_state *css);

    /* task events */
    int  (*can_attach)(struct cgroup_taskset *tset);
    void (*attach)(struct cgroup_taskset *tset);
    int  (*fork)(struct task_struct *task);
    void (*exit)(struct task_struct *task);
    void (*release)(struct task_struct *task);

    /* controller identity */
    int          id;
    const char  *name;
    bool         threaded;
};
```

`css_alloc` is called when a new cgroup is created via `mkdir`. The memory controller uses it to allocate a `struct mem_cgroup`; the CPU controller allocates a `struct task_group` with per-CPU scheduler entities. If allocation fails here, the `mkdir` fails — there are no partially-initialized cgroups left in the tree.

`css_online` is called after all controllers have successfully allocated state for the new cgroup. This is the "go live" signal: the memory controller registers its per-NUMA accounting here; the CPU controller links the new `task_group` into the CFS hierarchy. The split between `css_alloc` and `css_online` exists so that if `css_online` fails for one controller, all previously online controllers can be taken offline cleanly before anything is freed.

`can_attach` and `attach` bracket the task migration window. `can_attach` runs before any state changes and is the place to reject a migration — the memory controller uses it to check whether migrating a task would cause an immediate OOM in the target cgroup. `attach` runs after the task has moved and is used for post-migration bookkeeping.

`fork` is called on every `clone()`/`fork()` even when no cgroup migration is happening. The pids controller uses this hook to increment its counter and enforce `pids.max` before the child process ever executes a single instruction.

### Enabling Controllers

In cgroup v2 there is a top-down delegation model: a controller must be enabled at a cgroup for its children to have access to it. Enabling a controller does not affect the cgroup where you write the `+controller` directive — it enables the controller's files in that cgroup's children.

```bash
# See which controllers the kernel has compiled in and enabled globally:
cat /sys/fs/cgroup/cgroup.controllers
# cpuset cpu io memory hugetlb pids rdma misc

# Enable memory and cpu for children of a cgroup:
echo "+memory +cpu" > /sys/fs/cgroup/mygroup/cgroup.subtree_control

# Verify the enabled set propagates to the child:
mkdir /sys/fs/cgroup/mygroup/child
cat /sys/fs/cgroup/mygroup/child/cgroup.controllers
# memory cpu
```

The internal-node rule: a cgroup that has tasks directly attached cannot enable controllers. This is not a bug — it is an intentional design choice that prevents a class of subtle accounting errors. If tasks were in a cgroup that also acted as a controller-delegation point, it would be ambiguous whether those tasks' resource usage should count toward their own limits or their children's. The rule forces a clean split: leaf cgroups hold tasks, internal cgroups manage delegation.

## The Memory Controller

### The Problem: Why Memory Accounting Is Different

CPU and I/O have natural time-slicing: if a process uses too much CPU, you just don't schedule it for a while. There is no lasting state to unwind. Memory is different. When a process allocates 10 MB of anonymous memory, those pages persist in RAM until the process either frees them or the kernel reclaims them. To enforce a memory limit on a group of processes you need to know, at every allocation, how much memory the group is already using, and you need a plan for what to do when the limit is approached or exceeded.

This requires per-cgroup accounting of every page. The memory controller hooks into the allocator so that every anonymous and file-backed page is charged to the cgroup of the process that faulted it in. When a page is reclaimed or freed, its charge is removed. The result is a running total that reflects, at any moment, how much physical memory this cgroup's processes own.

### struct mem_cgroup

```c
// include/linux/memcontrol.h (simplified — see source for full struct)
struct mem_cgroup {
    struct cgroup_subsys_state css;  /* must be first */

    struct page_counter memory;      /* anonymous + file pages */
    struct page_counter swap;
    struct page_counter memsw;       /* memory + swap combined */

    unsigned long       soft_limit;  /* memory.high in pages */

    bool                oom_group;   /* kill whole cgroup on OOM */
    int                 oom_kill_disable;

    struct mem_cgroup_per_node __percpu *nodeinfo;  /* per-NUMA stats */

    struct cgroup_file  events_file;
    atomic_long_t       memory_events[MEMCG_NR_MEMORY_EVENTS];
    struct list_head    event_list;
    spinlock_t          event_list_lock;
};
```

`page_counter` is the internal accounting unit. It keeps a running `usage` in pages (not bytes — the page is the kernel's natural granularity), a `max` for the hard limit, and `high`, `min`, and `low` fields for soft limits and protection. The multiple limit fields are not redundant — they represent fundamentally different policies.

### Four Limits, Four Behaviors

`memory.min` is a protection guarantee: the kernel will not reclaim memory from this cgroup to satisfy other cgroups' demands as long as this cgroup stays below `min`. It is the memory equivalent of a CPU `requests` value — a floor guarantee, not a ceiling.

`memory.low` is a softer version of `min`. Memory below `low` is still reclaimed if the system is under global pressure, but the kernel gives these pages lower priority as reclaim candidates. It gives the cgroup a chance to shed its own memory before others reach in.

`memory.high` is a soft ceiling. When a cgroup exceeds `high`, the allocating process is throttled: after every allocation, the kernel calls `schedule()` to force the task to yield, which gives the background reclaim daemon (`kswapd`) time to reclaim pages from this cgroup. The process continues running — it is not killed — but its allocation rate is deliberately slowed. This is the mechanism behind Kubernetes' "burstable" QoS class: you can momentarily exceed your `requests` value, but the kernel will make you pay in latency.

`memory.max` is the hard ceiling. When a process attempts to allocate memory that would take the cgroup over `max`, the kernel tries reclaim first. If reclaim cannot free enough pages, the OOM killer fires inside the cgroup. This is what Kubernetes `limits.memory` maps to.

```bash
# Current usage:
cat /sys/fs/cgroup/kubepods/pod<uid>/memory.current

# Full accounting breakdown — anon, file, kernel, slab, sock:
cat /sys/fs/cgroup/kubepods/pod<uid>/memory.stat

# Event counters — oom is "hit the limit", oom_kill is "process killed":
cat /sys/fs/cgroup/kubepods/pod<uid>/memory.events

# PSI pressure percentages:
cat /sys/fs/cgroup/kubepods/pod<uid>/memory.pressure
```

### The Charge Path

Every page allocation that might be charged runs through `mem_cgroup_charge()`. The hot path:

```
page fault → handle_mm_fault() → do_anonymous_page()
  └─ mem_cgroup_charge()
       └─ try_charge()
            ├─ [usage < high] → charge and return (fast path, no penalty)
            ├─ [high < usage < max] → charge, then mem_cgroup_handle_over_high()
            │    └─ schedule() — yield and hope reclaim catches up
            └─ [usage == max, reclaim fails] → mem_cgroup_out_of_memory()
                 └─ out_of_memory() → oom_kill_process() → SIGKILL
```

The fast path (usage below `high`) is a single `atomic_long_add` to `page_counter.usage` plus a comparison. This runs on every page allocation in every container on the system. Its performance matters — it is the reason `page_counter.usage` is an `atomic_long_t` rather than a protected struct: the cost of taking a lock on every allocation would be intolerable.

```bash
# bpftrace: trace cgroup OOM kills
bpftrace -e 'kprobe:mem_cgroup_out_of_memory {
    printf("OOM kill: pid=%d comm=%s\n", pid, comm);
}'

# bpftrace: observe memory.high throttling
bpftrace -e 'kprobe:mem_cgroup_handle_over_high {
    printf("throttled: pid=%d comm=%s\n", pid, comm);
}'
```

## The CPU Controller

### The Problem: Sharing Time Without Starvation

The CPU scheduler without cgroups works reasonably well for workloads where you want proportional fairness: each process gets CPU time proportional to its nice value. But proportional fairness breaks down in container environments for two distinct reasons.

First, proportional fairness is relative. If one container has 1000 shares and another has 1000 shares, each gets roughly half the CPU. But if one container has all the CPU for itself, it gets 100% — there is nothing to share proportionally. This is fine if you want "maximize utilization," but it means you cannot guarantee that a container will use *at most* N% of the CPU. For multi-tenant nodes, that guarantee matters.

Second, Kubernetes allocates CPU in millicores — `500m` means 500/1000 of one CPU. To translate that into a hard ceiling, you need quota: not "try to give this cgroup half a CPU" but "this cgroup may use at most 50ms in every 100ms window." That is CFS bandwidth control.

### struct task_group and struct cfs_bandwidth

```c
// kernel/sched/sched.h (simplified)
struct task_group {
    struct cgroup_subsys_state css;  /* must be first */

    struct cfs_bandwidth    cfs_bandwidth; /* quota/period state */
    struct sched_entity   **se;            /* per-CPU scheduling entity */
    struct cfs_rq         **cfs_rq;       /* per-CPU runqueue */
    unsigned long           shares;        /* cpu.weight → CFS shares */

    struct sched_rt_entity **rt_se;
    struct rt_rq           **rt_rq;
};

struct cfs_bandwidth {
    raw_spinlock_t  lock;
    ktime_t         period;        /* the measurement window */
    u64             quota;         /* nanoseconds of CPU time per period */
    u64             runtime;       /* how much quota remains this period */
    u8              idle;
    u8              period_active;
    struct hrtimer  period_timer;  /* fires when period ends — refills runtime */
    struct hrtimer  slack_timer;   /* coalesces small replenishment wakeups */
};
```

`period` and `quota` are the two knobs behind `cpu.max`. The default period is 100ms. If you write `200000 100000` to `cpu.max`, you are saying: in every 100ms window, this cgroup may use 200ms of CPU time — which means it can run on two CPUs simultaneously at full utilization. Writing `50000 100000` means 50ms per 100ms window: half a CPU, what Kubernetes calls `500m`.

`runtime` is the remaining budget for the current period. Every time a process in the cgroup runs, the scheduler deducts from `runtime`. When `runtime` reaches zero, `throttle_cfs_rq()` dequeues the cgroup's tasks from the runqueue — they are simply not runnable until `period_timer` fires and refills `runtime` to `quota`. The process is not signaled, not sleeping by choice — it is sitting in the scheduler's throttled list, waiting for the period to roll over.

### The Throttle-Unthrottle Cycle

```
scheduler tick → scheduler_tick() → task_tick_fair()
  └─ update_curr() → account_cfs_rq_runtime()
       └─ [runtime ≤ 0] → throttle_cfs_rq()
            └─ dequeue all tasks in this cgroup from the runqueue

period_timer fires (every 100ms by default)
  └─ do_sched_cfs_period_timer()
       └─ cfs_bandwidth.runtime = quota    # refill
            └─ unthrottle_cfs_rq()         # re-enqueue tasks
```

The throttle-unthrottle cycle is the source of "CPU throttling" visible in `cpu.stat`. Every time a cgroup hits its quota and gets throttled before the period ends, `nr_throttled` increments and `throttled_usec` accumulates. A pod that reads 40% `nr_throttled` is spending 40% of its time sitting in the throttled list, unable to make forward progress, even if the node has spare CPU capacity. This is a common cause of latency spikes in Kubernetes — not because the CPU is actually busy, but because the pod has exhausted its quota for the current period and is waiting for the refill.

### cpu.weight and CFS Shares

`cpu.weight` (range 1–10000, default 100) controls relative CPU scheduling priority, separate from bandwidth limits. A cgroup with `cpu.weight=200` gets twice as much CPU as one with `cpu.weight=100` when both are runnable and competing for the same CPU — this is the fair-scheduling guarantee of CFS. Kubernetes sets `cpu.weight` from `requests.cpu`: a pod requesting `500m` gets `cpu.weight=51` (via the formula `max(2, min(262144, (1024 * milliCPU) / 1000))`). A pod requesting `2000m` gets `cpu.weight=204`. The result is that under contention, pods with higher CPU requests get proportionally more CPU — they are not throttled, they are favored.

```bash
# Quota and period — how cpu.max translates to milliCores:
cat /sys/fs/cgroup/kubepods/pod<uid>/cpu.max
# 50000 100000  →  500m CPU limit

# Throttle statistics:
cat /sys/fs/cgroup/kubepods/pod<uid>/cpu.stat
# usage_usec, user_usec, system_usec
# nr_periods, nr_throttled, throttled_usec  ← the health metrics

# Weight (proportional scheduling priority):
cat /sys/fs/cgroup/kubepods/pod<uid>/cpu.weight

# bpftrace: trace throttle events
bpftrace -e 'kprobe:throttle_cfs_rq {
    printf("throttled: pid=%d comm=%s\n", pid, comm);
}'
```

## The IO Controller

### Why I/O Limiting Is Harder

Memory has a clean accounting model: each page is owned by one process at a time. CPU has a clean scheduling model: a task either runs or it doesn't. I/O has neither.

A single `read()` call may trigger readahead — the kernel speculatively fetches pages beyond what was requested, believing the process will need them. Those readahead pages are charged to the process that triggered them, but they may actually serve other processes. Buffered writes are not immediately sent to the device — they sit in the page cache, and the kernel decides when to flush them. A write from one process may be merged with writes from another before hitting the disk. Tracking which cgroup is responsible for which I/O is genuinely complicated.

The io controller hooks into `submit_bio()` — the function called whenever a bio (block I/O request) is submitted to the block layer — and routes each bio through per-cgroup accounting and throttling code. The primary struct is `struct blkcg`:

```c
// include/linux/blk-cgroup.h
struct blkcg {
    struct cgroup_subsys_state  css;
    spinlock_t                  lock;
    struct radix_tree_root      blkg_tree;   /* per-device state */
    struct blkcg_gq __rcu      *blkg_hint;  /* cached last-used device state */
    struct hlist_head           blkg_list;
    struct blkcg_policy_data   *cpd[BLKCG_MAX_POLS];
    struct list_head            all_blkcgs_node;
};
```

The `blkg_tree` (block-cgroup-queue tree) maps device major:minor numbers to `struct blkcg_gq` — the per-device-per-cgroup state. When a bio arrives from a process, the kernel looks up the `blkcg_gq` for this cgroup on this device and applies the throttle or weight configured there.

### io.max, io.weight, io.latency

`io.max` is rate limiting: `8:0 rbps=10485760 wbps=10485760` limits reads and writes on device `8:0` (typically `/dev/sda`) to 10 MB/s each. This is a hard cap — I/O exceeding the limit is held in a throttle queue and issued only when the rate allows. Kubernetes does not set `io.max` from standard Pod spec fields; it requires device-level knowledge that the kubelet doesn't have.

`io.weight` is proportional scheduling, analogous to `cpu.weight`. It only has effect when multiple cgroups are competing for the same device and the block scheduler supports it (BFQ, the Budget Fair Queueing scheduler, does; mq-deadline also has cgroup-aware variants). On a heavily loaded database node with multiple pods reading from the same SSD, `io.weight` determines which pods get their I/O requests satisfied first.

`io.latency` is a latency-target mode unique to the io controller. Rather than specifying a rate limit, you specify a target latency in milliseconds per device. The kernel monitors the actual I/O latency for this cgroup and throttles other cgroups (those with lower weight or no explicit target) when latency exceeds the target. This is useful for latency-sensitive workloads like etcd, where you want to guarantee that disk latency stays below a threshold rather than guaranteeing a specific throughput.

```bash
# I/O stats per device (rbytes, wbytes, rios, wios, dbytes, dios):
cat /sys/fs/cgroup/kubepods/pod<uid>/io.stat

# Set a bandwidth limit:
lsblk -no MAJ:MIN /dev/sda
echo "8:0 rbps=52428800 wbps=52428800" > /sys/fs/cgroup/mygroup/io.max

# PSI pressure for I/O:
cat /sys/fs/cgroup/kubepods/pod<uid>/io.pressure
```

## The pids Controller

### The Fork Bomb Problem

The pids controller is the conceptually simplest of the four, but it protects against a critical class of failure: fork bombs.

A fork bomb is a program that calls `fork()` in a tight loop. Each child is a new process consuming a PID, kernel stack, page tables, file descriptor table, and other per-process kernel structures. Given enough iterations, the system runs out of PIDs (`/proc/sys/kernel/pid_max`, typically 4 million) or exhausts kernel memory for per-process data structures. On a Kubernetes node without PID limits, a single container could render all other pods on the node unable to spawn new processes — not because of CPU or memory exhaustion, but because the PID space is depleted.

The pids controller prevents this with a counter and a limit:

```c
// kernel/cgroup/pids.c
struct pids_cgroup {
    struct cgroup_subsys_state  css;

    atomic64_t  counter;      /* pids.current */
    atomic64_t  limit;        /* pids.max */
    local_t     events_limit; /* pids.events.max — times limit was hit */
};
```

The counter tracks all threads and processes in the cgroup — not just direct forks. A process with 8 goroutines counts as 9 (process + 8 threads, since Go uses OS threads). The limit applies to the combined total.

### Enforcement: EAGAIN Not SIGKILL

A crucial design choice: when a fork hits the pids limit, `pids_fork()` returns `-EAGAIN`. This propagates to `clone()` as an error return, not a signal. The new process is never created; the parent receives an error and can choose to handle it. The pids controller does not kill any existing process — it only prevents new ones from being created.

This is the right behavior. Killing an existing process to make room for a new fork would be non-deterministic and potentially dangerous. Returning EAGAIN at fork time is visible to the application as a controlled failure that can be caught, logged, and handled.

```
clone()/fork() → copy_process()
  └─ pids_fork()                # kernel/cgroup/pids.c
       └─ pids_try_charge(cgroup, 1)
            ├─ [counter + 1 <= limit] → atomic64_inc(&counter); return 0
            └─ [counter >= limit] → atomic_long_inc(&events_limit)
                                     return -EAGAIN
                                          → clone() returns EAGAIN
                                          → shell: "fork: retry: Resource temporarily unavailable"
```

```bash
# Current process/thread count in a pod:
cat /sys/fs/cgroup/kubepods/pod<uid>/pids.current

# The limit (set by kubelet from PodPidsLimit feature):
cat /sys/fs/cgroup/kubepods/pod<uid>/pids.max

# How many times fork was rejected:
cat /sys/fs/cgroup/kubepods/pod<uid>/pids.events
# max N

# bpftrace: catch pids.max rejections
bpftrace -e 'kretprobe:pids_try_charge {
    if (retval < 0) {
        printf("pids.max hit: pid=%d comm=%s\n", pid, comm);
    }
}'
```

Kubernetes enables per-pod PID limits via the `PodPidsLimit` feature gate (GA since 1.20) and the `--pod-max-pids` kubelet flag. Without these, `pids.max` is set to `max` (unlimited) and fork bombs are possible.

## From Pod Spec to Kernel Enforcement

Every line of a Kubernetes resource spec maps to a specific cgroup file written by the kubelet:

| Pod spec field | cgroup file written | Value | Kernel enforcement |
|----------------|---------------------|-------|-------------------|
| `limits.memory: 256Mi` | `memory.max` | `268435456` | OOM kill via `mem_cgroup_out_of_memory()` |
| `requests.memory: 128Mi` | `memory.min` | `134217728` | Reclaim protection in `mm/vmscan.c` |
| `limits.cpu: 1` | `cpu.max` | `100000 100000` | CFS throttle via `throttle_cfs_rq()` |
| `requests.cpu: 500m` | `cpu.weight` | `51` | Proportional fair scheduling via `cfs_rq.shares` |
| `requests.cpu: 500m` | `cpu.max` | only set if limits.cpu also set | None (requests alone don't throttle) |
| kubelet `--pod-max-pids` | `pids.max` | integer | Fork rejection via `pids_try_charge()` |

The `cpu.weight` formula deserves a note: `max(2, min(262144, (1024 * milliCPU) / 1000))`. This maps 1000m (1 CPU) to cpu.weight 1024, which matches the traditional CFS weight for nice value 0. The factor of 1024 was chosen to align with the pre-cgroup-v2 `cpu.shares` default so that existing tools interpreting CPU shares would see consistent values.

Notice that `requests.cpu` without `limits.cpu` only writes `cpu.weight` — it does not set `cpu.max`. A burstable pod (requests but no limits) will get proportionally more CPU when the node is under contention, but it is not throttled when the node has spare capacity. Only `limits.cpu` installs the hard quota.

```bash
# Verify the kubelet's writes for a running pod:
POD_UID="abc123..."  # from kubectl get pod -o jsonpath='{.metadata.uid}'
CGROUP="/sys/fs/cgroup/kubepods/burstable/pod${POD_UID}"

cat $CGROUP/memory.max     # hard limit (from limits.memory)
cat $CGROUP/memory.min     # protection floor (from requests.memory, if set)
cat $CGROUP/cpu.max        # quota/period (from limits.cpu)
cat $CGROUP/cpu.weight     # shares (from requests.cpu)
cat $CGROUP/pids.max       # PID limit (from kubelet flag, if enabled)

# bpftrace: observe all cgroup file writes (catch kubelet activity)
bpftrace -e 'kprobe:cgroup_file_write {
    printf("write: pid=%d comm=%s\n", pid, comm);
}'
```

## Key Kernel References

| Symbol | File | Link |
|--------|------|------|
| `struct cgroup_subsys` | include/linux/cgroup-defs.h | https://elixir.bootlin.com/linux/v6.9/source/include/linux/cgroup-defs.h |
| `struct cgroup_subsys_state` | include/linux/cgroup-defs.h | https://elixir.bootlin.com/linux/v6.9/source/include/linux/cgroup-defs.h |
| `struct mem_cgroup` | mm/memcontrol.h | https://elixir.bootlin.com/linux/v6.9/source/mm/memcontrol.h |
| `struct page_counter` | include/linux/page_counter.h | https://elixir.bootlin.com/linux/v6.9/source/include/linux/page_counter.h |
| `mem_cgroup_charge()` | mm/memcontrol.c | https://elixir.bootlin.com/linux/v6.9/source/mm/memcontrol.c |
| `mem_cgroup_out_of_memory()` | mm/memcontrol.c | https://elixir.bootlin.com/linux/v6.9/source/mm/memcontrol.c |
| `struct task_group` | kernel/sched/sched.h | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/sched.h |
| `struct cfs_bandwidth` | kernel/sched/sched.h | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/sched.h |
| `throttle_cfs_rq()` | kernel/sched/fair.c | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/fair.c |
| `struct blkcg` | include/linux/blk-cgroup.h | https://elixir.bootlin.com/linux/v6.9/source/include/linux/blk-cgroup.h |
| `struct pids_cgroup` | kernel/cgroup/pids.c | https://elixir.bootlin.com/linux/v6.9/source/kernel/cgroup/pids.c |
| `pids_fork()` | kernel/cgroup/pids.c | https://elixir.bootlin.com/linux/v6.9/source/kernel/cgroup/pids.c |
