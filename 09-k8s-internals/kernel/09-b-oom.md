# 09-b: OOM Killer — `struct oom_control`, `oom_badness()`, `memory.events`

## The Decision to Sacrifice One to Save the Rest

In 1991, when Linus Torvalds released the first Linux kernel, running out of memory was a fatal condition. The kernel would attempt to allocate a page, find nothing available, and panic. This was acceptable for a research operating system. It was not acceptable for a production server that might run for months without intervention.

The OOM killer was the kernel's attempt at graceful degradation: when memory allocation fails and reclaim cannot help, instead of halting the machine, find the process most responsible for the memory crisis and kill it. The idea is brutal but sound — one process dies so that all the others can continue. The mechanism was added to Linux in the late 1990s and has been tuned, criticized, and improved ever since.

The hard problem is choosing which process to kill. Kill a critical system process and you've traded an OOM panic for a different kind of system failure. Kill a small, innocent process that happened to be in the wrong cgroup and you've wasted a kill without recovering meaningful memory. The `oom_badness()` function is the kernel's scoring algorithm: it computes a score for each process as a function of its memory footprint (RSS + swap) relative to the total memory in the relevant scope, adjusted by `oom_score_adj`. Higher score means more likely to be killed.

Kubernetes controls `oom_score_adj` deliberately. BestEffort pods get `oom_score_adj = 1000` — they are killed first, always. Burstable pods get values proportional to their memory requests vs limits ratio. Guaranteed pods get `oom_score_adj = -997` — the kernel will kill almost anything else first. This is not a Kubernetes invention; it is a direct mapping onto a kernel mechanism that has existed for decades, now used to implement Kubernetes QoS semantics. Understanding `oom_badness()` is understanding why your BestEffort pods die first when a node runs out of memory, and why your Guaranteed pods survive.

## 1. Source Locations

| File | Key Symbols | URL |
|------|-------------|-----|
| `mm/oom_kill.c` | `out_of_memory()`, `oom_kill_process()`, `select_bad_process()`, `oom_badness()` | https://elixir.bootlin.com/linux/v6.9/source/mm/oom_kill.c |
| `include/linux/oom.h` | `struct oom_control`, `OOM_SCORE_ADJ_MIN`, `OOM_SCORE_ADJ_MAX` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/oom.h |
| `fs/proc/base.c` | `proc_oom_score_read()`, `proc_oom_score_adj_write()` | https://elixir.bootlin.com/linux/v6.9/source/fs/proc/base.c |
| `mm/memcontrol.c` | `mem_cgroup_out_of_memory()`, `memory_events` counters | https://elixir.bootlin.com/linux/v6.9/source/mm/memcontrol.c |
| `include/linux/memcontrol.h` | `struct mem_cgroup`, `enum memcg_memory_event` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/memcontrol.h |

## 2. `struct oom_control`

```c
// include/linux/oom.h (Linux 6.9)
struct oom_control {
    struct zonelist         *zonelist;      // NUMA zone list for allocation
    nodemask_t              *nodemask;      // allowed NUMA nodes
    struct mem_cgroup       *memcg;         // cgroup that triggered OOM (NULL = global)
    const gfp_t             gfp_mask;       // allocation flags that triggered OOM
    const int               order;          // allocation order (0 = page, 9 = 2MB)
    unsigned long           totalpages;     // total pages available (for badness scaling)
    struct task_struct      *chosen;        // selected victim (output)
    long                    chosen_points;  // victim's badness score (output)
    enum oom_constraint     constraint;     // CONSTRAINT_NONE / MEMORY_POLICY / CPUSET / MEMCG
};
```

The OOM killer is invoked when the page allocator fails to satisfy a GFP_KERNEL allocation after exhausting direct reclaim. Entry point: `out_of_memory()` in `mm/oom_kill.c`, called from `__alloc_pages_slowpath()` in `mm/page_alloc.c`.

## 3. `oom_badness()` — Victim Selection Algorithm

`select_bad_process()` iterates all tasks and calls `oom_badness()` for each. The task with the highest score is the victim:

```
// mm/oom_kill.c
long oom_badness(struct task_struct *p, unsigned long totalpages)
{
    // 1. Skip kernel threads and processes with OOM_SCORE_ADJ_MIN (-1000)
    if (oom_unkillable_task(p))
        return LONG_MIN;

    // 2. Base score = proportional RSS use
    //    points = (rss + pgtables + swap_entries) * 1000 / totalpages
    points = get_mm_rss(p->mm) + get_mm_counter(p->mm, MM_SWAPENTS)
             + mm_pgtables_bytes(p->mm) / PAGE_SIZE;
    points = points * 1000 / totalpages;

    // 3. Adjust by oom_score_adj (range -1000 to +1000)
    //    adj = oom_score_adj * totalpages / 1000
    adj = (long)p->signal->oom_score_adj * totalpages / 1000;

    // 4. Return adjusted score; select_bad_process() keeps the running maximum
    return points + adj;
}
```

