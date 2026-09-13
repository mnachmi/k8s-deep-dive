# Chapter 02 — Namespaces Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Write Chapter 02 (Namespaces) in full: kernel deep dives for `struct nsproxy` and all 8 namespace types, `setns()`/`unshare()` mechanics, K8s connection doc, C `mini-unshare` exercise, Go `namespace-inspector` exercise, and `kube-inspect` checkpoint 02 (namespace enumeration per pod).

**Architecture:** Three tracks per chapter — `kernel/` (teach from kernel source), `k8s/` (how Kubernetes uses namespaces), `exercises/` (standalone C + Go programs). The kube-inspect checkpoint extends `internal/proc` with namespace enumeration built on top of checkpoint 01's process listing.

**Tech Stack:** Markdown, C (gcc -Wall -Wextra -Werror), Go 1.22+, Linux 6.9 kernel source (elixir.bootlin.com), bpftrace

## Global Constraints

- Every kernel struct/function reference MUST include elixir.bootlin.com URL with exact file path and line number for Linux 6.9
- bpftrace commands: use `kernel_clone` (not `do_fork`); `copy_process` takes `struct kernel_clone_args *` as arg3
- C programs compile with `gcc -Wall -Wextra -Werror`; compiled binaries excluded via `.gitignore`
- Go code passes `go vet ./...`; binaries excluded via `.gitignore`
- Every exercise folder: `README.md`, `Makefile` with `build`/`run`/`clean`, `.gitignore`
- Data structure deep dive mandatory: full struct definition quoted, every field explained, memory layout, lifecycle, locking discipline, object graph, live observation commands
- No placeholder text anywhere — all sections must contain real content
- Kernel version: Linux 6.9 throughout

---

## Task 1: Chapter 02 README + `struct nsproxy` deep dive

**Files:**
- Modify: `02-namespaces/README.md` (replace stub)
- Create: `02-namespaces/kernel/02-a-nsproxy.md`

**Interfaces:**
- Produces: nothing consumed by later tasks — standalone doc

- [ ] **Step 1: Write `02-namespaces/README.md`**

Replace the stub with full content:

```markdown
# Chapter 02 — Namespaces

Linux namespaces are the kernel mechanism that gives each container its isolated
view of the system. This chapter teaches every namespace type from source, then
shows how `runc` and `containerd` use them to build container isolation.

## Objectives

After this chapter you will be able to:
1. Read `/proc/<pid>/ns/` and determine which namespaces a process shares
2. Explain `struct nsproxy` field-by-field and trace how `clone()` creates a new one
3. Describe the kernel implementation of each of the 8 namespace types
4. Trace a container creation through `containerd` → `runc` → `clone3()` → namespace setup
5. Use `lsns`, `nsenter`, `bpftrace`, and `kube-inspect` to inspect pod namespaces live

## Prerequisites

- Chapter 00 (syscall entry path — how `clone3()` reaches `kernel_clone()`)
- Chapter 01 (process model — `task_struct`, `clone()` flags, PID namespaces)

## Reading Order

| Step | File | What you learn |
|------|------|---------------|
| 1 | [kernel/02-a-nsproxy.md](kernel/02-a-nsproxy.md) | `struct nsproxy` — the namespace pointer bundle |
| 2 | [kernel/02-b-namespace-types.md](kernel/02-b-namespace-types.md) | All 8 namespace type deep dives |
| 3 | [kernel/02-c-setns-unshare.md](kernel/02-c-setns-unshare.md) | `setns()`, `unshare()`, `/proc/self/ns/` |
| 4 | [k8s/02-k8s-connection.md](k8s/02-k8s-connection.md) | How runc/containerd use namespaces |
| 5 | [exercises/mini-unshare/](exercises/mini-unshare/) | C: build a minimal `unshare(1)` clone |
| 6 | [exercises/namespace-inspector/](exercises/namespace-inspector/) | Go: inspect all namespaces per PID |
| 7 | [kube-inspect checkpoint 02](../../kube-inspect/CHECKPOINT.md) | Add namespace enumeration per pod |

## The Big Picture

```
task_struct
  └─ nsproxy ──────────┬─ mnt_ns  (struct mnt_namespace)
                       ├─ uts_ns  (struct uts_namespace)
                       ├─ ipc_ns  (struct ipc_namespace)
                       ├─ pid_ns_for_children (struct pid_namespace)
                       ├─ net_ns  (struct net)
                       ├─ cgroup_ns (struct cgroup_namespace)
                       └─ time_ns (struct time_namespace)
```

User namespaces (`struct user_namespace`) are stored separately in
`task_struct.cred->user_ns`, not in `nsproxy`.
```

- [ ] **Step 2: Write `02-namespaces/kernel/02-a-nsproxy.md`**

Content must include:

**Section 1 — Source location**
File: `include/linux/nsproxy.h` — https://elixir.bootlin.com/linux/v6.9/source/include/linux/nsproxy.h

**Section 2 — Full struct definition (quoted from source)**
```c
// include/linux/nsproxy.h
struct nsproxy {
    refcount_t count;
    struct uts_namespace  *uts_ns;
    struct ipc_namespace  *ipc_ns;
    struct mnt_namespace  *mnt_ns;
    struct pid_namespace  *pid_ns_for_children;
    struct net            *net_ns;
    struct time_namespace *time_ns;
    struct time_namespace *time_ns_for_children;
    struct cgroup_namespace *cgroup_ns;
};
```
Note: `struct user_namespace` is NOT in `nsproxy` — it lives in `task_struct->cred->user_ns`.

