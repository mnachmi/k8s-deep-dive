# 05-b: Mount Subsystem

The mount subsystem tracks every attached filesystem in the kernel. Each `mount`
point is a `struct mount` — an internal node in a tree that maps device trees onto
directory trees. This document walks from the raw structs through the mount(2)
call path to the per-process mountinfo view that containers depend on.

---

## 1 — Source Locations

| File | Purpose | Source |
|------|---------|--------|
| `fs/mount.h` | Internal `struct mount`, `struct mnt_namespace`, `struct mountpoint` — kernel-private, not exported to filesystems | https://elixir.bootlin.com/linux/v6.9/source/fs/mount.h |
| `fs/namespace.c` | `sys_mount`, `do_mount`, `path_mount`, `do_new_mount`, `graft_tree`, namespace clone/unshare | https://elixir.bootlin.com/linux/v6.9/source/fs/namespace.c |
| `include/linux/mount.h` | Public `struct vfsmount` — the only mount type filesystem code is allowed to see | https://elixir.bootlin.com/linux/v6.9/source/include/linux/mount.h |

---

## 2 — struct vfsmount and struct mount

### 2.1 struct vfsmount — the public half

`struct vfsmount` is declared in `include/linux/mount.h` and is the only mount
type visible to filesystem drivers. It holds the minimum information a filesystem
needs to function:

```c
// include/linux/mount.h
struct vfsmount {
    struct dentry *mnt_root;           // root dentry of the mounted filesystem
    struct super_block *mnt_sb;        // superblock of the mounted filesystem
    int mnt_flags;                     // MNT_NOSUID, MNT_NODEV, MNT_NOEXEC, MNT_READONLY...
    struct mnt_idmap *mnt_idmap;       // idmapping for uid/gid translation (since Linux 6.3)
};
```

`mnt_flags` is a bitmask tested during path lookup — `MNT_NOSUID` suppresses
setuid bits, `MNT_NOEXEC` blocks `execve()`. Separate from superblock `s_flags`.

### 2.2 struct mount — the internal full record

`struct mount` lives in `fs/mount.h` (not exported). It embeds `struct vfsmount`
and carries all the bookkeeping the namespace, propagation, and lifecycle code
needs:

```c
// fs/mount.h (key fields)
struct mount {
    struct hlist_node  mnt_hash;         // hash table node for fast lookup
    struct mount      *mnt_parent;       // parent mount
    struct dentry     *mnt_mountpoint;   // dentry in parent where this is attached
    struct vfsmount    mnt;              // embedded public struct (MUST use real_mount() to go back)
    union {
        struct rcu_head   mnt_rcu;       // for RCU-delayed freeing
        struct llist_node mnt_llist;     // for lock-free batch freeing
    };
    struct list_head   mnt_mounts;       // list head of child mounts
    struct list_head   mnt_child;        // link in parent's mnt_mounts list
    struct list_head   mnt_instance;     // sb->s_mounts — all mounts of same superblock
    const char        *mnt_devname;      // device name (e.g., "/dev/sda1", "overlay")
    struct mnt_namespace *mnt_ns;        // containing mount namespace
    struct mountpoint *mnt_mp;           // where this is mounted
    struct hlist_node  mnt_mp_list;      // link in mnt_mp->m_list (all mounts at same point)
    struct list_head   mnt_umounting;    // link during lazy umount processing
    struct list_head   mnt_list;         // namespace's list of all mounts
    int                mnt_id;           // unique mount ID (shown in /proc/pid/mountinfo)
    int                mnt_group_id;     // peer group for shared mounts
};
```

Source: https://elixir.bootlin.com/linux/v6.9/source/fs/mount.h

### 2.3 The split design and real_mount()

Filesystem code receives only `struct vfsmount *`. This enforces a boundary:
filesystems see exactly what they need, and the kernel can evolve the internal
`struct mount` freely without breaking filesystem drivers.

When the namespace or propagation code needs the full `struct mount` from a
`struct vfsmount *`, it uses the `real_mount()` helper:

```c
static inline struct mount *real_mount(struct vfsmount *mnt)
{
    return container_of(mnt, struct mount, mnt);
}
```

`container_of` subtracts the offset of `mnt` within `struct mount` from the
pointer, yielding the enclosing `struct mount *` — a standard kernel embedding
pattern for type-safe upcasting without vtables.

---

## 3 — struct mnt_namespace

Each process belongs to exactly one mount namespace. The namespace object owns
the complete list of mounts visible to processes inside it:

