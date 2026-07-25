# 05-k8s: VFS and Storage — Linux Kernel Mechanisms in Kubernetes

This document connects the kernel concepts from 05-a through 05-d (VFS, mounts, OverlayFS, block I/O) to how Kubernetes manages pod storage. Every volume type, image layer, and secret mount ultimately resolves to the primitives covered in those chapters.

---

## K8s Volume Architecture Overview

| K8s Volume Type | Kernel Mechanism | Mount Type | cgroup File | Persistence |
|-----------------|------------------|------------|-------------|-------------|
| `emptyDir` (default) | Anonymous pages | tmpfs mount in pod mount namespace | `memory.current` | Pod lifetime |
| `emptyDir` (medium: Memory) | tmpfs with explicit size limit | tmpfs (`-o size=N`) | `memory.current` | Pod lifetime |
| `configMap` | In-memory key-value projected files | tmpfs projected volume | N/A | Sync'd from API server |
| `secret` | In-memory key-value, mode 0400 | tmpfs mode 0400 | N/A | Sync'd from API server |
| `hostPath` | Host filesystem passthrough | Bind mount into pod namespace | N/A | Host persistent |
| PVC (block) | Block device, formatted ext4/xfs | Regular block device mount | `io.weight` / `blkio.weight` | Persistent |
| PVC (NFS) | NFS kernel client (`nfs.ko`) | NFS mount | N/A | Persistent, shared |
| Ephemeral (CSI) | CSI driver managed | Plugin-managed (bind mount) | N/A | Pod lifetime |

---

## Container Image Layers (OverlayFS)

When `containerd` pulls an image and starts a container, it assembles the image layers using the kernel's OverlayFS. This is the same `ovl_inode`, copy-up, and whiteout mechanism described in `05-c-overlayfs.md`.

### Pull to Running: Step by Step

1. **Image pull**: `containerd pull` downloads each OCI layer as a compressed tar archive and extracts it into a separate snapshot directory under `/var/lib/containerd/io.containerd.snapshotter.v1.overlayfs/snapshots/<N>/fs`.

2. **Snapshot chain**: Each layer has a parent snapshot reference. `containerd` uses the `overlayfs` snapshotter to build the layer chain. The bottom-most snapshot is the base image layer (e.g., the `FROM ubuntu:22.04` layer); each subsequent layer adds files on top.

3. **Container snapshot preparation**: For the container itself, `containerd` creates a new snapshot entry with an `upperdir` (for writes) and a `workdir` (for OverlayFS atomic operations). These are fresh, empty directories.

4. **runc mount call**: When `runc` starts the container, it constructs and issues the overlay mount:
   ```
   mount -t overlay overlay \
     -o lowerdir=<layer-N>:<layer-N-1>:...:<layer-1>,\
        upperdir=<container-upper>,\
        workdir=<container-work> \
     <merged-rootfs>
   ```
   The kernel's `ovl_get_tree()` validates the directory stack and returns a merged view.

5. **Container process root**: runc creates a new mount namespace, bind-mounts `<merged-rootfs>` as `/`, then calls `pivot_root(2)` (or `chroot(2)`) to make it the container's root filesystem.

6. **Writes trigger copy-up**: Any write to a file that exists only in `lowerdir` triggers `ovl_copy_up()` — the kernel copies the file to `upperdir` before modifying it. Reads always hit the highest layer containing the file, with no copy needed.

### Inspecting Layers on a Live Node

```bash
# Get the PID of the first running container:
CPID=$(crictl inspect $(crictl ps -q | head -1) | jq -r '.info.pid')

# See the overlay mount entry:
grep overlay /proc/$CPID/mountinfo

# Extract the full overlay option string:
grep overlay /proc/$CPID/mountinfo | grep -oP '\- overlay overlay \K.*'

# Count how many lower layers this image has:
grep overlay /proc/$CPID/mountinfo | grep -oP 'lowerdir=\K[^\s,]+' | tr ':' '\n' | wc -l
```

---

## emptyDir and tmpfs Memory Accounting

`emptyDir` volumes are in-memory filesystems backed by anonymous pages — they are mounted as `tmpfs` inside the pod's mount namespace. When the pod terminates or restarts, the kubelet unmounts the tmpfs and all data is lost.

