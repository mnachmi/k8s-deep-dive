# 05-a — VFS: The Virtual Filesystem Switch

## The Abstraction Layer That Made Linux Extensible

In the early 1990s, when Linux was a hobby kernel for a single machine, it had one filesystem: Minix. Adding support for a second filesystem (ext, then ext2) meant the open/read/write syscall implementations had to dispatch to different function tables depending on the filesystem type. This got messy fast. By the time Linux needed to support ten filesystems simultaneously — ext2 for the local disk, proc for kernel state, nfs for remote files, tmpfs for volatile memory, devfs for devices — it became clear that there needed to be an abstraction layer between the syscall interface and the filesystem implementations.

Sun Microsystems had already solved this problem in SunOS 2.0 (1985) with the Virtual File System interface, and Linus Torvalds adapted the concept for Linux. The VFS is the contract between the kernel's syscall layer and the filesystem implementations. It says: if you want to be a filesystem in Linux, implement these four vtables (`super_operations`, `inode_operations`, `dentry_operations`, `file_operations`), and every tool that goes through the standard syscall interface will work with your filesystem automatically.

The consequences for Kubernetes are profound. The OCI container image format is built on the VFS. When a container starts, an overlay filesystem assembles multiple image layers into a single coherent directory tree — each layer is a separate lower directory in the VFS, and writes go to a per-container upper directory. The resulting view is just a normal VFS mount; container processes call `open()` and `read()` exactly as they would on any filesystem, and the VFS dispatches to OverlayFS's vtable implementations, which transparently handle the copy-on-write semantics. Understanding VFS means understanding why container image layers work the way they do, why `kubectl exec` can read files in a running container, and why `/proc` inside a container shows different information from `/proc` on the host.

The Virtual Filesystem Switch is the kernel subsystem that makes every filesystem look identical to userspace. `open()`, `read()`, `write()`, `stat()`, `rename()`, and `unlink()` all resolve to the same VFS entry points regardless of whether the target is on ext4, xfs, tmpfs, OverlayFS, or procfs. VFS achieves this through four core objects — `super_block`, `inode`, `dentry`, and `file` — each carrying a pointer to a filesystem-specific vtable.

---

## Source Locations

| File | Link | Contents |
|------|------|----------|
| `include/linux/fs.h` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/fs.h | `struct super_block`, `struct inode`, `struct file`; all VFS vtable definitions |
| `include/linux/dcache.h` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/dcache.h | `struct dentry`, `struct dentry_operations`, dentry cache API |
| `fs/namei.c` | https://elixir.bootlin.com/linux/v6.9/source/fs/namei.c | `path_openat()`, `link_path_walk()`, `walk_component()`, `lookup_fast()`, `lookup_slow()`, `do_filp_open()` |
| `fs/open.c` | https://elixir.bootlin.com/linux/v6.9/source/fs/open.c | `do_sys_openat2()`, `vfs_open()`, `do_dentry_open()` |
| `fs/dcache.c` | https://elixir.bootlin.com/linux/v6.9/source/fs/dcache.c | Dentry cache: allocation, hashing, LRU eviction, `d_lookup()`, `d_add()` |
| `fs/inode.c` | https://elixir.bootlin.com/linux/v6.9/source/fs/inode.c | Inode lifecycle: `alloc_inode()`, `iput()`, `inode_init_always()`, inode LRU |

---

## `struct super_block` — One Per Mounted Filesystem

When the kernel mounts a filesystem — any filesystem — it allocates a `super_block` to represent that mounted instance. A `super_block` is not tied to a device: tmpfs mounts, procfs, and overlayfs all have superblocks but no persistent block device backing them. The `super_block` is the root of everything: it anchors the inode list, the root dentry, and the filesystem's operation vtable.

```c
// include/linux/fs.h (key fields)
struct super_block {
    struct list_head             s_list;          // all superblocks: linked into super_blocks list
    dev_t                        s_dev;           // device identifier (0 for pseudo-filesystems)
    unsigned char                s_blocksize_bits;
    unsigned long                s_blocksize;     // block size in bytes (typically 4096)
    loff_t                       s_maxbytes;      // maximum file size supported by this FS
    struct file_system_type     *s_type;          // e.g., &ext4_fs_type, &tmpfs_fs_type
    const struct super_operations *s_op;         // statfs, alloc_inode, destroy_inode, sync_fs...
    const struct dquot_operations *dq_op;        // quota operations
    const struct quotactl_ops   *s_qcop;         // quota control interface
    const struct export_operations *s_export_op; // NFS export support
    unsigned long                s_flags;         // MS_RDONLY, MS_NOSUID, MS_NODEV, MS_NOEXEC...
    unsigned long                s_iflags;        // internal VFS flags (SB_SUBMOUNT, SB_FORCE, ...)
    unsigned long                s_magic;         // filesystem magic number (see below)
    struct dentry               *s_root;          // root dentry of this mounted filesystem
    struct rw_semaphore          s_umount;        // held write during unmount; read for all else
    int                          s_count;         // reference count on super_block itself
    atomic_t                     s_active;        // active mount count (vfsmounts using this sb)
    struct list_head             s_inodes;        // all inodes belonging to this superblock
    spinlock_t                   s_inode_list_lock;
    struct list_head             s_mounts;        // all struct vfsmount instances for this sb
    struct block_device         *s_bdev;          // underlying block device (NULL for pseudo-fs)
    char                         s_id[32];        // informational name (e.g., "sda1", "tmpfs")
    uuid_t                       s_uuid;          // filesystem UUID (from on-disk superblock)
};
```