**Section 3 — Field-by-field: each field must have: type, purpose, which namespace type it points to, elixir source link**
- `count` — refcount_t; reference count; incremented by `get_nsproxy()`, decremented by `put_nsproxy()`; when it hits zero, `free_nsproxy()` is called
- `uts_ns` — pointer to UTS namespace (hostname, domainname); `struct uts_namespace` in `include/linux/utsname.h`
- `ipc_ns` — pointer to IPC namespace (SysV IPC, POSIX MQ); `struct ipc_namespace` in `include/linux/ipc_namespace.h`
- `mnt_ns` — pointer to mount namespace (filesystem topology); `struct mnt_namespace` in `fs/mount.h`
- `pid_ns_for_children` — PID namespace for NEW children; the current process sees its own PID namespace via `task_active_pid_ns(current)`
- `net_ns` — pointer to network namespace; `struct net` in `include/net/net_namespace.h`
- `time_ns` — time namespace of the current process (for reading clocks)
- `time_ns_for_children` — time namespace that will be assigned to new children (for `unshare(CLONE_NEWTIME)`)
- `cgroup_ns` — cgroup namespace; `struct cgroup_namespace` in `include/linux/cgroup.h`

**Section 4 — Lifecycle**
```
copy_process()                          # kernel/fork.c
  └─ copy_namespaces(flags, p)          # kernel/nsproxy.c
       ├─ [if no CLONE_NEW* flags]
       │    get_nsproxy(old_ns)         # just increment refcount — share parent's nsproxy
       └─ [if any CLONE_NEW* flag]
            create_new_namespaces()     # allocate new nsproxy, clone each flagged ns
              ├─ copy_mnt_ns()          # fs/namespace.c
              ├─ copy_utsname()         # kernel/utsname.c
              ├─ copy_ipcs()            # ipc/namespace.c
              ├─ copy_pid_ns()          # kernel/pid_namespace.c
              ├─ copy_net_ns()          # net/core/net_namespace.c
              ├─ copy_time_ns()         # kernel/time_namespace.c
              └─ copy_cgroup_ns()       # kernel/cgroup/namespace.c

do_exit() → exit_task_namespaces()
  └─ put_nsproxy(ns)
       └─ [refcount == 0] free_nsproxy(ns)
            └─ put_* each member namespace
```

**Section 5 — Locking discipline**
- `nsproxy.count` — refcount_t; atomic; no lock needed
- `nsproxy` pointer in `task_struct` — protected by `task_lock()` (= `task_struct.alloc_lock` spinlock) on write; RCU on read via `task_nsproxy()`
- Each namespace struct has its own lock (covered per namespace in 02-b)

**Section 6 — The sharing model: when nsproxy is shared vs copied**
When a process forks without any `CLONE_NEW*` flags, the child increments the parent's `nsproxy` refcount — both point to the same struct. This is why threads (CLONE_THREAD) always share namespaces. Only `unshare()` or `clone()` with `CLONE_NEW*` flags allocate a new `nsproxy`.

**Section 7 — Live observation**
```bash
# Show nsproxy pointer and refcount for a process via bpftrace
bpftrace -e 'kprobe:copy_namespaces {
    $p = (struct task_struct *)arg1;
    printf("pid=%d nsproxy=%p count=%d\n",
        $p->pid,
        $p->nsproxy,
        $p->nsproxy->count.refs.counter);
}'

# Count how many processes share each nsproxy (same namespace set)
# Processes with same /proc/<pid>/ns/mnt inode share their mount namespace
ls -la /proc/1/ns/

# See all nsproxies in the system via lsns
lsns

# Check if two processes share a namespace
stat /proc/$PID1/ns/net /proc/$PID2/ns/net
# Same inode → same network namespace
```

- [ ] **Step 3: git add and commit**

```bash
git add 02-namespaces/
git commit -m "feat(ch02): README and nsproxy deep dive"
```

---

## Task 2: All 8 namespace types deep dive

**Files:**
- Create: `02-namespaces/kernel/02-b-namespace-types.md`

- [ ] **Step 1: Write `02-namespaces/kernel/02-b-namespace-types.md`**

This document covers all 8 namespace types. For each type, the document must include:
1. Flag value and kernel file where it's implemented
2. What it isolates
3. Key struct definition (full, from source) with elixir link
4. Key fields explained
5. How to observe it

**1. Mount Namespace — `CLONE_NEWNS` (0x00020000)**
- Isolates: filesystem topology (mount points)
- Kernel file: `fs/namespace.c`, struct: `struct mnt_namespace` in `fs/mount.h`
- Source: https://elixir.bootlin.com/linux/v6.9/source/fs/mount.h
- Key fields: `struct user_namespace *user_ns`, `struct ns_common ns`, `struct list_head list` (all mounts), `unsigned int mounts` (count), `seqlock_t lock`
- Oldest namespace (Linux 2.4.19) — the only one that predates the `CLONE_NEW*` prefix
- Observe: `cat /proc/$PID/mounts`, `findmnt --pid $PID`

**2. UTS Namespace — `CLONE_NEWUTS` (0x04000000)**
- Isolates: hostname and NIS domainname
- Kernel file: `kernel/utsname.c`, struct: `struct uts_namespace` in `include/linux/utsname.h`
- Source: https://elixir.bootlin.com/linux/v6.9/source/include/linux/utsname.h
- Key fields: `struct new_utsname name` (contains `nodename[65]` = hostname, `domainname[65]`), `struct user_namespace *user_ns`, `struct ns_common ns`
- Observe: `nsenter --uts=/proc/$PID/ns/uts hostname`

**3. IPC Namespace — `CLONE_NEWIPC` (0x08000000)**
- Isolates: System V IPC (semaphores, shared memory, message queues) and POSIX message queues
- Kernel file: `ipc/namespace.c`, struct: `struct ipc_namespace` in `include/linux/ipc_namespace.h`
- Source: https://elixir.bootlin.com/linux/v6.9/source/include/linux/ipc_namespace.h
- Key fields: `struct ipc_ids ids[3]` (sem, msg, shm), `unsigned int msg_ctlmax`, `size_t shm_ctlmax`, `struct ns_common ns`
- Observe: `nsenter --ipc=/proc/$PID/ns/ipc ipcs`

**4. PID Namespace — `CLONE_NEWPID` (0x20000000)**
- Already covered in Chapter 01 (`struct pid_namespace`, `struct pid`, child_reaper)
- Summary here with cross-reference to `01-c-pid-namespaces.md`
- Observe: `lsns -t pid`

