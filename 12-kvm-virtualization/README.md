# Chapter 12 — KVM: The Hypervisor Beneath Your Kubernetes Nodes

Every cloud provider Kubernetes node — every AWS EC2 instance, every GCP compute VM, every Azure virtual machine — is a KVM guest running on a Linux host. The Kubernetes cluster you operate is not running on metal. It is running inside a virtual machine, and that virtual machine is scheduled, memory-managed, and I/O-handled by a Linux kernel running on a physical server you have never seen and cannot access. Everything from chapters 00-11 happens inside a guest. The host is running the same mechanisms — cgroups, the CFS scheduler, Netfilter — to manage the virtual machines the same way the guest kernel manages containers.

This layer is invisible until it fails. When it does, the failure looks like a Kubernetes problem — high pod latency, memory pressure, intermittent timeouts — but the root cause is in the hypervisor. A CPU steal spike looks exactly like CFS throttling but has a completely different fix. A memory balloon event looks exactly like PSI memory pressure but is triggered by the host, not by the container. A live migration blackout looks exactly like a GC pause but is 100ms of complete network silence while the VM's memory pages are transferred across a physical network.

This chapter teaches the kernel mechanisms that make KVM work: the hardware virtualization extensions, the virtual machine control structures, the paravirtualized device model, and the accounting mechanisms that expose what the hypervisor is doing to the guest. By the end, you will be able to diagnose whether a Kubernetes performance problem originated inside the node or was imposed on it from below.

## Learning Objectives

By the end of this chapter you will be able to:

1. Explain the KVM architecture: `kvm.ko`, `kvm-intel.ko`/`kvm-amd.ko`, `/dev/kvm`, and the `struct kvm_vcpu` as a `task_struct`
2. Understand Intel EPT and AMD NPT — why the two-level page walk costs 24 memory accesses and when IOMMU/SR-IOV bypasses it
3. Read `virtio` packet flow from a pod's socket through `virtqueue` to the host kernel's `vhost-net` thread
4. Measure CPU steal time, memory balloon inflation, and live migration events from inside a Kubernetes node
5. Distinguish CFS throttle from steal time, PSI pressure from balloon inflation, GC pauses from live migration blackouts
6. Use `kube-inspect --virt` to expose the hypervisor layer beneath a pod

## Chapter Contents

| File | What It Covers |
|------|---------------|
| `kernel/12-a-kvm-arch.md` | KVM module architecture, VMCS, VM entry/exit, vCPU as `task_struct` |
| `kernel/12-b-ept-memory.md` | Extended Page Tables, two-level TLB, IOMMU, SR-IOV |
| `kernel/12-c-virtio.md` | `virtio` device model, `virtqueue` ring buffer, `vhost-net`, `virtio-blk` |
| `kernel/12-d-steal-balloon.md` | CPU steal accounting, memory balloon driver, live migration mechanics |
| `k8s/12-k8s-connection.md` | Steal ≠ throttle, balloon ≠ OOM, SR-IOV for CNI, instance type effects |
| `exercises/vcpu-inspector/` | Go: detect hypervisor, read steal time, inspect vCPU thread state |
| `exercises/virtio-stats-demo/` | C: read virtio device stats, queue depths, interrupt counts via sysfs |

## kube-inspect Checkpoint

```bash
sudo ./kube-inspect --virt

Virtualization:
  Hypervisor:       KVM
  Running in VM:    yes
  Physical CPUs:    64 (host)
  vCPUs assigned:   4
  Steal time (10s): 3.7%    ← host is overcommitting physical CPUs
  Balloon pages:    0        ← no active deflation right now
  Last migration:   unknown  ← check dmesg for "live migration" events

  Warning: steal > 2% — CPU performance unpredictable; do not diagnose
           CFS throttle until steal time is ruled out
```