Source: https://elixir.bootlin.com/linux/v6.9/source/include/linux/fs.h

### Field-by-Field Explanation

**`s_list`** — Links this superblock into the global `super_blocks` list (protected by `sb_lock`). Tools like `df` iterate this list (via `/proc/mounts`) to enumerate all mounted filesystems.

**`s_dev`** — The device number of the block device backing this filesystem. For pseudo-filesystems (tmpfs, procfs, sysfs, overlayfs) this is zero or a synthetic value assigned by `get_anon_bdev()`. Two filesystems on different devices always have different `s_dev` values, which is how `stat(2)` reports `st_dev` and how `rename(2)` detects cross-device moves.

**`s_blocksize` / `s_blocksize_bits`** — The native block size of the filesystem. `s_blocksize_bits` is `log2(s_blocksize)` stored as a shift for fast arithmetic. The block size constrains the minimum I/O unit for direct I/O and affects page cache alignment.

**`s_maxbytes`** — The largest file the filesystem supports. ext4 with 64-bit offsets supports 16 TiB; FAT32 caps at 4 GiB; tmpfs uses `MAX_LFS_FILESIZE`. The VFS enforces this in `generic_write_checks()` before calling into the filesystem.

**`s_type`** — Points to the `file_system_type` registered by the filesystem module (e.g., `ext4_fs_type`, `tmpfs_fs_type`). This is how the kernel identifies what filesystem is mounted: `cat /proc/mounts` reads `s_type->name`.

**`s_op`** — The superblock operations vtable. Key callbacks:
- `alloc_inode()` — allocate a filesystem-specific inode (e.g., `ext4_inode_info` which embeds `struct inode`)
- `destroy_inode()` — free a filesystem-specific inode via RCU
- `dirty_inode()` — called when inode metadata is modified (triggers writeback)
- `sync_fs()` — flush in-memory state to disk (used by `sync(2)` and `syncfs(2)`)
- `statfs()` — fill in `struct kstatfs` for the `statfs(2)` syscall

**`s_flags`** — Mount flags set at mount time and reflected in `/proc/mounts`. Important values:

| Flag | Value | Meaning |
|------|-------|---------|
| `MS_RDONLY` | `1` | Filesystem mounted read-only; writes return `EROFS` |
| `MS_NOSUID` | `2` | Set-UID and set-GID bits are ignored during exec |
| `MS_NODEV` | `4` | Block and character device files cannot be opened |
| `MS_NOEXEC` | `8` | No executables can be run from this filesystem |
| `MS_SYNCHRONOUS` | `16` | All writes are synchronous (no delayed writeback) |
| `MS_NOATIME` | `1024` | Do not update `i_atime` on reads (reduces write traffic) |

**`s_magic`** — A per-filesystem-type constant written during `mkfs` and checked at mount time to verify the filesystem type. Mismatches cause mount failures. Well-known values:

| Magic | Hex | Filesystem |
|-------|-----|------------|
| `0xEF53` | `61267` | ext4 (and ext2/ext3 — they share the magic) |
| `0x58465342` | `1481003842` | XFS |
| `0x01021994` | `16914836` | tmpfs / shmem |
| `0x794C7630` | `2035054128` | OverlayFS |
| `0x9FA0` | `40864` | procfs |
| `0x62656572` | `1650812274` | sysfs (kernfs) |

**`s_root`** — The dentry of the filesystem's root directory. Every path walk that enters this filesystem starts here. When the kernel mounts a filesystem on top of `/mnt`, it links `s_root` into the mount's `mnt_root` pointer; path resolution crosses the mount boundary using `follow_mount()`.

**`s_umount`** — A read-write semaphore. The write side is held exclusively during `umount(2)` to prevent new users from entering the filesystem. All other superblock operations (path lookup, inode creation) hold the read side, which allows concurrent access.

**`s_count` vs `s_active`** — `s_count` is the number of kernel references to the `super_block` object itself (for lifetime management). `s_active` counts the number of active `vfsmount` instances using this superblock — when `s_active` drops to zero via `deactivate_super()`, the filesystem's `put_super()` is called to release on-disk resources.

