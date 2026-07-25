# namespace-inspector

A Go tool that reads `/proc/<pid>/ns/` for all running processes and shows which processes share namespace instances. Useful for understanding how Linux namespaces create isolation boundaries — and how Kubernetes uses them to group containers into pods.

## What It Demonstrates

- **Reading `/proc/<pid>/ns/` symlinks**: each file's inode is the canonical namespace identifier — two processes with the same inode for a given type are in the same namespace instance. This is how the kernel tracks namespace membership via `fs/nsfs.c`.

- **Grouping processes by shared namespaces**: by comparing inodes across all processes, we can identify container boundaries — the pause container, app containers, and sidecars that share a pod's namespaces appear as a cluster with matching net/ipc inodes.

- **The difference between namespace types**: `net` and `ipc` are shared within a pod (all containers in a pod share the same network stack and IPC resources); `mnt`, `pid`, `uts`, and `cgroup` are per-container (each container gets its own filesystem, process tree, hostname, and cgroup hierarchy).

## Build and Run

```bash
make build
# Produces: ./namespace-inspector
```

**All-processes mode:**
```bash
make run
# Or: ./namespace-inspector
```

Expected output (on a node running containers):
```
Scanned 312 processes from /proc

Shared namespace groups (2-8 members, likely container groups):
  net  inode 4026532456   3342(pause), 3401(nginx), 3445(sidecar)
  ipc  inode 4026532457   3342(pause), 3401(nginx), 3445(sidecar)
  mnt  inode 4026532460   3401(nginx), 3402(nginx-worker)
  pid  inode 4026532461   3401(nginx), 3402(nginx-worker)

Distinct namespace instances per type:
  net                     4
  mnt                     7
  pid                     5
  ipc                     4
  uts                     5
  cgroup                  6
  user                    1
  time                    1
```

The `net` and `ipc` groups show the pause container PID alongside app containers — confirming they share the pod's network and IPC namespaces.

**Single-PID mode:**
```bash
PID=1 make run-pid
# Or: ./namespace-inspector 1
```

Expected output:
```
PID 1 (systemd) namespace map:
  TYPE                    SYMLINK TARGET                    INODE
  ----------------------  --------------------------------  ------------------
  cgroup                  cgroup:[4026531835]               4026531835
  ipc                     ipc:[4026531839]                  4026531839
  mnt                     mnt:[4026531841]                  4026531841
  net                     net:[4026531992]                  4026531992
  pid                     pid:[4026531836]                  4026531836
  ...

Processes sharing namespaces with PID 1:
  net                     host namespace (247 processes)
  mnt                     host namespace (195 processes)
  ...
```

## Kernel References

| Concept | Kernel file | Link |
|---------|------------|------|
| nsfs inode creation | `fs/nsfs.c` | https://elixir.bootlin.com/linux/v6.9/source/fs/nsfs.c |
| ns_common (base type) | `include/linux/ns_common.h` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/ns_common.h |
| /proc/pid/ns/ entries | `fs/proc/base.c` | https://elixir.bootlin.com/linux/v6.9/source/fs/proc/base.c |
| nsproxy | `include/linux/nsproxy.h` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/nsproxy.h |

## Exercises

1. **Find your container namespace group**: Run `kubectl exec -it <pod> -- sleep 100 &` then use `./namespace-inspector` to find the sleep process's namespace group. You should see pause, the sleep process, and any sidecars sharing the same net inode.

2. **Add JSON output**: Modify `main.go` to accept a `--json` flag and output the namespace info as JSON. Hint: `encoding/json` + a struct with JSON tags.

3. **Filter by namespace type**: Accept a `--type net` argument and only show net namespace groups. Useful for quickly finding which containers share a network namespace.

4. **Measure namespace creation overhead**: Combine with `time` and `unshare` to measure how long namespace creation takes: `time unshare --net true`. Typical result: ~1ms per namespace.

## K8s Connection

This tool is the manual version of `kube-inspect --namespaces`. It shows the same data that kube-inspect uses to group processes by pod: the network namespace inode is the pod boundary marker. All processes with the same net namespace inode belong to the same pod network stack.

Run it on a Kubernetes node after `kubectl run nginx --image=nginx` and you'll see a group of 2 processes (pause + nginx) sharing net and ipc namespace inodes.