**5. Network Namespace — `CLONE_NEWNET` (0x40000000)**
- Isolates: network stack (interfaces, routing tables, iptables, sockets, `/proc/net`)
- Kernel file: `net/core/net_namespace.c`, struct: `struct net` in `include/net/net_namespace.h`
- Source: https://elixir.bootlin.com/linux/v6.9/source/include/net/net_namespace.h
- Key fields: `struct list_head list` (all netns), `struct net_device *loopback_dev`, `struct netns_ipv4 ipv4`, `struct netns_ipv6 ipv6`, `struct netns_nftables nft`, `struct ns_common ns`
- Note: `struct net` is by far the largest namespace struct — it embeds the entire IPv4/IPv6/netfilter state
- Observe: `ip netns list`, `ip -n <nsname> addr`, `nsenter --net=/proc/$PID/ns/net ip addr`

**6. User Namespace — `CLONE_NEWUSER` (0x10000000)**
- Isolates: user and group IDs — maps UIDs/GIDs between host and container
- NOT in `nsproxy` — stored in `task_struct->cred->user_ns`
- Kernel file: `kernel/user_namespace.c`, struct: `struct user_namespace` in `include/linux/user_namespace.h`
- Source: https://elixir.bootlin.com/linux/v6.9/source/include/linux/user_namespace.h
- Key fields: `struct uid_gid_map uid_map` (uid_map entries from `/proc/self/uid_map`), `struct uid_gid_map gid_map`, `struct user_namespace *parent`, `kuid_t owner`, `kgid_t group`, `struct ns_common ns`
- Why it's different: Creating a user namespace doesn't require `CAP_SYS_ADMIN` — it's the gateway to unprivileged container creation
- Observe: `cat /proc/$PID/uid_map`, `cat /proc/$PID/gid_map`

