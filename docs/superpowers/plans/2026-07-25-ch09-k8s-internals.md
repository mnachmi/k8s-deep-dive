# Chapter 09 — Kubernetes Internals Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build Chapter 09 covering the kernel interfaces that Kubernetes uses at runtime — kubelet's /proc and /sys/fs/cgroup reads, the OOM killer and oom_score_adj, Pressure Stall Information (PSI) for eviction, and the full pod-creation chain from API server through containerd/runc to clone() — linking each mechanism to the kernel data structures that back it.

**Architecture:** Same structure as previous chapters: `kernel/` (3 deep-dive docs), `k8s/` (1 connection doc), `exercises/` (C + Go), `kube-inspect/` (checkpoint 09). C exercise reads and sets oom_score_adj via /proc — no extra libraries. Go exercise parses /proc/pressure/* and cgroup memory.events — stdlib only. kube-inspect checkpoint reports node-level PSI and per-pod OOM event counts from memory.events.

**Tech Stack:** Markdown, C (gcc, glibc only), Go 1.22, Linux 6.9 kernel source via elixir.bootlin.com

## Global Constraints

- All kernel struct fields/functions must be accurate for Linux 6.9
- All elixir.bootlin.com URLs: bare format (`https://elixir.bootlin.com/linux/v6.9/source/...`), never `[text](url)`
- C compile: `gcc -Wall -Wextra -Werror -o <name> <name>.c`
- Go: `go build ./...` + `go vet ./...` must pass
- Go module for exercises: `github.com/linux-to-k8s/<exercise-name>`, go 1.22
- No placeholder text (no TBD, TODO, etc.)
- Every kernel doc has a `## Key Kernel References` table with ≥ 5 entries (bare URLs)
- Reading order: 09-a → 09-b → 09-c → k8s-connection
- Prerequisites: Ch01 (task_struct), Ch03 (cgroups), Ch04 (memory/PSI), Ch08 (scheduler/QoS)

---

## Task 1: README + `09-a-kubelet.md` — kubelet kernel interface and pod sandbox creation

**Files:**
- Modify: `09-k8s-internals/README.md` (replace stub)
- Create: `09-k8s-internals/kernel/09-a-kubelet.md`

### `09-k8s-internals/README.md`

Replace the `(coming soon)` stub:

```markdown
# Chapter 09 — Kubernetes Internals

Every pod lifecycle event is a sequence of kernel calls. This chapter follows kubelet from its API-server watch through the CRI gRPC call to containerd, through runc's clone() invocation, and into the kernel data structures that govern resource isolation and pressure reporting. It then dives into two mechanisms kubelet relies on most heavily at runtime: the OOM killer (which enforces memory limits when reclaim fails) and Pressure Stall Information (which tells kubelet when a node or cgroup is under resource stress).

## Learning Objectives

1. Understand how kubelet reads /proc and /sys/fs/cgroup to monitor pods
2. Trace a pod sandbox creation from RunPodSandbox (CRI) through runc to the kernel clone() call
3. Understand the OOM killer: struct oom_control, oom_badness(), oom_score_adj, memory.events
4. Understand PSI: struct psi_group, /proc/pressure/*, per-cgroup PSI files, kubelet eviction thresholds
5. Read node and pod resource pressure from the kernel's own accounting interfaces

## Prerequisites

- Chapter 01 — Process Model (task_struct, clone, namespaces)
- Chapter 03 — cgroups (cgroup v2 hierarchy, memory.max, cpu.max)
- Chapter 04 — Memory (OOM fundamentals, reclaim)
- Chapter 08 — Scheduler (QoS classes, cpu.weight, oom_score_adj values)

## Reading Order

| File | Topic |
|------|-------|
| `kernel/09-a-kubelet.md` | kubelet kernel interface: /proc, /sys/fs/cgroup, CRI, runc, clone() |
| `kernel/09-b-oom.md` | OOM killer: struct oom_control, oom_badness, oom_score_adj, memory.events |
| `kernel/09-c-psi.md` | PSI: struct psi_group, /proc/pressure/*, cgroup PSI, kubelet eviction |
| `k8s/09-k8s-connection.md` | Full pod lifecycle: API server → kubelet → containerd → runc → kernel |
| `exercises/oom-score-demo/` | C: read/write oom_score_adj; observe OOM badness score |
| `exercises/node-pressure-reader/` | Go: parse /proc/pressure/* and cgroup memory.events |
| `kube-inspect` checkpoint 09 | Node PSI + per-pod OOM events |

## Kernel to K8s Bridge

```
API server (etcd)
    │  watch event
    ▼
kubelet (user space)
    ├─ reads /proc/<pid>/oom_score_adj          → sets per-QoS OOM priority
    ├─ reads /sys/fs/cgroup/.../memory.events   → detects OOM events
    ├─ reads /proc/pressure/{cpu,memory,io}     → node resource pressure
    ├─ reads /sys/fs/cgroup/.../memory.pressure → per-cgroup PSI
    └─ CRI gRPC → containerd → runc
                                └─ clone(CLONE_NEWPID|CLONE_NEWNET|...) → kernel
```
```

### `09-k8s-internals/kernel/09-a-kubelet.md`

Full Data Structure Deep Dive. Required sections:

**1. Source Locations**

| File | Key Symbols | URL |
|------|-------------|-----|
| `kernel/fork.c` | `copy_process()`, `kernel_clone()` | https://elixir.bootlin.com/linux/v6.9/source/kernel/fork.c |
| `fs/proc/base.c` | `/proc/<pid>/oom_score_adj`, `/proc/<pid>/oom_score` | https://elixir.bootlin.com/linux/v6.9/source/fs/proc/base.c |
| `kernel/cgroup/cgroup.c` | `cgroup_attach_task()`, `cgroup_procs_write()` | https://elixir.bootlin.com/linux/v6.9/source/kernel/cgroup/cgroup.c |
| `kernel/cgroup/cpuset.c` | `cpuset_attach()`, `cpuset_write_resmask()` | https://elixir.bootlin.com/linux/v6.9/source/kernel/cgroup/cpuset.c |
| `include/uapi/linux/sched.h` | `CLONE_NEWPID`, `CLONE_NEWNET`, `CLONE_NEWNS`, `CLONE_NEWIPC`, `CLONE_NEWUTS` | https://elixir.bootlin.com/linux/v6.9/source/include/uapi/linux/sched.h |

**2. kubelet's Kernel Interface**

kubelet communicates with the kernel exclusively through the VFS — no kernel modules, no custom syscalls:

| Interface | Path | What kubelet reads/writes |
|-----------|------|--------------------------|
| Process listing | `/proc/<pid>/status`, `/proc/<pid>/cgroup` | pid→cgroup mapping |
| OOM priority | `/proc/<pid>/oom_score_adj` | sets -998 (Guaranteed), 1000 (BestEffort) |
| Memory usage | `/sys/fs/cgroup/.../memory.current` | current RSS |
| Memory events | `/sys/fs/cgroup/.../memory.events` | oom, oom_kill counters |
| Node pressure | `/proc/pressure/memory`, `/proc/pressure/cpu` | PSI averages |
| cgroup PSI | `/sys/fs/cgroup/.../memory.pressure` | per-pod PSI |
| CPU quota | `/sys/fs/cgroup/.../cpu.max` | writes limits on pod admit |
| CPU pinning | `/sys/fs/cgroup/.../cpuset.cpus` | writes CPU affinity |

kubelet opens these files with `open(2)` and reads them with `read(2)` on a polling interval (default 10 s for resource metrics, 1 s for OOM events via inotify). For `memory.events` monitoring, kubelet registers an inotify watch so the kernel notifies it on each OOM event without polling.

**3. Pod Sandbox Creation — Kernel View**

When kubelet calls the CRI `RunPodSandbox`:

```
kubelet
  │  gRPC: RunPodSandbox(PodSandboxConfig)
  ▼
containerd (containerd.sock)
  │  starts pause container (holds network/IPC/UTS namespaces)
  │  calls: runc create --bundle <ocispec>
  ▼
runc
  │  reads OCI spec: namespaces, cgroups, mounts, seccomp
  │  calls: clone(CLONE_NEWPID|CLONE_NEWNET|CLONE_NEWNS|CLONE_NEWIPC|CLONE_NEWUTS|SIGCHLD)
  ▼
kernel: kernel_clone() → copy_process()
  │  copy_process() allocates task_struct, copies mm, files, signal, nsproxy
  │  creates new namespaces for each CLONE_NEW* flag
  │  returns: child PID visible in parent's namespace
  ▼
runc (child process in new namespaces)
  │  unshare(CLONE_NEWUSER) if user namespace enabled
  │  pivot_root() to container rootfs
  │  mount(MS_BIND) for volumes and /dev
  │  exec("/pause") — the pause container main loop
```

The pause binary runs an infinite sleep loop. Its sole purpose is to hold the network, IPC, and UTS namespaces alive so that app containers can join with `setns(2)`.

**4. Joining Namespaces — `setns(2)` Path**

When kubelet calls `CreateContainer` for an app container:

```
containerd
  │  opens /proc/<pause_pid>/ns/net   (fd for network namespace)
  │       /proc/<pause_pid>/ns/ipc   (fd for IPC namespace)
  │       /proc/<pause_pid>/ns/uts   (fd for UTS namespace)
  │  calls: runc create (second container)
  ▼
runc (app container)
  │  setns(net_fd, CLONE_NEWNET)   → joins pause's network namespace
  │  setns(ipc_fd, CLONE_NEWIPC)  → joins pause's IPC namespace
  │  setns(uts_fd, CLONE_NEWUTS)  → joins pause's UTS namespace
  │  clone(CLONE_NEWPID)          → NEW PID namespace for this container
  │  exec(container_entrypoint)
  ▼
kernel: setns() → switch_task_namespaces()
  │  replaces nsproxy fields: net_ns, ipc_ns, uts_ns
  │  increments ns reference counts
```

**5. cgroup Assignment**

After fork, runc writes the new process PID to the pod's cgroup:

```bash
echo <pid> > /sys/fs/cgroup/kubepods.slice/kubepods-pod<uid>.slice/<container>.scope/cgroup.procs
```

The kernel's `cgroup_procs_write()` in `kernel/cgroup/cgroup.c` moves the task to the new cgroup, updating all per-cgroup resource counters (memory.current, cpu.stat, etc.). After this write, the cgroup's memory controller begins charging the task's allocations to the pod's memory.max limit.

**6. Live Observation**

```bash
# Watch kubelet's file descriptor activity (what it reads at pod create)
sudo strace -f -e openat,read -p $(pidof kubelet) 2>&1 | grep -E "cgroup|pressure|oom_score"

# Trace all clone() calls from containerd (see namespace flags)
bpftrace -e '
tracepoint:syscalls:sys_enter_clone {
    if (comm == "runc:[2:INIT]") {
        printf("runc clone flags=0x%lx pid=%d\n", args->clone_flags, pid);
    }
}'

# Watch cgroup.procs writes (task assignment to pod cgroup)
bpftrace -e 'kprobe:cgroup_procs_write { printf("cgroup assign pid=%d comm=%s\n", pid, comm); }'
```

**7. Key Kernel References**

| Symbol | File | URL |
|--------|------|-----|
| `kernel_clone()` | `kernel/fork.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/fork.c |
| `copy_process()` | `kernel/fork.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/fork.c |
| `CLONE_NEW*` flags | `include/uapi/linux/sched.h` | https://elixir.bootlin.com/linux/v6.9/source/include/uapi/linux/sched.h |
| `setns(2)` → `switch_task_namespaces()` | `kernel/nsproxy.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/nsproxy.c |
| `cgroup_procs_write()` | `kernel/cgroup/cgroup.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/cgroup/cgroup.c |
| `proc_oom_score_adj_write()` | `fs/proc/base.c` | https://elixir.bootlin.com/linux/v6.9/source/fs/proc/base.c |

- [ ] **Step 1: Read the existing ch09 README stub**

```bash
cat 09-k8s-internals/README.md
```

- [ ] **Step 2: Write `09-k8s-internals/README.md`** — replace stub with full chapter intro as specified above

- [ ] **Step 3: Write `09-k8s-internals/kernel/09-a-kubelet.md`** — all 7 sections exactly as specified above

- [ ] **Step 4: Verify no placeholder text, all URLs are bare v6.9, ≥ 5 Key References**

- [ ] **Step 5: Commit**

```bash
git add 09-k8s-internals/README.md 09-k8s-internals/kernel/09-a-kubelet.md
git commit -m "docs(ch09): README + 09-a kubelet kernel interface, pod sandbox, setns, cgroup assign"
```

---

## Task 2: `09-b-oom.md` — OOM killer: struct oom_control, oom_badness, memory.events

**Files:**
- Create: `09-k8s-internals/kernel/09-b-oom.md`

Full Data Structure Deep Dive. Required sections:

**1. Source Locations**

| File | Key Symbols | URL |
|------|-------------|-----|
| `mm/oom_kill.c` | `out_of_memory()`, `oom_kill_process()`, `select_bad_process()`, `oom_badness()` | https://elixir.bootlin.com/linux/v6.9/source/mm/oom_kill.c |
| `include/linux/oom.h` | `struct oom_control`, `OOM_SCORE_ADJ_MIN`, `OOM_SCORE_ADJ_MAX` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/oom.h |
| `fs/proc/base.c` | `proc_oom_score_read()`, `proc_oom_score_adj_write()` | https://elixir.bootlin.com/linux/v6.9/source/fs/proc/base.c |
| `mm/memcontrol.c` | `mem_cgroup_out_of_memory()`, `memory_events` counters | https://elixir.bootlin.com/linux/v6.9/source/mm/memcontrol.c |
| `include/linux/memcontrol.h` | `struct mem_cgroup`, `enum memcg_memory_event` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/memcontrol.h |

**2. `struct oom_control`**

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

**3. `oom_badness()` — victim selection algorithm**

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

    // 4. Return adjusted score (minimum 1 so no zero-score processes are skipped)
    return points + adj;
}
```

`oom_score_adj = -1000` (OOM_SCORE_ADJ_MIN): the task is never killed. Used for critical system processes.
`oom_score_adj = +1000` (OOM_SCORE_ADJ_MAX): the task is killed first regardless of RSS.

**4. `oom_kill_process()`**

After selecting the victim, `oom_kill_process()`:
1. Sends `SIGKILL` to the victim task and all tasks sharing its mm (threads).
2. If the victim is a process group leader, also kills tasks in the same process group.
3. Calls `mark_oom_victim(p)` which sets `TIF_MEMDIE` on the task — giving it access to memory reserves to exit quickly.
4. Calls `wake_oom_reaper()` which wakes a dedicated kernel thread (`oom_reaper`) to asynchronously free the victim's anonymous memory without waiting for the victim to schedule.

**5. cgroup OOM — `memory.events`**

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

**6. `/proc/<pid>/oom_score` and `/proc/<pid>/oom_score_adj`**

`/proc/<pid>/oom_score` (read-only): the current badness score as computed by `oom_badness()`. Normalized to 0–2000 by `proc_oom_score_read()`.

`/proc/<pid>/oom_score_adj` (read-write, range -1000 to +1000): the per-process adjustment. Inherited by children across `fork()`. Kubelet writes this after forking each container process:

| QoS class | oom_score_adj | Rationale |
|-----------|---------------|-----------|
| Guaranteed | -998 | Near-immune; -1000 reserved for kubelet itself |
| Burstable | 2–999 | Proportional to memory request vs node capacity |
| BestEffort | 1000 | First to be killed |

**7. Live Observation**

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

**8. Key Kernel References**

| Symbol | File | URL |
|--------|------|-----|
| `out_of_memory()` | `mm/oom_kill.c` | https://elixir.bootlin.com/linux/v6.9/source/mm/oom_kill.c |
| `oom_badness()` | `mm/oom_kill.c` | https://elixir.bootlin.com/linux/v6.9/source/mm/oom_kill.c |
| `struct oom_control` | `include/linux/oom.h` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/oom.h |
| `mem_cgroup_out_of_memory()` | `mm/memcontrol.c` | https://elixir.bootlin.com/linux/v6.9/source/mm/memcontrol.c |
| `wake_oom_reaper()` | `mm/oom_kill.c` | https://elixir.bootlin.com/linux/v6.9/source/mm/oom_kill.c |
| `proc_oom_score_adj_write()` | `fs/proc/base.c` | https://elixir.bootlin.com/linux/v6.9/source/fs/proc/base.c |
| `enum memcg_memory_event` | `include/linux/memcontrol.h` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/memcontrol.h |

- [ ] **Step 1: Write `09-k8s-internals/kernel/09-b-oom.md`** — all 8 sections exactly as specified above

- [ ] **Step 2: Verify no placeholder text, all URLs bare v6.9, ≥ 5 Key References**

- [ ] **Step 3: Commit**

```bash
git add 09-k8s-internals/kernel/09-b-oom.md
git commit -m "docs(ch09): 09-b OOM killer oom_control, oom_badness, memory.events"
```

---

## Task 3: `09-c-psi.md` — PSI: struct psi_group, /proc/pressure/*, cgroup PSI, kubelet eviction

**Files:**
- Create: `09-k8s-internals/kernel/09-c-psi.md`

Full Data Structure Deep Dive. Required sections:

**1. Source Locations**

| File | Key Symbols | URL |
|------|-------------|-----|
| `kernel/sched/psi.c` | `psi_task_change()`, `psi_group_change()`, `psi_avgs_work()` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/psi.c |
| `include/linux/psi_types.h` | `struct psi_group`, `enum psi_task_count`, `enum psi_states` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/psi_types.h |
| `kernel/sched/psi.c` | `psi_show()` — formats /proc/pressure/* output | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/psi.c |
| `kernel/cgroup/cgroup.c` | PSI per-cgroup registration | https://elixir.bootlin.com/linux/v6.9/source/kernel/cgroup/cgroup.c |
| `include/linux/psi.h` | `psi_memstall_enter()`, `psi_memstall_leave()` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/psi.h |

**2. PSI Overview**

Pressure Stall Information (PSI, introduced in Linux 4.20, always enabled in modern distros) measures the fraction of time tasks are stalled waiting for a resource. It answers: "What fraction of my compute capacity am I losing to resource contention?"

Three resources tracked: **memory** (reclaim stalls), **CPU** (runqueue wait), **IO** (I/O wait).

Two stall categories:
- **some**: at least one runnable task is stalled (partial loss)
- **full**: ALL runnable tasks are stalled simultaneously (total loss; CPU has no "full" since there is always the idle task)

**3. `struct psi_group`**

```c
// include/linux/psi_types.h (simplified, Linux 6.9)
struct psi_group {
    struct mutex            avgs_lock;
    struct psi_group_cpu __percpu *pcpu;    // per-CPU stall state
    u64                     avg_last_update;
    u64                     avg_next_update;
    struct delayed_work     avgs_work;       // periodic average update (2s interval)

    u64                     total[NR_PSI_STATES];   // cumulative stall times (ns)
    unsigned long           avg[NR_PSI_TASK_COUNTS * 3]; // 10/60/300s exponential averages

    /* trigger/polling support */
    struct mutex            trigger_lock;
    struct list_head        triggers;
    u32                     nr_triggers[NR_PSI_STATES];
    u32                     poll_states;
    wait_queue_head_t       poll_wait;
    atomic_t                poll_scheduled;
    struct kthread_worker   *poll_kworker;
    struct kthread_delayed_work poll_work;

    bool                    enabled;
};
```

`NR_PSI_STATES = 7` (combinations of CPU/IO/memory stall bits). `NR_PSI_TASK_COUNTS = 3` (IOWAIT, MEMSTALL, RUNNING).

**4. /proc/pressure/* File Format**

```
# /proc/pressure/memory
some avg10=0.12 avg60=0.05 avg300=0.01 total=123456789
full avg10=0.04 avg60=0.01 avg300=0.00 total=45678901