**`s_inodes`** — A linked list (protected by `s_inode_list_lock`) of every inode currently in memory for this superblock. The writeback subsystem (`fs/fs-writeback.c`) iterates this list to find dirty inodes to flush. Pseudo-filesystems that regenerate inodes on demand (procfs, sysfs) still anchor their inodes here.

**`s_bdev`** — The `block_device` struct for the underlying storage device, or NULL for pseudo-filesystems, network filesystems, and OverlayFS (which delegates to its component filesystems). Block-layer I/O is submitted through `s_bdev`.

**`s_id` and `s_uuid`** — Human-readable and machine-readable identifiers. `s_id` is typically the device name (e.g., `"nvme0n1p2"`) or filesystem type (e.g., `"tmpfs"`). `s_uuid` is the 128-bit UUID from the on-disk superblock, used by `blkid` and `fstab` UUID-based mounts.

---

## `struct inode` — One Per File (Metadata Only)

An inode represents a file's metadata: ownership, permissions, timestamps, size, and the mapping from file offsets to disk blocks (via the address_space). Crucially, an inode does not contain the filename — names live in dentries. This separation enables hard links: multiple filenames pointing to the same inode.

```c
// include/linux/fs.h (key fields)
struct inode {
    umode_t                      i_mode;         // file type bits + permission bits
    unsigned short               i_opflags;      // internal operation cache flags
    kuid_t                       i_uid;          // owner UID (kernel-internal, mapped from user ns)
    kgid_t                       i_gid;          // owner GID
    unsigned int                 i_flags;        // S_IMMUTABLE, S_APPEND, S_NOATIME...
    const struct inode_operations *i_op;         // lookup, create, mkdir, rename, readlink...
    struct super_block           *i_sb;          // back-pointer to the superblock
    struct address_space         *i_mapping;     // page cache for file data (usually &i_data)
    unsigned long                i_ino;          // inode number (unique within filesystem)
    union {
        const unsigned int       i_nlink;        // hard link count (read-only view)
        unsigned int             __i_nlink;      // mutable alias for internal use
    };
    dev_t                        i_rdev;         // device number (for S_IFCHR / S_IFBLK files)
    loff_t                       i_size;         // file size in bytes
    struct timespec64            __i_atime;      // last access time   (use inode_get_atime())
    struct timespec64            __i_mtime;      // last modification time (use inode_set_mtime_to_ts())
    struct timespec64            __i_ctime;      // last change time   (use inode_get_ctime())
    spinlock_t                   i_lock;         // protects i_state, i_nlink, i_size changes
    unsigned short               i_bytes;        // bytes in the last allocated block
    u8                           i_blkbits;      // block size = 1 << i_blkbits
    u8                           i_write_hint;   // writeback hint for NVMe streams
    blkcnt_t                     i_blocks;       // number of 512-byte blocks allocated on disk
    atomic_t                     i_count;        // kernel reference count (struct inode users)
    atomic_t                     i_writecount;   // open-for-write count (negative = mmap'd exec)
    const struct file_operations *i_fop;         // default file operations for new struct file
    struct file_lock_context     *i_flctx;       // POSIX and flock file lock list
    struct address_space         i_data;         // embedded address_space for regular file data
    struct list_head             i_lru;          // LRU list for inode reclaim (shrinker)
    struct hlist_node            i_hash;         // inode hash table (keyed by sb + ino)
    struct list_head             i_sb_list;      // all inodes on this superblock (s_inodes)
    union {
        struct hlist_head        i_dentry;       // all dentries pointing to this inode (hard links)
        struct rcu_head          i_rcu;          // used during RCU-delayed inode freeing
    };
    atomic64_t                   i_version;      // NFS change attribute (bumped on metadata change)
    atomic_t                     i_dio_count;    // number of direct-I/O operations in flight
    void                        *i_private;      // filesystem-specific private data pointer
};
```

Source: https://elixir.bootlin.com/linux/v6.9/source/include/linux/fs.h

### Field-by-Field Explanation

**`i_mode` — File Type and Permissions**

`i_mode` is a 16-bit field encoding both the file type (upper 4 bits) and the Unix permission bits (lower 12 bits). The type bits are extracted with the `S_IFMT` mask:

| Macro | Value | File Type |
|-------|-------|-----------|
| `S_IFREG` | `0100000` | Regular file |
| `S_IFDIR` | `0040000` | Directory |
| `S_IFLNK` | `0120000` | Symbolic link |
| `S_IFCHR` | `0020000` | Character device |
| `S_IFBLK` | `0060000` | Block device |
| `S_IFIFO` | `0010000` | Named pipe (FIFO) |
| `S_IFSOCK` | `0140000` | Unix domain socket |