**7. Cgroup Namespace — `CLONE_NEWCGROUP` (0x02000000)**
- Isolates: the view of the cgroup hierarchy (what `/sys/fs/cgroup/` shows)
- Kernel file: `kernel/cgroup/namespace.c`, struct: `struct cgroup_namespace` in `include/linux/cgroup.h`
- Source: https://elixir.bootlin.com/linux/v6.9/source/include/linux/cgroup.h
- Key fields: `refcount_t count`, `struct user_namespace *user_ns`, `struct css_set *root_cset` (the cgroup root from this namespace's perspective), `struct ns_common ns`
- Effect: inside the cgroup namespace, the process's own cgroup appears as `/` — the real path on the host is hidden
- Observe: `cat /proc/$PID/cgroup` from host vs inside container

**8. Time Namespace — `CLONE_NEWTIME` (0x00000080)**
- Newest namespace (Linux 5.6, March 2020)
- Isolates: `CLOCK_MONOTONIC` and `CLOCK_BOOTTIME` offsets — allows containers to have different "uptime"
- Kernel file: `kernel/time_namespace.c`, struct: `struct time_namespace` in `include/linux/time_namespace.h`
- Source: https://elixir.bootlin.com/linux/v6.9/source/include/linux/time_namespace.h
- Key fields: `struct user_namespace *user_ns`, `struct timens_offsets offsets` (monotonic_offset, boottime_offset), `struct page *vvar_page`, `bool frozen_offsets`, `struct ns_common ns`
- Why two nsproxy fields: `time_ns` (current process's time view) vs `time_ns_for_children` (unshare semantics — the offset only takes effect for new children, not the calling process)
- Observe: `lsns -t time`, `clock_gettime(CLOCK_MONOTONIC)` before and after entering time namespace

**Section 9 — Namespace summary table**

| Namespace | Flag | Value | Added | Key struct | Kernel file | What it isolates |
|-----------|------|-------|-------|-----------|-------------|-----------------|
| Mount | CLONE_NEWNS | 0x00020000 | 2.4.19 | mnt_namespace | fs/namespace.c | filesystem topology |
| UTS | CLONE_NEWUTS | 0x04000000 | 2.6.19 | uts_namespace | kernel/utsname.c | hostname, domainname |
| IPC | CLONE_NEWIPC | 0x08000000 | 2.6.19 | ipc_namespace | ipc/namespace.c | SysV IPC, POSIX MQ |
| PID | CLONE_NEWPID | 0x20000000 | 3.8 | pid_namespace | kernel/pid_namespace.c | PID number space |
| Net | CLONE_NEWNET | 0x40000000 | 2.6.24 | net | net/core/net_namespace.c | network stack |
| User | CLONE_NEWUSER | 0x10000000 | 3.8 | user_namespace | kernel/user_namespace.c | UID/GID mappings |
| Cgroup | CLONE_NEWCGROUP | 0x02000000 | 4.6 | cgroup_namespace | kernel/cgroup/namespace.c | cgroup hierarchy view |
| Time | CLONE_NEWTIME | 0x00000080 | 5.6 | time_namespace | kernel/time_namespace.c | monotonic/boot clocks |

- [ ] **Step 2: git add and commit**

```bash
git add 02-namespaces/kernel/02-b-namespace-types.md
git commit -m "feat(ch02): all 8 namespace types deep dive"
```

---

## Task 3: `setns()`, `unshare()`, and `/proc/self/ns/` mechanics

**Files:**
- Create: `02-namespaces/kernel/02-c-setns-unshare.md`

- [ ] **Step 1: Write `02-namespaces/kernel/02-c-setns-unshare.md`**

**Section 1 — `/proc/<pid>/ns/` directory**
- Each entry is a bind-mountable file; its inode number is the namespace identity
- `stat /proc/$PID/ns/net` → inode is globally unique namespace ID
- `/proc/self/ns/` contains: cgroup, ipc, mnt, net, pid, pid_for_children, time, time_for_children, user, uts
- `nsfs` pseudo-filesystem: https://elixir.bootlin.com/linux/v6.9/source/fs/nsfs.c
- `ns_get_path()` in `fs/nsfs.c` — how the inode is created
- Bind-mounting: `mount --bind /proc/$PID/ns/net /tmp/saved-net-ns` preserves the namespace even after all processes in it exit

**Section 2 — `setns()` — joining an existing namespace**
- Syscall: `int setns(int fd, int nstype)` — `fd` is an open file descriptor to a `/proc/*/ns/*` file; `nstype` is the expected namespace type (0 = any)
- Kernel path: `SYSCALL_DEFINE2(setns, ...)` → https://elixir.bootlin.com/linux/v6.9/source/kernel/nsproxy.c
- Implementation: `validate_ns()` checks the fd, `ns->ops->install()` calls the namespace-specific install function (e.g., `netns_install()`, `mntns_install()`)
- Restrictions: cannot join a user namespace where you'd gain capabilities you don't have; cannot join a PID namespace that is a descendant of your current one
- How runc uses it: joins the network namespace created during `RunPodSandbox` (the pause container's netns)

**Section 3 — `unshare()` — detaching from shared namespaces without forking**
- Syscall: `int unshare(int flags)` — same `CLONE_NEW*` flags as `clone()`
- Kernel path: `SYSCALL_DEFINE1(unshare, ...)` → `kernel/fork.c:ksys_unshare()` → https://elixir.bootlin.com/linux/v6.9/source/kernel/fork.c
- Difference from `clone()`: unshare operates on the calling process in place — no child is created
- `unshare(CLONE_NEWNS)` → calls `unshare_nsproxy_namespaces()` → allocates new `nsproxy` pointing to a new `mnt_namespace` (copy of current)
- `unshare(CLONE_NEWTIME)` special case: sets `time_ns_for_children` but NOT `time_ns` — the calling process keeps its old time view; only new children see the new offsets
- Shell usage: `unshare --mount --pid --fork bash` — used in exercises

**Section 4 — The namespace file descriptor lifecycle**
```c
// Opening a namespace fd (as runc does):
int ns_fd = open("/proc/<pid>/ns/net", O_RDONLY | O_CLOEXEC);

// Using it:
setns(ns_fd, CLONE_NEWNET);   // current thread joins that network namespace

// Preserving it across all process exits:
mount("/proc/<pid>/ns/net", "/tmp/netns", "none", MS_BIND, NULL);
// Now /tmp/netns holds the namespace open even after PID <pid> exits
```

**Section 5 — Live observation tools**
```bash
# List all namespaces on the host
lsns

# List specific type
lsns -t net

# Enter a namespace (all types)
nsenter --target $PID --mount --uts --ipc --net --pid -- bash

# Enter just the network namespace
nsenter --net=/proc/$PID/ns/net -- ip addr

# Compare namespace IDs of two processes
stat -L /proc/$PID1/ns/net /proc/$PID2/ns/net
# Same ino → same network namespace

# bpftrace: trace every setns() call
bpftrace -e 'kprobe:__sys_setns {
    printf("pid=%d comm=%s nstype=0x%x\n", pid, comm, (int)arg1);
}'

# bpftrace: trace namespace creation
bpftrace -e 'kprobe:create_new_namespaces {
    printf("pid=%d comm=%s flags=0x%lx\n", pid, comm,
        ((struct kernel_clone_args *)arg0)->flags);
}'
```

- [ ] **Step 2: git add and commit**

```bash
git add 02-namespaces/kernel/02-c-setns-unshare.md
git commit -m "feat(ch02): setns, unshare, and proc ns mechanics"
```

---

## Task 4: K8s connection doc

**Files:**
- Create: `02-namespaces/k8s/02-k8s-connection.md`

- [ ] **Step 1: Write `02-namespaces/k8s/02-k8s-connection.md`**

**Section 1 — Which namespaces runc creates for a pod**

Table showing each namespace, whether runc creates a new one or joins an existing one, and which component is responsible:

| Namespace | Default pod behaviour | Component | Kernel call |
|-----------|----------------------|-----------|-------------|
| Network | Shared with pause container | runc joins via `setns()` | `setns(pause_netns_fd, CLONE_NEWNET)` |
| IPC | Shared with pause container | runc joins via `setns()` | `setns(pause_ipcns_fd, CLONE_NEWIPC)` |
| PID | Each container gets its own (default) | runc `clone3()` | `CLONE_NEWPID` |
| Mount | Each container gets its own | runc `clone3()` | `CLONE_NEWNS` |
| UTS | Each container gets its own (pod hostname) | runc `clone3()` | `CLONE_NEWUTS` |
| User | NOT used by default | — | (rootless containers only) |
| Cgroup | Each container gets its own | runc `clone3()` | `CLONE_NEWCGROUP` |
| Time | NOT used | — | (not supported by runc by default) |

**Section 2 — The pause container namespace lifecycle**

1. kubelet → containerd: `RunPodSandbox` gRPC call
2. containerd creates pause container: `clone3(CLONE_NEWNET|CLONE_NEWIPC|CLONE_NEWUTS|CLONE_NEWPID|CLONE_NEWNS|CLONE_NEWCGROUP)`
3. pause container runs `pause()` syscall — holds all namespaces open
4. For each app container: containerd calls `CreateContainer` → runc opens `/proc/<pause_pid>/ns/{net,ipc}` → `setns()` → then `clone3()` with remaining flags for new mount/pid/uts/cgroup namespaces

**Section 3 — Verifying namespace sharing per pod**

```bash
# Get pause container PID for a pod
POD_NAME=nginx
PAUSE_PID=$(crictl pods --name $POD_NAME -q | xargs crictl inspectp | jq -r '.info.pid')

# Get app container PID
APP_PID=$(crictl ps --pod $(crictl pods --name $POD_NAME -q) -q | head -1 | xargs crictl inspect | jq -r '.info.pid')

# Verify net and IPC namespaces are shared (same inode)
stat -L /proc/$PAUSE_PID/ns/net /proc/$APP_PID/ns/net
# Both should show same ino

# Verify mount and PID namespaces are separate (different inodes)
stat -L /proc/$PAUSE_PID/ns/mnt /proc/$APP_PID/ns/mnt
stat -L /proc/$PAUSE_PID/ns/pid /proc/$APP_PID/ns/pid
```

**Section 4 — `hostNetwork`, `hostPID`, `hostIPC` pod spec options**

Explain what each does at the kernel level:
- `hostNetwork: true` → runc calls `setns()` with the host's `/proc/1/ns/net` fd instead of the pause container's
- `hostPID: true` → `CLONE_NEWPID` is NOT passed to `clone3()` — child joins host PID namespace
- `hostIPC: true` → `CLONE_NEWIPC` is NOT passed — child joins host IPC namespace

Verify each with `kubectl get pod -o yaml` + `lsns` comparison.

**Section 5 — kube-inspect integration preview**
The `kube-inspect --pod <uid>` command after checkpoint 02 will show all namespace inodes per process, grouping shared namespaces.

- [ ] **Step 2: git add and commit**

```bash
git add 02-namespaces/k8s/02-k8s-connection.md
git commit -m "feat(ch02): k8s connection doc — namespace isolation model"
```

---

## Task 5: C exercise — `mini-unshare`

**Files:**
- Create: `02-namespaces/exercises/mini-unshare/mini_unshare.c`
- Create: `02-namespaces/exercises/mini-unshare/Makefile`
- Create: `02-namespaces/exercises/mini-unshare/README.md`
- Create: `02-namespaces/exercises/mini-unshare/.gitignore`

**What it demonstrates:** Implement a minimal `unshare(1)` clone in C. The program calls `unshare()` with user-specified namespace flags, then `execvp()` a shell command in the new namespaces. Teaches: `unshare()` syscall, namespace flag parsing, how namespace isolation is visible from inside vs outside.

- [ ] **Step 1: Write `mini_unshare.c`**

```c
#define _GNU_SOURCE
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>
#include <sched.h>
#include <sys/types.h>
#include <sys/wait.h>
#include <sys/mount.h>
#include <sys/stat.h>
#include <fcntl.h>
#include <errno.h>

static const struct {
    const char *name;
    int flag;
    const char *desc;
} ns_flags[] = {
    { "mount",   CLONE_NEWNS,      "mount namespace"   },
    { "uts",     CLONE_NEWUTS,     "UTS namespace"     },
    { "ipc",     CLONE_NEWIPC,     "IPC namespace"     },
    { "net",     CLONE_NEWNET,     "network namespace" },
    { "pid",     CLONE_NEWPID,     "PID namespace"     },
    { "user",    CLONE_NEWUSER,    "user namespace"    },
    { "cgroup",  CLONE_NEWCGROUP,  "cgroup namespace"  },
};
#define NS_FLAGS_LEN (sizeof(ns_flags) / sizeof(ns_flags[0]))

static void usage(const char *prog) {
    fprintf(stderr, "usage: %s [--<ns>...] -- <command> [args...]\n\n", prog);
    fprintf(stderr, "Namespace options:\n");
    for (size_t i = 0; i < NS_FLAGS_LEN; i++)
        fprintf(stderr, "  --%-10s  create new %s\n", ns_flags[i].name, ns_flags[i].desc);
    fprintf(stderr, "\nExamples:\n");
    fprintf(stderr, "  %s --uts -- hostname\n", prog);
    fprintf(stderr, "  %s --net -- ip addr\n", prog);
    fprintf(stderr, "  %s --user --pid --mount -- bash\n", prog);
}

int main(int argc, char *argv[]) {
    int flags = 0;
    int cmd_idx = -1;

    for (int i = 1; i < argc; i++) {
        if (strcmp(argv[i], "--") == 0) {
            cmd_idx = i + 1;
            break;
        }
        if (strncmp(argv[i], "--", 2) != 0) {
            fprintf(stderr, "error: expected --<namespace> or --, got '%s'\n", argv[i]);
            usage(argv[0]);
            return 1;
        }
        const char *name = argv[i] + 2;
        int found = 0;
        for (size_t j = 0; j < NS_FLAGS_LEN; j++) {
            if (strcmp(name, ns_flags[j].name) == 0) {
                flags |= ns_flags[j].flag;
                found = 1;
                break;
            }
        }
        if (!found) {
            fprintf(stderr, "error: unknown namespace '%s'\n", name);
            usage(argv[0]);
            return 1;
        }
    }

    if (flags == 0 || cmd_idx < 0 || cmd_idx >= argc) {
        usage(argv[0]);
        return 1;
    }

    /* Show our namespace inodes BEFORE unshare */
    printf("[before] /proc/self/ns/net  -> ");
    fflush(stdout);
    system("readlink /proc/self/ns/net");
    printf("[before] /proc/self/ns/uts  -> ");
    fflush(stdout);
    system("readlink /proc/self/ns/uts");
    printf("[before] /proc/self/ns/mnt  -> ");
    fflush(stdout);
    system("readlink /proc/self/ns/mnt");

    /* Call unshare() — detach from shared namespaces */
    if (unshare(flags) < 0) {
        perror("unshare");
        if (errno == EPERM)
            fprintf(stderr, "hint: try running with sudo, or add --user to use unprivileged user namespace\n");
        return 1;
    }

    /* Show our namespace inodes AFTER unshare */
    printf("[after]  /proc/self/ns/net  -> ");
    fflush(stdout);
    system("readlink /proc/self/ns/net");
    printf("[after]  /proc/self/ns/uts  -> ");
    fflush(stdout);
    system("readlink /proc/self/ns/uts");
    printf("[after]  /proc/self/ns/mnt  -> ");
    fflush(stdout);
    system("readlink /proc/self/ns/mnt");

    /* If PID namespace was requested, fork so the exec'd child is PID 1 in it.
     * unshare(CLONE_NEWPID) only affects CHILDREN, not the calling process. */
    if (flags & CLONE_NEWPID) {
        pid_t child = fork();
        if (child < 0) { perror("fork"); return 1; }
        if (child > 0) {
            int status;
            waitpid(child, &status, 0);
            return WIFEXITED(status) ? WEXITSTATUS(status) : 1;
        }
        /* child continues and execs below */
    }

    /* Mount a new /proc if we have a new PID and mount namespace */
    if ((flags & CLONE_NEWPID) && (flags & CLONE_NEWNS)) {
        if (mount("proc", "/proc", "proc", 0, NULL) < 0)
            perror("mount /proc (continuing anyway)");
    }

    execvp(argv[cmd_idx], argv + cmd_idx);
    perror("execvp");
    return 1;
}
```

- [ ] **Step 2: Write `Makefile`**

```makefile
.PHONY: build run-uts run-net run-pid clean

TARGET = mini_unshare

build:
	gcc -Wall -Wextra -Werror -o $(TARGET) $(TARGET).c

# Demo 1: new UTS namespace — hostname change is isolated
run-uts: build
	sudo ./$(TARGET) --uts -- sh -c 'hostname container-demo && hostname'

# Demo 2: new network namespace — only loopback visible
run-net: build
	sudo ./$(TARGET) --net -- ip addr

# Demo 3: new PID + mount namespace — ps shows only our processes
run-pid: build
	sudo ./$(TARGET) --pid --mount -- ps aux

clean:
	rm -f $(TARGET)
```

- [ ] **Step 3: Write `.gitignore`**

```
mini_unshare
```

- [ ] **Step 4: Write `README.md`**

Sections:
1. **What it demonstrates** — `unshare()` syscall, namespace flag API, before/after inode comparison proving namespace creation
2. **Build and run** — `make run-uts`, `make run-net`, `make run-pid` with expected output for each
3. **Expected output for `make run-uts`**:
```
[before] /proc/self/ns/net  -> net:[4026531992]
[before] /proc/self/ns/uts  -> uts:[4026531838]
[before] /proc/self/ns/mnt  -> mnt:[4026531841]
[after]  /proc/self/ns/net  -> net:[4026531992]     ← unchanged (didn't unshare)
[after]  /proc/self/ns/uts  -> uts:[4026532247]     ← NEW inode — new namespace
[after]  /proc/self/ns/mnt  -> mnt:[4026531841]     ← unchanged
container-demo                                       ← hostname changed inside ns
```
4. **Kernel references** — `SYSCALL_DEFINE1(unshare)` in `kernel/fork.c:ksys_unshare()`, `unshare_nsproxy_namespaces()`, `create_new_namespaces()` in `kernel/nsproxy.c`
5. **Exercises** — (a) Add `--user` to avoid `sudo`. (b) Add `--net` + run `ip link add dummy0 type dummy` — verify the dummy interface is not visible from the host. (c) Observe the namespace inode in `/proc/self/ns/` from inside the child and from another terminal on the host — they should match.
6. **K8s connection** — runc does exactly this for every container: `unshare()` for mount/uts/cgroup namespaces, `setns()` for net/ipc (joining the pause container's namespaces)

- [ ] **Step 5: Test**

```bash
cd 02-namespaces/exercises/mini-unshare
make build
# Expected: compiles with no warnings
sudo make run-uts
# Expected: before/after inode output, hostname changed inside
```

- [ ] **Step 6: git add and commit**

```bash
cd ../../..
git add 02-namespaces/exercises/mini-unshare/
git commit -m "feat(ch02): mini-unshare C exercise"
```

---

## Task 6: Go exercise — `namespace-inspector`

**Files:**
- Create: `02-namespaces/exercises/namespace-inspector/go.mod`
- Create: `02-namespaces/exercises/namespace-inspector/main.go`
- Create: `02-namespaces/exercises/namespace-inspector/Makefile`
- Create: `02-namespaces/exercises/namespace-inspector/README.md`
- Create: `02-namespaces/exercises/namespace-inspector/.gitignore`

**What it demonstrates:** For a given PID (or all processes), read all 10 namespace symlinks from `/proc/<pid>/ns/`, resolve each inode, group processes that share namespaces, and show a human-readable namespace map.

- [ ] **Step 1: Write `go.mod`**

```
module github.com/linux-to-k8s/namespace-inspector

go 1.22
```

- [ ] **Step 2: Write `main.go`**

```go
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

// NsInfo holds all namespace identifiers for one process.
type NsInfo struct {
	PID      int
	Comm     string
	NS       map[string]uint64 // namespace type → inode number
}

// nsTypes is the ordered list of namespace symlinks we inspect.
var nsTypes = []string{
	"cgroup", "ipc", "mnt", "net", "pid",
	"pid_for_children", "time", "time_for_children", "user", "uts",
}

func readComm(pid int) string {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
	if err != nil {
		return "?"
	}
	return strings.TrimSpace(string(data))
}

// nsInode returns the inode number of /proc/<pid>/ns/<nstype>.
// Returns 0 if the file is unreadable (permission denied, process gone, etc).
func nsInode(pid int, nstype string) uint64 {
	var st syscall.Stat_t
	path := fmt.Sprintf("/proc/%d/ns/%s", pid, nstype)
	if err := syscall.Stat(path, &st); err != nil {
		return 0
	}
	return st.Ino
}

// nsSymlink reads the symlink target (e.g. "net:[4026531992]") for display.
func nsSymlink(pid int, nstype string) string {
	target, err := os.Readlink(fmt.Sprintf("/proc/%d/ns/%s", pid, nstype))
	if err != nil {
		return "?"
	}
	return target
}

func listAllProcesses() ([]NsInfo, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var result []NsInfo
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		ns := make(map[string]uint64, len(nsTypes))
		for _, t := range nsTypes {
			ns[t] = nsInode(pid, t)
		}
		result = append(result, NsInfo{
			PID:  pid,
			Comm: readComm(pid),
			NS:   ns,
		})
	}
	return result, nil
}

func inspectPID(pid int) {
	comm := readComm(pid)
	fmt.Printf("PID %d (%s) namespace map:\n", pid, comm)
	fmt.Printf("  %-22s  %-30s  %s\n", "TYPE", "SYMLINK", "INODE")
	fmt.Printf("  %-22s  %-30s  %s\n", strings.Repeat("-", 22), strings.Repeat("-", 30), strings.Repeat("-", 18))
	for _, t := range nsTypes {
		sym := nsSymlink(pid, t)
		ino := nsInode(pid, t)
		inoStr := "-"
		if ino != 0 {
			inoStr = strconv.FormatUint(ino, 10)
		}
		fmt.Printf("  %-22s  %-30s  %s\n", t, sym, inoStr)
	}
}

func findSharedNamespaces(procs []NsInfo) {
	// For each namespace type, group PIDs that share the same inode.
	fmt.Printf("\n=== Shared namespaces across all processes ===\n")
	for _, t := range []string{"net", "pid", "mnt", "ipc", "uts", "user", "cgroup"} {
		groups := make(map[uint64][]NsInfo)
		for _, p := range procs {
			ino := p.NS[t]
			if ino == 0 {
				continue
			}
			groups[ino] = append(groups[ino], p)
		}
		// Show groups with more than one process (shared namespaces)
		var inodes []uint64
		for ino := range groups {
			if len(groups[ino]) > 1 {
				inodes = append(inodes, ino)
			}
		}
		if len(inodes) == 0 {
			continue
		}
		sort.Slice(inodes, func(i, j int) bool { return inodes[i] < inodes[j] })
		fmt.Printf("\n%s namespace sharing:\n", t)
		for _, ino := range inodes {
			group := groups[ino]
			sort.Slice(group, func(i, j int) bool { return group[i].PID < group[j].PID })
			pids := make([]string, len(group))
			for i, p := range group {
				pids[i] = fmt.Sprintf("%d(%s)", p.PID, p.Comm)
			}
			// Only show groups of 2-8 processes (larger groups are host namespaces)
			if len(group) <= 8 {
				fmt.Printf("  inode %d: %s\n", ino, strings.Join(pids, ", "))
			} else {
				fmt.Printf("  inode %d: %d processes (host namespace)\n", ino, len(group))
			}
		}
	}
}

func main() {
	if len(os.Args) >= 2 {
		// Single PID mode
		pid, err := strconv.Atoi(os.Args[1])
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: invalid PID %q\n", os.Args[1])
			os.Exit(1)
		}
		inspectPID(pid)

		// Also show which other processes share each namespace with this PID
		procs, err := listAllProcesses()
		if err != nil {
			fmt.Fprintf(os.Stderr, "error listing /proc: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("\n=== Processes sharing namespaces with PID %d ===\n", pid)
		var target NsInfo
		for _, p := range procs {
			if p.PID == pid {
				target = p
				break
			}
		}
		for _, t := range []string{"net", "mnt", "pid", "ipc", "uts"} {
			targetIno := target.NS[t]
			if targetIno == 0 {
				continue
			}
			var peers []string
			for _, p := range procs {
				if p.PID != pid && p.NS[t] == targetIno {
					peers = append(peers, fmt.Sprintf("%d(%s)", p.PID, p.Comm))
				}
			}
			if len(peers) > 0 && len(peers) <= 10 {
				fmt.Printf("  %s: shared with %s\n", t, strings.Join(peers, ", "))
			} else if len(peers) > 10 {
				fmt.Printf("  %s: host namespace (%d processes)\n", t, len(peers))
			}
		}
		return
	}

	// All-processes mode: show shared namespace groups
	procs, err := listAllProcesses()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Scanned %d processes\n", len(procs))
	findSharedNamespaces(procs)

	// Show namespace symlinks for a few interesting paths
	fmt.Printf("\n=== Your shell's namespaces ===\n")
	inspectPID(os.Getpid())

	// Show count of distinct namespaces per type
	fmt.Printf("\n=== Distinct namespace count per type ===\n")
	for _, t := range []string{"net", "mnt", "pid", "ipc", "uts", "cgroup"} {
		seen := make(map[uint64]struct{})
		for _, p := range procs {
			if ino := p.NS[t]; ino != 0 {
				seen[ino] = struct{}{}
			}
		}
		fmt.Printf("  %-22s  %d distinct namespaces\n", t, len(seen))
	}

	// Find the ns symlinks path for reference
	fmt.Printf("\n=== /proc/self/ns/ contents ===\n")
	links, _ := filepath.Glob("/proc/self/ns/*")
	for _, l := range links {
		target, err := os.Readlink(l)
		if err == nil {
			fmt.Printf("  %s -> %s\n", filepath.Base(l), target)
		}
	}
}
```

- [ ] **Step 3: Write `Makefile`**

```makefile
.PHONY: build run run-pid clean

TARGET = namespace-inspector

build:
	go build -o $(TARGET) .

# Show shared namespaces across all processes
run: build
	./$(TARGET)

# Show all namespaces for a specific PID
run-pid: build
	./$(TARGET) $(PID)

clean:
	rm -f $(TARGET)
```

- [ ] **Step 4: Write `.gitignore`**

```
namespace-inspector
```

- [ ] **Step 5: Write `README.md`**

Sections:
1. **What it demonstrates** — reading `/proc/<pid>/ns/` symlinks, inode as canonical namespace identity, grouping processes by shared namespaces
2. **Build and run**:
   - `make run` — scans all processes, shows shared namespace groups. On a node running containers: shows container groups with 2-3 processes sharing net/ipc namespaces
   - `make run PID=<n>` — shows all 10 namespace symlinks for a specific PID and which processes share each
3. **Expected output (make run, simplified)**:
```
Scanned 312 processes

=== Shared namespaces across all processes ===

net namespace sharing:
  inode 4026531992: host namespace (288 processes)
  inode 4026532456: 3342(pause), 3401(nginx), 3445(sidecar)

pid namespace sharing:
  inode 4026532120: 3401(nginx), 3445(sidecar)

=== Your shell's namespaces ===
PID 47821 (bash) namespace map:
  TYPE                    SYMLINK                         INODE
  ----------------------  ------------------------------  ------------------
  net                     net:[4026531992]                4026531992
  mnt                     mnt:[4026531841]                4026531841
  ...

=== Distinct namespace count per type ===
  net                     4 distinct namespaces
  mnt                     7 distinct namespaces
```
4. **Kernel references** — `fs/nsfs.c:ns_get_path()`, `fs/proc/base.c:proc_ns_dir_inode_operations`, `include/linux/nsproxy.h:struct nsproxy`
5. **Exercises** — (a) Find which namespace is shared between nginx and sidecar containers in a pod. (b) Add a `--json` flag for machine-readable output. (c) Add filtering by namespace type: `./namespace-inspector --net` shows only net namespace groups.
6. **K8s connection** — this is exactly how `kube-inspect` identifies pod boundaries: processes that share a network namespace inode belong to the same pod

- [ ] **Step 6: Test**

```bash
cd 02-namespaces/exercises/namespace-inspector
go build -o namespace-inspector .
go vet ./...
./namespace-inspector
# Expected: "Scanned N processes", shared namespace groups, your shell's namespace map
```

- [ ] **Step 7: git add and commit**

```bash
cd ../../..
git add 02-namespaces/exercises/namespace-inspector/
git commit -m "feat(ch02): namespace-inspector Go exercise"
```

---

## Task 7: kube-inspect checkpoint 02 — namespace enumeration per pod

**Files:**
- Modify: `kube-inspect/internal/proc/proc.go` (add `NsInfo` struct and `ListPodNamespaces()`)
- Modify: `kube-inspect/cmd/kube-inspect/main.go` (add `--namespaces` flag)
- Modify: `kube-inspect/CHECKPOINT.md` (mark checkpoint 02 done)

**Interfaces:**
- Consumes: `proc.ListPodProcesses(podUID)` from checkpoint 01
- Produces: `proc.NsInfo`, `proc.ListPodNamespaces(podUID) ([]NsInfo, error)`

- [ ] **Step 1: Add `NsInfo` and `ListPodNamespaces` to `internal/proc/proc.go`**

Add to the end of the existing file (after `NsSymlinks`):

```go
// NsInfo holds the namespace identifiers for one process in a pod.
type NsInfo struct {
	PID  int
	Comm string
	// NS maps namespace type (e.g. "net", "mnt") to its inode number.
	// Inode is the canonical namespace identity: two processes with the same
	// inode share that namespace (see fs/nsfs.c:ns_get_path).
	NS map[string]uint64
}

// nsTypes is the set of namespace types we read from /proc/<pid>/ns/.
var nsTypes = []string{"cgroup", "ipc", "mnt", "net", "pid", "user", "uts"}

func readNsInode(pid int, nstype string) uint64 {
	var st syscall.Stat_t
	if err := syscall.Stat(fmt.Sprintf("/proc/%d/ns/%s", pid, nstype), &st); err != nil {
		return 0
	}
	return st.Ino
}

// ListPodNamespaces returns per-process namespace info for all processes
// belonging to the given pod UID (matched via cgroup v2 path).
func ListPodNamespaces(podUID string) ([]NsInfo, error) {
	procs, err := ListPodProcesses(podUID)
	if err != nil {
		return nil, fmt.Errorf("listing pod processes: %w", err)
	}
	result := make([]NsInfo, 0, len(procs))
	for _, p := range procs {
		ns := make(map[string]uint64, len(nsTypes))
		for _, t := range nsTypes {
			ns[t] = readNsInode(p.PID, t)
		}
		result = append(result, NsInfo{
			PID:  p.PID,
			Comm: p.Comm,
			NS:   ns,
		})
	}
	return result, nil
}
```

Also add `"syscall"` to the imports if not already present.

- [ ] **Step 2: Add `--namespaces` flag to `cmd/kube-inspect/main.go`**

Add the flag declaration and handling:

```go
// Add to var block:
flagNamespaces = flag.Bool("namespaces", false, "Show namespace inodes per process")

// Add to the --pod handling block, after the process table:
if *flagNamespaces {
    nsInfos, err := proc.ListPodNamespaces(*flagPod)
    if err != nil {
        fmt.Fprintf(os.Stderr, "namespaces error: %v\n", err)
    } else {
        fmt.Printf("\nNamespace map for pod %s:\n", *flagPod)
        fmt.Printf("  %-8s %-20s %-10s %-10s %-10s %-10s %-10s %-10s %-10s\n",
            "PID", "COMM", "cgroup", "ipc", "mnt", "net", "pid", "user", "uts")
        for _, n := range nsInfos {
            fmt.Printf("  %-8d %-20s %-10d %-10d %-10d %-10d %-10d %-10d %-10d\n",
                n.PID, n.Comm,
                n.NS["cgroup"], n.NS["ipc"], n.NS["mnt"],
                n.NS["net"], n.NS["pid"], n.NS["user"], n.NS["uts"])
        }
    }
}
```

- [ ] **Step 3: Update `CHECKPOINT.md`**

Change checkpoint 02 row from `pending` to `done`.

- [ ] **Step 4: Verify**

```bash
cd kube-inspect
go vet ./...
# Expected: no output

make build
# Expected: bin/kube-inspect created

./bin/kube-inspect --pod nonexistent --namespaces
# Expected:
# Processes for pod nonexistent:
#   PID      PPID     COMM      ...
# Total: 0 processes
#
# Namespace map for pod nonexistent:
#   PID      COMM     cgroup  ipc  mnt  net  pid  user  uts
```

- [ ] **Step 5: git add and commit**

```bash
cd ..
git add kube-inspect/
git commit -m "feat(kube-inspect): checkpoint 02 — namespace enumeration per pod"
```

---

## Self-Review

**Spec coverage:**
- ✅ `02-namespaces/README.md` — objectives, reading order, big picture diagram
- ✅ `kernel/02-a-nsproxy.md` — struct nsproxy field-by-field, lifecycle, locking, object graph, live observation
- ✅ `kernel/02-b-namespace-types.md` — all 8 namespace types with struct definitions, kernel files, observation
- ✅ `kernel/02-c-setns-unshare.md` — setns(), unshare(), /proc/self/ns/ bind-mount mechanics
- ✅ `k8s/02-k8s-connection.md` — which namespaces runc creates/joins, verification commands, hostNetwork/hostPID/hostIPC
- ✅ `exercises/mini-unshare/` — C program, Makefile, README, .gitignore
- ✅ `exercises/namespace-inspector/` — Go program, go.mod, Makefile, README, .gitignore
- ✅ `kube-inspect/` — `NsInfo`, `ListPodNamespaces()`, `--namespaces` flag, checkpoint 02 marked done

**Placeholder scan:** None found — all code blocks are complete.

**Type consistency:** `proc.NsInfo` defined in Task 7 step 1; `proc.ListPodNamespaces()` defined same step; consumed in step 2 — names match exactly.