# /proc/pressure/cpu
some avg10=2.45 avg60=1.23 avg300=0.89 total=987654321
# (no "full" line for CPU)

# /proc/pressure/io
some avg10=0.50 avg60=0.30 avg300=0.10 total=234567890
full avg10=0.20 avg60=0.10 avg300=0.03 total=89012345
```

- `avg10/60/300`: exponential moving averages over 10/60/300 seconds; value is percentage (0.00–100.00)
- `total`: cumulative stall time in **microseconds** since boot

The averages are updated every 2 seconds by `avgs_work` (a `delayed_work` in the PSI group). Each CPU updates its per-CPU stall counters on every scheduler tick and on task state transitions via `psi_task_change()`.

**5. Per-cgroup PSI**

Each cgroup v2 directory exposes:
- `memory.pressure` — memory PSI for tasks in this cgroup and its descendants
- `cpu.pressure` — CPU PSI
- `io.pressure` — I/O PSI

```bash
# /sys/fs/cgroup/kubepods.slice/kubepods-pod<uid>.slice/memory.pressure
some avg10=0.80 avg60=0.30 avg300=0.05 total=56789012
full avg10=0.20 avg60=0.05 avg300=0.01 total=12345678
```

The per-cgroup PSI is computed by the same `psi_group` machinery, but each cgroup has its own `struct psi_group` embedded in `struct cgroup` (via `psi` field in `struct cgroup_subsys_state` for the CPU/memory/IO subsystems).

**6. PSI Trigger Interface (inotify-free polling)**

The kernel provides a **poll-based trigger** for PSI: userspace writes a threshold to `/proc/pressure/memory` and polls the file descriptor. The kernel wakes the poller when the accumulated stall exceeds the threshold:

```bash
# Register a trigger: wake if >50ms stall in any 1-second window
echo "some 50000 1000000" > /proc/pressure/memory
# Then poll() on the file descriptor
```

This is the mechanism kubelet uses for memory pressure alerts — more efficient than periodic `stat(2)` polling.

**7. kubelet Eviction Thresholds**

kubelet EvictionManager computes `memory.available` by reading cgroup stats and comparing to the node's total allocatable memory. It uses `/proc/pressure/memory` to detect pressure trends.

Default eviction thresholds (configurable via `--eviction-hard`):

| Signal | Hard threshold | Effect |
|--------|---------------|--------|
| `memory.available` | `< 100Mi` | Immediate pod eviction |
| `nodefs.available` | `< 10%` | Immediate pod eviction |
| `imagefs.available` | `< 15%` | Immediate pod eviction |

Soft eviction thresholds (graceful, via `--eviction-soft`):

| Signal | Soft threshold | Grace period | Effect |
|--------|---------------|-------------|--------|
| `memory.available` | `< 1.5Gi` | 1m30s | Evict after sustained pressure |

Eviction order: BestEffort pods first, then Burstable pods exceeding their requests, then Guaranteed pods (only if the node itself is under pressure).

**8. Live Observation**

```bash
# Node-level PSI snapshot
cat /proc/pressure/memory
cat /proc/pressure/cpu

