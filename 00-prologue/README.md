# Chapter 00 — Prologue: From Syscalls to Pods

> **Why start with syscalls?** Every container is a group of Linux processes.
> Every Linux process communicates with the kernel exclusively through system calls.
> Before you can understand why a Pod starts slowly, why a container is killed, or why
> a network packet is dropped, you must understand the boundary where user space ends
> and kernel space begins. This chapter builds that foundation.

---

## What This Chapter Covers

Kubernetes abstracts away the operating system — until something goes wrong. When a node
runs out of memory, when a container is OOM-killed, when a CNI plugin fails to plumb a
veth pair, or when seccomp blocks an unexpected syscall, you are suddenly dealing with
raw Linux kernel behaviour. Operators who have only ever seen Kubernetes from the
`kubectl` side are defenceless at that point.

This chapter makes the mental model shift explicit:

```
kubectl run nginx --image=nginx
     │
     ▼
REST call ──► API Server ──► etcd write
                                │
                                ▼
                         Scheduler assigns node
                                │
                                ▼
                         kubelet watches etcd
                                │
                                ▼
                         containerd pulls image
                                │
                                ▼
                         runc calls clone(3)  ◄── kernel boundary
                                │
                                ▼
                    New process in new namespaces
                    (that is your nginx container)
```

The bottom of that chain — `clone()`, `unshare()`, `setns()`, `pivot_root()`, `mount()` —
are ordinary Linux system calls. A container is not a virtual machine. It is a process
that the kernel treats differently because it lives inside a set of namespaces and a
cgroup hierarchy. That is it. This chapter proves it.

### Key mental model shift

| Old model (infrastructure) | New model (kernel-aware) |
|---|---|
| A container is a lightweight VM | A container is a process with a restricted view |
| Kubernetes manages machines | Kubernetes manages kernel resource boundaries |
| Pods are the unit of work | Processes are the unit of work; Pods are a scheduling hint |
| Logs come from the app | Logs come from file descriptors wired through the VFS |
| A Pod "fails" | A specific syscall returns an error code |

---

## Objectives

After completing this chapter you will be able to:

1. **List the syscalls used by `kubectl run`** — from the Go runtime's `connect()` call
   to the API server through runc's `clone3()` that creates the container process.

2. **Trace a pod creation to its first `clone()` call** — using `strace -f` and
   `bpftrace` tracepoints so you can observe the exact moment a new namespace is created.

3. **Explain why containers are not VMs** — using the kernel's own data structures:
   `struct task_struct` carries a pointer to a `struct nsproxy` (the namespace set)
   and a pointer to a `struct css_set` (the cgroup set). No hypervisor, no emulated
   hardware, no separate kernel.

4. **Read a syscall table entry** — understand how `arch/x86/entry/syscalls/syscall_64.tbl`
   maps a number to a C symbol, and how that C symbol is dispatched via `sys_call_table[]`.

5. **Describe the kernel entry path** — walk through `entry_SYSCALL_64` in assembly,
   understand the `swapgs` / register-save / `call do_syscall_64` sequence, and identify
   where seccomp filters intercept execution.

---

## Reading Order

Work through the documents in this order. Each one builds on the previous.

| Step | Document | Topic |
|------|----------|-------|
| 1 | [kernel/00-a-syscall-table.md](kernel/00-a-syscall-table.md) | The syscall table: how numbers map to kernel functions |
| 2 | [kernel/00-b-entry-path.md](kernel/00-b-entry-path.md) | `entry_SYSCALL_64`: the assembly path from userspace to C |
| 3 | [k8s/00-k8s-connection.md](k8s/00-k8s-connection.md) | How `kubectl run` becomes a `clone()` call |
| 4 | exercises/ | Hands-on: strace a pod creation, write a minimal seccomp profile |

---

## The Project — kube-inspect

Throughout the course you build **kube-inspect**, a Go tool that inspects running pods at
the kernel level. Each chapter adds one capability.

Chapter 00 establishes the CLI skeleton. No collectors are wired yet — this chapter is
about building the conceptual model you will need to write the real inspection code in
later chapters.

See [kube-inspect/CHECKPOINT.md](../kube-inspect/CHECKPOINT.md) for the checkpoint table.
Checkpoint 00 is already marked `done` — the CLI skeleton was scaffolded in Task 3.

After reading this chapter, run the tool and observe the no-op output:

```bash
cd kube-inspect
go run ./cmd/kube-inspect --help
go run ./cmd/kube-inspect --node
```

The output is empty placeholders for now. By Chapter 07 those placeholders will be
replaced by live eBPF-derived syscall counts per pod.

---

## Prerequisites

- A Linux machine (or VM) running kernel ≥ 5.15. Check with `uname -r`.
- `strace`, `perf`, and optionally `bpftrace` installed:
  ```bash
  sudo apt-get install strace linux-perf bpftrace   # Debian/Ubuntu
  sudo dnf install strace perf bpftrace             # Fedora/RHEL
  ```
- A working `kubectl` pointed at any cluster (even `kind` or `minikube`).
- Go 1.22+ for the kube-inspect exercises.

---

## Further Reading

- Linux man page: `man 2 syscall` — the generic syscall interface
- [The Linux Kernel documentation: entry_SYSCALL_64](https://www.kernel.org/doc/html/latest/x86/entry_64.html)
- Lameter, "Kernel Recipes 2016: Slab allocators" — for later chapters
- Kerrisk, *The Linux Programming Interface*, chapters 3–5 (syscall interface) and 28–30 (process creation)