### Memory Accounting Matters

Pages stored in an `emptyDir` tmpfs **are charged to the pod's memory cgroup** (`memory.current`). This means:
- A pod writing large amounts of data to an `emptyDir` will see its memory usage rise.
- If the pod exceeds `memory.max`, the OOM killer fires and the pod is evicted — even though the data is "on disk" from the application's perspective.
- This is the same cgroup accounting path described in Chapter 03.

### With `medium: Memory`

When `emptyDir.medium: Memory` is set, the kubelet explicitly passes a size limit to the mount:

```bash
mount -t tmpfs -o size=512Mi tmpfs \
  /var/lib/kubelet/pods/<uid>/volumes/kubernetes.io~empty-dir/<name>
```

Without `medium: Memory`, the kubelet still uses tmpfs but may omit the size limit, allowing the tmpfs to grow up to the kernel's default (50% of total RAM).

### Pod Spec

```yaml
volumes:
  - name: cache
    emptyDir:
      medium: Memory
      sizeLimit: 512Mi
```

### Verifying emptyDir Type

```bash
# Confirm the volume is mounted as tmpfs:
cat /proc/$CPID/mountinfo | grep tmpfs

# Check size and usage:
df -h /proc/$CPID/root/<mountpoint>

# Confirm memory cgroup charges:
cat /sys/fs/cgroup/kubepods/pod<uid>/<container-id>/memory.current
```

---

## ConfigMap and Secret Volumes (Atomic Writes)

Both `configMap` and `secret` volumes are projected into the pod as files on a **tmpfs** mount. The kubelet watches the Kubernetes API server for changes and re-projects the files when a ConfigMap or Secret is updated.

### Atomic Write Protocol

The kubelet never writes secret or configMap data in place. Instead, it follows the atomic-rename pattern (the same pattern shown in the inotify-demo exercise from Chapter 05):

1. Write new content to a temporary file in the same tmpfs directory.
2. Call `rename(2)` to atomically replace the target filename.
3. The container process either sees the old file or the new file — never a partial write.

On ConfigMap or Secret update, the kubelet goes further: it creates an entirely new temporary directory with the updated projection, then atomically swaps the symlink that the pod's mount point resolves through. This guarantees that a container reading multiple files from a configMap sees a consistent snapshot — either all old values or all new values.

### Secret File Permissions

Secret volume files are created with mode `0400` and owned by `root:root`. The container's service account token is a special case: it is rotated by the kubelet and projected atomically.

### Verification

```bash
# Confirm secrets are on tmpfs (not a real disk):
cat /proc/$CPID/mountinfo | grep secret

# Check file permissions (should be 0400):
stat /proc/$CPID/root/var/run/secrets/kubernetes.io/serviceaccount/token

# Verify atomic replacement: inode number changes on each rotation:
ls -i /proc/$CPID/root/var/run/secrets/kubernetes.io/serviceaccount/token
```

---

## CSI Volumes (Persistent Storage)

The **Container Storage Interface (CSI)** is the standard plug-in mechanism for persistent volumes. A CSI driver runs as a DaemonSet pod on each node and exposes a gRPC service over a Unix domain socket that the kubelet calls.

### Mount Flow

```
PVC bound → kubelet NodeStageVolume() RPC
  └─ CSI driver: attaches block device, formats if needed (mkfs.ext4), stages to global mount point
       └─ kubelet NodePublishVolume() RPC
            └─ CSI driver: bind-mounts staged path into pod's mount namespace
```

From the kernel's perspective, CSI is just orchestration: the CSI driver issues standard `mount(2)` syscalls. The block device (e.g., an EBS volume, a Ceph RBD device) is attached and formatted at the node level, then bind-mounted into the pod's mount namespace. The pod sees it as a regular filesystem directory.

### I/O Accounting for PVCs

Because PVC-backed volumes are real block devices, I/O goes through the full blk-mq path described in `05-d-block-io.md`. The `bi_blkg` field in each `struct bio` links the I/O to the pod's blkcg cgroup entry, enabling `io.weight` and `io.max` enforcement.

### Verification