# Watch per-pod PSI (requires cgroup v2)
watch -n1 "cat /sys/fs/cgroup/kubepods.slice/kubepods-pod<uid>.slice/memory.pressure"

# bpftrace: trace PSI stall entry (when a task starts stalling on memory)
bpftrace -e 'kprobe:psi_memstall_enter { printf("memstall pid=%d comm=%s\n", pid, comm); }'

# Monitor PSI totals and compute delta (poor man's pressure meter)
while true; do
    awk '/some/{printf "mem_some_total=%s\n", $NF}' /proc/pressure/memory
    sleep 5
done
```

**9. Key Kernel References**

| Symbol | File | URL |
|--------|------|-----|
| `struct psi_group` | `include/linux/psi_types.h` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/psi_types.h |
| `psi_task_change()` | `kernel/sched/psi.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/psi.c |
| `psi_avgs_work()` | `kernel/sched/psi.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/psi.c |
| `psi_show()` | `kernel/sched/psi.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/psi.c |
| `psi_memstall_enter()` | `include/linux/psi.h` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/psi.h |
| `psi_memstall_leave()` | `include/linux/psi.h` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/psi.h |

Note: `09-c-psi.md` has 9 sections (one more than typical) because PSI has both a node-level and per-cgroup interface, plus the trigger mechanism, that each need a dedicated section. This is intentional.

