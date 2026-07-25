# 05-c: OverlayFS — Container Layers in the Kernel

OverlayFS is the filesystem that makes container images work. It stacks multiple
read-only directory trees (image layers) under a single writable layer and presents
a unified merged view to the container. Every `docker pull`, every OCI image, every
running container rootfs is backed by an OverlayFS mount. Understanding its kernel
implementation explains why containers start in milliseconds, why copy-on-write can
stall writes to large files, and why a deleted file in a container doesn't vanish
from the image.

---

## 1 — Source Locations

| File | Link | Contents |
|------|------|----------|
| `fs/overlayfs/` | https://elixir.bootlin.com/linux/v6.9/source/fs/overlayfs/ | OverlayFS subsystem root |
| `fs/overlayfs/ovl_entry.h` | https://elixir.bootlin.com/linux/v6.9/source/fs/overlayfs/ovl_entry.h | `struct ovl_inode`, `struct ovl_path`, `struct ovl_entry` |
| `fs/overlayfs/inode.c` | https://elixir.bootlin.com/linux/v6.9/source/fs/overlayfs/inode.c | Inode operations: `ovl_new_inode()`, `ovl_getattr()`, `ovl_setattr()` |
| `fs/overlayfs/copy_up.c` | https://elixir.bootlin.com/linux/v6.9/source/fs/overlayfs/copy_up.c | Copy-up engine: `ovl_copy_up()`, `ovl_copy_up_one()`, `ovl_copy_up_data()` |
| `fs/overlayfs/dir.c` | https://elixir.bootlin.com/linux/v6.9/source/fs/overlayfs/dir.c | Directory ops: `ovl_mkdir()`, `ovl_check_whiteout()`, `ovl_create_real()` |

---

## 2 — OverlayFS Architecture

OverlayFS merges multiple directory trees into a single coherent filesystem view.
Three or four paths are wired together at mount time:

```
Upper layer (writable):   /var/lib/containerd/.../snapshots/<id>/fs/
Lower layers (read-only): image layers, left = highest priority (shadows right)
Work dir:                 /var/lib/containerd/.../snapshots/<id>/work/
Merged (container root):  /run/containerd/io.containerd.runtime.v2.task/<id>/rootfs/
```

The actual mount command containerd passes to the kernel:

```bash
mount -t overlay overlay \
  -o lowerdir=/layer3:/layer2:/layer1,upperdir=/upper,workdir=/work \
  /merged
```

Multiple lower layers are separated by `:`. `/layer3` is checked first, then
`/layer2`, then `/layer1`. The first directory that contains a filename wins —
this is the lookup priority that makes image layers composable. Upper always
shadows all lower layers, so a file modified inside a running container
(upper layer) is always returned instead of the original image file (lower layer).

**Practical layer map for a containerd container:**

```
/layer3  ← top image layer (e.g., app binaries installed by Dockerfile RUN)
/layer2  ← intermediate layer (e.g., system packages)
/layer1  ← base image layer (e.g., Ubuntu 22.04 rootfs)
/upper   ← container's writable layer (empty at start, grows with writes)
/work    ← kernel scratch space (used for atomic renames during copy-up)
/merged  ← what the container's pid 1 sees as /
```

The `workdir` must be on the same filesystem as `upperdir` — OverlayFS uses
`rename()` to atomically commit copy-up results, which requires same-device
atomicity.

---

## 3 — struct ovl_inode

Every file accessed through an OverlayFS mount has a corresponding `struct
ovl_inode` allocated by `ovl_new_inode()`. It wraps a standard VFS `struct inode`
and adds the overlay-specific state:

```c
// fs/overlayfs/ovl_entry.h (key fields)
struct ovl_inode {
    union {
        struct ovl_dir_cache *cache;         // directory entry cache
        const char           *lowerdata_redirect;
    };
    const char     *redirect;               // redirect xattr for cross-layer renames
    u64             version;                // version counter for d_revalidate
    unsigned long   flags;                  // OVL_COPY_UP_FINISHED, OVL_WHITEOUT, OVL_INDEX...
    struct inode    vfs_inode;              // embedded VFS inode (MUST be last field)
    struct dentry  *__upperdentry;          // upper layer dentry (NULL if not copied up yet)
    struct ovl_path lowerpath;              // primary lower layer path {dentry, mnt}
    struct ovl_path *lowerstack;            // all lower paths for multi-layer files
};
```

