# 04-c — OOM Killer and PSI: Reading Memory Pressure

## When Memory Runs Out

Memory pressure in Linux kills containers in two distinct ways, and most operators only know about one of them.

The well-known way is the **OOM kill**: the cgroup's memory limit is exhausted, a `try_charge()` call in the memory controller fails to find available memory after reclaim, and the kernel invokes `mem_cgroup_out_of_memory()`. It selects a process based on the `oom_score_adj` + memory usage heuristic, sends it `SIGKILL`, and waits for the pages to return. Kubernetes sees this as a container exiting with `OOMKilled` reason and `exit code 137`. The fix looks obvious: raise the memory limit.

The subtler way is **PSI (Pressure Stall Information)**: the kernel measures the fraction of time that tasks are stalled waiting for memory — not yet killed, but unable to make progress because page reclaim cannot keep up with demand. A container might be alive, responsive to health checks, and within its memory limit while secretly spending 20% of its wall-clock time stalled in `direct reclaim`. PSI surfaces this. Kubernetes 1.22+ uses PSI data to improve eviction decisions, preferring to evict pods that are causing pressure before they trigger OOM kills.

The OOM killer and PSI together answer the two questions about memory: "did this container die because of memory?" and "is this container about to die because of memory?" Both require understanding the kernel's memory pressure infrastructure.

## Source Locations

| File | Link | Contents |
|------|------|----------|
| `mm/oom_kill.c` | https://elixir.bootlin.com/linux/v6.9/source/mm/oom_kill.c | OOM killer — `out_of_memory()`, `oom_kill_process()`, `oom_badness()` |
| `mm/memcontrol.c` | https://elixir.bootlin.com/linux/v6.9/source/mm/memcontrol.c | Cgroup memory controller — `mem_cgroup_out_of_memory()` |
| `kernel/sched/psi.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/psi.c | PSI accounting — `psi_task_change()`, `psi_group_change()` |
| `include/linux/psi_types.h` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/psi_types.h | `struct psi_group`, `enum psi_task_count`, `enum psi_states` |

---

## The OOM Killer

When the kernel fails to reclaim enough physical memory to satisfy an allocation, it must either stall forever or sacrifice a process. The OOM (Out-Of-Memory) killer is the mechanism that makes this decision: it selects the "worst" process, sends it SIGKILL, and waits for the victim to release its pages.

### When Does OOM Fire?

The OOM killer is reached through the slow allocation path after reclaim has been exhausted:

```
alloc_pages(GFP_KERNEL, order)
  └─ __alloc_pages()                       // mm/page_alloc.c
       ├─ get_page_from_freelist()          // fast path — fails under pressure
       └─ __alloc_pages_slowpath()          // slow path
            ├─ wake_all_kswapds()           // ask kswapd to reclaim
            ├─ try_to_free_pages()          // direct reclaim — shrink_node() etc.
            └─ [reclaim did not free enough, watermarks still not met]
                 should_alloc_retry()       // decide: retry or give up
                   └─ [retry budget exhausted]
                        out_of_memory()     // mm/oom_kill.c — the OOM killer entry point
                          ├─ [cgroup OOM — allocation is charged to a cgroup]
                          │    mem_cgroup_out_of_memory()   // mm/memcontrol.c
                          │      └─ select task within the offending cgroup's task list
                          └─ [global OOM — system-wide memory exhaustion]
                               select_bad_process()         // score all processes
                                 └─ oom_kill_process()      // send SIGKILL to the victim