- [ ] **Step 1: Write `09-k8s-internals/kernel/09-c-psi.md`** — all 9 sections exactly as specified above

- [ ] **Step 2: Verify no placeholder text, all URLs bare v6.9, ≥ 5 Key References**

- [ ] **Step 3: Commit**

```bash
git add 09-k8s-internals/kernel/09-c-psi.md
git commit -m "docs(ch09): 09-c PSI psi_group, /proc/pressure/*, cgroup PSI, kubelet eviction thresholds"
```

---

## Task 4: `09-k8s-connection.md` — full pod lifecycle, eviction, node conditions

**Files:**
- Create: `09-k8s-internals/k8s/09-k8s-connection.md`

Required sections:

**1. Architecture Overview**

Full pod lifecycle table:

| Phase | Actor | Kernel Interface |
|-------|-------|-----------------|
| Watch | kubelet | `inotify_add_watch()` on API server events (via client-go) |
| Admit | kubelet | Writes `cpu.max`, `memory.max`, `cpuset.cpus` to pod cgroup |
| Sandbox | containerd | `clone(CLONE_NEWPID|CLONE_NEWNET|CLONE_NEWNS|...)` |
| OOM setup | kubelet | Writes oom_score_adj to `/proc/<pid>/oom_score_adj` |
| Running | kubelet | Polls `/proc/pressure/*`, `memory.events`, `memory.current` |
| Eviction | kubelet | Sends SIGTERM via CRI StopContainer; SIGKILL after grace period |
| OOM kill | kernel | `oom_kill_process()` → SIGKILL; increments `memory.events oom_kill` |

**2. Full Pod Creation Chain**

```
kubectl apply → API server (etcd write)
     │
     │  Watch event (informer)
     ▼
kubelet.SyncPod()
     │  1. Creates cgroup hierarchy:
     │       mkdir /sys/fs/cgroup/kubepods.slice/kubepods-pod<uid>.slice/
     │       echo <memory_limit> > memory.max
     │       echo "<quota> <period>" > cpu.max
     │
     │  2. Calls CRI: containerd.RunPodSandbox()
     ▼
containerd
     │  pulls pause image if needed
     │  creates OCI spec (namespaces, cgroups, mounts, seccomp)
     │  calls: runc create --bundle /run/containerd/io.containerd.runtime.v2.task/<id>/
     ▼
runc (init process in new namespaces)
     │  clone(CLONE_NEWPID|CLONE_NEWNET|CLONE_NEWNS|CLONE_NEWIPC|CLONE_NEWUTS|SIGCHLD)
     │  pivot_root() → container rootfs
     │  exec(/pause)
     ▼
kernel: pause container PID 1 in pod network namespace
     │
     │  3. kubelet: writes echo <pid> > .../cgroup.procs
     │  4. kubelet: writes echo -998 > /proc/<pid>/oom_score_adj  (Guaranteed pod)
     │
     │  5. containerd.CreateContainer() for each app container:
     │       runc create + setns(net_fd, ipc_fd, uts_fd) + clone(CLONE_NEWPID) + exec
     │
     ▼
App container running in pod namespaces
```

**3. OOM Kill Lifecycle**

```
App container allocates memory
     │
     │  allocation hits memory.max
     ▼
kernel: direct reclaim (kswapd + direct)
     │  if reclaim insufficient:
     ▼
mem_cgroup_out_of_memory()
     │  select_bad_process() within cgroup
     │  oom_kill_process(victim)
     │     ├─ sends SIGKILL
     │     ├─ marks TIF_MEMDIE on victim
     │     └─ wakes oom_reaper to free anonymous memory
     │
     │  increments: memory.events oom += 1, oom_kill += 1
     ▼
kubelet EvictionManager
     │  inotify on memory.events fires
     │  reads oom_kill counter
     │  records OOMKilling event in pod status
     │  may evict pod (if OOM + resource pressure exceeds threshold)
```

**4. Node Conditions and PSI**

kubelet maps kernel PSI data to Kubernetes node conditions:

| Node Condition | Kernel Signal | Threshold (default) |
|----------------|--------------|---------------------|
| `MemoryPressure=True` | `/proc/pressure/memory` some avg10 | > 0% sustained |
| `DiskPressure=True` | `nodefs.available` | < 10% |
| `PIDPressure=True` | `/proc/sys/kernel/pid_max` vs active PIDs | > 95% |

When `MemoryPressure=True`, the scheduler marks the node with the `node.kubernetes.io/memory-pressure` taint. New BestEffort pods cannot be scheduled there. Eviction of existing pods follows the QoS order.

**5. Eviction Decision Flow**