Source: https://elixir.bootlin.com/linux/v6.9/source/fs/overlayfs/ovl_entry.h

### Field-by-Field Explanation

**`__upperdentry` — Upper Layer Dentry**

This field is `NULL` for any file that still lives exclusively in a lower
(read-only) layer. It is set atomically at the end of copy-up to point at the
newly created upper-layer dentry. The double underscore signals that callers must
use `ovl_upperdentry_dereference()` rather than accessing it directly — that
accessor handles RCU races where a concurrent copy-up may be completing on another
CPU.

**`flags` — State Bits**

Three flag bits drive the fast paths:

| Flag | Meaning |
|------|---------|
| `OVL_COPY_UP_FINISHED` | Copy-up has completed; no need to acquire the copy-up lock on write |
| `OVL_WHITEOUT` | This inode represents a deletion marker; the underlying dentry is a 0:0 character device |
| `OVL_INDEX` | The upper layer has an index entry for this inode, enabling hard-link tracking across copy-up |

Once `OVL_COPY_UP_FINISHED` is set, every subsequent write goes directly to the
upper layer without taking any lock. It is the hot path for all write operations
inside a long-running container.

**`vfs_inode` — Must Be the Last Field**

OverlayFS uses the standard kernel embedding pattern: `struct inode` is the last
field in `struct ovl_inode`, and `container_of` is used to recover the full
`ovl_inode` from a plain `struct inode *`. The VFS layer hands the filesystem a
`struct inode *`; OverlayFS casts it back with:

```c
static inline struct ovl_inode *OVL_I(struct inode *inode)
{
    return container_of(inode, struct ovl_inode, vfs_inode);
}
```

If `vfs_inode` were not the last field, `container_of` arithmetic would be wrong.
This is the same pattern used by `ext4_inode_info`, `xfs_inode`, and every other
filesystem that extends `struct inode`.

**`lowerstack` — Multi-Layer Lower Paths**

For a file that exists in more than one lower layer (e.g., `/etc/passwd` is
present in both `/layer1` and `/layer2`), `lowerstack` holds an array of `struct
ovl_path { struct dentry *dentry; struct vfsmount *mnt; }` covering every lower
layer that contains the file. The first entry (lowest index) is the topmost lower
layer — the one that wins the lookup. The array length is tracked separately in
the per-entry `numlower` field.

**`redirect` — Cross-Layer Rename Tracking**

When a file is renamed inside a container and the source and destination are in
different layers, OverlayFS must record the original path so that the lower-layer
entry can still be found. It writes the original path as an xattr on the upper
dentry: `trusted.overlay.redirect = /original/path`. The `redirect` field caches
this string in memory to avoid repeated xattr reads during lookup.

**`cache` / `lowerdata_redirect`**

For directories, `cache` points to an `ovl_dir_cache` that stores the merged
directory listing. OverlayFS builds this cache lazily on the first `readdir()`.
For regular files, the union member holds `lowerdata_redirect` — the path to the
lower data file when the upper only contains metadata (used in metacopy mode).

---

## 4 — Copy-Up Mechanism

Copy-up is the copy-on-write engine at the heart of OverlayFS. When any write
operation (write, truncate, chmod, chown, setxattr) targets a file that lives
only in a read-only lower layer, OverlayFS must copy the entire file to the upper
layer before servicing the write. The lower layer file is never modified.

```
write() to lower-layer file
  └─ ovl_write_iter()                        # fs/overlayfs/file.c
       └─ ovl_copy_up()                      # fs/overlayfs/copy_up.c
            └─ ovl_copy_up_one(parent, dentry, flags)
                 ├─ [create parent dirs in upper if needed]
                 ├─ ovl_create_real()         # create empty upper file
                 ├─ ovl_copy_up_data()        # copy file contents via vfs_iter_read/write
                 ├─ ovl_copy_xattrs()         # copy security.* and trusted.overlay.* xattrs
                 ├─ ovl_set_timestamps()      # restore atime/mtime from lower
                 └─ ovl_do_rename()           # atomic rename into final upper position
                      └─ set __upperdentry = upper dentry
```

