# Chapter 05 — VFS & Storage Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Write Chapter 05 (Linux VFS & Storage) in full: kernel deep dives on the VFS layer (superblock, inode, dentry, file), the mount subsystem (struct mount, mount namespaces, mount_tree()), OverlayFS (upper/lower/work layers, copy-up), and block I/O (struct bio, request queue, writeback); K8s connection covering container image layers, CSI volumes, ephemeral volumes, and configMap/secret mounts; C exercise demonstrating inotify and direct I/O; Go exercise reading mount namespace info; kube-inspect checkpoint 05 adding mount namespace inspection and overlay layer count per pod.

**Architecture:** Three-track structure — `kernel/` (four docs), `k8s/` (K8s connection), `exercises/` (C + Go). kube-inspect extends `internal/proc` with mount namespace and overlay layer reading.

**Tech Stack:** Markdown, C (gcc -Wall -Wextra -Werror), Go 1.22+, Linux 6.9 kernel source, bpftrace, /proc/mounts, /proc/pid/mountinfo, overlayfs

## Global Constraints

- Every kernel struct/function reference MUST include elixir.bootlin.com URL for Linux 6.9
- bpftrace: use `kernel_clone` (not `do_fork`); `copy_process` arg3 = `struct kernel_clone_args *`
- C: `gcc -Wall -Wextra -Werror`, binaries excluded via `.gitignore`
- Go: `go vet ./...` clean, binaries excluded
- Exercise folders: `README.md`, `Makefile` (build/run/clean), `.gitignore`
- References tables: use bare URLs (raw https:// — NOT [link](url) markdown shorthand)
- Data structure deep dive mandatory: full struct, every field, lifecycle, locking, object graph, live observation
- No placeholder text anywhere
- Kernel version: Linux 6.9

---

## Task 1: Chapter 05 README + VFS layer deep dive

**Files:**
- Modify: `05-vfs-storage/README.md` (replace stub)
- Create: `05-vfs-storage/kernel/05-a-vfs.md`

- [ ] **Step 1: Write `05-vfs-storage/README.md`**

Include:
- Intro paragraph: Every read(), write(), open(), and stat() call goes through the Linux Virtual Filesystem Switch (VFS). VFS is an abstraction layer that presents a uniform interface regardless of the underlying filesystem (ext4, xfs, tmpfs, overlayfs, procfs). Containers use VFS heavily: every image layer is an OverlayFS mount, every configMap and secret is a tmpfs mount, every container sees a complete filesystem tree assembled by the kernel's mount namespace mechanism. This chapter teaches VFS from the kernel data structures up to the Kubernetes storage API.
- Learning objectives (5):
  1. Navigate /proc/pid/mountinfo and interpret every field for a container process
  2. Explain the VFS object model: superblock → inode → dentry → file, and how they relate
  3. Trace an open(2) call through vfs_open() to a filesystem's open handler
  4. Explain how OverlayFS implements copy-on-write for container layers
  5. Map Kubernetes volume types (emptyDir, configMap, secret, PVC) to kernel mount operations
- Prerequisites: Chapter 02 (mount namespaces, struct mnt_namespace), Chapter 03 (cgroup filesystems, kernfs)
- Reading order table: 05-a-vfs.md, 05-b-mount.md, 05-c-overlayfs.md, 05-d-block-io.md, k8s/05-k8s-connection.md, exercises/inotify-demo/, exercises/mount-inspector/
- ASCII diagram of the VFS object hierarchy:
```
open("/etc/hosts", O_RDONLY)
  │
  ▼
sys_open() → do_sys_openat2() → do_filp_open() → path_openat()
  │
  ├─ namei: walk dentry tree → struct dentry (cached name lookup)
  │          └─ struct inode (file metadata, i_op vtable)
  │
  ├─ vfs_open() → inode->i_op->open() [filesystem-specific]
  │
  └─ struct file (per-open-file-description)
       ├─ f_path.dentry → struct dentry
       ├─ f_inode → struct inode
       ├─ f_op → struct file_operations vtable
       └─ f_pos (current file offset)

struct super_block (one per mounted filesystem)
  └─ s_root → struct dentry (root of this filesystem's dentry tree)
       └─ d_inode → struct inode (root directory inode)
            └─ i_sb → back-pointer to super_block
```

- [ ] **Step 2: Write `05-vfs-storage/kernel/05-a-vfs.md`**

Full VFS deep dive. Must include ALL of these sections:

**Section 1 — Source locations**
- `include/linux/fs.h` — https://elixir.bootlin.com/linux/v6.9/source/include/linux/fs.h (superblock, inode, file, dentry)
- `fs/namei.c` — https://elixir.bootlin.com/linux/v6.9/source/fs/namei.c (path lookup, open)
- `fs/dcache.c` — https://elixir.bootlin.com/linux/v6.9/source/fs/dcache.c (dentry cache)
- `fs/inode.c` — https://elixir.bootlin.com/linux/v6.9/source/fs/inode.c (inode lifecycle)

**Section 2 — struct super_block**
```c
// include/linux/fs.h (key fields)
struct super_block {
    struct list_head    s_list;         // all superblocks linked here
    dev_t               s_dev;          // device identifier
    unsigned char       s_blocksize_bits;
    unsigned long       s_blocksize;    // block size in bytes
    loff_t              s_maxbytes;     // max file size
    struct file_system_type *s_type;   // e.g., &ext4_fs_type
    const struct super_operations *s_op; // statfs, alloc_inode, etc.
    const struct dquot_operations *dq_op;
    const struct quotactl_ops *s_qcop;
    const struct export_operations *s_export_op;
    unsigned long       s_flags;        // MS_RDONLY, MS_NOSUID, MS_NODEV...
    unsigned long       s_iflags;       // internal flags
    unsigned long       s_magic;        // filesystem magic number (0xEF53 for ext4)
    struct dentry      *s_root;         // root dentry of this filesystem
    struct rw_semaphore s_umount;       // protects unmount
    int                 s_count;        // reference count
    atomic_t            s_active;       // active users
    struct list_head    s_inodes;       // all inodes of this superblock
    spinlock_t          s_inode_list_lock;
    struct list_head    s_mounts;       // all vfsmounts using this superblock
    struct block_device *s_bdev;        // underlying block device (NULL for pseudo-fs)
    char                s_id[32];       // informational name (e.g., "sda1")
    uuid_t              s_uuid;         // filesystem UUID
};
```
Source: https://elixir.bootlin.com/linux/v6.9/source/include/linux/fs.h

Explain each field. Note: s_magic values: 0xEF53 (ext4), 0x58465342 (xfs), 0x01021994 (tmpfs/shmem), 0x794C7630 (overlayfs). s_flags: MS_RDONLY, MS_NOSUID (no setuid exec), MS_NODEV (no device files), MS_NOEXEC (no exec), MS_SYNCHRONOUS (sync writes).

**Section 3 — struct inode**
```c
// include/linux/fs.h (key fields)
struct inode {
    umode_t             i_mode;         // file type + permissions (S_IFREG, S_IFDIR, S_IFLNK...)
    unsigned short      i_opflags;
    kuid_t              i_uid;          // owner UID
    kgid_t              i_gid;          // owner GID
    unsigned int        i_flags;        // S_IMMUTABLE, S_APPEND, S_NOATIME...
    const struct inode_operations *i_op; // lookup, create, mkdir, rename, readlink...
    struct super_block *i_sb;           // back-pointer to superblock
    struct address_space *i_mapping;    // page cache for file data
    unsigned long       i_ino;          // inode number (unique within filesystem)
    union {
        const unsigned int i_nlink;     // hard link count
        unsigned int __i_nlink;
    };
    dev_t               i_rdev;         // device number (for device files)
    loff_t              i_size;         // file size in bytes
    struct timespec64   i_atime;        // last access time
    struct timespec64   i_mtime;        // last modification time
    struct timespec64   i_ctime;        // last change time (metadata)
    spinlock_t          i_lock;
    unsigned short      i_bytes;        // bytes in last block (with i_blocks)
    u8                  i_blkbits;
    u8                  i_write_hint;
    blkcnt_t            i_blocks;       // number of 512-byte blocks allocated
    atomic_t            i_count;        // reference count (struct inode users)
    atomic_t            i_writecount;   // writers (negative = mmap'd)
    const struct file_operations *i_fop; // default file operations
    struct file_lock_context *i_flctx;  // file locks
    struct address_space i_data;        // page cache (for regular files)
    struct list_head    i_lru;          // LRU list for inode reclaim
    struct hlist_node   i_hash;         // hash table for inode lookup
    struct list_head    i_sb_list;      // all inodes on this superblock
    union {
        struct hlist_head i_dentry;     // dentries pointing to this inode
        struct rcu_head   i_rcu;
    };
    atomic64_t          i_version;      // inode version (for NFS change detection)
    atomic_t            i_dio_count;    // direct I/O in progress
    void               *i_private;      // filesystem-specific private data
};
```
Source: https://elixir.bootlin.com/linux/v6.9/source/include/linux/fs.h

Explain: inode number uniqueness (per-filesystem), hard link count (file deleted when i_nlink reaches 0 AND i_count reaches 0), i_mode type bits (S_IFREG=regular, S_IFDIR=directory, S_IFLNK=symlink, S_IFCHR=char device, S_IFBLK=block device, S_IFIFO=pipe, S_IFSOCK=socket), i_op vtable (lookup finds a dentry in a directory, create creates a new inode), address_space (i_mapping) for page cache.

**Section 4 — struct dentry**
```c
// include/linux/dcache.h (key fields)
struct dentry {
    unsigned int        d_flags;        // DCACHE_ENTRY_TYPE, DCACHE_REFERENCED...
    seqcount_spinlock_t d_seq;          // sequence counter for lockless reads
    struct hlist_bl_node d_hash;        // hash table for name lookup
    struct dentry      *d_parent;       // parent dentry (self for root)
    struct qstr         d_name;         // filename (length + hash + pointer to string)
    struct inode       *d_inode;        // inode (NULL = negative dentry = file doesn't exist)
    unsigned char       d_iname[DNAME_INLINE_LEN]; // short names stored inline
    struct lockref      d_lockref;      // reference count + spinlock
    const struct dentry_operations *d_op; // d_revalidate, d_hash, d_compare...
    struct super_block *d_sb;           // superblock
    unsigned long       d_time;         // used by d_revalidate
    void               *d_fsdata;       // filesystem-specific data
    union {
        struct list_head d_lru;         // LRU when not in use (reclaimable)
        wait_queue_head_t *d_wait;
    };
    struct hlist_node   d_sib;          // sibling list in parent
    struct hlist_head   d_children;     // child dentries
    union {
        struct hlist_node d_alias;      // d_inode->i_dentry (hard links)
        struct hlist_bl_node d_in_lookup_hash;
        struct rcu_head d_rcu;
    };
};
```
Source: https://elixir.bootlin.com/linux/v6.9/source/include/linux/dcache.h — https://elixir.bootlin.com/linux/v6.9/source/include/linux/dcache.h

Explain: dentries are the name-to-inode mapping. Multiple dentries can point to the same inode (hard links — via d_alias list). Negative dentries (d_inode = NULL) cache "file does not exist" to avoid repeated disk lookups. The dentry cache (dcache) is the kernel's path component cache — reduces open() latency by avoiding repeated directory traversals.

**Section 5 — struct file**
```c
// include/linux/fs.h (key fields)
struct file {
    union {
        struct callback_head    f_task_work;
        struct llist_node       f_llist;
        unsigned int            f_iocb_flags;
    };
    spinlock_t                  f_lock;
    fmode_t                     f_mode;     // FMODE_READ, FMODE_WRITE, FMODE_EXEC...
    atomic_long_t               f_count;    // reference count (dup, fork share this)
    struct mutex                f_pos_lock;
    loff_t                      f_pos;      // current file position
    unsigned int                f_flags;    // O_RDONLY, O_WRONLY, O_NONBLOCK, O_APPEND...
    struct fown_struct          f_owner;    // for SIGIO/SIGURG delivery
    const struct cred          *f_cred;     // credentials at open time
    struct file_ra_state        f_ra;       // readahead state
    u64                         f_version;
    void                       *f_security; // LSM data (SELinux, etc.)
    void                       *private_data; // filesystem-specific (e.g., socket)
    struct address_space       *f_mapping;  // page cache (== inode->i_mapping usually)
    errseq_t                    f_wb_err;   // writeback error sticky bit
    errseq_t                    f_sb_err;
    struct path                 f_path;     // { struct vfsmount *mnt; struct dentry *dentry; }
    struct inode               *f_inode;    // == f_path.dentry->d_inode (cached)
    const struct file_operations *f_op;    // read, write, mmap, ioctl, poll...
};
```
Source: https://elixir.bootlin.com/linux/v6.9/source/include/linux/fs.h

Explain: struct file represents one open file description. Multiple file descriptors in one or more processes can share the same struct file (via dup2, fork, or passing via SCM_RIGHTS). f_count tracks all references. f_pos is protected by f_pos_lock (allowing concurrent pread/pwrite without conflict). f_path carries both the vfsmount and dentry — needed for resolution relative to the mount namespace.

**Section 6 — open(2) call path**
```
open("/etc/hosts", O_RDONLY)
  └─ sys_open() → do_sys_openat2()       # fs/open.c
       └─ do_filp_open()                  # fs/namei.c
            └─ path_openat()
                 ├─ link_path_walk()      # walk each / component
                 │    └─ walk_component()
                 │         ├─ [cache hit] lookup_fast() → dcache lookup → struct dentry
                 │         └─ [cache miss] lookup_slow() → inode->i_op->lookup()
                 │                                          → reads from disk → new dentry
                 └─ do_open()
                      └─ vfs_open()       # fs/open.c
                           └─ do_dentry_open()
                                └─ inode->i_fop->open()  # filesystem handler
```
Source: fs/open.c — https://elixir.bootlin.com/linux/v6.9/source/fs/open.c
Source: fs/namei.c — https://elixir.bootlin.com/linux/v6.9/source/fs/namei.c

**Section 7 — Live Observation**
```bash
# All open files for a process:
ls -la /proc/$PID/fd/           # fd → file symlinks
ls -la /proc/$PID/fdinfo/       # per-fd: pos, flags, mount_id

# Dentry cache stats:
cat /proc/sys/fs/dentry-state   # nr_dentry, nr_unused, age_limit, want_pages

# Inode cache stats:
cat /proc/sys/fs/inode-state    # nr_inodes, nr_unused

# Filesystem type of an inode:
stat /etc/hosts                 # shows inode number, block count

# bpftrace: trace every open(2) with filename
bpftrace -e 'tracepoint:syscalls:sys_enter_openat {
    printf("open: pid=%-6d comm=%-20s file=%s\n",
           pid, comm, str(args->filename));
}'

# bpftrace: trace vfs_open (after path resolution)
bpftrace -e 'kprobe:vfs_open {
    printf("vfs_open: pid=%d comm=%s\n", pid, comm);
}'

# bpftrace: count dcache misses (lookup_slow calls per comm)
bpftrace -e 'kprobe:lookup_slow { @[comm] = count(); }'
```

**Section 8 — Key References table**
struct super_block, struct inode, struct dentry, struct file, vfs_open, path_openat, lookup_slow — each with file path and bare elixir v6.9 URL.

- [ ] **Step 3: git add and commit**
```bash
git add 05-vfs-storage/
git commit -m "feat(ch05): README and VFS layer deep dive"
```

---

## Task 2: Mount subsystem deep dive

**Files:**
- Create: `05-vfs-storage/kernel/05-b-mount.md`

- [ ] **Step 1: Write `05-vfs-storage/kernel/05-b-mount.md`**

**Section 1 — Source locations**
- `fs/mount.h` — https://elixir.bootlin.com/linux/v6.9/source/fs/mount.h
- `fs/namespace.c` — https://elixir.bootlin.com/linux/v6.9/source/fs/namespace.c
- `include/linux/mount.h` — https://elixir.bootlin.com/linux/v6.9/source/include/linux/mount.h

**Section 2 — struct vfsmount and struct mount**
`struct vfsmount` (the public-facing half):
```c
// include/linux/mount.h
struct vfsmount {
    struct dentry *mnt_root;        // root dentry of the mounted filesystem
    struct super_block *mnt_sb;     // superblock of the mounted filesystem
    int mnt_flags;                  // MNT_NOSUID, MNT_NODEV, MNT_NOEXEC, MNT_READONLY...
    struct user_namespace *mnt_userns; // user namespace for uid/gid mapping
};
```

`struct mount` (the internal, complete mount record — fs/mount.h):
```c
// fs/mount.h (key fields)
struct mount {
    struct hlist_node mnt_hash;         // hash table for lookup
    struct mount     *mnt_parent;       // mount this is mounted on
    struct dentry    *mnt_mountpoint;   // dentry in parent where this is attached
    struct vfsmount   mnt;              // embedded public struct
    union {
        struct rcu_head mnt_rcu;
        struct llist_node mnt_llist;
    };
    struct list_head mnt_mounts;        // child mounts
    struct list_head mnt_child;         // link in parent's mnt_mounts list
    struct list_head mnt_instance;      // sb->s_mounts list
    const char       *mnt_devname;      // device name (e.g., "/dev/sda1", "overlay")
    union {
        struct mnt_namespace *mnt_ns;   // containing namespace
        struct mnt_pcp __percpu *mnt_pcp; /* for count */
    };
    struct mountpoint *mnt_mp;          // where this is mounted
    struct hlist_node mnt_mp_list;
    struct list_head mnt_umounting;
    struct list_head mnt_list;          // namespace's list of all mounts
    int mnt_id;                         // unique mount ID (shown in /proc/pid/mountinfo)
    int mnt_group_id;                   // peer group for shared mounts
    int mnt_expiry_mark;
    struct hlist_head mnt_pins;
    struct hlist_head mnt_stuck_children;
};
```
Source: https://elixir.bootlin.com/linux/v6.9/source/fs/mount.h

Explain the struct mount / vfsmount split: `struct mount` is the kernel-internal full record (in fs/mount.h, not exposed to filesystem code). Filesystem code only receives `struct vfsmount *` (the embedded public portion). This allows the kernel to add fields to `struct mount` without changing the filesystem API. Access the full `struct mount` from a `struct vfsmount *` using `container_of(mnt, struct mount, mnt)` or the `real_mount()` helper.

**Section 3 — struct mnt_namespace**
```c
// fs/mount.h (key fields)
struct mnt_namespace {
    struct ns_common    ns;         // common namespace header (inum for /proc/pid/ns/mnt)
    struct mount       *root;       // root mount of this namespace
    struct list_head    list;       // all mounts in this namespace (mnt->mnt_list)
    spinlock_t          ns_lock;
    struct user_namespace *user_ns;
    u64                 seq;        // sequence number for /proc/pid/mountstats
    wait_queue_head_t   poll;       // for poll() on /proc/pid/mounts
    u64                 event;      // incremented on every mount/umount
    unsigned int        mounts;     // number of mounts
    unsigned int        pending_mounts;
};
```
Source: https://elixir.bootlin.com/linux/v6.9/source/fs/mount.h

Each process inherits its parent's mount namespace unless `CLONE_NEWNS` is passed to `clone()`. Mount namespaces are the mechanism that gives containers their own filesystem view.

**Section 4 — mount(2) kernel path**
```
mount("overlay", "/merged", "overlay", MS_RDONLY, "lowerdir=...")
  └─ sys_mount() → do_mount() → path_mount()          # fs/namespace.c
       ├─ [new mount] do_new_mount()
       │    ├─ fs_context_for_mount() → alloc_fs_context() # per-fs setup
       │    ├─ vfs_get_tree()                           # calls overlayfs fill_super
       │    └─ do_new_mount_fc()
       │         └─ graft_tree()                        # attach new mount to parent
       └─ [bind mount] do_loopback()                    # clone existing tree
```
Source: fs/namespace.c — https://elixir.bootlin.com/linux/v6.9/source/fs/namespace.c

**Section 5 — /proc/pid/mountinfo format**
Each line in `/proc/pid/mountinfo` corresponds to one `struct mount` and has this format:
```
36 35 98:0 /mnt1 /mnt2 rw,noatime master:1 - ext3 /dev/root rw,errors=continue
│  │  │    │     │     │           │         │ │    │          │
│  │  │    │     │     │           │         │ │    │          └─ superblock options
│  │  │    │     │     │           │         │ │    └─ device (mnt_devname)
│  │  │    │     │     │           │         │ └─ filesystem type
│  │  │    │     │     │           │         └─ separator
│  │  │    │     │     │           └─ optional: peer group/master info
│  │  │    │     │     └─ mount options (mnt_flags)
│  │  │    │     └─ mount point in namespace (mnt_mountpoint path)
│  │  │    └─ root of mount relative to filesystem root (mnt_root)
│  │  └─ major:minor (dev_t of the block device or pseudo-device)
│  └─ parent mount ID (mnt_parent->mnt_id)
└─ mount ID (mnt_id)
```

For a container process, /proc/<container-pid>/mountinfo shows only the mounts visible within that container's mount namespace.

**Section 6 — Shared/Slave/Private mounts (mount propagation)**
Mount propagation controls whether a mount event in one namespace is visible in others:
- **Shared**: `mount --make-shared /mnt` — events propagate to all peers in the same peer group. A mount inside a shared subtree appears in all sharing namespaces.
- **Slave**: `mount --make-slave /mnt` — receives propagation from the master, but does not send events back. Container bind mounts are typically slave mounts of the host.
- **Private**: `mount --make-private /mnt` — no propagation in either direction. An isolated namespace.
- **Unbindable**: `mount --make-unbindable /mnt` — private AND cannot be used as a bind-mount source.

runc uses `make-rslave` on the container root to inherit host mounts without leaking container mounts back to the host.

**Section 7 — Live Observation**
```bash
# All mounts in current namespace:
cat /proc/self/mountinfo

# Mounts in a container's namespace:
cat /proc/$CONTAINER_PID/mountinfo

# Count mounts per namespace:
ls /proc/*/mountinfo | wc -l

# bpftrace: trace mount(2) syscalls
bpftrace -e 'tracepoint:syscalls:sys_enter_mount {
    printf("mount: pid=%-6d comm=%-20s source=%s target=%s type=%s\n",
           pid, comm,
           str(args->dev_name), str(args->dir_name), str(args->type));
}'

# bpftrace: trace unmount
bpftrace -e 'tracepoint:syscalls:sys_enter_umount {
    printf("umount: pid=%d comm=%s name=%s\n", pid, comm, str(args->name));
}'
```

**Section 8 — Key References table**
struct vfsmount, struct mount, struct mnt_namespace, do_new_mount, path_mount — each with file path and bare elixir v6.9 URL.

- [ ] **Step 2: git add and commit**
```bash
git add 05-vfs-storage/kernel/05-b-mount.md
git commit -m "feat(ch05): mount subsystem deep dive — struct mount, mnt_namespace, mountinfo"
```

---

## Task 3: OverlayFS deep dive

**Files:**
- Create: `05-vfs-storage/kernel/05-c-overlayfs.md`

- [ ] **Step 1: Write `05-vfs-storage/kernel/05-c-overlayfs.md`**

**Section 1 — Source locations**
- `fs/overlayfs/` — https://elixir.bootlin.com/linux/v6.9/source/fs/overlayfs/
- `fs/overlayfs/ovl_entry.h` — https://elixir.bootlin.com/linux/v6.9/source/fs/overlayfs/ovl_entry.h
- `fs/overlayfs/inode.c` — https://elixir.bootlin.com/linux/v6.9/source/fs/overlayfs/inode.c
- `fs/overlayfs/copy_up.c` — https://elixir.bootlin.com/linux/v6.9/source/fs/overlayfs/copy_up.c

**Section 2 — OverlayFS architecture**
OverlayFS merges multiple filesystem layers into a single view. In containers:
```
Upper layer (writable):   /var/lib/containerd/io.containerd.snapshotter.v1.overlayfs/snapshots/<id>/fs/
Lower layers (read-only): image layers, from top to bottom
Work dir:                 /var/lib/containerd/.../snapshots/<id>/work/
Merged (container root):  /run/containerd/io.containerd.runtime.v2.task/<id>/rootfs/
```

Mount command used by containerd/runc:
```bash
mount -t overlay overlay \
  -o lowerdir=/layer3:/layer2:/layer1,upperdir=/upper,workdir=/work \
  /merged
```
Multiple lower layers are separated by `:`. The leftmost lower layer has highest priority (shadows names in layers to its right).

**Section 3 — struct ovl_inode**
```c
// fs/overlayfs/ovl_entry.h (key fields)
struct ovl_inode {
    union {
        struct ovl_dir_cache *cache;    // directory entries cache
        const char           *lowerdata_redirect;
    };
    const char     *redirect;          // redirect xattr (for renames across layers)
    u64             version;           // version counter for d_revalidate
    unsigned long   flags;             // OVL_COPY_UP_FINISHED, OVL_WHITEOUT, ...
    struct inode    vfs_inode;         // embedded VFS inode (MUST be last)
    struct dentry  *__upperdentry;     // upper layer dentry (NULL if not copied up)
    struct ovl_path lowerpath;         // primary lower layer path
    struct ovl_path *lowerstack;       // all lower paths (for multi-layer)
};
```
Source: https://elixir.bootlin.com/linux/v6.9/source/fs/overlayfs/ovl_entry.h

Explain: `__upperdentry` is NULL for read-only files (still in lower layer). It is set after copy-up completes. `OVL_WHITEOUT` flag marks a file that was deleted in the upper layer — it appears as a character device with major:minor 0:0 or a xattr whiteout.

**Section 4 — Copy-up mechanism**
When a file in a lower (read-only) layer is first written, OverlayFS copies it to the upper layer before the write:
```
write() to lower-layer file
  └─ ovl_write_iter() or ovl_setattr()    # fs/overlayfs/file.c
       └─ ovl_copy_up()                   # fs/overlayfs/copy_up.c
            └─ ovl_copy_up_one()
                 ├─ [create parent dirs in upper if needed]
                 ├─ ovl_create_real() → create empty file in upper layer
                 ├─ ovl_copy_up_data() → copy file contents (vfs_iter_read → vfs_iter_write)
                 ├─ ovl_copy_xattrs() → copy xattrs (security labels etc.)
                 └─ ovl_do_rename() → atomic rename into final position
                      └─ set __upperdentry = upper dentry
```
Source: https://elixir.bootlin.com/linux/v6.9/source/fs/overlayfs/copy_up.c

After copy-up, reads and writes go directly to the upper layer. The lower layer file is unchanged — this is the O in CoW (copy-on-write). Copy-up can be expensive for large files (e.g., modifying a 500MB library that lives in a lower layer copies 500MB to the upper layer before the first byte is written).

**Section 5 — Whiteout files (deletion)**
When a file from a lower layer is deleted:
1. OverlayFS creates a "whiteout" in the upper layer at that path
2. Whiteouts are character devices with 0:0 major:minor
3. During directory listing, whiteouts hide the corresponding lower-layer entries
4. `ovl_check_whiteout()` in `fs/overlayfs/dir.c` tests for whiteouts

```bash
# View whiteouts in a container's upper layer:
find /var/lib/containerd/.../snapshots/<id>/fs/ -type c
```

**Section 6 — Opaque directories**
When a directory is created in the upper layer that shadows a directory in a lower layer, it gets the xattr `trusted.overlay.opaque=y`. This tells OverlayFS not to merge the lower directory's contents — the upper directory is opaque. Used when a directory is deleted and recreated.

**Section 7 — Live Observation**
```bash
# See overlay mounts:
cat /proc/$PID/mountinfo | grep overlay

# Inspect container layers (containerd):
CONTAINER_ID=$(crictl ps -q | head -1)
crictl inspect $CONTAINER_ID | jq '.info.runtimeSpec.mounts[] | select(.type=="bind")'

# Count layers for a container (number of lowerdir entries):
cat /proc/$PID/mountinfo | grep overlay | \
  grep -oP 'lowerdir=\K[^,]+' | tr ':' '\n' | wc -l

# bpftrace: trace copy-up events
bpftrace -e 'kprobe:ovl_copy_up_one {
    printf("copy-up: pid=%d comm=%s\n", pid, comm);
}'

# bpftrace: trace overlay inode creation
bpftrace -e 'kprobe:ovl_new_inode {
    printf("ovl_inode: pid=%d comm=%s\n", pid, comm);
}'
```

**Section 8 — Key References table**
struct ovl_inode, ovl_copy_up, ovl_copy_up_one, ovl_check_whiteout, ovl_new_inode — each with file path and bare elixir v6.9 URL.

- [ ] **Step 2: git add and commit**
```bash
git add 05-vfs-storage/kernel/05-c-overlayfs.md
git commit -m "feat(ch05): OverlayFS deep dive — layers, copy-up, whiteouts"
```

---

## Task 4: Block I/O + K8s connection

**Files:**
- Create: `05-vfs-storage/kernel/05-d-block-io.md`
- Create: `05-vfs-storage/k8s/05-k8s-connection.md`

- [ ] **Step 1: Write `05-vfs-storage/kernel/05-d-block-io.md`**

**Section 1 — Source locations**
- `include/linux/bio.h` — https://elixir.bootlin.com/linux/v6.9/source/include/linux/bio.h
- `block/blk-core.c` — https://elixir.bootlin.com/linux/v6.9/source/block/blk-core.c
- `mm/page-writeback.c` — https://elixir.bootlin.com/linux/v6.9/source/mm/page-writeback.c

**Section 2 — struct bio (Block I/O)**
```c
// include/linux/bio.h (key fields)
struct bio {
    struct bio          *bi_next;       // linked list of bios
    struct block_device *bi_bdev;       // target block device
    blk_opf_t           bi_opf;         // REQ_OP_READ, REQ_OP_WRITE, REQ_OP_FLUSH...
    unsigned short      bi_flags;
    unsigned short      bi_ioprio;      // I/O priority (CFQ scheduler)
    blk_status_t        bi_status;      // completion status (BLK_STS_OK, BLK_STS_IOERR...)
    atomic_t            __bi_remaining; // for chained bios
    struct bvec_iter    bi_iter;        // current position in bi_io_vec
    blk_qc_t            bi_cookie;
    bio_end_io_t       *bi_end_io;      // completion callback
    void               *bi_private;     // private data for bi_end_io
    struct blkcg_gq    *bi_blkg;        // blkcg group (for io controller accounting)
    struct bio_vec      bi_inline_vecs[]; // inline scatter-gather list (for small bios)
    struct bio_vec     *bi_io_vec;      // scatter-gather list of pages
    unsigned short      bi_vcnt;        // number of entries in bi_io_vec
    unsigned short      bi_max_vecs;    // max entries
    struct bvec_iter    bi_iter;        // {sector, size, bi_idx, bi_bvec_done}
};
```
Source: https://elixir.bootlin.com/linux/v6.9/source/include/linux/bio.h

Explain: A bio is a single I/O request. It carries a scatter-gather list of pages (bio_vec = page + offset + length). Multiple bio_vecs allow a single I/O operation to transfer data from/to non-contiguous memory pages (zero-copy DMA scatter-gather).

`bi_opf` flags: `REQ_OP_READ` (0), `REQ_OP_WRITE` (1), `REQ_OP_FLUSH` (2 — writeback), `REQ_OP_DISCARD` (3 — TRIM for SSDs), `REQ_SYNC` (sync I/O, don't defer), `REQ_META` (filesystem metadata), `REQ_FUA` (force unit access — write to stable storage immediately).

**Section 3 — Page writeback**
Dirty pages accumulate in the page cache until:
1. **Background writeback**: pdflush/writeback threads flush dirty pages at a rate controlled by `vm.dirty_background_ratio` (default: 10% of RAM) or `vm.dirty_background_bytes`
2. **Throttled writeback**: if dirty pages exceed `vm.dirty_ratio` (default: 20%) or `vm.dirty_bytes`, the writing process is throttled
3. **Explicit sync**: `fsync(2)` / `fdatasync(2)` → `vfs_fsync()` → inode writeback

Call path:
```
dirty page threshold exceeded
  └─ balance_dirty_pages_ratelimited()   # mm/page-writeback.c
       └─ wakeup_flusher_threads()
            └─ wb_start_writeback()      # via work queue
                 └─ writeback_inodes_wb() → writepage() → submit_bio()
```

Observing writeback: `/proc/vmstat` fields: `nr_dirty`, `nr_writeback`, `nr_unstable`, `pgpgout` (pages paged out).

**Section 4 — I/O scheduler and request queue**
Modern kernels use blk-mq (multi-queue block layer):
- `struct blk_mq_ctx`: per-CPU software queue for staging bios
- `struct blk_mq_hw_ctx`: hardware dispatch queue (one per NVMe queue, CPU core, etc.)
- I/O scheduler (elevator): kyber (latency-oriented), bfq (fairness), mq-deadline (latency bound), none (SSD passthrough)

```bash
# Current I/O scheduler for a device:
cat /sys/block/sda/queue/scheduler

# I/O stats per device:
cat /proc/diskstats
iostat -x 1

# bpftrace: trace block I/O submissions
bpftrace -e 'tracepoint:block:block_rq_issue {
    printf("blk: pid=%-6d comm=%-16s dev=%d:%d op=%d sector=%llu size=%u\n",
           pid, comm, args->dev >> 20, args->dev & 0xFFFFF,
           args->rwbs[0], args->sector, args->nr_sector);
}'
```

**Section 5 — Key References table**
struct bio, struct bio_vec, submit_bio, balance_dirty_pages_ratelimited, blk_mq_submit_bio — each with file path and bare elixir v6.9 URL.

- [ ] **Step 2: Write `05-vfs-storage/k8s/05-k8s-connection.md`**

**Section 1 — K8s Volume Architecture overview**
Table: K8s Volume Type | Kernel mechanism | Mount type | Persistence
- emptyDir | tmpfs | tmpfs mount in pod's namespace | pod lifetime
- configMap | tmpfs | tmpfs with projected files | sync'd from API server
- secret | tmpfs | tmpfs with mode 0400 files | sync'd from API server
- hostPath | bind mount | bind mount into pod's namespace | host filesystem
- PVC (block) | block device | raw block or ext4/xfs mount | persistent
- PVC (NFS) | NFS client | NFS mount | persistent, shared
- ephemeral (CSI) | depends on driver | CSI plugin manages | pod lifetime

**Section 2 — Container image layers (OverlayFS)**
Walk through how containerd assembles layers:
1. Pull image: each layer is a tar.gz, extracted to a snapshot directory
2. Mount: `mount -t overlay overlay -o lowerdir=<layer3>:<layer2>:<layer1>,upperdir=<upper>,workdir=<work> <merged>`
3. runc creates a mount namespace for the container and bind-mounts the merged dir as `/`
4. Container writes go to upperdir; reads hit the highest layer that has the file

Show how to inspect layers live:
```bash
# Get container PID:
CPID=$(crictl inspect $(crictl ps -q | head -1) | jq -r '.info.pid')

# See the overlay mount:
grep overlay /proc/$CPID/mountinfo

# Parse lowerdir count:
grep overlay /proc/$CPID/mountinfo | grep -oP 'lowerdir=\K[^,\s]+' | tr ':' '\n' | wc -l
```

**Section 3 — emptyDir and tmpfs**
`emptyDir` volumes use `tmpfs` — an in-memory filesystem backed by anonymous pages (not disk). When a pod restarts, the tmpfs is destroyed and recreated.

```bash
# Verify emptyDir is tmpfs:
cat /proc/$CPID/mountinfo | grep tmpfs

# Size of an emptyDir:
df -h /proc/$CPID/root/tmp  # or wherever the emptyDir is mounted
```

With `emptyDir.medium: Memory`, the kubelet sets the tmpfs size limit. Without it, tmpfs can use up to half the node's RAM.

Memory consumed by tmpfs is charged to the pod's memory cgroup (`memory.current`) — important for OOM accounting (from chapter 04).

**Section 4 — ConfigMap and Secret volumes**
Both use in-memory tmpfs mounts projected into the pod. The kubelet watches the API server and updates the files when the ConfigMap/Secret changes:
```
ConfigMap updated → API server event → kubelet informer → remount/rewrite tmpfs files
```

Secret volume files are created with `0400` permissions and owner `root:root`. The files are populated atomically: the kubelet writes to a temp file in the same tmpfs and then renames it — so a container never sees a partially written secret file.

```bash
# Verify secret is on tmpfs:
cat /proc/$CPID/mountinfo | grep -A2 "secret\|token"
stat /proc/$CPID/root/var/run/secrets/kubernetes.io/serviceaccount/token
```

**Section 5 — CSI volumes**
Container Storage Interface (CSI) is the plug-in mechanism for persistent volumes. The CSI driver runs as a pod on the node and communicates with kubelet via gRPC over a Unix domain socket.

Mount flow:
```
PVC bound → kubelet NodeStageVolume() → CSI driver stages device → NodePublishVolume() → bind mount into pod
```

The CSI driver manages the actual filesystem mount (e.g., attaching an EBS volume, formatting it with ext4, mounting it). From the kernel's perspective, it's just a regular block device mount followed by a bind mount into the pod's mount namespace.

**Section 6 — inotify and Kubernetes**
The kubelet uses inotify (via informers backed by kube-apiserver watch) at the API level, but at the node level, it uses inotify on the `/sys/fs/cgroup/` and `/proc/` trees to detect container state changes:

```
kubelet → fsnotify (Go library) → inotify_add_watch() → kernel inotify subsystem
```

From Chapter 09 (K8s Internals), the kube-apiserver uses long-polling HTTP watches that translate to inotify internally on etcd's storage.

**Section 7 — Common Storage Failure Patterns**
Table: Symptom | Cause | Diagnosis
- Pod stuck in ContainerCreating | overlay mount failed | `journalctl -u containerd`, `dmesg | grep overlay`
- Container writes failing with ENOSPC | Upper layer disk full | `df -h $(dirname <upperdir>)`; check node storage
- Secret not updated in container | kubelet inotify watch missed event | check kubelet logs; remount
- High I/O wait on node | Writeback storm | `iostat -x 1`, `cat /proc/pressure/io`
- Mount namespace leak | containerd/runc crash without cleanup | `cat /proc/mounts | wc -l`; check for zombie mounts
- emptyDir OOM | tmpfs consuming too much memory | check memory.current for the pod's cgroup

**Section 8 — Key Kernel References table**
vfs_open, do_new_mount, ovl_copy_up, sys_inotify_add_watch, submit_bio — each with file path and bare elixir v6.9 URL.

- [ ] **Step 3: git add and commit**
```bash
git add 05-vfs-storage/kernel/05-d-block-io.md 05-vfs-storage/k8s/05-k8s-connection.md
git commit -m "feat(ch05): block I/O deep dive and k8s VFS/storage connection"
```

---

## Task 5: C exercise — `inotify-demo`

**Files:**
- Create: `05-vfs-storage/exercises/inotify-demo/inotify_demo.c`
- Create: `05-vfs-storage/exercises/inotify-demo/Makefile`
- Create: `05-vfs-storage/exercises/inotify-demo/README.md`
- Create: `05-vfs-storage/exercises/inotify-demo/.gitignore`

**What it demonstrates:** Use inotify to watch a directory for file creation, modification, deletion, and rename events. Show the event mask, filename, and inotify_event struct fields. Demonstrate why kubelet and container runtimes use inotify for watching config files and secret mounts.

The program:
1. Creates a temp directory under /tmp
2. Adds an inotify watch on it with `IN_CREATE | IN_MODIFY | IN_DELETE | IN_MOVED_FROM | IN_MOVED_TO | IN_CLOSE_WRITE`
3. Forks a child process that creates/modifies/deletes files in the directory
4. Parent reads events from the inotify fd in a loop and prints them
5. Prints decoded event mask for each event

```c
#define _GNU_SOURCE
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>
#include <fcntl.h>
#include <sys/inotify.h>
#include <sys/stat.h>
#include <sys/wait.h>
#include <errno.h>

#define EVENT_BUF_LEN (10 * (sizeof(struct inotify_event) + NAME_MAX + 1))

static const char *decode_mask(uint32_t mask, char *buf, size_t len)
{
    buf[0] = '\0';
    if (mask & IN_CREATE)      strncat(buf, "CREATE|",   len - strlen(buf) - 1);
    if (mask & IN_MODIFY)      strncat(buf, "MODIFY|",   len - strlen(buf) - 1);
    if (mask & IN_DELETE)      strncat(buf, "DELETE|",   len - strlen(buf) - 1);
    if (mask & IN_MOVED_FROM)  strncat(buf, "MOVED_FROM|", len - strlen(buf) - 1);
    if (mask & IN_MOVED_TO)    strncat(buf, "MOVED_TO|", len - strlen(buf) - 1);
    if (mask & IN_CLOSE_WRITE) strncat(buf, "CLOSE_WRITE|", len - strlen(buf) - 1);
    if (mask & IN_ISDIR)       strncat(buf, "ISDIR|",    len - strlen(buf) - 1);
    if (mask & IN_IGNORED)     strncat(buf, "IGNORED|",  len - strlen(buf) - 1);
    /* trim trailing | */
    size_t l = strlen(buf);
    if (l > 0 && buf[l-1] == '|') buf[l-1] = '\0';
    return buf;
}
```

The child should perform:
1. Create `file1.txt`, write "hello", close
2. Open `file1.txt` for append, write " world", close
3. Rename `file1.txt` → `file2.txt`
4. Create `secret.txt`, write "password", close (simulate atomic secret write via rename)
5. Rename `tmp_secret.txt` → `secret.txt` (show atomic rename pattern)
6. Unlink `file2.txt` and `secret.txt`
7. Sleep 100ms between each operation to let parent process events
8. exit(0)

Parent reads inotify events in a loop until the child exits (detected via waitpid WNOHANG between read() calls).

Compile: `gcc -Wall -Wextra -Werror -o inotify_demo inotify_demo.c`

README sections:
1. What It Demonstrates (inotify fd, IN_* masks, event loop, atomic rename pattern)
2. Build and run: `make run`
3. Expected output (showing each event type with decoded mask)
4. Kernel path: `inotify_add_watch(2)` → `sys_inotify_add_watch()` → `inotify_update_watch()` at https://elixir.bootlin.com/linux/v6.9/source/fs/notify/inotify/inotify_user.c; events generated by `fsnotify_parent()` in `fs/notify/fsnotify.c`
5. K8s connection: the kubelet watches `/sys/fs/cgroup/` via inotify (through Go's fsnotify library) to detect cgroup file changes; the secret volume controller uses `atomic rename` (write to tmp file, rename into place) to prevent containers from reading partial secret data — exactly what Demo 4/5 shows
6. Exercises: (a) Watch `/sys/fs/cgroup/` and observe events when a container starts/stops. (b) Add `IN_ACCESS` to the watch mask to see read events (note: very noisy). (c) Use `inotify_add_watch()` on multiple paths simultaneously.

- [ ] **Step 5: Test compilation and commit**
```bash
cd 05-vfs-storage/exercises/inotify-demo
gcc -Wall -Wextra -Werror -o inotify_demo inotify_demo.c
rm -f inotify_demo
cd ../../..
git add 05-vfs-storage/exercises/inotify-demo/
git commit -m "feat(ch05): inotify-demo C exercise — filesystem event watching"
```

---

## Task 6: Go exercise — `mount-inspector`

**Files:**
- Create: `05-vfs-storage/exercises/mount-inspector/go.mod`
- Create: `05-vfs-storage/exercises/mount-inspector/main.go`
- Create: `05-vfs-storage/exercises/mount-inspector/Makefile`
- Create: `05-vfs-storage/exercises/mount-inspector/README.md`
- Create: `05-vfs-storage/exercises/mount-inspector/.gitignore`

**Module:** `github.com/linux-to-k8s/mount-inspector`, go 1.22

**What it demonstrates:** Parse `/proc/<pid>/mountinfo` and display the mount table for a process (or the current process). Show overlay mounts with layer counts. Show tmpfs mounts. Filter by filesystem type.

Key type:
```go
type MountInfo struct {
    MountID        int
    ParentID       int
    Major, Minor   int
    Root           string
    MountPoint     string
    MountOptions   string
    OptionalFields []string
    FSType         string
    Source         string
    SuperOptions   string
}
```

Parse `/proc/<pid>/mountinfo` line by line. Split on space, handle the optional fields section (terminated by `-`).

Modes:
- `mount-inspector` — current process mounts
- `mount-inspector --pid <pid>` — target process mounts
- `mount-inspector --pid <pid> --overlay` — show only overlay mounts with layer count
- `mount-inspector --pid <pid> --type <fstype>` — filter by filesystem type

For `--overlay`, parse the `lowerdir=...` superblock option and count `:` separators to get layer count.

Makefile targets: build, run, run-pid (PID=...), clean

README sections:
1. What It Demonstrates
2. Build and run with examples
3. mountinfo format explanation (refer to 05-b-mount.md)
4. Expected output for an overlay container process
5. Exercises: (a) Count how many mounts a container has vs the host. (b) Parse the optional fields to find shared/slave/peer-group mounts. (c) Add --json output.
6. K8s connection: containerd creates one overlay mount per container; kubelet creates additional tmpfs mounts for secrets, configMaps, and serviceaccount tokens; each shows in the container's mountinfo

Verify: `go build -o mount-inspector .`, `go vet ./...`

- [ ] **Step 6: Commit**
```bash
git add 05-vfs-storage/exercises/mount-inspector/
git commit -m "feat(ch05): mount-inspector Go exercise — parse /proc/pid/mountinfo"
```

---

## Task 7: kube-inspect checkpoint 05 — mount ns inspection + overlay layer count

**Files:**
- Modify: `kube-inspect/internal/proc/proc.go` (add mount namespace reading)
- Modify: `kube-inspect/cmd/kube-inspect/main.go` (add `--mounts` flag)
- Modify: `kube-inspect/CHECKPOINT.md` (mark checkpoint 05 done)

**What it adds:**
- `proc.MountInfo` struct: MountID, ParentID, Root, MountPoint, FSType, Source, Options
- `proc.ListPodMounts(podUID) ([]MountInfo, error)` — reads /proc/<pid>/mountinfo for the first process in the pod
- `proc.CountOverlayLayers(podUID) (int, error)` — returns the number of overlay lower layers
- `--mounts` flag in main.go: shows mount table + overlay layer count

```go
// MountInfo represents one line from /proc/pid/mountinfo
type MountInfo struct {
    MountID     int    `json:"mount_id"`
    ParentID    int    `json:"parent_id"`
    Root        string `json:"root"`
    MountPoint  string `json:"mount_point"`
    Options     string `json:"options"`
    FSType      string `json:"fstype"`
    Source      string `json:"source"`
    SuperOpts   string `json:"super_opts"`
}
```

`ListPodMounts(podUID)`: find a PID for the pod (reuse existing `ListPodProcesses`), read `/proc/<pid>/mountinfo`, parse each line, return slice.

`CountOverlayLayers(podUID)`: call `ListPodMounts`, find the mount with `FSType == "overlay"` and `MountPoint == "/"`, parse the `lowerdir=...` from SuperOpts, count `:` + 1.

`--mounts` output:
```
Mounts for pod <uid>:
  ID    PARENT  FSTYPE      SOURCE               MOUNTPOINT
  23    22      overlay     overlay              /
    overlay layers: 5
  24    23      proc        proc                 /proc
  25    23      tmpfs       tmpfs                /dev
  ...
```

- [ ] **Step 7: Verify and commit**
```bash
cd kube-inspect
go build ./...
go vet ./...
cd ..
git add kube-inspect/
git commit -m "feat(kube-inspect): checkpoint 05 — mount namespace inspection and overlay layer count"
```

Update CHECKPOINT.md: change checkpoint 05 status from "pending" to "done".

---

## Self-Review

**Spec coverage:**
- ✅ `05-vfs-storage/README.md` — objectives, prerequisites, reading order, VFS diagram
- ✅ `kernel/05-a-vfs.md` — superblock, inode, dentry, file, open() path, live observation
- ✅ `kernel/05-b-mount.md` — struct mount, mnt_namespace, mountinfo format, propagation
- ✅ `kernel/05-c-overlayfs.md` — ovl_inode, copy-up path, whiteouts, opaque dirs
- ✅ `kernel/05-d-block-io.md` — struct bio, writeback, I/O scheduler
- ✅ `k8s/05-k8s-connection.md` — image layers, emptyDir/tmpfs, configMap/secret, CSI, inotify
- ✅ `exercises/inotify-demo/` — C exercise compiles with -Wall -Wextra -Werror
- ✅ `exercises/mount-inspector/` — Go exercise, go vet clean
- ✅ `kube-inspect/` — MountInfo, ListPodMounts, CountOverlayLayers, --mounts flag, checkpoint 05 done

**Placeholder scan:** All sections require complete content.

**Type consistency:** `proc.MountInfo` defined in Task 7; `proc.ListPodMounts()` and `proc.CountOverlayLayers()` same; `main.go` uses `mounts[i].FSType` etc. — must match struct fields exactly.