```
kubelet EvictionManager (every 10s)
     │
     │  reads:
     │    /proc/pressure/memory  → some avg10, full avg10
     │    memory.current / memory.max for each pod cgroup
     │    memory.events oom_kill for each pod cgroup
     │    nodeCapacity from /proc/meminfo
     │
     ├─ if memory.available < 100Mi (hard eviction threshold):
     │    evict immediately, starting with BestEffort pods
     │
     └─ if memory.available < 1.5Gi (soft eviction threshold):
          start grace-period timer (default: 1m30s)
          if sustained: evict lowest-priority pod
```

**6. Common Failure Patterns**

| Symptom | Kernel Cause | Diagnosis |
|---------|-------------|-----------|
| Pod OOMKilled status | memory.events oom_kill > 0 | `cat <pod-cgroup>/memory.events` |
| Node MemoryPressure | /proc/pressure/memory some avg10 > threshold | `cat /proc/pressure/memory` |
| Container throttled | cpu.stat throttled_usec rising | `cat <pod-cgroup>/cpu.stat` |
| Slow pod startup | runc clone() > 100ms | bpftrace on sys_enter_clone |
| High involuntary ctxt switches | RT task preempting container | `cat /proc/<pid>/status` |
| PSI spikes but no eviction | Soft threshold not met | Tune `--eviction-soft` |

**7. Verification Commands**

```bash
# Check OOM events for all pods
for cg in /sys/fs/cgroup/kubepods.slice/kubepods-pod*.slice; do
  oom=$(awk '/oom_kill/{print $2}' $cg/memory.events 2>/dev/null)
  [ "$oom" -gt 0 ] 2>/dev/null && echo "$cg: oom_kill=$oom"
done

# Node memory pressure (PSI some avg10)
awk '/some/{printf "memory.some.avg10=%s\n", $2}' /proc/pressure/memory

# OOM score for a container process
cat /proc/$(pgrep -n nginx)/oom_score_adj
cat /proc/$(pgrep -n nginx)/oom_score

# runc clone flags when a pod is created
bpftrace -e 'tracepoint:syscalls:sys_enter_clone { if (comm == "runc:[2:INIT]") printf("flags=%lx\n", args->clone_flags); }'
```

**8. Key Kernel References**

| Symbol | File | URL |
|--------|------|-----|
| `out_of_memory()` | `mm/oom_kill.c` | https://elixir.bootlin.com/linux/v6.9/source/mm/oom_kill.c |
| `mem_cgroup_out_of_memory()` | `mm/memcontrol.c` | https://elixir.bootlin.com/linux/v6.9/source/mm/memcontrol.c |
| `psi_task_change()` | `kernel/sched/psi.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/psi.c |
| `cgroup_procs_write()` | `kernel/cgroup/cgroup.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/cgroup/cgroup.c |
| `kernel_clone()` | `kernel/fork.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/fork.c |
| `proc_oom_score_adj_write()` | `fs/proc/base.c` | https://elixir.bootlin.com/linux/v6.9/source/fs/proc/base.c |

- [ ] **Step 1: Write `09-k8s-internals/k8s/09-k8s-connection.md`** — all 8 sections as specified above

- [ ] **Step 2: Verify all facts against Linux 6.9, no placeholder text**

- [ ] **Step 3: Commit**

```bash
git add 09-k8s-internals/k8s/09-k8s-connection.md
git commit -m "docs(ch09): 09-k8s-connection full pod lifecycle, OOM kill path, PSI-based eviction"
```

---

## Task 5: C exercise — `oom-score-demo`

**Files:**
- Create: `09-k8s-internals/exercises/oom-score-demo/oom_score_demo.c`
- Create: `09-k8s-internals/exercises/oom-score-demo/Makefile`
- Create: `09-k8s-internals/exercises/oom-score-demo/README.md`

### `oom_score_demo.c`

```c
/*
 * oom_score_demo.c — demonstrate /proc/<pid>/oom_score_adj and observe
 * how the kernel computes the OOM badness score.
 *
 * Demonstrates:
 *   a) Read current oom_score and oom_score_adj for this process
 *   b) Write oom_score_adj values matching kubelet's QoS assignments:
 *        Guaranteed = -998, BestEffort = 1000, Burstable = 500
 *   c) Fork three children with different oom_score_adj values and
 *      print each child's resulting oom_score
 *   d) Show the top-5 processes by oom_score (highest = first killed)
 *
 * Build:  gcc -Wall -Wextra -Werror -o oom_score_demo oom_score_demo.c
 * Run:    sudo ./oom_score_demo   (root needed to write oom_score_adj)
 *
 * Kernel path:
 *   /proc/<pid>/oom_score_adj → fs/proc/base.c:proc_oom_score_adj_write()
 *   /proc/<pid>/oom_score     → fs/proc/base.c:proc_oom_score_read()
 *   https://elixir.bootlin.com/linux/v6.9/source/fs/proc/base.c
 */

#define _GNU_SOURCE
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>
#include <errno.h>
#include <fcntl.h>
#include <sys/types.h>
#include <sys/wait.h>
#include <dirent.h>

static int read_int_file(const char *path)
{
    FILE *f = fopen(path, "r");
    if (!f) return -1;
    int v = -1;
    fscanf(f, "%d", &v);
    fclose(f);
    return v;
}

static int write_int_file(const char *path, int v)
{
    FILE *f = fopen(path, "w");
    if (!f) return -1;
    fprintf(f, "%d\n", v);
    fclose(f);
    return 0;
}

static void show_oom_info(pid_t pid, const char *label)
{
    char path[64];
    snprintf(path, sizeof(path), "/proc/%d/oom_score_adj", pid);
    int adj = read_int_file(path);
    snprintf(path, sizeof(path), "/proc/%d/oom_score", pid);
    int score = read_int_file(path);
    printf("  %-20s pid=%-6d oom_score_adj=%-6d oom_score=%d\n",
           label, pid, adj, score);
}

static void set_oom_adj(pid_t pid, int adj)
{
    char path[64];
    snprintf(path, sizeof(path), "/proc/%d/oom_score_adj", pid);
    if (write_int_file(path, adj) != 0)
        fprintf(stderr, "  warning: cannot write %s: %s\n", path, strerror(errno));
}

static void show_top_oom(int n)
{
    printf("\n=== Top %d processes by oom_score ===\n", n);
    /* Collect (score, pid, comm) for all /proc/<pid> entries */
    DIR *d = opendir("/proc");
    if (!d) { perror("opendir /proc"); return; }

    /* Simple insertion sort into a small fixed array */
    struct entry { int score; int pid; char comm[64]; } top[16] = {0};
    int count = 0;
    if (n > 16) n = 16;

    struct dirent *de;
    while ((de = readdir(d)) != NULL) {
        if (de->d_name[0] < '1' || de->d_name[0] > '9') continue;
        int pid = atoi(de->d_name);
        if (pid <= 0) continue;

        char path[64];
        snprintf(path, sizeof(path), "/proc/%d/oom_score", pid);
        int score = read_int_file(path);
        if (score <= 0) continue;

        snprintf(path, sizeof(path), "/proc/%d/comm", pid);
        char comm[64] = "(unknown)";
        FILE *f = fopen(path, "r");
        if (f) { fgets(comm, sizeof(comm), f); fclose(f); }
        comm[strcspn(comm, "\n")] = '\0';

        /* insert into top[] if score is higher than minimum */
        if (count < n || score > top[count-1].score) {
            int i = (count < n) ? count++ : n - 1;
            top[i].score = score;
            top[i].pid = pid;
            strncpy(top[i].comm, comm, sizeof(top[i].comm) - 1);
            /* bubble up */
            for (; i > 0 && top[i].score > top[i-1].score; i--) {
                struct entry tmp = top[i]; top[i] = top[i-1]; top[i-1] = tmp;
            }
        }
    }
    closedir(d);

    for (int i = 0; i < count; i++)
        printf("  #%-2d oom_score=%-6d pid=%-6d %s\n",
               i+1, top[i].score, top[i].pid, top[i].comm);
}