```bash
# List non-overlay, non-tmpfs mounts in a container (likely CSI volumes):
cat /proc/$CPID/mountinfo | grep -v overlay | grep -v tmpfs | grep -v proc | grep -v sysfs

# Check block I/O stats for the underlying device:
iostat -x 1 /dev/<device>
```

---

## inotify and Kubernetes

The kubelet uses inotify at multiple levels through the Go `fsnotify` library, which calls `inotify_add_watch(2)` in the kernel.

**cgroup file watching**: The kubelet monitors `/sys/fs/cgroup/` for OOM kills and container state changes. When a container's cgroup is removed (because the container exited), the kubelet receives an inotify event and reconciles pod state.

**Pod directory watching**: The kubelet watches `/var/lib/kubelet/pods/` for directory creation and removal to detect pods that have been scheduled onto the node (static pods, etc.).

**Secret/ConfigMap re-projection**: Secrets and ConfigMaps are not watched via inotify on the node. Instead, the kubelet uses Kubernetes informers — long-lived HTTP watch connections to the API server — to receive change notifications. The kubelet then re-projects the volume using the atomic write protocol above.

**etcd and inotify**: Within `etcd`, the bbolt storage engine uses `mmap(2)` on its data file. Linux's `mmap`-backed page cache pages are dirtied in place; inotify is not involved at the etcd level. The Kubernetes watch API is implemented as a long-poll over HTTP/2 or HTTP/1.1.

```bash
# Check inotify watch limits:
cat /proc/sys/fs/inotify/max_user_watches   # default 65536

# Count inotify file descriptors held by the kubelet:
grep -c inotify /proc/$(pgrep kubelet)/fdinfo/* 2>/dev/null | grep -v ':0' | wc -l

# See what the kubelet is watching:
ls -la /proc/$(pgrep kubelet)/fd | grep anon_inode | head -20
```

---

## Common Storage Failure Patterns

| Symptom | Likely Cause | Diagnosis Command |
|---------|-------------|-------------------|
| Pod stuck in `ContainerCreating` | Overlay mount failed (disk full, stale mount, kernel bug) | `journalctl -u containerd`, `dmesg \| grep overlay` |
| `ENOSPC` writing inside container | Upper layer's parent disk is full | `df -h <upperdir-parent>`, `du -sh <upperdir>` |
| Secret not refreshed in container | kubelet inotify watch missed event or informer lag | Check kubelet logs: `journalctl -u kubelet`; `kubectl describe pod` |
| High node I/O wait (`wa` in `top`) | Writeback storm — dirty page flush overwhelming disk | `iostat -x 1`, `cat /proc/pressure/io`, `grep nr_dirty /proc/vmstat` |
| `emptyDir` triggers OOM kill | tmpfs pages charged to `memory.current` pushed pod over limit | Check `memory.current` vs `memory.max` for pod cgroup; consider `sizeLimit` |
| Mount namespace leak after crash | Container runtime crashed without cleanup; stale overlay mounts remain | `wc -l /proc/self/mountinfo`; look for zombie overlay entries |
| PVC mount fails at pod start | CSI driver not running, or block device not attached | `kubectl describe pvc`, `kubectl logs -n kube-system <csi-driver-pod>` |

---

## Key Kernel References

| Symbol | File | URL |
|--------|------|-----|
| `submit_bio` | `block/blk-core.c` | https://elixir.bootlin.com/linux/v6.9/source/block/blk-core.c |
| `ovl_copy_up` | `fs/overlayfs/copy_up.c` | https://elixir.bootlin.com/linux/v6.9/source/fs/overlayfs/copy_up.c |
| `do_new_mount` | `fs/namespace.c` | https://elixir.bootlin.com/linux/v6.9/source/fs/namespace.c |
| `sys_inotify_add_watch` | `fs/notify/inotify/inotify_user.c` | https://elixir.bootlin.com/linux/v6.9/source/fs/notify/inotify/inotify_user.c |
| `balance_dirty_pages_ratelimited` | `mm/page-writeback.c` | https://elixir.bootlin.com/linux/v6.9/source/mm/page-writeback.c |
| `vfs_fsync` | `fs/sync.c` | https://elixir.bootlin.com/linux/v6.9/source/fs/sync.c |
| `vfs_open` | `fs/open.c` | https://elixir.bootlin.com/linux/v6.9/source/fs/open.c |
