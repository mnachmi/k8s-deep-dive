# Chapter 01 — The Process Model: task_struct, clone(), and Container Identity

> **Core thesis:** A container is not a virtual machine and not a special kernel
> object. It is an ordinary Linux process whose `task_struct` carries a pointer to a
> restricted set of namespaces (`nsproxy`) and a pointer to a cgroup membership set
> (`css_set`). Everything Kubernetes does to "isolate" a container maps directly to
> fields in a single 9 KB kernel data structure.

---

## Objectives

After completing this chapter you will be able to:

1. **Describe `struct task_struct` from memory** — its size, cache-line regions,
   all key field groups, and which subsystem owns each region.

2. **Trace `clone()` flags to kernel state** — know that `CLONE_NEWPID` causes
   `copy_process()` to call `copy_namespaces()` which allocates a new
   `struct pid_namespace` and wires it into the new task's `nsproxy`.

3. **Explain why PID 1 in a container is special** — inside a PID namespace the
   first process has PID 1 and receives `SIGKILL` from any orphaned child in that
   namespace; it also cannot be killed by `SIGKILL` sent from outside its PID
   namespace unless the sender is in an ancestor namespace.

4. **Follow the kubelet-to-kernel chain** — from `kubelet` calling the CRI gRPC
   endpoint on containerd, through runc invoking `clone3()` with namespace flags,
   all the way to `copy_process()` allocating a new `task_struct` from the slab
   allocator.

5. **Read `task_struct` fields live** — using `/proc/<pid>/status`, `bpftrace`
   kprobes, and `pahole` to confirm the in-memory layout on a running kernel.

---

## Prerequisites

- Complete Chapter 00 (syscall table + entry path). You need to understand:
  - How `clone3()` is dispatched via `sys_call_table[]`
  - The role of `do_syscall_64()` and `copy_process()`
  - How bpftrace attaches to kprobes

---

## Reading Order

Work through the documents in this order. Each one builds on the previous.

| Step | Document | Topic |
|------|----------|-------|
| 1 | [kernel/01-a-task-struct.md](kernel/01-a-task-struct.md) | `task_struct` deep dive: fields, lifecycle, locking, live observation |
| 2 | kernel/01-b-clone-flags.md | `clone()` flags: how CLONE_NEWPID, CLONE_NEWNET, CLONE_NEWUSER map to namespace allocation |
| 3 | kernel/01-c-pid-namespaces.md | PID namespaces: the vpid/pid distinction, pid_namespace tree, PID 1 semantics |
| 4 | k8s/01-k8s-connection.md | How kubelet calls containerd calls runc calls `clone3()` |
| 5 | exercises/clone-demo/ | Write a C program that calls `clone()` with namespace flags and observe the new task_struct fields in `/proc` |
| 6 | exercises/proc-walker/ | Write a Go program that walks `/proc` to reconstruct the process tree from `task_struct.children`/`sibling` equivalents |
| 7 | kube-inspect checkpoint 01 | Wire the proc-walker into kube-inspect to list pod processes |

---

## The Big Picture

```
kubelet (Go)
  │  CRI gRPC: RunPodSandbox()
  ▼
containerd
  │  OCI bundle prepared; calls runc via exec
  ▼
runc
  │  clone3(CLONE_NEWPID | CLONE_NEWNET | CLONE_NEWNS |
  │         CLONE_NEWUTS | CLONE_NEWIPC | CLONE_NEWCGROUP,
  │         ...)
  ▼
kernel/fork.c: sys_clone3()
  │  copy_process()
  │    ├── dup_task_struct()        alloc new task_struct + kernel stack
  │    ├── copy_namespaces()        allocate new nsproxy + namespace structs
  │    ├── copy_cgroup_ns()         attach new css_set
  │    ├── copy_mm()                COW the mm_struct
  │    └── attach_pid()             insert into PID namespace hash
  ▼
New task_struct:
  .nsproxy ──► struct nsproxy
                  .pid_ns_for_children ──► struct pid_namespace (new, contains PID 1)
                  .net_ns              ──► struct net (new, empty veth pair wired by CNI)
                  .mnt_ns              ──► struct mnt_namespace (new, OCI overlay mounted)
                  .uts_ns              ──► struct uts_namespace (new, container hostname)
                  .ipc_ns              ──► struct ipc_namespace (new, isolated SysV IPC)
                  .cgroup_ns           ──► struct cgroup_namespace (new or shared)
  .cgroups  ──► struct css_set
                  → cgroup_subsys_state per subsystem (cpu, memory, pids, ...)
                  → the cgroup path kubelet set: /kubepods/burstable/pod<uid>/...
```

This diagram is the entire chapter in one picture. The rest of the chapter explains
every node in this graph in precise detail.

---

## The kube-inspect Project — Checkpoint 01

Checkpoint 01 adds a `/proc` walker to kube-inspect that lists all processes belonging
to a given pod by walking the process tree and checking which PID namespace each process
belongs to.

After completing the exercises, run:

```bash
cd kube-inspect
go run ./cmd/kube-inspect --node
```

Expected output will list each running pod with its container processes, their PIDs
(both host-namespace and container-namespace PIDs), and the comm field read from
`/proc/<pid>/comm`.

See [kube-inspect/CHECKPOINT.md](../kube-inspect/CHECKPOINT.md) for the full checkpoint
table.

---

## Further Reading

- Bovet & Cesati, *Understanding the Linux Kernel*, Chapter 3 (Processes)
- Kerrisk, *The Linux Programming Interface*, Chapters 24–28 (Process Creation,
  Execution, Termination) and Chapter 28 (Monitoring Child Processes)
- Robert Love, *Linux Kernel Development*, Chapter 3 (Process Management)
- The kernel documentation:
  [`Documentation/admin-guide/namespaces/`](https://www.kernel.org/doc/html/latest/admin-guide/namespaces/)
- LWN: "Namespaces in operation" series by Michael Kerrisk (2013):
  https://lwn.net/Articles/531114/