```

The distinction between the two OOM paths matters operationally:

- **Cgroup OOM** fires when a cgroup hits its `memory.max` limit and its own reclaim cannot bring usage below the limit. The victim is chosen only from tasks within that cgroup. This is what Kubernetes observes when a Pod exceeds its `resources.limits.memory`.
- **Global OOM** fires when the entire system runs out of anonymous + swap memory. The victim is chosen from all tasks system-wide, weighted by `oom_badness()`. A global OOM on a Kubernetes node is a severe event that usually indicates the sum of Pod limits exceeds available RAM.

### `oom_badness()` — Scoring All Processes

Source: [mm/oom_kill.c](https://elixir.bootlin.com/linux/v6.9/source/mm/oom_kill.c)

Before the kernel can kill anything, it must choose which process to sacrifice. The scoring function `oom_badness()` computes a raw page-count score for each candidate (the caller normalises to 0–1000 for `/proc/<pid>/oom_score`):

```
oom_badness(task, totalpages)
  ├─ points = get_mm_rss(task->mm)     // resident anonymous + file pages
  │         + get_mm_counter(task->mm, MM_SWAPENTS)  // swap usage
  │         + mm_pgtables_bytes(task->mm) / PAGE_SIZE // page table overhead
  │
  ├─ adj = task->signal->oom_score_adj    // userspace adjustment (-1000 to +1000)
  │
  ├─ [adj == OOM_SCORE_ADJ_MIN (-1000)]
  │    return LONG_MIN   // never kill — exempt from OOM selection
  │
  └─ adj *= totalpages / 1000          // scale adj to page units:
       points += adj                   //   adj=+1000 adds ~totalpages (big boost to large processes)
       return points                   //   adj=-999 nearly zeroes the score
                                       // caller: oom_score = points * 1000 / totalpages → 0-1000
```

The process with the highest `badness` score is selected. The formula rewards killing large-RSS processes (which free the most memory) and processes that have opted in to being killed first via a high `oom_score_adj`.

### `/proc/<pid>/oom_score` and `/proc/<pid>/oom_score_adj`

| File | Range | Meaning |
|------|-------|---------|
| `/proc/<pid>/oom_score` | 0–1000 | Kernel-computed badness score. Read-only. 0 = exempt (adj = -1000). Higher = more likely to be killed. |
| `/proc/<pid>/oom_score_adj` | -1000 to +1000 | Userspace adjustment. Writable. -1000 = never kill. +1000 = kill first. Inherited by child processes. |

**Kubernetes QoS classes set `oom_score_adj` at Pod startup:**

| QoS Class | `oom_score_adj` | Rationale |
|-----------|-----------------|-----------|
| Guaranteed | -997 | Requests == limits for all containers; protect these Pods. |
| Burstable | 2–999 (proportional to request fraction) | Partial guarantees — kill before Guaranteed, after BestEffort. |
| BestEffort | 1000 | No requests or limits; kill these first when the node is pressured. |

The kubelet sets these values via `/proc/<pid>/oom_score_adj` immediately after forking each container process. This means even without cgroup memory limits, Kubernetes achieves a predictable kill order under node-level memory pressure.

### `oom_kill_process()` — The Kill Path

```
oom_kill_process(oc, message)             // oc = struct oom_control
  └─ __oom_kill_process(victim, message)
       ├─ pr_err("Killed process %d (%s) ...")   // writes the "Killed process" dmesg line
       ├─ set_bit(MMF_OOM_VICTIM, &victim->mm->flags)
       │    // marks the mm as an OOM victim — mmap_lock acquisition will yield to the victim
       │    // so it can exit faster without contending on the address space lock
       ├─ do_send_sig_info(SIGKILL, SEND_SIG_PRIV, victim, PIDTYPE_TGID)
       │    // deliver SIGKILL to every thread in the victim's thread group
       └─ [OOM reaper — mm/oom_kill.c]
            wake_up_process(oom_reaper_th)   // wake oom_reaper kernel thread
              └─ oom_reap_task()
                   └─ unmap_page_range()     // forcibly unmap victim's VMAs
                        → pages freed back to buddy allocator
                        // happens before victim even calls exit() — speeds up memory recovery