```c
// fs/mount.h (key fields)
struct mnt_namespace {
    struct ns_common     ns;             // common header — inum is inode for /proc/pid/ns/mnt
    struct mount        *root;           // root mount of this namespace
    struct list_head     list;           // all mounts: linked via mnt->mnt_list
    spinlock_t           ns_lock;
    struct user_namespace *user_ns;
    u64                  seq;            // sequence number for mountstats
    wait_queue_head_t    poll;           // for poll() on /proc/pid/mounts
    u64                  event;          // incremented on every mount/umount
    unsigned int         mounts;         // total mount count in namespace
    unsigned int         pending_mounts;
};
```

Source: https://elixir.bootlin.com/linux/v6.9/source/fs/mount.h

**Namespace inheritance.** A new process inherits its parent's mount namespace.
Pass `CLONE_NEWNS` to `clone()` (or call `unshare(CLONE_NEWNS)`) to get a private
copy; the kernel calls `copy_mnt_ns()` which deep-copies the mount tree.

**Container isolation.** Each container runtime calls `unshare(CLONE_NEWNS)` before
pivoting the root. Mounts inside the container are invisible to the host and vice
versa — they live in separate `struct mnt_namespace` objects.

---

## 4 — mount(2) Kernel Call Path

When userspace calls `mount(2)`, control descends through `fs/namespace.c` and
diverges at `path_mount()` depending on mount flags:

```
mount("overlay", "/merged", "overlay", 0, "lowerdir=lower,upperdir=upper,workdir=work")
  └─ sys_mount()                                        # entry point
       └─ do_mount()                                    # decode flags
            └─ path_mount()                             # fs/namespace.c
                 ├─ [new filesystem] do_new_mount()
                 │    ├─ fs_context_for_mount()         # allocate & init struct fs_context
                 │    ├─ vfs_parse_fs_string()          # parse "lowerdir=..." options
                 │    ├─ vfs_get_tree()                 # call overlayfs fill_super / get_tree
                 │    └─ do_new_mount_fc()
                 │         └─ graft_tree()              # attach new struct mount to parent
                 │
                 ├─ [bind mount: MS_BIND] do_loopback() # clone existing mount subtree
                 │
                 ├─ [remount: MS_REMOUNT] do_remount()  # update flags on existing mount
                 │
                 └─ [propagation change] do_change_type() # shared/slave/private/unbindable
```

Source: https://elixir.bootlin.com/linux/v6.9/source/fs/namespace.c

Key steps in `do_new_mount()`:

1. **`fs_context_for_mount()`** — allocates a `struct fs_context` and calls the
   filesystem's `init_fs_context` op. Holds all per-mount options until commit.
2. **`vfs_get_tree()`** — calls `fc->ops->get_tree(fc)` (the `fill_super`
   equivalent). For overlayfs this sets up the layer stack.
3. **`graft_tree()`** — links the new `struct mount` into the parent's
   `mnt_mounts` list and the namespace's `mnt_list`. Mount goes live here.

---

## 5 — /proc/pid/mountinfo Format

Every line in `/proc/pid/mountinfo` represents one `struct mount`. The format is
defined by `show_mountinfo()` in `fs/proc_namespace.c`:

```
36 35 98:0 /mnt1 /mnt2 rw,noatime master:1 - ext3 /dev/root rw,errors=continue
│  │  │    │     │     │           │         │ │    │          └─ superblock options (s_options)
│  │  │    │     │     │           │         │ │    └─ device name (mnt_devname)
│  │  │    │     │     │           │         │ └─ filesystem type (s_type->name)
│  │  │    │     │     │           │         └─ separator (always "-")
│  │  │    │     │     │           └─ optional fields: peer group / master propagation info
│  │  │    │     │     └─ per-mount options (derived from mnt_flags: nosuid, nodev, noexec…)
│  │  │    │     └─ mount point path in this namespace (path to mnt_mountpoint)
│  │  │    └─ root of mount relative to filesystem root (path from mnt_root)
│  │  └─ major:minor of the block device or pseudo-device (dev_t)
│  └─ parent mount ID (mnt_parent->mnt_id)
└─ mount ID (mnt->mnt_id)
```

Field notes:

- **Mount ID** — `mnt_id`, unique for the mount's kernel lifetime. Referenced in
  `/proc/pid/fdinfo` for open file descriptors.
- **Root of mount** — for a bind mount of `/home/alice`, this shows `/alice`
  (path within the source filesystem), not `/`.
- **Optional fields** — zero or more `key:value` pairs before the `-` separator.
  `master:N` means this is a slave mount whose master is peer group N.
- **Container view** — `/proc/<container-pid>/mountinfo` shows only mounts in
  that container's `mnt_namespace`. Host-only mounts are invisible to the
  container and vice versa — the foundation of container filesystem isolation.