Helper macros `S_ISREG(m)`, `S_ISDIR(m)`, `S_ISLNK(m)` etc. test these bits. The kernel uses `i_mode` to dispatch the correct default `file_operations` at `open()` time — a character device gets `def_chr_fops`, a directory gets `simple_dir_operations`, and so on.

**`i_uid` and `i_gid`** — The kernel uses `kuid_t`/`kgid_t` (kernel-internal UID/GID) rather than raw `uid_t`. In containers with user namespace remapping, the translation between container UIDs (e.g., 0) and host UIDs (e.g., 100000) happens at the boundary between user namespace and VFS. `i_uid` always stores the host-namespace value; the mapping is applied in `inode_owner_or_capable()` and in the stat output path.

**`i_flags`** — Per-inode flags settable by `ioctl(FS_IOC_SETFLAGS)`:
- `S_IMMUTABLE` — no modification, renaming, or deletion allowed (even by root)
- `S_APPEND` — writes can only append; the file cannot be truncated
- `S_NOATIME` — do not update `i_atime` on reads
- `S_SYNC` — all writes are synchronous for this inode

**`i_op`** — The inode operations vtable. Key callbacks:
- `lookup(dir, dentry, flags)` — find a child name within a directory inode; called by `lookup_slow()` on a dcache miss
- `create(dir, dentry, mode, excl)` — create a new regular file
- `mkdir(dir, dentry, mode)` — create a new directory
- `rename(old_dir, old_dentry, new_dir, new_dentry, flags)` — rename or move a file
- `readlink(dentry, buf, bufsize)` — read a symbolic link target
- `permission(inode, mask)` — check DAC/MAC access permissions

