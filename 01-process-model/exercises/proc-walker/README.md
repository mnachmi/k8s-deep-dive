# proc-walker

## What it demonstrates

`proc-walker` walks `/proc` to build a live picture of every process on the host, then groups them by PID namespace using the inode of `/proc/<pid>/ns/pid`. Two processes sharing the same inode are in the same namespace; a different inode means a different namespace — and almost certainly a container.

This is the kernel's own `/proc` filesystem acting as a live view of `task_struct` entries. Every numeric directory under `/proc` corresponds to a kernel `task_struct`. The namespace inode is the canonical identity used by tools like `crictl` and `kube-inspect` to correlate container PIDs with their host-visible counterparts.

## Build and run

```bash
make run
```

Expected output (host only, no containers):

```
=== PID namespace ns-inode:4026531836 (host) ===
  PID      PPID     COMM
  1        0        systemd
  2        0        kthreadd
  ...

--- Summary ---
Total processes: 312
Distinct PID namespaces: 1
```

If Docker or containerd is running with active containers, you will see additional namespace groups:

```
=== PID namespace ns-inode:4026532345 ===
  PID      PPID     COMM
  4231     4210     nginx
  ...

--- Summary ---
Total processes: 318
Distinct PID namespaces: 3
Non-host namespaces: 2 (likely containers)
```

To also print the namespace symlinks for a specific PID:

```bash
make run-pid PID=1
```

## Kernel references

- `/proc` is implemented in `fs/proc/`; each numeric PID entry is created via `proc_pid_make_inode()` in `fs/proc/base.c`.
- The namespace symlink inode comes from `ns_get_path()` in `fs/nsfs.c` — every namespace object gets a stable inode that persists as long as any process or bind-mount holds a reference.
- `task_struct` fields `nsproxy` and `pid_ns_for_children` govern which namespace a process belongs to after `clone(CLONE_NEWPID)`.

## Exercises

**(a) Filter to non-host namespaces only.**
Modify `main()` to skip the group where `nsID == hostNS`. This shows only container processes — the same view `crictl ps` presents.

**(b) Walk the PPID chain for any PID back to PID 1.**
Build a `map[int]Process` keyed by PID, then follow `.PPID` links until you reach PID 0 or a cycle. Print the ancestor chain.

**(c) Read `/proc/<pid>/cgroup` to map processes to cgroup paths.**
Each line in that file shows a cgroup controller and path. This is a preview of Chapter 03: the cgroup path is how `kubelet` identifies which pod a process belongs to (e.g., `/kubepods/burstable/pod<uid>/...`).

## Kubernetes connection

`kube-inspect` (checkpoint 01 in this repo) uses exactly this technique: it walks `/proc`, reads the PID namespace inode for each process, and groups them to find which PIDs belong to which container. `crictl inspect` and `kubectl exec` both ultimately resolve to a host PID via this same inode comparison. Understanding `/proc/<pid>/ns/pid` is the foundation for all container-to-host PID mapping.