```

The `MMF_OOM_VICTIM` flag is central to the OOM reaper design. When set, the kernel's `mmap_lock` trylock paths will yield rather than spin, ensuring that the OOM reaper thread can walk and unmap the victim's address space quickly without deadlocking on the lock held by another process. This decouples the memory release from the victim's own exit path, which may be slow if it is blocked in I/O or waiting for a mutex.

### OOM Events in `memory.events`

The cgroup v2 file `memory.events` exposes two OOM-related counters:

| Counter | Meaning |
|---------|---------|
| `oom` | `memory.max` was hit and the cgroup's own reclaim failed to bring usage below the limit. Incremented every time the OOM path is entered for this cgroup. Does not necessarily mean a process was killed (an OOM-protected process might absorb the failure as an allocation error). |
| `oom_kill` | The OOM killer selected a task within this cgroup and sent it SIGKILL. Incremented once per kill event. |

The distinction matters: `oom` counts limit-hit events; `oom_kill` counts actual kills. A container can have many `oom` events without any `oom_kill` if the process handles `ENOMEM` gracefully or if an OOM policy prevents killing. Typically in Kubernetes, `oom_kill > 0` is what triggers a `OOMKilled` pod restart.

---

## PSI: Pressure Stall Information

PSI was added in Linux 4.20 (October 2018). It solves a problem that RSS and free-memory metrics cannot: they tell you how much memory is used, but not whether tasks are being delayed waiting for it. A node might have 10% free memory but zero stall if the workload fits in the remaining 90%, or it might show the same 10% free while dozens of processes block on page reclaim.

PSI measures the **fraction of time tasks spend stalled** waiting for a resource. Three resources are tracked: `cpu`, `memory`, and `io`.

Source: [kernel/sched/psi.c](https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/psi.c)

### PSI Interface Files

**System-wide (one file per resource):**
```
/proc/pressure/cpu
/proc/pressure/memory
/proc/pressure/io
```

**Per-cgroup (requires CONFIG_PSI and cgroup PSI enabled at boot):**
```
/sys/fs/cgroup/<path>/cpu.pressure
/sys/fs/cgroup/<path>/memory.pressure
/sys/fs/cgroup/<path>/io.pressure
```

In Kubernetes, each Pod's cgroup is at `/sys/fs/cgroup/kubepods/pod<uid>/`. PSI files there reflect pressure only from tasks within that Pod's cgroup subtree — exactly what you need for per-Pod diagnosis.

### PSI File Format

```
some avg10=0.14 avg60=0.07 avg300=0.01 total=182364
full avg10=0.00 avg60=0.00 avg300=0.00 total=0
```

**`some` line:** At least one task in the tracked set was stalled during the measurement window. Other tasks could still make progress. For CPU pressure, `some` means at least one runnable task was waiting for a CPU slot. For memory pressure, at least one task was sleeping in `direct_reclaim` or waiting for a page fault to complete.

**`full` line:** ALL runnable tasks were simultaneously stalled. The system made zero forward progress during those intervals. For memory, this means every runnable task was blocked on reclaim at the same instant. For CPU, it is undefined (one CPU is always available if any task is runnable, so `full` is always zero for `cpu`). For I/O, it means all runnable tasks were blocked on I/O simultaneously.

**`avg10`, `avg60`, `avg300`:** Exponential moving averages of the stall fraction over the last 10 seconds, 60 seconds, and 300 seconds, expressed as a percentage (0.00–100.00). A value of `15.00` means 15% of the time in that window, tasks were stalled.

**`total`:** Cumulative microseconds spent in a stall state since boot. Use this for precise delta calculations between two point-in-time readings.

### `struct psi_group` — The Core Data Structure

Source: [include/linux/psi_types.h](https://elixir.bootlin.com/linux/v6.9/source/include/linux/psi_types.h)

Every cgroup and the system-wide PSI state is tracked in a `struct psi_group`:

```c
// include/linux/psi_types.h
struct psi_group {
    struct mutex         avgs_lock;
    struct psi_group_cpu __percpu *pcpu;  // per-CPU state — updated on every task switch
    u64                  avg_last_update;
    u64                  avg_next_update;
    struct delayed_work  avgs_work;       // periodic work: recalculates moving averages
    u64                  total[NR_PSI_AGGREGATORS][NR_PSI_STATES];  // raw stall time accumulators
    unsigned long        avg[NR_PSI_STATES][3]; // avg10, avg60, avg300 for each PSI state
    /* poll/notification fields */
    struct list_head     poll_triggers;   // registered threshold triggers
    u32                  poll_min_period;
    struct kthread_worker *poll_kworker;  // kthread that checks triggers
    struct kthread_delayed_work poll_work;
    wait_queue_head_t    poll_wait;
    atomic_t             poll_scheduled;
};
```

**Key field explanations:**

`pcpu` is a per-CPU `struct psi_group_cpu`. Each CPU updates its own structure on every scheduler context switch without needing a lock — this is the key to PSI's low overhead. The per-CPU counts are aggregated by `avgs_work` into the global `total` array.

`avgs_work` is a `delayed_work` that fires periodically (default every 2 seconds) to recompute the exponential moving averages from the accumulated `total` counters. The work item runs in a kernel workqueue thread, not in the hot path.

`total[NR_PSI_AGGREGATORS][NR_PSI_STATES]` stores the raw accumulated stall time in nanoseconds, split by state. `NR_PSI_AGGREGATORS` separates `some` from `full` counting.

`avg[NR_PSI_STATES][3]` holds the three moving averages per state in fixed-point format (scaled by `FIXED_1`), decoded to percentage strings when the PSI file is read.

### PSI Task States

Source: [include/linux/psi_types.h](https://elixir.bootlin.com/linux/v6.9/source/include/linux/psi_types.h)

```c
enum psi_task_count {
    NR_IOWAIT,           // waiting for I/O — in iowait sleep
    NR_MEMSTALL,         // stalled on memory reclaim (direct reclaim, page fault wait)
    NR_RUNNING,          // runnable but not necessarily on CPU (includes on-CPU tasks)
    NR_ONCPU,            // actually executing on a CPU right now
    NR_MEMSTALL_RUNNING, // both running on CPU AND in a memory stall context
                         // (e.g., a kernel thread reclaiming memory while scheduled)
};
```

The scheduler calls `psi_task_change()` on every context switch and whenever a task transitions between these states. The function updates the per-CPU counts atomically and without taking any global lock, which is why PSI overhead is measured at less than 1% in most benchmarks.

**How `some` and `full` are derived from these counts:**

- **Memory `some`**: `NR_MEMSTALL > 0` — at least one task is in a memory stall.
- **Memory `full`**: `NR_MEMSTALL > 0 AND NR_RUNNING == NR_MEMSTALL_RUNNING` — every runnable task is either stalling for memory or actively performing reclaim. This means the workload as a whole is blocked.
- **I/O `some`**: `NR_IOWAIT > 0` — at least one task is in I/O wait.
- **I/O `full`**: `NR_IOWAIT > 0 AND NR_RUNNING == 0` — all tasks are in I/O wait; none are runnable.

The `full` condition is stringent and represents genuine saturation. A memory `full` stall of even a few percent is operationally significant — it means the application is intermittently completely frozen waiting for the kernel to reclaim pages.

### PSI Threshold Notifications (Poll-Based)

PSI supports `poll()`-based notifications so that daemons can react to pressure events without polling on a timer. A trigger is written to the PSI file describing a stall budget and window:

```c
// Write trigger: "some 50000 1000000"
//   → notify when 'some' stall exceeds 50ms in any 1-second window
int fd = open("/sys/fs/cgroup/kubepods/pod<uid>/memory.pressure", O_RDWR | O_NONBLOCK);
write(fd, "some 50000 1000000", 18);