Source: https://elixir.bootlin.com/linux/v6.9/source/fs/overlayfs/copy_up.c

### Step-by-Step Walk

1. **`ovl_create_real()`** — creates a new, empty file in the upper layer in the
   work directory (not in its final position). The work directory is the staging
   area for in-progress copy-ups.

2. **`ovl_copy_up_data()`** — reads the lower-layer file's contents in chunks
   using `vfs_iter_read()` and writes them to the work-directory file using
   `vfs_iter_write()`. This is a synchronous, blocking copy of the entire file.

3. **`ovl_copy_xattrs()`** — copies all extended attributes from lower to upper.
   This includes `security.selinux`, `security.capability`, and any
   `trusted.overlay.*` attributes already present. Without this step, security
   labels would be lost after copy-up.

4. **`ovl_set_timestamps()`** — restores `atime` and `mtime` from the lower-layer
   inode. Without this, the new upper file would show the timestamp of the copy-up
   operation rather than the file's true modification time.

5. **`ovl_do_rename()`** — atomically renames the staging file from the work
   directory into its final position in the upper layer. Because rename is atomic
   at the VFS level and both paths are on the same filesystem, no reader ever sees
   a partially-copied file. After the rename, `__upperdentry` is set and
   `OVL_COPY_UP_FINISHED` is raised.

### Performance Implication

Copy-up copies the **entire file** before the first write byte is accepted.
Modifying a 500 MB library that lives in a lower image layer forces a 500 MB
copy-up to the upper layer — even if the write only changes one byte. This is the
single largest performance pitfall in OverlayFS:

- A container that writes to many large lower-layer files on startup will have a
  measurable cold-start penalty.
- Benchmark tools that write small amounts to large files will show surprising
  throughput numbers because the first write is dominated by copy-up I/O.
- Container image design should put frequently-modified files in the topmost image
  layer to minimise copy-up distance. Files modified at container start (e.g.,
  `/etc/hostname`, `/etc/resolv.conf`) are typically bind-mounted from the host to
  bypass copy-up entirely.

Once `OVL_COPY_UP_FINISHED` is set on an inode, all subsequent writes go directly
to the upper layer without entering the copy-up path. The flag is sticky: it is
never cleared while the container is running.

---

## 5 — Whiteout Files (Deletion)

Deleting a file that exists only in a lower layer is impossible — lower layers are
read-only. OverlayFS handles deletion by creating a **whiteout** in the upper layer.

The whiteout mechanism:

1. When `unlink()` or `rmdir()` is called on a lower-layer file, OverlayFS calls
   `ovl_create_real()` to create a character device file in the upper layer at
   the same path with major:minor = `0:0`. This is the whiteout marker.

2. During directory listing (`ovl_readdir()`), OverlayFS checks each upper-layer
   entry with `ovl_check_whiteout()`. If an entry is a 0:0 character device, it
   is treated as a deletion marker for all lower layers — the corresponding name
   is excluded from the merged directory listing.

3. During path lookup, if the upper layer has a whiteout at a path, OverlayFS
   returns `ENOENT` for that path even though the file exists in a lower layer.
   The whiteout shadows the lower entry.

4. `ovl_check_whiteout()` in `fs/overlayfs/dir.c` performs the test: it checks
   `S_ISCHR(inode->i_mode) && inode->i_rdev == WHITEOUT_DEV` where
   `WHITEOUT_DEV = MKDEV(0, 0)`.

```bash
# View whiteouts in upper layer — they appear as character devices:
find /var/lib/containerd/.../snapshots/<id>/fs/ -type c 2>/dev/null
# ls output:  c 0, 0  <filename>
```

Whiteouts are why a container image that deletes many files from a base layer does
not shrink the image — the whiteouts in the upper layer add size. Tools like
`docker squash` or `crane flatten` merge layers to eliminate whiteout overhead.

---

## 6 — Opaque Directories

When upper and lower layers both contain a directory with the same name, OverlayFS
normally merges their contents: the directory listing shows entries from both. This
is correct behaviour for additive layering (adding files on top of a base layer's
directory).

