# Chapter 02 — Namespaces

Linux namespaces are the kernel mechanism that partitions global system resources so that each partition appears to the processes inside it as a separate, isolated instance of that resource. Every container runtime — Docker, containerd, CRI-O — creates containers by calling `clone(2)` or `unshare(2)` with a combination of `CLONE_NEW*` flags, which causes the kernel to allocate fresh namespace structs and wire them into the new process's `task_struct`. Without namespaces there is no container isolation: a process can see every PID, mount, network interface, and hostname on the host. Understanding how the kernel implements, shares, and tears down namespaces is therefore the foundation for understanding everything from container startup latency to privilege-escalation CVEs.

## Learning Objectives

By the end of this chapter you will be able to:

1. Navigate `/proc/<pid>/ns/` and interpret every symlink to identify which namespace instance a process belongs to.
2. Explain every field of `struct nsproxy` (kernel/nsproxy.c), including why `struct user_namespace` is stored in `task_struct->cred->user_ns` instead of in the proxy struct.
3. Describe all 8 Linux namespace types (UTS, IPC, Mount, PID, Network, Time, Cgroup, User), their kernel structs, the resource they isolate, and the `CLONE_NEW*` flag that creates each one.
4. Trace a full container-creation event from containerd through runc, through the `clone3(2)` syscall, into `copy_process()` → `copy_namespaces()` → per-namespace `copy_*()` helpers using bpftrace.
5. Use `lsns`, `nsenter`, bpftrace probes, and `kube-inspect` checkpoints to observe live namespace state on a running Kubernetes node.

## Prerequisites

| Chapter | Topics needed |
|---------|--------------|
| [Chapter 00 — Syscall Entry Path](../00-syscall-entry/README.md) | How userspace transitions to kernel mode; `do_syscall_64`; system call table |
| [Chapter 01 — Process Model](../01-process-model/README.md) | `task_struct` layout; `clone(2)` flags; PID namespaces; `copy_process()` overview |

## Reading Order

Work through the documents in this sequence. Each one builds on the previous.

| Order | Document | What you learn |
|-------|----------|----------------|
| 1 | [kernel/02-a-nsproxy.md](kernel/02-a-nsproxy.md) | `struct nsproxy` field-by-field; lifecycle; locking; sharing model |
| 2 | [kernel/02-b-namespace-types.md](kernel/02-b-namespace-types.md) | All 8 namespace types: kernel structs, clone flags, what each isolates |
| 3 | [kernel/02-c-setns-unshare.md](kernel/02-c-setns-unshare.md) | `setns(2)` and `unshare(2)` kernel paths; CLONE_NEWTIME two-phase design |
| 4 | [k8s/02-k8s-connection.md](k8s/02-k8s-connection.md) | How containerd and runc translate Pod spec into namespace syscalls |
| 5 | [exercises/mini-unshare/](exercises/mini-unshare/) | Hands-on: write a minimal C program that replicates `unshare -muip` |
| 6 | [exercises/namespace-inspector/](exercises/namespace-inspector/) | Hands-on: inspect live namespaces with `lsns`, `nsenter`, `/proc` |
| 7 | [kube-inspect checkpoint 02](../kube-inspect/checkpoints/02/) | Automated verification that you can identify namespace boundaries in a live cluster |

## The Big Picture

The diagram below shows how the kernel connects a process to its namespaces. Every `task_struct` holds a pointer to an `nsproxy` struct that in turn holds typed pointers to the actual namespace objects. The `nsproxy` is reference-counted so sibling threads and forked children can share it without copying.

```
task_struct
  ├─ nsproxy ──┬─ uts_ns    ──► struct uts_namespace   (hostname, domainname)
  │  (shared   ├─ ipc_ns    ──► struct ipc_namespace   (SysV IPC, POSIX MQ)
  │   via      ├─ mnt_ns    ──► struct mnt_namespace   (filesystem topology)
  │   refcount)├─ pid_ns_for_children
  │            │              ──► struct pid_namespace  (PID numbering for new children)
  │            ├─ net_ns    ──► struct net              (full network stack)
  │            ├─ time_ns   ──► struct time_namespace  (clock offsets, current process)
  │            ├─ time_ns_for_children
  │            │              ──► struct time_namespace (clock offsets, new children)
  │            └─ cgroup_ns ──► struct cgroup_namespace (cgroup hierarchy view)
  │
  └─ cred
       └─ user_ns ──► struct user_namespace
                       (UID/GID mapping — NOT in nsproxy!)
```

Note: `struct user_namespace` is deliberately absent from `nsproxy`. Because user namespace membership is security-sensitive (it controls capability checks and UID mapping), the kernel keeps it in `task_struct->cred->user_ns` alongside other credential data so that credential-switching (`setresuid`, `execve`) and namespace membership are updated atomically under the same `cred` RCU mechanism.
