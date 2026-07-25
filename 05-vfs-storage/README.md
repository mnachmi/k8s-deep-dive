# Chapter 05 — VFS & Storage

Every `read()`, `write()`, `open()`, and `stat()` call goes through the Linux Virtual Filesystem Switch (VFS). VFS is an abstraction layer that presents a uniform interface regardless of the underlying filesystem — ext4, xfs, tmpfs, overlayfs, procfs, or kernfs all expose the same inode, dentry, and file objects to the rest of the kernel. Containers use VFS constantly and pervasively: every container image layer is an OverlayFS mount assembled from lower read-only layers and an upper read-write layer, every ConfigMap and Secret is a tmpfs mount injected into the container's filesystem tree, and every container sees a complete and independent filesystem hierarchy assembled by the kernel's mount namespace mechanism. This chapter teaches VFS from the kernel data structures up through the Kubernetes storage API.

## Learning Objectives

By the end of this chapter you will be able to:

1. Navigate `/proc/<pid>/mountinfo` and interpret every field — peer group, propagation type, root of mount, mount options — for a container process running inside its own mount namespace.
2. Explain the VFS object model: `super_block` → `inode` → `dentry` → `file`, how these four structures relate to each other, and how the vtables (`super_operations`, `inode_operations`, `file_operations`) make VFS filesystem-agnostic.
3. Trace an `open(2)` call through `do_sys_openat2()` → `path_openat()` → `link_path_walk()` → `vfs_open()` to the filesystem-specific `inode->i_fop->open()` handler, explaining what each layer does and where the dentry cache is consulted.
4. Explain how OverlayFS implements copy-on-write for container image layers: how the upper/lower/work directories are combined, when a file is copied up from a lower read-only layer to the upper layer, and how deletions are represented as whiteout entries.
5. Map Kubernetes volume types — `emptyDir`, `configMap`, `secret`, `hostPath`, and PersistentVolumeClaims — to the underlying kernel mount operations and filesystem types that the kubelet performs when starting a pod.

## Prerequisites

- **Chapter 02** — mount namespaces (`struct mnt_namespace`, `struct vfsmount`), `clone(CLONE_NEWNS)`, bind mounts, and mount propagation. VFS path resolution is always relative to the mount namespace of the calling process — you must understand how the kernel attaches a mount tree to `struct mnt_namespace` before the mount-point traversal in `path_openat()` makes sense.
- **Chapter 03** — cgroup filesystems and kernfs. The cgroup hierarchy is exposed as a pseudo-filesystem (`cgroupfs`, `cgroup2fs`) built on top of kernfs, which implements its own VFS object lifecycle. Understanding kernfs demystifies why `/sys/fs/cgroup` behaves differently from `/proc` or a block-device filesystem.

## VFS Object Hierarchy

```
open("/etc/hosts", O_RDONLY)
  │
  ▼
sys_open() → do_sys_openat2() → do_filp_open() → path_openat()
  │
  ├─ namei: walk dentry tree → struct dentry (cached name lookup)
  │          └─ struct inode (file metadata, i_op vtable)
  │
  ├─ vfs_open() → inode->i_fop->open()  [filesystem-specific handler]
  │
  └─ struct file (per-open-file-description, returned as fd)
       ├─ f_path.dentry → struct dentry
       ├─ f_inode       → struct inode
       ├─ f_op          → struct file_operations vtable
       └─ f_pos         (current file offset)

struct super_block  (one per mounted filesystem instance)
  └─ s_root → struct dentry  (root of this filesystem's dentry tree)
       └─ d_inode → struct inode  (root directory inode)
            └─ i_sb → back-pointer to super_block
```

## Reading Order

Work through the files in this order. Each document builds on the previous.

| File | Topic |
|------|-------|
| `kernel/05-a-vfs.md` | `super_block`, `inode`, `dentry`, `file`, and the `open(2)` call path through VFS |
| `kernel/05-b-mount.md` | `vfsmount`, `mount`, `mnt_namespace`; bind mounts, propagation, `mountinfo` parsing |
| `kernel/05-c-overlayfs.md` | OverlayFS internals: upper/lower/work layers, copy-up, whiteouts, redirect dir |
| `kernel/05-d-block-io.md` | Block layer: `bio`, request queues, `blk-mq`, writeback |
| `k8s/05-k8s-connection.md` | How kubelet maps Kubernetes volume types to kernel mount operations |
| `exercises/inotify-demo/` | C program using `inotify(7)` to watch filesystem events; trace `fsnotify` in kernel |
| `exercises/mount-inspector/` | Go tool that reads `/proc/<pid>/mountinfo` and renders the mount tree with propagation |
