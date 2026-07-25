# 02-b — Linux Namespace Types: All Eight In Depth

Linux namespaces are the kernel mechanism that turns a single system into the illusion of many independent systems. Each namespace type wraps a specific slice of global kernel state and presents every process inside it with a private copy of that state. Together, the eight namespace types cover the filesystem mount table, hostname, IPC objects, process IDs, the full network stack, user and group identity, the cgroup hierarchy view, and monotonic clock offsets. Introduced incrementally from Linux 2.4.19 through 5.6, they collectively enable what users call "containers" — isolated workloads sharing a single kernel without hypervisor overhead. This document covers each type with the same format: what it isolates, the key kernel struct and its fields, how the kernel copies or initialises it on `clone(2)`/`unshare(2)`, and how to observe it live from the command line or with bpftrace.

---

## Summary Table

| Namespace | Flag | Hex Value | Kernel version added | Key struct | Kernel file | What it isolates |
|-----------|------|-----------|---------------------|------------|-------------|-----------------|
| Mount | `CLONE_NEWNS` | `0x00020000` | 2.4.19 | `mnt_namespace` | `fs/namespace.c` | filesystem topology (mount table) |
| UTS | `CLONE_NEWUTS` | `0x04000000` | 2.6.19 | `uts_namespace` | `kernel/utsname.c` | hostname, NIS domainname |
| IPC | `CLONE_NEWIPC` | `0x08000000` | 2.6.19 | `ipc_namespace` | `ipc/namespace.c` | SysV IPC, POSIX MQ |
| PID | `CLONE_NEWPID` | `0x20000000` | 3.8 | `pid_namespace` | `kernel/pid_namespace.c` | PID number space |
| Network | `CLONE_NEWNET` | `0x40000000` | 2.6.24 | `net` | `net/core/net_namespace.c` | full network stack |
| User | `CLONE_NEWUSER` | `0x10000000` | 3.8 | `user_namespace` | `kernel/user_namespace.c` | UID/GID mappings, capabilities |
| Cgroup | `CLONE_NEWCGROUP` | `0x02000000` | 4.6 | `cgroup_namespace` | `kernel/cgroup/namespace.c` | cgroup hierarchy view |
| Time | `CLONE_NEWTIME` | `0x00000080` | 5.6 | `time_namespace` | `kernel/time_namespace.c` | `CLOCK_MONOTONIC`/`CLOCK_BOOTTIME` offsets |

---

## 1. Mount Namespace — `CLONE_NEWNS` (`0x00020000`)

### Introduced

Linux 2.4.19 (2002). The mount namespace is the oldest namespace in the kernel and the only one that does not follow the `CLONE_NEW*` naming convention it predates — `CLONE_NEWNS` reads "new namespace" generically, because at the time no one expected there would be more than one kind.

### What it isolates

The mount table: the ordered list of `struct mount` objects that describes which filesystem is attached at which path. When a process calls `mount(2)` or `umount(2)`, the change is applied to the mount namespace it belongs to. Other namespaces are completely unaffected. Each namespace starts as a copy of its parent's mount table at the moment of `clone(2)` or `unshare(2)`, after which the two trees diverge independently.

This is the mechanism behind bind mounts inside containers and the `/proc` overlay that makes `nsenter(1)` work: the container's rootfs is a private mount tree entirely separate from the host's.

### Key struct: `struct mnt_namespace`