---

## 6 — Mount Propagation: Shared / Slave / Private / Unbindable

Mount propagation determines whether a mount event (attach or detach) inside one
subtree is replicated into peer subtrees in other namespaces. The type is stored
in `mnt->mnt_flags` as `MNT_SHARED`, `MNT_SLAVE`, `MNT_UNBINDABLE`, or none
(private).

### Shared (`mount --make-shared`)

```bash
mount --make-shared /mnt
```

All mounts in the same peer group see each other's mount and unmount events.
If you mount a USB drive under `/mnt` in one namespace, all namespaces sharing
that peer group will also see it. The peer group ID is shown in mountinfo as
`shared:N`.

**Use case:** host directories that should remain synchronised across all
containers or bind namespaces — e.g. a shared NFS mount.

### Slave (`mount --make-slave`)

```bash
mount --make-slave /mnt
```

The slave mount receives propagation from its master peer group but does not
propagate events back. Mounts performed on the master appear in the slave;
mounts performed inside the slave are invisible to the master.

**Use case:** container bind mounts of `/proc`, `/sys`, `/dev`. The container
sees changes the host makes to these trees, but mounting something inside the
container doesn't affect the host.

### Private (`mount --make-private`)

```bash
mount --make-private /mnt
```

No propagation in either direction. Completely isolated. The default for new
namespaces created by `unshare(CLONE_NEWNS)` unless the parent was shared.

**Use case:** fully isolated namespaces — security sandboxes, test environments.

### Unbindable (`mount --make-unbindable`)

```bash
mount --make-unbindable /mnt
```

Private propagation AND the mount cannot be used as a bind-mount source. Any
attempt to `mount --bind` from an unbindable mount returns `EINVAL`.

**Use case:** sensitive filesystems that must never be cloned via bind mount.

### What runc does

When starting a container, runc calls:

```c
mount(NULL, "/", NULL, MS_SLAVE | MS_REC, NULL);
```

This makes the entire filesystem tree a recursive slave of the host. The effect:

- The container inherits all host mounts that exist at container start time.
- New mounts created inside the container do not propagate to the host.
- The host can still change its own mount tree independently.

This single `mount(2)` call is the foundation of container filesystem isolation
before `pivot_root` or `chroot` is applied.

---

## 7 — Live Observation

```bash
# All mounts in the current process's namespace:
cat /proc/self/mountinfo

# Mounts visible to a specific container process:
cat /proc/$CONTAINER_PID/mountinfo

# Count mounts in current namespace:
wc -l < /proc/self/mountinfo

# bpftrace: trace every mount(2) syscall with source, target, and type
bpftrace -e 'tracepoint:syscalls:sys_enter_mount {
    printf("mount: pid=%-6d comm=%-20s source=%s target=%s type=%s\n",
           pid, comm,
           str(args->dev_name), str(args->dir_name), str(args->type));
}'

# bpftrace: trace every umount(2) syscall
bpftrace -e 'tracepoint:syscalls:sys_enter_umount {
    printf("umount: pid=%d comm=%s name=%s\n", pid, comm, str(args->name));
}'

# bpftrace: trace every new mount operation at the kernel level
# (fires on bind mounts, new filesystems — anything that calls do_new_mount)
bpftrace -e 'kprobe:do_new_mount {
    printf("new mount: pid=%d comm=%s\n", pid, comm);
}'
```

The `tracepoint:syscalls:sys_enter_mount` probe fires for every `mount(2)` call
regardless of outcome. `kprobe:do_new_mount` fires only for new filesystem mounts;
bind mounts go through `do_loopback` and need a separate probe to capture.

---

## 8 — Key References

| Symbol | File | Source |
|--------|------|--------|
| `struct vfsmount` | `include/linux/mount.h` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/mount.h |
| `struct mount` | `fs/mount.h` | https://elixir.bootlin.com/linux/v6.9/source/fs/mount.h |
| `struct mnt_namespace` | `fs/mount.h` | https://elixir.bootlin.com/linux/v6.9/source/fs/mount.h |
| `do_new_mount()` | `fs/namespace.c` | https://elixir.bootlin.com/linux/v6.9/source/fs/namespace.c |
| `path_mount()` | `fs/namespace.c` | https://elixir.bootlin.com/linux/v6.9/source/fs/namespace.c |
| `graft_tree()` | `fs/namespace.c` | https://elixir.bootlin.com/linux/v6.9/source/fs/namespace.c |

---

**Next:** [05-c-overlayfs.md](05-c-overlayfs.md) — overlayfs layer stacking and container copy-on-write rootfs.