int main(void)
{
    printf("=== Part a: this process OOM info ===\n");
    show_oom_info(getpid(), "self");

    printf("\n=== Part b: simulate kubelet QoS assignments ===\n");
    printf("  (writing oom_score_adj — requires root)\n");

    /* Fork three children and assign QoS-like oom_score_adj values */
    struct { int adj; const char *label; } qos[] = {
        { -998, "Guaranteed" },
        {  500, "Burstable" },
        { 1000, "BestEffort" },
    };

    printf("\n=== Part c: children with QoS oom_score_adj ===\n");
    for (int i = 0; i < 3; i++) {
        pid_t child = fork();
        if (child == 0) {
            /* child: set adj, sleep briefly so parent can read our score */
            set_oom_adj(getpid(), qos[i].adj);
            usleep(200000);  /* 200 ms */
            _exit(0);
        }
        /* parent: wait a moment then read child score */
        usleep(50000);  /* 50 ms — let child write its adj */
        show_oom_info(child, qos[i].label);
        waitpid(child, NULL, 0);
    }

    show_top_oom(5);

    return 0;
}
```

### `Makefile`

```makefile
CC      = gcc
CFLAGS  = -Wall -Wextra -Werror
TARGET  = oom_score_demo

all: $(TARGET)

$(TARGET): oom_score_demo.c
	$(CC) $(CFLAGS) -o $@ $<

run: $(TARGET)
	sudo ./$(TARGET)

clean:
	rm -f $(TARGET)

.PHONY: all run clean
```

### `README.md`

```markdown
# oom-score-demo

Demonstrates `/proc/<pid>/oom_score_adj` — the per-process knob that biases the kernel OOM killer — and shows how the resulting `/proc/<pid>/oom_score` changes.

## What It Shows

| Part | Demonstrates |
|------|-------------|
| Part a | Current process OOM score and adj |
| Part b | kubelet's QoS adj values (-998, 500, 1000) |
| Part c | Three children with Guaranteed/Burstable/BestEffort adj; each child's resulting oom_score |
| Top-5 | Highest oom_score processes (OOM kill candidates) |

## Build and Run

```
make
sudo ./oom_score_demo   # root required to write oom_score_adj
```

## Kernel Path

```
write /proc/<pid>/oom_score_adj
  → fs/proc/base.c:proc_oom_score_adj_write()
    → task->signal->oom_score_adj = value

read /proc/<pid>/oom_score
  → fs/proc/base.c:proc_oom_score_read()
    → oom_badness(task, totalpages) — normalized to 0-2000
```

Source:
https://elixir.bootlin.com/linux/v6.9/source/fs/proc/base.c
https://elixir.bootlin.com/linux/v6.9/source/mm/oom_kill.c

## Exercises

a) Change the Burstable adj formula to match kubelet:
   `adj = min(max(2, 1000 - (1000 * rss_pages / total_pages)), 999)`.
   Use `/proc/<pid>/status` Vm fields to estimate rss_pages.

b) Add a Part d that allocates increasing amounts of memory in a loop and
   observes how oom_score rises as the process's RSS grows.

c) Read `/proc/sys/kernel/panic_on_oom` and `/proc/sys/vm/overcommit_memory`.
   Print their current values and explain what each setting means for
   OOM behavior.
```
```

- [ ] **Step 1: Write `09-k8s-internals/exercises/oom-score-demo/oom_score_demo.c`** exactly as above

- [ ] **Step 2: Write `09-k8s-internals/exercises/oom-score-demo/Makefile`** exactly as above

- [ ] **Step 3: Write `09-k8s-internals/exercises/oom-score-demo/README.md`** exactly as above

- [ ] **Step 4: Build and verify**

```bash
cd 09-k8s-internals/exercises/oom-score-demo
gcc -Wall -Wextra -Werror -o oom_score_demo oom_score_demo.c
```
Expected: compiles with no warnings or errors.

- [ ] **Step 5: Commit**

```bash
cd ../../..
git add 09-k8s-internals/exercises/oom-score-demo/
git commit -m "feat(ch09): C exercise oom-score-demo — oom_score_adj, QoS assignments, top-5 OOM candidates"
```

---

## Task 6: Go exercise — `node-pressure-reader`

**Files:**
- Create: `09-k8s-internals/exercises/node-pressure-reader/main.go`
- Create: `09-k8s-internals/exercises/node-pressure-reader/go.mod`
- Create: `09-k8s-internals/exercises/node-pressure-reader/README.md`

### `main.go`

