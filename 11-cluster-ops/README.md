# Chapter 11 — Cluster Operations

A Kubernetes node is a Linux machine. When it fails — kernel panic, lockup, OOM, or hardware error — the recovery path starts inside the kernel before Kubernetes even notices. This chapter covers the kernel mechanisms that govern node health: how panics propagate through the notification chain, how the NMI watchdog catches CPU lockups, how kdump/kexec preserves a memory snapshot for post-mortem analysis, and how Kubernetes detects and recovers from node failures. The final kube-inspect checkpoint reads kernel version, taint flags, and watchdog state to give operators a single tool for node health auditing.

## Learning Objectives

1. Understand the Linux kernel panic path: `panic()`, `die()`, `struct die_args`, and panic notifiers
2. Understand kernel taint flags and how to interpret `/proc/sys/kernel/tainted`
3. Understand NMI watchdog: softlockup vs hardlockup, PMU-triggered NMIs, `/proc/sys/kernel/nmi_watchdog`
4. Understand kexec/kdump: `kexec_load(2)`, crash kernel memory reservation, `/proc/vmcore`
5. Diagnose node failures in Kubernetes: NodeNotReady lifecycle, taint-based eviction, rolling upgrades

## Prerequisites

- Chapter 01 — Process Model (task_struct, kernel threads)
- Chapter 03 — cgroups (kubelet resource model)
- Chapter 08 — Scheduler (CPU context, watchdog kthreads)

## Reading Order

| File | Topic |
|------|-------|
| `kernel/11-a-panic.md` | kernel panic(), die notifiers, taint flags, oops handling |
| `kernel/11-b-nmi-watchdog.md` | NMI, hardlockup/softlockup watchdog, register_nmi_handler |
| `kernel/11-c-kexec.md` | kexec_load(2), kdump, crash kernel, /proc/vmcore |
| `k8s/11-k8s-connection.md` | NodeNotReady lifecycle, rolling upgrades, etcd backup, node audit |
| `exercises/kernel-health-reader/` | C: read panic, taint, watchdog sysctl state |
| `exercises/node-health-reader/` | Go: /proc/version, taint flags, /dev/kmsg OOM scan |
| `kube-inspect` checkpoint 11 | Node health audit: kernel version, taint, watchdog state |

## Kernel to K8s Bridge

```
Hardware fault / BUG() / NULL deref
    │
    ▼
die() → die_chain notifiers → oops_enter()
    │  if panic_on_oops=1 or unrecoverable:
    ▼
panic()
    │  smp_send_stop (halt other CPUs)
    │  panic_notifier_list callbacks
    │  if kexec_loaded: machine_kexec() → crash kernel boots
    ▼
/proc/vmcore (crash kernel reads previous kernel's memory)
makedumpfile → vmcore.flat → crash tool

Kubernetes:
    kubelet health check fails → NodeNotReady
    → node.kubernetes.io/not-ready taint added
    → pods evicted after tolerationSeconds
    → node rejoins: taint cleared
```