`oom_score_adj = -1000` (OOM_SCORE_ADJ_MIN): the task is never killed. Used for critical system processes.
`oom_score_adj = +1000` (OOM_SCORE_ADJ_MAX): the task is killed first regardless of RSS.

## 4. `oom_kill_process()`

After selecting the victim, `oom_kill_process()`:
1. Sends `SIGKILL` to the victim task and all tasks sharing its mm (threads sharing memory via `for_each_process`/`for_each_thread` over `mm_users`).
2. Calls `mark_oom_victim(p)` which sets `TIF_MEMDIE` on the task — giving it access to memory reserves to exit quickly.
3. Calls `wake_oom_reaper()` which wakes a dedicated kernel thread (`oom_reaper`) to asynchronously free the victim's anonymous memory without waiting for the victim to schedule.

## 5. cgroup OOM — `memory.events`

When a cgroup's memory usage hits `memory.max`, the memory controller calls `mem_cgroup_out_of_memory()` in `mm/memcontrol.c` instead of the global OOM killer. This triggers within the cgroup scope: only tasks in the cgroup are considered for killing.

The result is recorded in `memory.events`:

```
# cat /sys/fs/cgroup/kubepods.slice/.../memory.events
low 0
high 12
max 3
oom 1
oom_kill 1
oom_group_kill 0
```

- `low`: reclaim triggered because usage crossed the soft low threshold
- `high`: direct reclaim triggered (usage at `memory.high`)
- `max`: allocation forced direct reclaim (usage at `memory.max`)
- `oom`: OOM condition detected within cgroup
- `oom_kill`: number of tasks killed by OOM within cgroup
- `oom_group_kill`: number of times the whole cgroup was killed (via `memory.oom.group`)

## 6. `/proc/<pid>/oom_score` and `/proc/<pid>/oom_score_adj`

`/proc/<pid>/oom_score` (read-only): the current badness score as computed by `oom_badness()`. Normalized to 0–2000 by `proc_oom_score_read()`.

`/proc/<pid>/oom_score_adj` (read-write, range -1000 to +1000): the per-process adjustment. Inherited by children across `fork()`. Kubelet writes this after forking each container process:

| QoS class | oom_score_adj | Rationale |
|-----------|---------------|-----------|
| Guaranteed | -998 | Near-immune; -1000 reserved for kubelet itself |
| Burstable | 2–999 | Proportional to memory request vs node capacity |
| BestEffort | 1000 | First to be killed |

## 7. Live Observation

```bash
# Show OOM score for all processes
awk '{printf "%-8s %s\n", $1, FILENAME}' /proc/*/oom_score 2>/dev/null | sort -rn | head -10

# Watch OOM kills in real time
bpftrace -e 'kprobe:oom_kill_process { printf("OOM kill: pid=%d comm=%s\n", pid, comm); }'

# Monitor memory.events for a pod
PODCG=/sys/fs/cgroup/kubepods.slice/kubepods-pod<uid>.slice
watch -n1 "cat $PODCG/memory.events"

# inotify-based OOM detection (as kubelet does it)
inotifywait -m -e modify $PODCG/memory.events
```

## 8. Key Kernel References

| Symbol | File | URL |
|--------|------|-----|
| `out_of_memory()` | `mm/oom_kill.c` | https://elixir.bootlin.com/linux/v6.9/source/mm/oom_kill.c |
| `oom_badness()` | `mm/oom_kill.c` | https://elixir.bootlin.com/linux/v6.9/source/mm/oom_kill.c |
| `struct oom_control` | `include/linux/oom.h` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/oom.h |
| `mem_cgroup_out_of_memory()` | `mm/memcontrol.c` | https://elixir.bootlin.com/linux/v6.9/source/mm/memcontrol.c |
| `wake_oom_reaper()` | `mm/oom_kill.c` | https://elixir.bootlin.com/linux/v6.9/source/mm/oom_kill.c |
| `proc_oom_score_adj_write()` | `fs/proc/base.c` | https://elixir.bootlin.com/linux/v6.9/source/fs/proc/base.c |
| `enum memcg_memory_event` | `include/linux/memcontrol.h` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/memcontrol.h |