```go
// node-pressure-reader: reads PSI files from /proc/pressure/* and
// memory.events from pod cgroup paths.
//
// Usage:
//   node-pressure-reader               — print node-level PSI
//   node-pressure-reader --pod <uid>   — also print pod cgroup memory.events
//
// Build: go build -o node-pressure-reader .
//
// Kernel paths:
//   /proc/pressure/{cpu,memory,io}  — kernel/sched/psi.c:psi_show()
//   cgroup memory.events             — mm/memcontrol.c
//
// Source:
//   https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/psi.c
//   https://elixir.bootlin.com/linux/v6.9/source/mm/memcontrol.c
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// PSIStats holds parsed data from a /proc/pressure/* or cgroup *.pressure file.
type PSIStats struct {
	Resource string
	SomeAvg10  float64
	SomeAvg60  float64
	SomeAvg300 float64
	SomeTotal  uint64 // microseconds
	FullAvg10  float64
	FullAvg60  float64
	FullAvg300 float64
	FullTotal  uint64 // microseconds
	HasFull    bool   // false for CPU
}

// MemoryEvents holds parsed data from cgroup memory.events.
type MemoryEvents struct {
	Low      uint64
	High     uint64
	Max      uint64
	OOM      uint64
	OOMKill  uint64
}

// parsePSIFile reads a PSI file and returns a PSIStats.
// Format per line: "some avg10=X avg60=X avg300=X total=Y"
func parsePSIFile(path, resource string) (PSIStats, error) {
	f, err := os.Open(path)
	if err != nil {
		return PSIStats{}, err
	}
	defer f.Close()

	stats := PSIStats{Resource: resource}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		kind := fields[0] // "some" or "full"
		vals := make(map[string]string)
		for _, f := range fields[1:] {
			kv := strings.SplitN(f, "=", 2)
			if len(kv) == 2 {
				vals[kv[0]] = kv[1]
			}
		}
		avg10, _ := strconv.ParseFloat(vals["avg10"], 64)
		avg60, _ := strconv.ParseFloat(vals["avg60"], 64)
		avg300, _ := strconv.ParseFloat(vals["avg300"], 64)
		total, _ := strconv.ParseUint(vals["total"], 10, 64)

		switch kind {
		case "some":
			stats.SomeAvg10 = avg10
			stats.SomeAvg60 = avg60
			stats.SomeAvg300 = avg300
			stats.SomeTotal = total
		case "full":
			stats.HasFull = true
			stats.FullAvg10 = avg10
			stats.FullAvg60 = avg60
			stats.FullAvg300 = avg300
			stats.FullTotal = total
		}
	}
	return stats, scanner.Err()
}

// parseMemoryEvents reads cgroup memory.events.
func parseMemoryEvents(path string) (MemoryEvents, error) {
	f, err := os.Open(path)
	if err != nil {
		return MemoryEvents{}, err
	}
	defer f.Close()

	var ev MemoryEvents
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		parts := strings.Fields(scanner.Text())
		if len(parts) != 2 {
			continue
		}
		v, _ := strconv.ParseUint(parts[1], 10, 64)
		switch parts[0] {
		case "low":
			ev.Low = v
		case "high":
			ev.High = v
		case "max":
			ev.Max = v
		case "oom":
			ev.OOM = v
		case "oom_kill":
			ev.OOMKill = v
		}
	}
	return ev, scanner.Err()
}

func printPSI(s PSIStats) {
	fmt.Printf("  %s pressure:\n", s.Resource)
	fmt.Printf("    some  avg10=%.2f%%  avg60=%.2f%%  avg300=%.2f%%  total=%d µs\n",
		s.SomeAvg10, s.SomeAvg60, s.SomeAvg300, s.SomeTotal)
	if s.HasFull {
		fmt.Printf("    full  avg10=%.2f%%  avg60=%.2f%%  avg300=%.2f%%  total=%d µs\n",
			s.FullAvg10, s.FullAvg60, s.FullAvg300, s.FullTotal)
	}
}

func printNodePressure() {
	fmt.Println("=== Node Pressure (PSI) ===")
	resources := []struct{ name, path string }{
		{"memory", "/proc/pressure/memory"},
		{"cpu", "/proc/pressure/cpu"},
		{"io", "/proc/pressure/io"},
	}
	for _, r := range resources {
		s, err := parsePSIFile(r.path, r.name)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  %s: %v\n", r.path, err)
			continue
		}
		printPSI(s)
	}
}

func findPodCgroup(podUID string) (string, error) {
	base := "/sys/fs/cgroup/kubepods.slice"
	pattern := fmt.Sprintf("*pod%s*", podUID)
	matches, _ := filepath.Glob(filepath.Join(base, "*", pattern))
	direct, _ := filepath.Glob(filepath.Join(base, pattern))
	all := append(matches, direct...)
	if len(all) == 0 {
		return "", fmt.Errorf("cgroup not found for pod %s", podUID)
	}
	return all[0], nil
}

func printPodStats(podUID string) {
	fmt.Printf("\n=== Pod %s ===\n", podUID)
	cgPath, err := findPodCgroup(podUID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  %v\n", err)
		return
	}
	fmt.Printf("  cgroup: %s\n", cgPath)

	// memory.events
	ev, err := parseMemoryEvents(filepath.Join(cgPath, "memory.events"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "  memory.events: %v\n", err)
	} else {
		fmt.Printf("  memory.events:\n")
		fmt.Printf("    low=%d  high=%d  max=%d  oom=%d  oom_kill=%d\n",
			ev.Low, ev.High, ev.Max, ev.OOM, ev.OOMKill)
	}

	// memory.pressure
	mp, err := parsePSIFile(filepath.Join(cgPath, "memory.pressure"), "memory")
	if err != nil {
		fmt.Fprintf(os.Stderr, "  memory.pressure: %v\n", err)
	} else {
		printPSI(mp)
	}
}

func main() {
	podUID := flag.String("pod", "", "Pod UID to inspect (optional)")
	flag.Parse()

	printNodePressure()

	if *podUID != "" {
		printPodStats(*podUID)
	}
}
```

### `go.mod`

```
module github.com/linux-to-k8s/node-pressure-reader

go 1.22
```

### `README.md`