Source: [`fs/mount.h`](https://elixir.bootlin.com/linux/v6.9/source/fs/mount.h)

```c
struct mnt_namespace {
    struct user_namespace   *user_ns;   /* owning user namespace */
    struct ns_common         ns;        /* common namespace header */
    struct list_head         list;      /* all struct mount objects in this ns */
    spinlock_t               ns_binfmt_lock;
    struct rb_root           mounts;    /* rbtree of all mounts keyed by mnt_id */
    struct mount            *root;      /* root mount of this namespace */
    unsigned int             mounts;    /* total mount count */
    seqlock_t                lock;      /* protects list iteration */
    u64                      seq;       /* sequence number for pending notifications */
    wait_queue_head_t        poll;      /* fanotify/inotify waiters */
    u64                      event;     /* mount event counter */
};
```

Field-by-field breakdown:

- **`user_ns`** (`struct user_namespace *`): The user namespace that owns this mount namespace. Determines which UID can administer it. Set at creation time by `alloc_mnt_ns()`. Read by permission checks whenever a process attempts `mount(2)` inside the namespace.

- **`ns`** (`struct ns_common`): The common header embedded in every namespace struct. Contains:
  - `atomic_long_t count` — reference count; the namespace is freed when this hits zero.
  - `unsigned int inum` — the inode number of the `/proc/self/ns/mnt` symlink target; stays stable for the lifetime of the namespace and is the canonical namespace identity token.
  - `const struct proc_ns_operations *ops` — vtable pointer used by `/proc` to implement `open`, `install`, `get`, and `put` operations on the namespace fd.

- **`list`** (`struct list_head`): Head of the doubly-linked list threading all `struct mount` objects that belong to this namespace. The kernel iterates this list to implement `getmntent(3)`, `findmnt(8)`, and `/proc/mounts`.

- **`mounts`** (`unsigned int`): Running count of mounted filesystems in this namespace. Checked against the per-user-namespace limit `sysctl_mount_max` on each new mount to prevent unbounded resource consumption.

- **`lock`** (`seqlock_t`): Reader-writer lock using sequence numbers. Writers (mount/umount operations) increment the sequence before and after the change; readers retry if the sequence changes during their read. This lets the kernel walk the mount list without taking a heavyweight lock on every read.

### Kernel copy path

`copy_mnt_ns()` in [`fs/namespace.c`](https://elixir.bootlin.com/linux/v6.9/source/fs/namespace.c) is called by `create_new_namespaces()` when `CLONE_NEWNS` is set. It allocates a fresh `mnt_namespace`, then deep-copies every `struct mount` from the parent namespace into the child, preserving the parent–child relationships but giving each copied mount its own `mnt_ns` pointer to the new namespace.

### Why "NEWNS" and not "NEWMNT"

When Janos Farkas and Al Viro implemented mount namespaces in 2001 for Linux 2.4.19, `CLONE_NEWNS` was the only new clone flag being added. The convention of `CLONE_NEW<RESOURCE>` did not yet exist. By the time UTS and IPC namespaces arrived in 2.6.19, the `CLONE_NEWNS` name was already embedded in userspace ABI and could not be changed.

### Live observation

```bash
# Show all mounts visible to a process
cat /proc/$PID/mounts

# Tree view of the mount namespace
findmnt --pid $PID

# Enter a container's mount namespace and inspect it
nsenter --mount=/proc/$PID/ns/mnt -- findmnt

# Count mount namespaces on the system
lsns -t mnt
```

### bpftrace — trace mount namespace creation

```
bpftrace -e 'kprobe:copy_mnt_ns { printf("pid=%d comm=%s new mount ns\n", pid, comm); }'
```

---

## 2. UTS Namespace — `CLONE_NEWUTS` (`0x04000000`)

### Introduced

Linux 2.6.19 (November 2006). "UTS" stands for UNIX Time-sharing System — the name of the `utsname` struct that the `uname(2)` syscall fills, which originated in early UNIX and was carried forward unchanged.

### What it isolates

The hostname (`uname -n` / `gethostname(2)`) and the NIS domainname (`domainname` / `getdomainname(2)`). Setting either value in one UTS namespace has no effect on any other UTS namespace. This is why every Kubernetes pod can report its own hostname (the pod name) without conflicting with other pods on the same node.

### Key struct: `struct uts_namespace`

Source: [`include/linux/utsname.h`](https://elixir.bootlin.com/linux/v6.9/source/include/linux/utsname.h)

```c
struct uts_namespace {
    struct new_utsname   name;      /* the actual strings */
    struct user_namespace *user_ns; /* owning user namespace */
    struct ucounts       *ucounts;  /* resource accounting */
    struct ns_common      ns;       /* common namespace header */
    struct rcu_head       rcu;      /* deferred RCU freeing */
};
```

Field-by-field breakdown:

- **`name`** (`struct new_utsname`): The payload. This is the same struct that the `uname(2)` syscall copies to userspace:
  ```c
  struct new_utsname {
      char sysname[65];    /* "Linux" */
      char nodename[65];   /* hostname — what gethostname() returns */
      char release[65];    /* kernel release, e.g. "6.9.0" */
      char version[65];    /* build version string */
      char machine[65];    /* architecture, e.g. "x86_64" */
      char domainname[65]; /* NIS domainname */
  };
  ```
  Only `nodename` and `domainname` are per-UTS-namespace. The other fields (`sysname`, `release`, `version`, `machine`) are inherited from `init_uts_ns` and are effectively read-only — they describe the running kernel, not the container.

- **`user_ns`** (`struct user_namespace *`): Owning user namespace. Controls who can call `sethostname(2)` inside this UTS namespace — requires `CAP_SYS_ADMIN` within the owning user namespace.

- **`ns`** (`struct ns_common`): Common header with reference count and inode number (visible as `/proc/self/ns/uts`).

- **`rcu`** (`struct rcu_head`): Used to defer the `kfree()` of the namespace struct until after all RCU readers (which access `name` without a lock) have finished their read-side critical sections. The UTS name fields are protected by `uts_sem` (a per-namespace rwsem) for writers and by RCU for readers.

### Kernel copy path

`copy_utsname()` in [`kernel/utsname.c`](https://elixir.bootlin.com/linux/v6.9/source/kernel/utsname.c). Allocates a new `uts_namespace` and copies the parent's `new_utsname` verbatim — the child starts with the parent's hostname and may then diverge.

### Live observation

```bash
# Read the hostname as seen by a specific process
nsenter --uts=/proc/$PID/ns/uts hostname

# Show all UTS namespaces on the system
lsns -t uts

# In Kubernetes: every pod gets its own UTS ns, hostname == pod name
kubectl exec -it $POD -- hostname
```

---

## 3. IPC Namespace — `CLONE_NEWIPC` (`0x08000000`)

### Introduced

Linux 2.6.19 (November 2006), alongside UTS namespaces.

### What it isolates

All System V IPC objects and POSIX message queues:

- **SysV semaphores** — `semget(2)`, `semop(2)`, `semctl(2)`. Each namespace has its own semaphore identifier space (`sem_ids`).
- **SysV message queues** — `msgget(2)`, `msgsnd(2)`, `msgrcv(2)`. Each namespace has its own message queue identifier space (`msg_ids`).
- **SysV shared memory** — `shmget(2)`, `shmat(2)`, `shmdt(2)`. Each namespace has its own shared memory identifier space (`shm_ids`).
- **POSIX message queues** — the `/dev/mqueue` pseudo-filesystem. Each IPC namespace mounts its own instance; `mq_open(3)` names are private to the namespace.

An IPC key that exists in one namespace is completely invisible in another. This prevents applications that use hardcoded SysV keys (common in legacy enterprise software) from interfering with each other.

### Key struct: `struct ipc_namespace`

Source: [`include/linux/ipc_namespace.h`](https://elixir.bootlin.com/linux/v6.9/source/include/linux/ipc_namespace.h)

```c
struct ipc_namespace {
    struct ipc_ids    ids[3];         /* [0]=sem, [1]=msg, [2]=shm */

    int               sem_ctls[4];    /* sysctl: SEMMSL,SEMMNS,SEMOPM,SEMMNI */
    int               used_sems;

    unsigned int      msg_ctlmax;     /* max message size */
    unsigned int      msg_ctlmnb;     /* max bytes per queue */
    unsigned int      msg_ctlmni;     /* max number of queues */
    struct percpu_counter msg_bytes;
    struct percpu_counter msg_hdrs;

    size_t            shm_ctlmax;     /* max shm segment size */
    size_t            shm_ctlall;     /* max total shm pages */
    unsigned long     shm_tot;        /* current total shm pages */
    int               shm_ctlmni;     /* max number of segments */
    int               shm_rmid_forced;

    struct notifier_block ipcns_nb;   /* for mqueue fs notifications */
    struct vfsmount  *mq_mnt;         /* /dev/mqueue mount */
    unsigned int      mq_queues_count;
    unsigned int      mq_queues_max;
    unsigned int      mq_msgsize_max;
    unsigned int      mq_msg_max;
    unsigned int      mq_unlink_timeout;

    struct user_namespace *user_ns;
    struct ucounts   *ucounts;
    struct llist_node mnt_llist;

    struct ns_common  ns;
} __randomize_layout;
```

Field-by-field breakdown (key fields):

- **`ids[3]`** (`struct ipc_ids`): The three identifier tables. Index 0 holds all semaphore sets, index 1 all message queues, index 2 all shared memory segments. Each `ipc_ids` contains a radix tree (`idr`) mapping integer keys to `struct kern_ipc_perm` objects, a mutex, and accounting counters. `ipcs -a` inside a namespace reads exactly these three tables.

- **`msg_ctlmax`** (`unsigned int`): Maximum allowed size in bytes for a single message. Corresponds to `/proc/sys/kernel/msgmax`. Setting this per-namespace lets containers have different message size limits from the host.

- **`msg_ctlmnb`** (`unsigned int`): Maximum number of bytes that can be queued in a single message queue. Corresponds to `/proc/sys/kernel/msgmnb`.

- **`msg_ctlmni`** (`unsigned int`): Maximum number of message queues in this namespace. Corresponds to `/proc/sys/kernel/msgmni`.

- **`shm_ctlmax`** (`size_t`): Maximum size in bytes of a single shared memory segment. Corresponds to `/proc/sys/kernel/shmmax`.

- **`shm_ctlall`** (`size_t`): Maximum total number of pages that can be used for shared memory across all segments in this namespace. Corresponds to `/proc/sys/kernel/shmall`.

- **`shm_tot`** (`unsigned long`): Running total of shared memory pages currently allocated in this namespace. Compared against `shm_ctlall` on each `shmget(2)`.

- **`mq_mnt`** (`struct vfsmount *`): The private `/dev/mqueue` mount for this namespace. POSIX MQ names resolve through this mount, so `mq_open("/myqueue")` in two different IPC namespaces refers to two completely unrelated queue objects.

- **`ns`** (`struct ns_common`): Common header with reference count and inode number for `/proc/self/ns/ipc`.

### Kernel copy path

`copy_ipcs()` in [`ipc/namespace.c`](https://elixir.bootlin.com/linux/v6.9/source/ipc/namespace.c). Creates a fresh `ipc_namespace` with empty `ids[]` tables — IPC objects are not inherited across namespace boundaries. The child starts with a clean slate.

### Live observation

```bash
# List all SysV IPC objects in the namespace of process $PID
nsenter --ipc=/proc/$PID/ns/ipc ipcs -a

# Show all IPC namespaces
lsns -t ipc

# In Kubernetes: check if a pod uses host IPC
kubectl get pod $POD -o jsonpath='{.spec.hostIPC}'
```

### Kubernetes note

Setting `hostIPC: true` in a `PodSpec` causes the pod's containers to join the host's IPC namespace rather than getting a new one. This is sometimes needed for legacy applications that use SysV shared memory to communicate with host services (e.g., certain database clients). It is a significant security boundary reduction.

---

## 4. PID Namespace — `CLONE_NEWPID` (`0x20000000`)

### Introduced

Linux 3.8 (February 2013).

### Cross-reference

PID namespaces are covered in depth in [Chapter 01, `01-c-pid-namespaces.md`](../../01-process-model/kernel/01-c-pid-namespaces.md), including the multi-level PID mapping mechanism and the `struct pid` design. This section provides a summary for completeness.

### What it isolates

The PID number space. Processes inside a PID namespace see only the PIDs assigned within that namespace; they cannot observe PIDs from the parent namespace. The first process created inside a new PID namespace receives PID 1 and becomes the reaper (`init`) for the namespace — when it exits, the kernel sends `SIGKILL` to all remaining processes in the namespace. This is the mechanism behind container `init` processes like `tini`.

### Key struct: `struct pid_namespace`

Source: [`include/linux/pid_namespace.h`](https://elixir.bootlin.com/linux/v6.9/source/include/linux/pid_namespace.h)

```c
struct pid_namespace {
    struct idr          idr;           /* PID allocator: int → struct pid * */
    struct rcu_head     rcu;
    unsigned int        pid_allocated; /* number of PIDs currently in use */
    struct task_struct *child_reaper;  /* the PID-1 process for this ns */
    struct kmem_cache  *pid_cachep;    /* slab cache for struct pid allocations */
    unsigned int        level;         /* nesting depth (0 = host) */
    struct pid_namespace *parent;      /* enclosing namespace */
    struct user_namespace *user_ns;
    struct ucounts      *ucounts;
    int                  reboot;       /* exit code if PID 1 calls reboot(2) */
    struct ns_common     ns;
} __randomize_layout;
```

Key fields:

- **`idr`** (`struct idr`): The integer-to-pointer radix tree that maps PID numbers (integers) to `struct pid *` objects within this namespace. `alloc_pid()` allocates a new PID by finding the next free slot in this IDR.
- **`child_reaper`** (`struct task_struct *`): Points to the PID-1 process. When any process in the namespace becomes orphaned (its parent exits), the kernel reparents it to `child_reaper`. If `child_reaper` itself exits, the entire namespace is torn down.
- **`level`** (`unsigned int`): Nesting depth. The host's init namespace is level 0. A container's PID namespace is level 1. A nested container is level 2. The kernel supports up to 32 levels; `struct pid` stores a PID number for each level it is visible at.
- **`parent`** (`struct pid_namespace *`): The enclosing namespace. A process in the parent namespace can see child-namespace PIDs via `/proc/$HOST_PID/status` (field `NSpid`).

### Important design: children-only semantics

`CLONE_NEWPID` is unusual: calling `unshare(CLONE_NEWPID)` does **not** move the calling process into the new PID namespace. The calling process keeps its existing PID in its existing namespace. The new namespace takes effect for processes subsequently created by the caller. This is why `nsproxy` has a separate field `pid_ns_for_children` — it holds the namespace that the next `fork()`/`clone()` will use, which may differ from the caller's own `task_struct->thread_pid->numbers[level].ns`.

### Live observation

```bash
# List all PID namespaces on the system
lsns -t pid

# From the host, see a process's PIDs in all namespaces
cat /proc/$PID/status | grep NSpid

# From inside a container
cat /proc/self/status | grep NSpid
# Output: NSpid: 1734  1    (host PID 1734, container PID 1)
```

---

## 5. Network Namespace — `CLONE_NEWNET` (`0x40000000`)

### Introduced

Linux 2.6.24 (January 2008), with incremental feature additions through 2.6.29.

### What it isolates

The complete network stack for a set of processes: network interfaces (including the loopback `lo`), IP addresses, routing tables, iptables/nftables rule sets, conntrack tables, raw sockets, `/proc/net`, and the port number space. Two processes in different network namespaces can independently bind to port 80 without conflict. Network namespaces are the reason each container has its own IP address.

### Key struct: `struct net`

Source: [`include/net/net_namespace.h`](https://elixir.bootlin.com/linux/v6.9/source/include/net/net_namespace.h)

`struct net` is by far the largest namespace struct in the kernel — it spans hundreds of fields and embeds the state for IPv4, IPv6, netfilter, nftables, network devices, routing, and more. Only the most architecturally significant fields are listed here:

```c
struct net {
    /* Reference counting: two levels */
    refcount_t          passive;      /* refs that don't prevent cleanup */
    refcount_t          count;        /* refs from live users */

    struct list_head    list;         /* links all net namespaces: net_namespace_list */
    struct list_head    exit_list;    /* used during teardown */
    struct llist_node   cleanup_list;

    struct user_namespace *user_ns;   /* owning user namespace */
    struct ucounts      *ucounts;

    struct ns_common     ns;          /* common header: inum, count, ops */

    /* Core network state */
    struct net_device   *loopback_dev;  /* the 'lo' interface */
    struct list_head     dev_base_head; /* all net_device objects in this ns */
    struct hlist_head   *dev_name_head; /* hash: device name → net_device */
    struct hlist_head   *dev_index_head;/* hash: ifindex → net_device */

    /* IPv4 state */
    struct netns_ipv4    ipv4;          /* routing, ARP, forwarding flags, ... */

    /* IPv6 state (if CONFIG_IPV6) */
    struct netns_ipv6    ipv6;

    /* Netfilter / nftables (if CONFIG_NETFILTER) */
    struct netns_nf      nf;
    struct netns_xt      xt;

    /* Socket and port management */
    struct sock         *rtnl;          /* rtnetlink socket for this ns */
    struct sock         *genl_sock;     /* generic netlink socket */

    /* Process-namespace plumbing */
    struct list_head     rules_ops;
    struct list_head     notifier_list;
} __randomize_layout;
```

Field-by-field breakdown (key fields):

- **`passive`** (`refcount_t`): A reference count for "weak" holders — things that hold a pointer to the `struct net` but do not prevent it from beginning its cleanup sequence. When `count` drops to zero, the cleanup starts, but the `net` struct is not freed until `passive` also reaches zero. This two-level refcount prevents use-after-free while allowing the network stack to begin unwinding before all references are dropped.

- **`count`** (`refcount_t`): The primary reference count. Non-zero means at least one live user (process, socket, device) holds a reference that prevents cleanup.

- **`list`** (`struct list_head`): Links this `struct net` into `net_namespace_list`, the global list of all network namespaces. This list is iterated by `for_each_net()`, used by subsystems (e.g., IPv4, netfilter) that need to perform per-namespace operations like garbage collection.

- **`loopback_dev`** (`struct net_device *`): The loopback interface (`lo`). Every network namespace gets exactly one loopback device, created during `setup_net()`. It is the first interface brought up and the last torn down.

- **`ipv4`** (`struct netns_ipv4`): All IPv4 routing state: the routing table (`fib_main`, `fib_default`), per-namespace sysctls (`ip_forward`, `conf[]`), ARP tables, and ICMP rate limiting. This single field represents hundreds of per-namespace IPv4 parameters.

- **`ns`** (`struct ns_common`): Common header; `ns.inum` is the inode number visible as `/proc/self/ns/net`.

### Kernel copy path

`copy_net_ns()` in [`net/core/net_namespace.c`](https://elixir.bootlin.com/linux/v6.9/source/net/core/net_namespace.c). Unlike most namespace copies, `copy_net_ns()` does not copy the parent's network state — it calls `setup_net()` to initialise a completely fresh network namespace with only a loopback interface and empty routing tables. All other interfaces must be explicitly moved (`ip link set dev eth0 netns $PID`) or created inside the new namespace.

### How Kubernetes uses network namespaces

Every Kubernetes pod starts a `pause` container (also called the "sandbox" or "infra" container) whose sole purpose is to hold the network namespace open for the lifetime of the pod. The `pause` process calls `clone(CLONE_NEWNET | ...)` to create the pod's network namespace, then sleeps indefinitely. The kubelet wires up the CNI plugin (Calico, Cilium, Flannel, etc.) to configure `eth0` and IP routing inside that namespace. All application containers in the pod then call `setns(fd, CLONE_NEWNET)` to join the same network namespace — this is why all containers in a pod share an IP address and port space, and can communicate via `localhost`.

### Live observation

```bash
# List all named network namespaces (ip netns adds a name bind-mount in /var/run/netns/)
ip netns list

# Show interfaces inside the network namespace of a specific process
nsenter --net=/proc/$PID/ns/net ip addr

# If the namespace has a name
ip -n <nsname> addr

# From host: find the container's veth peer
nsenter --net=/proc/$PID/ns/net ip link show eth0
# Note the 'link-netnsid' field — then on host:
ip link | grep -A1 "^$(( PEER_INDEX )):"
```

---

## 6. User Namespace — `CLONE_NEWUSER` (`0x10000000`)

### Introduced

Linux 3.8 (February 2013). Usable for full nesting from Linux 3.12.

### What it isolates

UID and GID mappings between the namespace and the host. Inside a user namespace, process credentials (UID, GID, supplementary groups) are interpreted relative to a mapping table. A process that is UID 0 (`root`) inside a user namespace maps to an unprivileged UID (e.g., 100000) on the host. Capabilities are re-evaluated per namespace: holding `CAP_SYS_ADMIN` inside a user namespace grants administrative power only within that namespace and its children — not on the host.

### Why user namespace lives in `cred`, not `nsproxy`

Every other namespace is referenced through `task_struct->nsproxy`. The user namespace is stored in `task_struct->cred->user_ns`. This is intentional: the user namespace governs capability checks and UID translation, which are tightly coupled to `struct cred`. Changing a task's user namespace is inseparable from changing its credentials — both must happen atomically, under the same RCU-protected credential replacement (`commit_creds()`). Splitting them across two different pointers would create a window where the capability check uses one user namespace while the credential holds another, which would be a security flaw.

### Key struct: `struct user_namespace`

Source: [`include/linux/user_namespace.h`](https://elixir.bootlin.com/linux/v6.9/source/include/linux/user_namespace.h)

```c
struct user_namespace {
    struct uid_gid_map   uid_map;    /* UID mapping: inside ↔ outside */
    struct uid_gid_map   gid_map;    /* GID mapping: inside ↔ outside */
    struct uid_gid_map   projid_map; /* project ID mapping */

    struct user_namespace *parent;   /* enclosing user namespace */
    int                   level;     /* nesting depth (0 = init_user_ns) */

    kuid_t                owner;     /* host UID of the creator */
    kgid_t                group;     /* host GID of the creator */

    struct ns_common      ns;        /* common header */

    unsigned long         flags;     /* USER_NS_SETGROUPS_ALLOWED, etc. */

    struct key           *persistent_keyring_register;
    struct rw_semaphore   persistent_keyring_register_sem;

    struct work_struct    work;      /* deferred cleanup */
    struct ctl_table_set  set;
    struct ctl_table_header *sysctls;

    struct ucounts       *ucounts;
    long                  ucount_max[UCOUNT_COUNTS];
    long                  rlimit_max[UCOUNT_RLIMIT_COUNTS];
} __randomize_layout;
```

Field-by-field breakdown:

- **`uid_map`** (`struct uid_gid_map`): The UID mapping table. Each entry stores a triple `(first_inside, first_outside, count)` meaning "UIDs `first_inside` through `first_inside + count - 1` inside this namespace map to host UIDs `first_outside` through `first_outside + count - 1`." Written by userspace via `/proc/$PID/uid_map`. Read by `from_kuid()` and `make_kuid()` whenever the kernel must translate a UID across a namespace boundary.

- **`gid_map`** (`struct uid_gid_map`): Same structure for GID mappings. Written via `/proc/$PID/gid_map`.

- **`parent`** (`struct user_namespace *`): The parent user namespace. User namespaces form a tree rooted at `init_user_ns`. The parent must be kept alive as long as the child exists.

- **`level`** (`int`): Nesting depth. `init_user_ns` is level 0. The kernel enforces a maximum depth of 32 to prevent unbounded nesting attacks.

- **`owner`** (`kuid_t`): The host UID of the process that created this user namespace. This UID is implicitly mapped to UID 0 inside the namespace at creation time, making the creator `root` within their own namespace.

- **`group`** (`kgid_t`): The host GID of the creator, similarly mapped to GID 0 inside the namespace.

- **`ns`** (`struct ns_common`): Common header; `ns.inum` identifies `/proc/self/ns/user`.

### Unprivileged creation

The user namespace is the **only** namespace that can be created without holding `CAP_SYS_ADMIN`. An ordinary user can call `unshare(CLONE_NEWUSER)` or `clone(CLONE_NEWUSER)` without any privileges. This is the entry point for rootless containers: `podman` and `rootless Docker` create a user namespace first, then create all other namespaces (which require `CAP_SYS_ADMIN`) within the context of the new user namespace where the unprivileged user is now `root`.

### UID mapping mechanics

```bash
# After creating a user namespace for child process $PID,
# map container UID 0 → host UID 1000 (one-to-one):
echo "0 1000 1" > /proc/$PID/uid_map

# Map a full range: container UIDs 0-65535 → host UIDs 100000-165535:
echo "0 100000 65536" > /proc/$PID/uid_map
```

Writing to `uid_map` is a one-shot operation — the file can only be written once. After the mapping is set, files created inside the namespace with UID 0 appear on the host as UID 1000, keeping the host filesystem unchanged.

### Live observation

```bash
# Show UID mapping for a process (three columns: inside_start, outside_start, count)
cat /proc/$PID/uid_map

# Show GID mapping
cat /proc/$PID/gid_map

# List all user namespaces
lsns -t user

# Verify which user namespace a process belongs to
readlink /proc/$PID/ns/user
```

---

## 7. Cgroup Namespace — `CLONE_NEWCGROUP` (`0x02000000`)

### Introduced

Linux 4.6 (May 2016).

### What it isolates

The **view** of the cgroup hierarchy. A cgroup namespace does not create new cgroups or change resource limits — it changes what a process sees when it reads `/proc/self/cgroup` or navigates `/sys/fs/cgroup`. Inside a cgroup namespace, the process's current cgroup appears as `/` (the root), hiding the full host path that would reveal its position in the host cgroup tree. This prevents a container from discovering its placement in the host cgroup hierarchy (e.g., `/kubepods/burstable/pod<uid>/container<id>`).

### Key struct: `struct cgroup_namespace`

Source: [`include/linux/cgroup.h`](https://elixir.bootlin.com/linux/v6.9/source/include/linux/cgroup.h)

```c
struct cgroup_namespace {
    struct ns_common    ns;       /* common header */
    struct user_namespace *user_ns;
    struct ucounts      *ucounts;
    struct css_set      *root_cset; /* the cgroup "root" from this ns's perspective */
};
```

Field-by-field breakdown:

- **`ns`** (`struct ns_common`): Common header. `ns.inum` is the inode for `/proc/self/ns/cgroup`.

- **`user_ns`** (`struct user_namespace *`): The owning user namespace. Creating a new cgroup namespace requires `CAP_SYS_ADMIN` within the owning user namespace.

- **`root_cset`** (`struct css_set *`): This is the heart of what the cgroup namespace does. A `css_set` (cgroup subsystem set) represents the combination of cgroups a process belongs to across all cgroup subsystems. When the cgroup namespace was created, the calling process's current `css_set` was captured as `root_cset`. From this point on, any process reading `/proc/self/cgroup` inside this namespace sees all paths relative to `root_cset` — that combination appears as `/`. Any ancestor cgroups above `root_cset` in the hierarchy are not visible.

### Effect on `/proc/self/cgroup`

```
# From the host, looking at a container process:
cat /proc/$CONTAINER_PID/cgroup
# Output (cgroup v2 example):
# 0::/kubepods/burstable/pod9a2f1e3b/7c8d5e2f1a4b

# From inside the container:
cat /proc/self/cgroup
# Output:
# 0::/
```

The container process is at the same cgroup in both cases — but the cgroup namespace makes the container see that cgroup as its root, hiding the full host path.

### Kernel copy path

`copy_cgroup_ns()` in [`kernel/cgroup/namespace.c`](https://elixir.bootlin.com/linux/v6.9/source/kernel/cgroup/namespace.c). Allocates a new `cgroup_namespace` and captures the calling process's current `css_set` as `root_cset`, incrementing its reference count.

### Kubernetes relevance

Every container in Kubernetes gets a cgroup namespace (kubelet passes `CLONE_NEWCGROUP` to runc). This ensures containers cannot enumerate host cgroup paths, which would reveal pod names and resource limits of other workloads on the same node.

### Live observation

```bash
# From the host: see the real cgroup path of a container process
cat /proc/$CONTAINER_PID/cgroup

# From inside the container (via kubectl exec):
cat /proc/self/cgroup
# Should show:  0::/

# List all cgroup namespaces
lsns -t cgroup
```

---

## 8. Time Namespace — `CLONE_NEWTIME` (`0x00000080`)

### Introduced

Linux 5.6 (March 2020) — the newest namespace type. Proposed and implemented by Andrei Vagin at Google.

### What it isolates

Offsets applied to two specific clocks: `CLOCK_MONOTONIC` and `CLOCK_BOOTTIME`. Processes inside a time namespace see a shifted version of these clocks, allowing a container to report a different "virtual uptime" from the host. `CLOCK_REALTIME` is deliberately **not** isolated — wall-clock time is shared with the host, because offsetting real time would break NTP synchronisation, SSL certificate validation, and any code that compares real-time timestamps across processes.

Use cases: checkpoint-restore (CRIU) needs to restore a process with the monotonic clock at the value it had when checkpointed; live migration of containers between hosts where uptime diverges.

### Key struct: `struct time_namespace`

Source: [`include/linux/time_namespace.h`](https://elixir.bootlin.com/linux/v6.9/source/include/linux/time_namespace.h)

```c
struct time_namespace {
    struct user_namespace *user_ns;
    struct ucounts        *ucounts;
    struct ns_common       ns;           /* common header */
    struct timens_offsets  offsets;      /* the actual clock offsets */
    struct page           *vvar_page;    /* vDSO page carrying the offsets */
    bool                   frozen_offsets; /* offsets are immutable once set */
};
```

Field-by-field breakdown:

- **`ns`** (`struct ns_common`): Common header; `ns.inum` identifies `/proc/self/ns/time`.

- **`user_ns`** (`struct user_namespace *`): Owning user namespace. Creating a time namespace requires `CAP_SYS_ADMIN` within the owning user namespace.

- **`offsets`** (`struct timens_offsets`): The payload — the actual time offsets:
  ```c
  struct timens_offsets {
      struct timespec64 monotonic; /* offset added to CLOCK_MONOTONIC */
      struct timespec64 boottime;  /* offset added to CLOCK_BOOTTIME */
  };
  ```
  Both are `struct timespec64` (seconds + nanoseconds). A positive `monotonic` offset makes the clock appear to have started earlier; a negative offset makes it appear to have started later. Written via `/proc/$PID/timens_offsets` before any process joins the namespace.

- **`vvar_page`** (`struct page *`): A dedicated page containing a copy of the time namespace offsets, mapped into the vDSO of every process in this namespace. This is the performance-critical field — see vDSO integration below.

- **`frozen_offsets`** (`bool`): Set to `true` the first time any process joins the time namespace (i.e., the first `clone()` or `setns()` that uses this namespace). After that point, the offsets cannot be changed. This ensures that all processes in the namespace see a consistent clock.

### The two-field design in `nsproxy`

The time namespace is the only namespace with **two** slots in `struct nsproxy`:

```c
struct nsproxy {
    /* ... other namespaces ... */
    struct time_namespace *time_ns;              /* this process's time view */
    struct time_namespace *time_ns_for_children; /* new children will use this */
};
```

When a process calls `unshare(CLONE_NEWTIME)`, the kernel creates a new `time_namespace` and sets `time_ns_for_children` to point at it. The calling process itself continues using its old `time_ns` — its own clock view does not change. Only processes born after the `unshare()` (via `fork()`/`clone()`) will inherit the new `time_ns_for_children` as their `time_ns`. This design avoids changing the caller's own time reference mid-execution, which could corrupt in-flight timeout calculations.

This is analogous to `pid_ns_for_children` in the PID namespace, but the time namespace makes it explicit with two named fields rather than a single field that changes meaning based on context.

### vDSO integration

`clock_gettime(CLOCK_MONOTONIC)` is one of the most frequently called syscalls on Linux. To avoid the overhead of a full syscall crossing into kernel mode for each call, the kernel uses a vDSO (virtual dynamic shared object): a small piece of kernel code mapped read-only into every process's address space. The vDSO implementation of `clock_gettime()` reads the current time from a shared page (the `vvar` page), applies any offsets, and returns — entirely in userspace, with no kernel crossing.

The time namespace's `vvar_page` is a per-namespace copy of this shared page, with the namespace's `offsets` baked in. When a process in a time namespace calls `clock_gettime(CLOCK_MONOTONIC)`, the vDSO automatically uses its own `vvar_page` (mapped at the right virtual address by the kernel's execve/mmap path) and returns the offset-adjusted value without ever entering kernel mode. The result is that time namespace isolation costs essentially nothing at runtime.

### Live observation

```bash
# List time namespaces on the system
# Most processes share init_time_ns; few containers use time namespaces in practice
lsns -t time

# Show the clock offsets for a process's time namespace
cat /proc/$PID/timens_offsets
# Output format: clockid  secs  nsecs
# Example for a checkpointed process restored with an offset:
# monotonic  1234567  0
# boottime   1234567  0

# Create a time namespace with a 1-hour monotonic offset
unshare --time --monotonic 3600 -- bash -c 'cat /proc/self/timens_offsets'
```

---

## Namespace Combinations

Linux namespaces compose freely — any combination of the eight flags can be passed to `clone(2)` or `unshare(2)`. Different container runtimes choose different combinations depending on the isolation level required.

### Typical Docker/runc container

A standard `docker run` uses six namespaces:

```
CLONE_NEWNS     = 0x00020000   (mount)
CLONE_NEWUTS    = 0x04000000   (uts)
CLONE_NEWIPC    = 0x08000000   (ipc)
CLONE_NEWPID    = 0x20000000   (pid)
CLONE_NEWNET    = 0x40000000   (network)
CLONE_NEWCGROUP = 0x02000000   (cgroup)
─────────────────────────────────────────
OR'd together   = 0x6e020000
```

User namespaces are **not** used by default in Docker (unless `--userns-remap` is configured), because they add complexity around file ownership. Time namespaces are not used by most container runtimes today.

### Kubernetes pod default

Kubernetes pods use a split model:

- The `pause` container calls `clone()` with `CLONE_NEWNS | CLONE_NEWUTS | CLONE_NEWPID | CLONE_NEWNET | CLONE_NEWCGROUP` to create the pod's namespaces.
- App containers use `setns()` to join the `pause` container's **network** and **IPC** namespaces (so all containers in the pod share one IP and one SysV IPC space), while getting their own **mount** namespace (so each container has its own filesystem view).
- The result: all containers in a pod have the same IP address but different filesystems.

### Unprivileged container with user namespace

User namespace is the gateway to rootless containers. This command creates an isolated shell with user, mount, and PID namespaces, without any root privileges:

```bash
unshare --user --mount --pid --fork --map-root-user bash
```

What happens step by step:
1. `--user` creates a new user namespace; the current unprivileged UID is mapped to UID 0 inside.
2. Because the calling process is now `root` inside the user namespace, it has `CAP_SYS_ADMIN` scoped to that namespace.
3. `--mount` creates a new mount namespace (requires `CAP_SYS_ADMIN`, now available from step 2).
4. `--pid --fork` creates a new PID namespace and forks a child to become PID 1.
5. `--map-root-user` writes the UID/GID maps (`0 <current-uid> 1`).
6. `bash` starts as root inside a fully isolated environment, with no host privileges.

This is the foundation of tools like `podman`, `buildah`, and rootless `nerdctl`.

### Namespace isolation flags quick reference

```c
/* All namespace flags together */
#define CLONE_ALL_NS  (CLONE_NEWNS     |   /* 0x00020000 */  \
                       CLONE_NEWCGROUP |   /* 0x02000000 */  \
                       CLONE_NEWUTS    |   /* 0x04000000 */  \
                       CLONE_NEWIPC    |   /* 0x08000000 */  \
                       CLONE_NEWUSER   |   /* 0x10000000 */  \
                       CLONE_NEWPID    |   /* 0x20000000 */  \
                       CLONE_NEWNET    |   /* 0x40000000 */  \
                       CLONE_NEWTIME)      /* 0x00000080 */
```