**`i_ino`** — The inode number, unique within this filesystem instance. Two files on different filesystems can share the same inode number. `ls -i` shows inode numbers. Inode 0 is never used for real files (it marks a free inode in ext4's inode table). Inode 2 is conventionally the root directory on disk-based filesystems.

**`i_nlink` — Hard Link Count**

Every directory entry pointing to an inode increments `i_nlink`. Creating a file sets `i_nlink = 1`. `ln file hardlink` increments it to 2. `unlink()` decrements it. A file's data is freed only when two conditions are simultaneously true: `i_nlink == 0` (no directory entries remain) AND `i_count == 0` (no open file descriptions hold a reference). This is why you can `unlink()` a file while it is open: the data persists until the last `close()`. Containers exploit this: runtime scratch files are often opened and immediately unlinked to ensure cleanup on crash.

**`i_size`** — The logical size of the file in bytes, as reported by `stat(2)`. For sparse files, `i_size` can be much larger than the actual disk space consumed. For directories on most filesystems, `i_size` is the number of bytes consumed by directory entries. For symbolic links, `i_size` is the length of the link target string.

**`__i_atime`, `__i_mtime`, `__i_ctime` — Timestamps (Linux 6.6+)**

The three POSIX timestamps. The raw fields are private in Linux 6.6+ (renamed `__i_atime`, `__i_mtime`, `__i_ctime`); use `inode_get_atime()`, `inode_get_mtime()`, and `inode_get_ctime()` to read them; use `inode_set_atime_to_ts()`, `inode_set_mtime_to_ts()`, `inode_set_ctime_to_ts()` to write them. `__i_atime` is updated on every read (unless `MS_NOATIME` or `S_NOATIME` suppresses it). `__i_mtime` is updated when file data changes. `__i_ctime` is updated when any inode metadata changes (permissions, ownership, link count, data).

**`i_mapping`** — Pointer to the `address_space` that manages the page cache for this inode's data. For regular files, this points to `i_data` (the embedded `address_space`). The `address_space` holds an `xarray` (`i_pages`) of cached pages indexed by page offset. `read()`, `mmap()`, and page faults all go through `i_mapping`. The `address_space_operations` vtable (`a_ops`) provides `readpage()`, `writepage()`, and `write_begin()/write_end()` for the filesystem to fill and flush cache pages.

**`i_count` vs `i_nlink`** — `i_count` is the kernel's reference count on the `struct inode` object in memory. It is incremented by `iget_locked()` (disk read) and `igrab()` (existing in-memory inode), decremented by `iput()`. The inode object is freed when `i_count` drops to zero. `i_nlink` is the on-disk link count. They are independent: `i_count` can be 3 (three open file descriptions) while `i_nlink` is 0 (the file was unlinked).

---

## `struct dentry` — The Name-to-Inode Mapping

A dentry (directory entry) maps a single pathname component (e.g., `"hosts"`, `"etc"`, `"bin"`) to an inode. Dentries exist only in memory — they are not stored on disk. The kernel builds them on demand during path resolution and caches them in the dentry cache (dcache) to avoid repeated disk reads. Dentries form a tree mirroring the filesystem's directory structure, with each dentry pointing to its parent.

```c
// include/linux/dcache.h (key fields)
struct dentry {
    unsigned int                  d_flags;       // DCACHE_ENTRY_TYPE, DCACHE_REFERENCED, ...
    seqcount_spinlock_t           d_seq;          // sequence counter for lockless d_name reads
    struct hlist_bl_node          d_hash;         // bucket in the global dentry hash table
    struct dentry                *d_parent;       // parent dentry (points to self for root)
    struct qstr                   d_name;         // { hash, len, name* } — the filename component
    struct inode                 *d_inode;        // the inode this name resolves to (NULL = negative)
    unsigned char                 d_iname[DNAME_INLINE_LEN]; // inline storage for short names (≤36 bytes)
    struct lockref                d_lockref;      // combined spinlock + reference count
    const struct dentry_operations *d_op;        // d_revalidate, d_hash, d_compare, d_delete...
    struct super_block           *d_sb;           // the superblock this dentry belongs to
    unsigned long                 d_time;         // used by d_revalidate for staleness checks
    void                         *d_fsdata;       // filesystem-specific data (e.g., nfs cookie)
    union {
        struct list_head          d_lru;          // LRU list for unused-dentry reclaim
        wait_queue_head_t        *d_wait;         // used during in-lookup state
    };
    struct hlist_node             d_sib;          // sibling list within parent's d_children
    struct hlist_head             d_children;     // child dentries (subdirectories and files)
    union {
        struct hlist_node         d_alias;        // node in inode's i_dentry list (for hard links)
        struct hlist_bl_node      d_in_lookup_hash; // used while lookup is in progress
        struct rcu_head           d_rcu;          // for RCU-deferred freeing
    };
};
```

Source: https://elixir.bootlin.com/linux/v6.9/source/include/linux/dcache.h

### Field-by-Field Explanation

**`d_name` — `struct qstr`**

`qstr` (quick string) holds the filename component as a `{ unsigned int hash; unsigned int len; const unsigned char *name; }` triple. The hash is precomputed at dentry creation and used to locate the dentry in the global hash table keyed by `(parent_dentry, hash)`. For short names (up to `DNAME_INLINE_LEN` bytes, typically 36), the string bytes are stored directly in `d_iname` and `d_name.name` points there, avoiding a heap allocation.

**`d_inode` — Positive vs Negative Dentries**

- **Positive dentry**: `d_inode != NULL` — the name exists and maps to this inode.
- **Negative dentry**: `d_inode == NULL` — the name does not exist in the directory. The kernel caches negative dentries too: if `open("/tmp/nonexistent")` fails, the dentry for `"nonexistent"` is cached as negative. The next call to the same path finds the negative dentry in the dcache and returns `ENOENT` immediately, without a disk read. Negative dentries are evicted sooner under memory pressure.

**`d_parent` and `d_children` / `d_sib`** — Dentries form a tree. `d_parent` points upward; `d_children` is the head of a hlist of child dentries. Each child links itself into the parent's `d_children` list via its `d_sib` node. The root dentry of a filesystem has `d_parent == d_parent` (self-referential). Path walks traverse this tree component by component.

**`d_alias` and Hard Links**

When two filenames (in any directories) point to the same inode, each filename has its own dentry, but both dentries share the same inode. The inode's `i_dentry` field is the head of a hlist, and each dentry links into it via `d_alias`. The kernel can traverse all names for a given inode by walking `inode->i_dentry`. This is how `nlink` is implemented and how `fsck` detects cross-directory hard links.

**`d_op` — Dentry Operations**

- `d_revalidate(dentry, flags)` — called before using a cached dentry to verify it is still valid. Network filesystems (NFS, CIFS) implement this to check with the server. Most local filesystems leave it NULL (dentries are always valid).
- `d_hash(dentry, qstr)` — compute the hash for a name within this filesystem's namespace (used by case-insensitive filesystems to normalize before hashing).
- `d_compare(parent, name, str)` — compare filenames (case-insensitive filesystems override this).
- `d_delete(dentry)` — called when a dentry's reference count drops to zero; return 1 to immediately free it (negative dentries often do this).

**`d_lockref`** — A combined spinlock and reference count in a single 64-bit word, designed for lockless reference count manipulation on 64-bit platforms. The kernel uses `lockref_get_not_dead()` to atomically check and increment the count, avoiding a separate lock acquisition for the common path.

**The Dentry Cache (dcache) and Path Lookup Performance**

Every `open()`, `stat()`, or `access()` call walks the filesystem path component by component. Without the dcache, each component lookup would require a disk read to search the parent directory for the filename. The dcache caches these lookups: `lookup_fast()` hashes `(parent, name)` and checks the global hash table. A cache hit returns the dentry without any I/O; a cache miss calls `lookup_slow()`, which acquires the directory's inode lock and calls `i_op->lookup()` to read from disk. On a warm system, most path components are in the dcache. `cat /proc/sys/fs/dentry-state` shows `nr_dentry` (total cached) and `nr_unused` (evictable).

---

## `struct file` — One Per Open File Description

A `struct file` represents an open file description — the kernel object created by `open()` and returned (indirectly) as a file descriptor. It is distinct from both the inode (which holds persistent metadata) and the dentry (which holds the name). Multiple file descriptors, even in different processes, can share the same `struct file` — this happens after `dup2()`, after `fork()`, and when a file descriptor is passed between processes via `SCM_RIGHTS`. The file descriptor table maps small integers (0, 1, 2, ...) to `struct file *` pointers.

```c
// include/linux/fs.h (key fields)
struct file {
    union {
        struct callback_head     f_task_work;    // used for async I/O completion callbacks
        struct llist_node        f_llist;        // used when file is on a delayed-put list
        unsigned int             f_iocb_flags;   // io_uring flags
    };
    spinlock_t                   f_lock;         // protects f_ep and f_flags changes
    fmode_t                      f_mode;         // FMODE_READ, FMODE_WRITE, FMODE_EXEC...
    atomic_long_t                f_count;        // reference count (dup, fork, SCM_RIGHTS share this)
    struct mutex                 f_pos_lock;     // protects f_pos for concurrent pread/pwrite
    loff_t                       f_pos;          // current file position (read/write cursor)
    unsigned int                 f_flags;        // O_RDONLY, O_WRONLY, O_NONBLOCK, O_APPEND...
    struct fown_struct           f_owner;        // SIGIO/SIGURG signal delivery target
    const struct cred           *f_cred;         // credentials captured at open() time
    struct file_ra_state         f_ra;           // readahead state (next page to prefetch, etc.)
    u64                          f_version;      // used by nfsd for lease tracking
    void                        *f_security;     // LSM security blob (SELinux inode label cache)
    void                        *private_data;   // filesystem-specific (e.g., socket pointer for sockfs)
    struct address_space        *f_mapping;      // page cache (== f_inode->i_mapping normally)
    errseq_t                     f_wb_err;       // writeback error — sticky bit for fsync error detection
    errseq_t                     f_sb_err;       // superblock-level writeback error
    struct path                  f_path;         // { struct vfsmount *mnt; struct dentry *dentry; }
    struct inode                *f_inode;        // == f_path.dentry->d_inode (cached for performance)
    const struct file_operations *f_op;         // read, write, llseek, mmap, ioctl, poll, release...
};
```

Source: https://elixir.bootlin.com/linux/v6.9/source/include/linux/fs.h

### Field-by-Field Explanation

**`f_count` — Reference Count**

`f_count` tracks all references to this `struct file` object. It starts at 1 when `open()` creates it. `dup()` and `dup2()` increment it (sharing the same `struct file`, and thus the same `f_pos`, between two file descriptors in the same process). `fork()` increments it for every file descriptor inherited by the child. After `close()` decrements `f_count` to zero, `fput()` calls `f_op->release()` and frees the struct. This is why a `close()` in a child process does not affect the parent's open file description.

**`f_pos` — File Position**

The current byte offset for `read()` and `write()`. Protected by `f_pos_lock` (a mutex). `pread()` and `pwrite()` supply their own offset and acquire `f_pos_lock` only temporarily. Because `f_pos` is per-`struct file`, two processes sharing the same file via `fork()` share the same cursor — a read in one process advances the offset seen by the other. This is the intended behavior for shell pipelines. Applications that want independent cursors must call `open()` independently (each call creates a new `struct file`).

**`f_flags` — Open Flags**

The flags passed to `open()`, stored here after the syscall:
- `O_RDONLY` / `O_WRONLY` / `O_RDWR` — access mode (reflected in `f_mode`)
- `O_NONBLOCK` — non-blocking I/O for sockets and FIFOs
- `O_APPEND` — `write()` always sets `f_pos` to end-of-file atomically (enforced in `vfs_write()`)
- `O_SYNC` / `O_DSYNC` — synchronous writes (no write-behind caching)
- `O_DIRECT` — bypass page cache; I/O goes directly to/from the block device
- `O_CLOEXEC` — close on `exec()` (set in the file descriptor flags, not `f_flags`)

**`f_cred`** — The credentials of the calling process at `open()` time, captured and held for the lifetime of the `struct file`. Used to enforce access checks in `read()`, `write()`, and `ioctl()`. For setuid programs, `f_cred` is the effective credentials after privilege elevation.

**`f_path` — Mount-Namespace-Aware Path**

`f_path` is a `struct path { struct vfsmount *mnt; struct dentry *dentry; }`. It holds both the dentry and the specific mount point at which the file was resolved. This pair is essential for mount-namespace correctness: two processes in different mount namespaces may have the same dentry (same inode) but different `vfsmount` pointers, making the file appear at different paths in each namespace. `f_path` is used by `/proc/<pid>/fd/<n>` symlinks to reconstruct the path in the appropriate namespace.

**`f_op` — File Operations Vtable**

The file operations assigned at `open()` time (from `inode->i_fop`). Key callbacks:
- `read(file, buf, count, pos)` — synchronous read
- `write(file, buf, count, pos)` — synchronous write
- `read_iter(kiocb, iov_iter)` — vectored and async read (used by `io_uring`)
- `write_iter(kiocb, iov_iter)` — vectored and async write
- `llseek(file, offset, whence)` — reposition `f_pos`
- `mmap(file, vma)` — set up `vma->vm_ops` for file-backed memory mapping
- `poll(file, wait)` — check readiness for `epoll`/`select`/`poll`
- `ioctl(file, cmd, arg)` — filesystem/device-specific control operations
- `release(inode, file)` — called when `f_count` drops to zero; filesystem cleanup

**`f_mapping`** — Points to the page cache for this file's data. Normally identical to `f_inode->i_mapping` (`&inode->i_data`). Some special files (sockets via `sockfs`, pipe endpoints) redirect `f_mapping` to a different `address_space` or set it to NULL. During `read()`, `generic_file_read_iter()` looks up pages in `f_mapping->i_pages`; during `mmap()`, the VMA's `vm_ops->fault` handler reads from `f_mapping`.

---

## `open(2)` Call Path — From Syscall to Filesystem Handler

Understanding the `open(2)` call path shows how all four VFS objects — superblock, inode, dentry, and file — come together. The path splits into two phases: **name resolution** (finding the dentry for the requested path) and **file opening** (creating the `struct file`).

```
open("/etc/hosts", O_RDONLY)
  │
  └─ do_sys_openat2()                            # fs/open.c
       │   allocate struct open_how; parse flags
       └─ do_filp_open()                         # fs/namei.c
            │   allocate struct nameidata (nd) on the stack
            └─ path_openat()                     # fs/namei.c
                 │
                 ├─── PHASE 1: name resolution ──────────────────────────────
                 │
                 ├─ set_nameidata() / nd_jump_root()
                 │    start nd at process's root (or AT_FDCWD, or a dirfd)
                 │
                 └─ link_path_walk("/etc/hosts", nd)   # walk each component
                      │
                      └─ for each component ("etc", then "hosts"):
                           └─ walk_component(nd, component)
                                │
                                ├─ [dcache hit] lookup_fast(nd, &path)
                                │    hash(parent_dentry, component_name)
                                │    → d_lookup() in the global dentry hash table
                                │    → returns struct dentry (positive or negative)
                                │
                                └─ [dcache miss] lookup_slow(nd, &path)
                                     │   acquire directory inode lock (i_mutex)
                                     ├─ __lookup_hash() → d_alloc() (new dentry)
                                     └─ inode->i_op->lookup(dir_inode, dentry, flags)
                                          → filesystem reads directory from disk
                                          → calls d_add(dentry, inode) to populate
                                          → dentry is added to hash table (dcache)
                 │
                 ├─── PHASE 2: file opening ──────────────────────────────────
                 │
                 └─ do_open(nd, file, op)         # fs/namei.c
                      │
                      └─ vfs_open(&nd->path, file) # fs/open.c
                           │   file->f_path = nd->path (dentry + vfsmount)
                           │   file->f_inode = dentry->d_inode
                           └─ do_dentry_open(file, inode, NULL)  # fs/open.c
                                │   set file->f_op = inode->i_fop
                                │   check permissions (MAY_OPEN via security_file_open)
                                └─ inode->i_fop->open(inode, file)
                                     # filesystem-specific open handler
                                     # e.g., ext4_file_open(), tmpfs does nothing here
```

Source: https://elixir.bootlin.com/linux/v6.9/source/fs/open.c
Source: https://elixir.bootlin.com/linux/v6.9/source/fs/namei.c

### Key Points in the Call Path

**Mount boundary crossing**: When `walk_component()` steps onto a directory that has a mount on top of it, `follow_mount()` is called to switch `nd->path.mnt` to the child mount's `vfsmount` and `nd->path.dentry` to the child mount's root dentry. This is how `/proc`, `/sys`, and container-layer OverlayFS mounts are transparently entered during path walks.

**Symlink following**: `walk_component()` checks if the resolved dentry is a symlink. If so, and if `O_NOFOLLOW` is not set, it calls `pick_link()` which pushes the symlink target onto `nd`'s link stack and restarts the component walk. The kernel enforces a maximum symlink follow depth (`MAXSYMLINKS = 40`) to prevent loops.

**`do_dentry_open()` — The Critical Path**: This function wires together the `struct file` with the filesystem. It calls `security_file_open()` (LSM hook — SELinux, AppArmor, and eBPF LSM run here), sets `f_op` from `i_fop`, and then calls `f_op->open()`. For character devices, `chrdev_open()` replaces `f_op` with the device driver's file operations at this point.

---

## Live Observation

### Inspecting Open Files

```bash
# List all open file descriptors for a process (fd → target symlinks)
ls -la /proc/$PID/fd/

# Per-fd details: pos (file offset), flags (O_ flags), mount_id, ino
cat /proc/$PID/fdinfo/3

# Which files does a container's init process have open?
PID=$(docker inspect --format '{{.State.Pid}}' mycontainer)
ls -la /proc/$PID/fd/
```

### Dentry and Inode Cache Statistics

```bash
# Dentry cache: nr_dentry total, nr_unused (evictable), want_pages (shrinker pressure)
cat /proc/sys/fs/dentry-state

# Inode cache: nr_inodes total, nr_unused (evictable)
cat /proc/sys/fs/inode-state

# Detailed memory usage of slab caches (dentry, inode_cache, ext4_inode_cache...)
cat /proc/slabinfo | grep -E "^(dentry|inode_cache|ext4_inode)"

# Or with slabtop for live view:
slabtop -o | head -20
```

### File and Inode Inspection

```bash
# Show inode number, block count, filesystem type, timestamps
stat /etc/hosts

# Show which filesystem type the inode is on
df -T /etc/hosts

# List inode numbers for all files in a directory
ls -li /etc/

# Count hard links (i_nlink) for a file
stat --format="%h" /etc/hosts

# Find all hard links to an inode (inode number from stat above)
find / -inum 123456 2>/dev/null
```

### bpftrace Probes

```bash
# Trace every open(2) syscall with filename and PID
bpftrace -e 'tracepoint:syscalls:sys_enter_openat {
    printf("open: pid=%-6d comm=%-20s file=%s\n",
           pid, comm, str(args->filename));
}'

# Trace vfs_open (after full path resolution, just before filesystem open handler)
bpftrace -e 'kprobe:vfs_open {
    printf("vfs_open: pid=%-6d comm=%-20s\n", pid, comm);
}'

# Count dcache misses (lookup_slow) per process — high counts indicate cold cache
bpftrace -e 'kprobe:lookup_slow { @[comm] = count(); }'

# Trace open() return values — catch ENOENT, EACCES, etc.
bpftrace -e 'tracepoint:syscalls:sys_exit_openat {
    if (args->ret < 0) {
        printf("open failed: pid=%-6d comm=%-16s ret=%d\n",
               pid, comm, args->ret);
    }
}'

# Measure open(2) latency (ns) per process as a histogram
bpftrace -e '
tracepoint:syscalls:sys_enter_openat { @start[tid] = nsecs; }
tracepoint:syscalls:sys_exit_openat
/@start[tid]/
{
    @latency_ns[comm] = hist(nsecs - @start[tid]);
    delete(@start[tid]);
}'

# Count inode lookup() calls per filesystem type (dcache misses that hit disk)
bpftrace -e 'kprobe:lookup_slow {
    $qs = (struct qstr *)arg0;
    printf("lookup_slow: comm=%-16s name=%s\n",
           comm, str($qs->name));
}'
```

---

## Key References

| Symbol | File | Source URL |
|--------|------|------------|
| `struct super_block` | `include/linux/fs.h` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/fs.h |
| `struct inode` | `include/linux/fs.h` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/fs.h |
| `struct dentry` | `include/linux/dcache.h` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/dcache.h |
| `struct file` | `include/linux/fs.h` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/fs.h |
| `vfs_open()` | `fs/open.c` | https://elixir.bootlin.com/linux/v6.9/source/fs/open.c |
| `do_dentry_open()` | `fs/open.c` | https://elixir.bootlin.com/linux/v6.9/source/fs/open.c |
| `do_sys_openat2()` | `fs/open.c` | https://elixir.bootlin.com/linux/v6.9/source/fs/open.c |
| `path_openat()` | `fs/namei.c` | https://elixir.bootlin.com/linux/v6.9/source/fs/namei.c |
| `link_path_walk()` | `fs/namei.c` | https://elixir.bootlin.com/linux/v6.9/source/fs/namei.c |
| `lookup_fast()` | `fs/namei.c` | https://elixir.bootlin.com/linux/v6.9/source/fs/namei.c |
| `lookup_slow()` | `fs/namei.c` | https://elixir.bootlin.com/linux/v6.9/source/fs/namei.c |
| `d_lookup()` | `fs/dcache.c` | https://elixir.bootlin.com/linux/v6.9/source/fs/dcache.c |
| `iput()` | `fs/inode.c` | https://elixir.bootlin.com/linux/v6.9/source/fs/inode.c |