```markdown
# node-pressure-reader

Reads `/proc/pressure/*` (node PSI) and, optionally, a pod cgroup's
`memory.events` and `memory.pressure` to show resource pressure at both
the node and pod level.

## Build and Run

```
go build -o node-pressure-reader .

# Node-level PSI only
./node-pressure-reader

# Node PSI + pod cgroup stats
./node-pressure-reader --pod <pod-uid>
```

## Sample Output

```
=== Node Pressure (PSI) ===
  memory pressure:
    some  avg10=0.12%  avg60=0.05%  avg300=0.01%  total=123456 µs
    full  avg10=0.04%  avg60=0.01%  avg300=0.00%  total=45678 µs
  cpu pressure:
    some  avg10=2.45%  avg60=1.23%  avg300=0.89%  total=987654 µs
  io pressure:
    some  avg10=0.50%  avg60=0.30%  avg300=0.10%  total=234567 µs
    full  avg10=0.20%  avg60=0.10%  avg300=0.03%  total=89012 µs

=== Pod abc123 ===
  cgroup: /sys/fs/cgroup/kubepods.slice/kubepods-besteffort-podabc123.slice
  memory.events:
    low=0  high=3  max=1  oom=0  oom_kill=0
  memory pressure:
    some  avg10=0.80%  avg60=0.30%  avg300=0.05%  total=56789 µs
    full  avg10=0.20%  avg60=0.05%  avg300=0.01%  total=12345 µs
```

## Kernel Paths

| File | Symbol |
|------|--------|
| `/proc/pressure/*` | `kernel/sched/psi.c:psi_show()` |
| `memory.events` | `mm/memcontrol.c` — incremented by `mem_cgroup_event()` |
| `memory.pressure` | per-cgroup `struct psi_group` |

Sources:
https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/psi.c
https://elixir.bootlin.com/linux/v6.9/source/mm/memcontrol.c

## Exercises

a) Add `--watch` flag that re-reads every 5 seconds and prints the delta
   in PSI totals (to see rate of pressure accumulation).

b) Add parsing for `cpu.pressure` and `io.pressure` from the pod cgroup.

c) Compare the pod's `memory.current` with `memory.max` and print a
   utilization percentage next to the memory.events summary.
```
```

- [ ] **Step 1: Write all three files** exactly as specified above

- [ ] **Step 2: Build and vet**

```bash
cd 09-k8s-internals/exercises/node-pressure-reader
go build ./...
go vet ./...
```
Expected: clean.

- [ ] **Step 3: Commit**

```bash
cd ../../..
git add 09-k8s-internals/exercises/node-pressure-reader/
git commit -m "feat(ch09): Go exercise node-pressure-reader — PSI /proc/pressure/* + cgroup memory.events"
```

---

## Task 7: kube-inspect checkpoint 09 — Node pressure + eviction thresholds

**Files:**
- Create: `kube-inspect/internal/kubelet/kubelet.go`
- Modify: `kube-inspect/cmd/kube-inspect/main.go` (add `--pressure` flag)
- Modify: `kube-inspect/CHECKPOINT.md` (mark checkpoint 09 done)

### `kube-inspect/internal/kubelet/kubelet.go`

New package `kubelet` that reads node-level PSI and per-pod memory events:

```go
package kubelet

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// NodePressure holds PSI data from /proc/pressure/*.
type NodePressure struct {
	MemorySomeAvg10  float64
	MemoryFullAvg10  float64
	CPUSomeAvg10     float64
	IOSomeAvg10      float64
	IOFullAvg10      float64
}

// PodMemEvents holds memory.events counters from a pod cgroup.
type PodMemEvents struct {
	PodUID  string
	CgPath  string
	Low     uint64
	High    uint64
	Max     uint64
	OOM     uint64
	OOMKill uint64
}

// parsePSIAvg10 reads a PSI file and returns the "some" and "full" avg10 values.
func parsePSIAvg10(path string) (someAvg10, fullAvg10 float64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		kind := fields[0]
		for _, kv := range fields[1:] {
			parts := strings.SplitN(kv, "=", 2)
			if len(parts) == 2 && parts[0] == "avg10" {
				v, _ := strconv.ParseFloat(parts[1], 64)
				if kind == "some" {
					someAvg10 = v
				} else if kind == "full" {
					fullAvg10 = v
				}
			}
		}
	}
	return someAvg10, fullAvg10, scanner.Err()
}

// GetNodePressure reads /proc/pressure/{memory,cpu,io} and returns avg10 values.
func GetNodePressure() (NodePressure, error) {
	var np NodePressure
	var err error

	np.MemorySomeAvg10, np.MemoryFullAvg10, err = parsePSIAvg10("/proc/pressure/memory")
	if err != nil {
		return np, fmt.Errorf("memory pressure: %w", err)
	}
	np.CPUSomeAvg10, _, err = parsePSIAvg10("/proc/pressure/cpu")
	if err != nil {
		return np, fmt.Errorf("cpu pressure: %w", err)
	}
	np.IOSomeAvg10, np.IOFullAvg10, err = parsePSIAvg10("/proc/pressure/io")
	if err != nil {
		return np, fmt.Errorf("io pressure: %w", err)
	}
	return np, nil
}

// parseMemoryEvents reads a cgroup memory.events file.
func parseMemoryEvents(path string) (low, high, max, oom, oomKill uint64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, 0, 0, 0, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		parts := strings.Fields(scanner.Text())
		if len(parts) != 2 {
			continue
		}
		v, _ := strconv.ParseUint(parts[1], 10, 64)
		switch parts[0] {
		case "low":
			low = v
		case "high":
			high = v
		case "max":
			max = v
		case "oom":
			oom = v
		case "oom_kill":
			oomKill = v
		}
	}
	return low, high, max, oom, oomKill, scanner.Err()
}

// GetPodMemEvents reads memory.events for the given pod UID's cgroup.
func GetPodMemEvents(podUID string) (PodMemEvents, error) {
	ev := PodMemEvents{PodUID: podUID}

	base := "/sys/fs/cgroup/kubepods.slice"
	pattern := fmt.Sprintf("*pod%s*", podUID)
	matches, _ := filepath.Glob(filepath.Join(base, "*", pattern))
	direct, _ := filepath.Glob(filepath.Join(base, pattern))
	all := append(matches, direct...)
	if len(all) == 0 {
		return ev, fmt.Errorf("cgroup not found for pod %s", podUID)
	}
	ev.CgPath = all[0]

	var err error
	ev.Low, ev.High, ev.Max, ev.OOM, ev.OOMKill, err =
		parseMemoryEvents(filepath.Join(ev.CgPath, "memory.events"))
	return ev, err
}
```

### `--pressure` flag in `main.go`

Add after the `--sched` block, following the exact same pattern:

```go
flagPressure = flag.Bool("pressure", false, "Show node PSI and pod memory.events (requires --pod for pod events)")
```

Output block (in the pod-mode section):

```go
if *flagPressure {
    np, err := kubelet.GetNodePressure()
    if err != nil {
        fmt.Fprintf(os.Stderr, "pressure: %v\n", err)
    } else {
        fmt.Printf("Node pressure (PSI avg10):\n")
        fmt.Printf("  memory some=%.2f%%  full=%.2f%%\n", np.MemorySomeAvg10, np.MemoryFullAvg10)
        fmt.Printf("  cpu    some=%.2f%%\n", np.CPUSomeAvg10)
        fmt.Printf("  io     some=%.2f%%  full=%.2f%%\n", np.IOSomeAvg10, np.IOFullAvg10)
        fmt.Println()
    }
    ev, err := kubelet.GetPodMemEvents(*flagPod)
    if err != nil {
        fmt.Fprintf(os.Stderr, "pod memory events: %v\n", err)
    } else {
        fmt.Printf("Pod %s memory.events:\n", ev.PodUID)
        fmt.Printf("  low=%d  high=%d  max=%d  oom=%d  oom_kill=%d\n",
            ev.Low, ev.High, ev.Max, ev.OOM, ev.OOMKill)
        fmt.Println()
    }
}
```

Import: `"github.com/linux-to-k8s/kube-inspect/internal/kubelet"`

Also add `--pressure` to the usage string: append `[--pressure]` after `[--sched]`.

### `CHECKPOINT.md`

Change `| 09 | Node pressure + eviction thresholds | internal/kubelet | pending |` to `| 09 | Node pressure + eviction thresholds | internal/kubelet | done |`

- [ ] **Step 1: Read existing `kube-inspect/cmd/kube-inspect/main.go`** for import block and flag/output pattern

- [ ] **Step 2: Create `kube-inspect/internal/kubelet/kubelet.go`** exactly as specified above

- [ ] **Step 3: Modify `kube-inspect/cmd/kube-inspect/main.go`** — add `--pressure` flag and output block

- [ ] **Step 4: Modify `kube-inspect/CHECKPOINT.md`** — mark checkpoint 09 done

- [ ] **Step 5: Build and vet**

```bash
cd kube-inspect
go build ./...
go vet ./...
```
Expected: clean.

- [ ] **Step 6: Commit**

```bash
cd ..
git add kube-inspect/internal/kubelet/kubelet.go kube-inspect/cmd/kube-inspect/main.go kube-inspect/CHECKPOINT.md
git commit -m "feat(kube-inspect): checkpoint 09 — node PSI pressure + pod memory.events"
```

---

## Self-Review

**Spec coverage:**
- ✅ `09-k8s-internals/README.md` — objectives, prerequisites, reading order, kernel-to-k8s bridge diagram
- ✅ `kernel/09-a-kubelet.md` — kubelet kernel interface table, pod sandbox chain, setns path, cgroup assignment (7 sections)
- ✅ `kernel/09-b-oom.md` — struct oom_control, oom_badness algorithm, oom_kill_process, memory.events, oom_score_adj QoS table (8 sections)
- ✅ `kernel/09-c-psi.md` — struct psi_group, file format, per-cgroup PSI, trigger interface, kubelet eviction thresholds (9 sections)
- ✅ `k8s/09-k8s-connection.md` — full pod creation chain, OOM kill lifecycle, node conditions, eviction flow, failure patterns (8 sections)
- ✅ `exercises/oom-score-demo/` — C exercise, oom_score_adj QoS assignments, top-5 OOM candidates
- ✅ `exercises/node-pressure-reader/` — Go exercise, PSI parser, memory.events parser, stdlib only
- ✅ `kube-inspect/internal/kubelet/kubelet.go` — NodePressure, PodMemEvents, GetNodePressure, GetPodMemEvents

**Placeholder scan:** All sections contain real content. No TBD/TODO.

**Type consistency:**
- `GetNodePressure()` returns `(NodePressure, error)` — used correctly in main.go
- `GetPodMemEvents(podUID string)` returns `(PodMemEvents, error)` — used correctly
- `PodMemEvents.OOMKill` (not OomKill) — consistent throughout kubelet.go and main.go
- `--pressure` flag appears in usage string after `--sched` — consistent with pattern