However, after `rm -rf dir/ && mkdir dir/` inside a container, the upper layer has
a new empty `dir/` that must completely hide the lower layer's `dir/`. If OverlayFS
merged them, the "deleted" lower files would reappear. To prevent this, OverlayFS
marks the new upper directory **opaque**:

- **xattr**: `trusted.overlay.opaque = y` is set on the upper directory.
- **Kernel check**: `ovl_is_opaque()` reads this xattr during lookup. If the
  directory is opaque, OverlayFS does not descend into the corresponding lower
  directory when building the merged view.
- The result: the new `dir/` in upper completely replaces the lower `dir/` — no
  merging, no bleed-through from below.

Opaque directories interact with whiteouts: when an entire directory tree is
deleted and recreated, the `opaque` xattr on the root directory makes individual
whiteouts for the old contents unnecessary. The opaque marker is a single
directory-level suppression rather than per-file suppression.

```bash
# Check if a directory in the upper layer is opaque:
getfattr -n trusted.overlay.opaque /var/lib/containerd/.../snapshots/<id>/fs/etc
# trusted.overlay.opaque="y"  →  opaque (lower layer etc/ is suppressed)
```

---

## 7 — Live Observation

```bash
# See overlay mounts for a container:
CPID=$(crictl inspect $(crictl ps -q | head -1) | jq -r '.info.pid')
grep overlay /proc/$CPID/mountinfo

# Parse lower layer count:
grep overlay /proc/$CPID/mountinfo | \
  grep -oP 'lowerdir=\K[^\s,]+' | tr ':' '\n' | wc -l

# Inspect upper layer for copy-up'd files:
ls /var/lib/containerd/.../snapshots/<id>/fs/

# Find whiteouts in upper layer:
find /var/lib/containerd/.../snapshots/<id>/fs/ -type c 2>/dev/null

# Check opaque xattr on a directory:
getfattr -n trusted.overlay.opaque /var/lib/containerd/.../snapshots/<id>/fs/etc

# bpftrace: trace copy-up events
bpftrace -e 'kprobe:ovl_copy_up_one {
    printf("copy-up: pid=%d comm=%s\n", pid, comm);
}'

# bpftrace: trace overlay inode creation
bpftrace -e 'kprobe:ovl_new_inode {
    printf("ovl_inode: pid=%d comm=%s\n", pid, comm);
}'

# Count copy-ups per process during a workload:
bpftrace -e 'kprobe:ovl_copy_up_one { @[comm] = count(); }
             interval:s:5 { print(@); clear(@); }'
```

**Reading the mountinfo output for a container overlay mount:**

```
# Example line from /proc/$CPID/mountinfo
1234 1 0:45 / /rootfs rw,relatime - overlay overlay \
  rw,lowerdir=/layer3:/layer2:/layer1,upperdir=/upper/fs,workdir=/upper/work
```

Fields: `mount-id parent-id major:minor root mountpoint mount-options` then after
`-`: `fstype fsname super-options`. The `lowerdir` option enumerates every image
layer in priority order — the leftmost is checked first.

---

## 8 — Key References

| Symbol | File | Source URL |
|--------|------|------------|
| `struct ovl_inode` | `fs/overlayfs/ovl_entry.h` | https://elixir.bootlin.com/linux/v6.9/source/fs/overlayfs/ovl_entry.h |
| `ovl_new_inode()` | `fs/overlayfs/inode.c` | https://elixir.bootlin.com/linux/v6.9/source/fs/overlayfs/inode.c |
| `ovl_copy_up()` | `fs/overlayfs/copy_up.c` | https://elixir.bootlin.com/linux/v6.9/source/fs/overlayfs/copy_up.c |
| `ovl_copy_up_one()` | `fs/overlayfs/copy_up.c` | https://elixir.bootlin.com/linux/v6.9/source/fs/overlayfs/copy_up.c |
| `ovl_check_whiteout()` | `fs/overlayfs/dir.c` | https://elixir.bootlin.com/linux/v6.9/source/fs/overlayfs/dir.c |
| `ovl_is_opaque()` | `fs/overlayfs/dir.c` | https://elixir.bootlin.com/linux/v6.9/source/fs/overlayfs/dir.c |

---

**Next:** [05-d-block-io.md](05-d-block-io.md) — block layer: `struct bio`, `struct request`, and how I/O reaches the device.