// poll() returns POLLPRI when the threshold is exceeded
struct pollfd pfd = { .fd = fd, .events = POLLPRI };
poll(&pfd, 1, -1);   // blocks until stall > 50ms/s
```

Format of the trigger string: `"<some|full> <stall_us> <window_us>"`

- `stall_us`: maximum tolerated stall in microseconds within the window before notifying.
- `window_us`: the measurement window in microseconds (minimum 500ms, maximum 10s).

**Users of PSI notifications:**

| Daemon | Trigger | Action |
|--------|---------|--------|
| `systemd-oomd` | memory `some` > configurable threshold | Proactively kills cgroups before kernel OOM fires |
| kubelet eviction manager | memory `some` on node or pod cgroup | Evicts Pods to prevent node OOM — softer than kernel OOM kill |
| Facebook's `oomd` (open-source) | memory and I/O `full` | Kills the highest-memory cgroup within the affected subtree |

The key advantage over `memory.events` is that PSI notifications fire before OOM: the eviction manager can remove a Pod gracefully (with a termination grace period, log draining, etc.) rather than having the kernel send SIGKILL with no warning.

---

## Practical Observation

```bash
# System-wide memory pressure (check avg10 for recent, avg300 for sustained):
cat /proc/pressure/memory

# Pod-level PSI (replace <uid> with the actual pod UID from `crictl pods`):
POD_CG=/sys/fs/cgroup/kubepods/pod<uid>
cat $POD_CG/memory.pressure
cat $POD_CG/cpu.pressure
cat $POD_CG/io.pressure

