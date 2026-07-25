# mount-inspector

A Go tool that reads and displays the mount table for any Linux process by parsing `/proc/<pid>/mountinfo`. It can filter by filesystem type and report overlay mount layer counts — the same information used by container runtimes to set up container filesystems.

## What It Demonstrates

- **Parsing `/proc/<pid>/mountinfo`** line by line using the 11-field kernel format
- **`MountInfo` struct fields**: MountID, ParentID, Major:Minor device number, Root, MountPoint, MountOptions, OptionalFields, FSType, Source, SuperOptions
- **Overlay layer count**: parsing `lowerdir=<a>:<b>:<c>` from SuperOptions; counting `:` separators plus one gives the number of image layers stacked in the overlay
- **Filter by fstype**: isolating tmpfs, ext4, overlay, or any other filesystem type from a process's mount namespace
- **Mount namespaces**: every process has its own view of the filesystem; inspecting `/proc/<pid>/mountinfo` shows exactly what mounts that process can see

## Build and Run

```bash
# Build the binary
make build

# Show mounts for the current process
make run

# Show mounts for a specific PID
make run-pid PID=1

# Show only overlay mounts with layer count for PID 1234
./mount-inspector --pid 1234 --overlay

# Show only tmpfs mounts for PID 1
./mount-inspector --pid 1 --type tmpfs

# Show all mounts for the current process
./mount-inspector

# Combine with nsenter to inspect a container process
PID=$(crictl inspect <container-id> | jq .info.pid)
sudo ./mount-inspector --pid $PID --overlay
```

## mountinfo Format Explanation

Each line of `/proc/<pid>/mountinfo` has this structure:

```
<MountID> <ParentID> <Major>:<Minor> <Root> <MountPoint> <MountOptions> [optfields...] - <FSType> <Source> <SuperOptions>
```

| Field | Example | Meaning |
|---|---|---|
| MountID | `23` | Unique ID for this mount in the mount namespace |
| ParentID | `22` | MountID of the parent mount |
| Major:Minor | `0:50` | Device number (major:minor) of the backing device |
| Root | `/` | Path within the filesystem that is mounted |
| MountPoint | `/var/lib/kubelet/pods/...` | Where it is mounted in the process's VFS tree |
| MountOptions | `rw,relatime` | Per-mount options |
| OptionalFields | `shared:17` | Zero or more `tag:value` pairs (peer groups, propagation) |
| `-` | `-` | Separator terminating optional fields |
| FSType | `overlay` | Filesystem type name |
| Source | `overlay` | Source device or "none" |
| SuperOptions | `lowerdir=...` | Superblock options set at mount time |

The kernel emits this format from `show_mountinfo()` defined in:
https://elixir.bootlin.com/linux/v6.9/source/fs/proc_namespace.c

The optional fields encode mount propagation: `shared:N` means this mount is in peer group N and propagates events; `master:N` means it receives propagation from peer group N (slave mount); `unbindable` means the mount cannot be bind-mounted.

## Expected Output

Default output for a container process running an overlay-based image:

```
Mounts for PID 4821:
  ID      PARENT  MAJOR:MINOR   FSTYPE        SOURCE                    MOUNTPOINT
  1       0       8:1           ext4          /dev/sda1                 /
  18      1       0:16          tmpfs         tmpfs                     /dev
  19      18      0:17          devpts        devpts                    /dev/pts
  20      1       0:18          proc          proc                      /proc
  23      22      0:50          overlay       overlay                   /
  24      23      0:51          tmpfs         tmpfs                     /dev
  25      23      0:52          tmpfs         shm                       /dev/shm
  26      23      0:53          tmpfs         tmpfs                     /run/secrets/kubernetes.io/serviceaccount
```

Overlay output (--overlay flag) for the same container process:

```
Overlay mounts for PID 4821:
  ID      PARENT  FSTYPE        SOURCE      MOUNTPOINT            LAYERS
  23      22      overlay       overlay     /                     5
```

The LAYERS column tells you how many OCI image layers are stacked in the container's root filesystem. A fresh `ubuntu:22.04` base image typically has 1 layer; a typical application image has 3–8 layers.

## Exercises

**(a) Count mounts in a container vs the host:**

```bash
# Host mounts
./mount-inspector --pid 1 | wc -l

# Container mounts (replace <PID> with the container's init PID)
sudo ./mount-inspector --pid <PID> | wc -l
```

Containers typically have 20–40 mounts compared to 50–150 on a typical host, because each container runs in its own mount namespace with only the mounts it needs.

**(b) Parse optional fields to find shared/slave/peer-group mounts:**

Extend the tool to parse `OptionalFields` and extract peer group IDs:
- `shared:N` — this mount is in peer group N (bidirectional propagation)
- `master:N` — this mount receives propagation from peer group N (slave)
- `peer:N` — alternative notation; same peer group

Group mounts by peer group ID to see which mounts share propagation. On a Kubernetes node, bind mounts for volumes use `shared:` so that mounts created inside a container are visible to the host kubelet (needed for `hostPath` and CSI volume drivers).

**(c) Add --json output:**

Add a `--json` flag that marshals the `[]MountInfo` slice to JSON. This makes the output pipeable to `jq` for further filtering:

```bash
./mount-inspector --pid 1 --json | jq '[.[] | select(.FSType == "tmpfs") | .MountPoint]'
```

## Kernel References

| Symbol | URL |
|---|---|
| `show_mountinfo()` (emits /proc/pid/mountinfo lines) | https://elixir.bootlin.com/linux/v6.9/source/fs/proc_namespace.c |
| `struct mount` (kernel mount object) | https://elixir.bootlin.com/linux/v6.9/source/fs/mount.h |
| `struct vfsmount` (embedded in struct mount) | https://elixir.bootlin.com/linux/v6.9/source/include/linux/mount.h |
| `fs/namespace.c` (mount/umount syscall implementation) | https://elixir.bootlin.com/linux/v6.9/source/fs/namespace.c |
| `fs/overlayfs/super.c` (overlay filesystem superblock) | https://elixir.bootlin.com/linux/v6.9/source/fs/overlayfs/super.c |

## Kubernetes Connection

Every Kubernetes container gets its own mount namespace. The container runtime (containerd or CRI-O) and kubelet together create the mount table the container sees:

- **containerd** creates one overlay mount for each container's root filesystem, stacking the OCI image layers as overlay lowerdirs. The number of lowerdirs equals the number of image layers. This shows up as the single `overlay` entry on `/` in the container's mountinfo.
- **kubelet** creates `tmpfs` mounts for Secrets, ConfigMaps, and ServiceAccount tokens inside the container's mount namespace. These are the `tmpfs` entries on paths like `/run/secrets/kubernetes.io/serviceaccount` and `/var/run/secrets`.
- **CSI volume drivers** and hostPath volumes appear as bind mounts — lines where Root is not `/` or where the Source is a real device path rather than `overlay`/`tmpfs`.
- **`/proc` and `/dev`** are remounted fresh inside each container namespace; the container's `/proc` shows only the container's PID namespace view.

Running `sudo mount-inspector --pid <container-PID> --type tmpfs` on a Kubernetes node shows every Secret and ConfigMap injected into that container.