# OOM events for a pod (oom = limit hit, oom_kill = process killed):
cat $POD_CG/memory.events

# bpftrace: trace OOM kills — __oom_kill_process(victim, message) where arg0 is task_struct*
# Note: probe __oom_kill_process, not oom_kill_process — the outer function receives
# struct oom_control* (arg0) and const char* (arg1); the task is inside oc->chosen.
bpftrace -e 'kprobe:__oom_kill_process {
    $task = (struct task_struct *)arg0;
    printf("OOM kill: pid=%d comm=%s\n", $task->pid, $task->comm);
}'

# bpftrace: trace PSI state transitions — watch which pids enter memory stall
bpftrace -e 'kprobe:psi_task_change {
    printf("psi change: pid=%d comm=%s flags=%d\n", pid, comm, arg2);
}'

# Watch oom_score for a running process:
watch -n 1 'cat /proc/$PID/oom_score /proc/$PID/oom_score_adj'

# Show oom_score_adj for all containers (useful for verifying QoS class assignments):
for pid in $(pgrep -d " " -f ""); do
    adj=$(cat /proc/$pid/oom_score_adj 2>/dev/null)
    [ -n "$adj" ] && echo "$adj $(cat /proc/$pid/comm 2>/dev/null) (pid=$pid)"
done | sort -n

# System-wide OOM events from kernel ring buffer:
dmesg | grep -i "oom\|killed process\|out of memory"

# Continuous delta on PSI total to calculate stall rate over a 10s interval:
while true; do
    t1=$(awk '/^some/ {print $NF}' /proc/pressure/memory | cut -d= -f2)
    sleep 10
    t2=$(awk '/^some/ {print $NF}' /proc/pressure/memory | cut -d= -f2)
    echo "memory stall last 10s: $(( (t2 - t1) / 1000 )) ms"
done
```

### Interpreting PSI Output

An `avg10` above **10%** on `memory some` indicates meaningful stall — investigate which Pod's cgroup is contributing by checking per-Pod `memory.pressure` files. An `avg10` above **50%** means the workload is heavily memory-constrained and latency will be substantially impacted.

`full` values above **1%** are a strong signal of saturation. Even brief `full` stalls cause complete application freezes from the perspective of end-to-end latency.

When `dmesg` shows repeated OOM kill lines, cross-reference with `memory.events` counters on the affected Pod cgroup: `oom_kill` incrementing confirms the kernel killed a process within that cgroup, while a high `oom` count with low `oom_kill` suggests the workload is hitting the limit frequently but recovering through its own error handling.

---

## Key Kernel References

| Symbol | File | Link | Purpose |
|--------|------|------|---------|
| `out_of_memory()` | `mm/oom_kill.c` | https://elixir.bootlin.com/linux/v6.9/source/mm/oom_kill.c | OOM killer entry point — selects victim and triggers kill |
| `oom_kill_process()` | `mm/oom_kill.c` | https://elixir.bootlin.com/linux/v6.9/source/mm/oom_kill.c | Sets `MMF_OOM_VICTIM`, delivers SIGKILL, wakes OOM reaper |
| `oom_badness()` | `mm/oom_kill.c` | https://elixir.bootlin.com/linux/v6.9/source/mm/oom_kill.c | Computes per-process OOM score (0–1000) factoring RSS and `oom_score_adj` |
| `select_bad_process()` | `mm/oom_kill.c` | https://elixir.bootlin.com/linux/v6.9/source/mm/oom_kill.c | Iterates all processes, calls `oom_badness()`, picks the highest-scoring victim |
| `struct psi_group` | `include/linux/psi_types.h` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/psi_types.h | Per-cgroup PSI accounting state — averages, totals, poll triggers |
| `enum psi_task_count` | `include/linux/psi_types.h` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/psi_types.h | Task state categories tracked by PSI on each context switch |
