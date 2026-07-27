# Chapter 13 — RPi5 Lab: ARM64, k3s, and a Real Two-Node Cluster

A Raspberry Pi 5 with 8GB RAM costs $80. It runs a 64-bit ARM Cortex-A76 CPU at 2.4GHz, a Linux 6.6+ kernel with full cgroup v2 support, eBPF with JIT compilation, and containerd as a container runtime. It can run k3s — a production-grade Kubernetes distribution — and host a real multi-node cluster when you connect a second board via Ethernet. Everything you learned in chapters 00-11 runs on this hardware. Most of it runs without modification.

The RPi5 lab serves two purposes. The first is validation: if you can run kube-inspect on a Kubernetes cluster running on ARM hardware you physically own, on a kernel you can read, in a room you can reach, then you understand the material at a level that no cloud VM can teach. The second is education: the RPi5 runs the same Linux abstractions (cgroups, namespaces, VFS, eBPF) on different silicon, and the places where the behavior differs reveal exactly what was architecture-specific in the earlier chapters and what is genuinely portable Linux.

The differences matter. ARM64 uses `el0_svc` instead of `entry_SYSCALL_64`. ARM's memory model is weakly ordered where x86 is TSO-strong — code that works on x86 without barriers can fail silently on ARM. The PMU uses system registers instead of x86 MSRs, but exposes the same `perf_event_open()` interface. KVM on ARM uses exception level 2 (EL2) instead of Intel VT-x VMCS. Understanding these differences makes you a better systems engineer because it forces you to separate the Linux abstraction (portable) from the architecture implementation (not).

The two-node expansion is where the lab becomes a real cluster. One RPi5 runs the k3s server. A second joins as an agent with one command. Pod scheduling across physical hardware, real inter-node Flannel traffic on Gigabit Ethernet, `kubectl drain` that actually evacuates workloads to a second machine — these are experiences that Kind clusters in a VM approximate but cannot replicate.

## Learning Objectives

By the end of this chapter you will be able to:

1. Install Ubuntu 24.04 on RPi5, configure cgroup v2, and run a fully functional k3s single-node cluster
2. Expand to a two-node cluster by joining a second RPi5 as a k3s agent
3. Explain the ARM64 exception model (EL0-EL3) and how `el0_svc` compares to `entry_SYSCALL_64`
4. Understand ARM's weak memory model, `dsb`/`dmb` barriers, and why x86 assumptions break on ARM
5. Use the ARM PMU via the same `perf_event_open()` interface, understand which events differ
6. Verify that every `kube-inspect` flag from chapters 01-12 works on ARM64
7. Read ARM system registers from C using the `mrs` instruction

## Chapter Contents

| File | What It Covers |
|------|---------------|
| `setup/01-os-and-kernel.md` | Ubuntu 24.04 for RPi5, cgroup v2, kernel params, SSH headless setup |
| `setup/02-k3s-single.md` | k3s single-node install, verifying all kube-inspect flags work |
| `setup/03-k3s-expand.md` | Adding RPi5 #2: static IP, token join, inter-node scheduling |
| `setup/04-hardware.md` | NVMe hat, cooling, power supply, network switch recommendations |
| `kernel/13-a-arm64-syscall.md` | `el0_svc`, exception levels, ARM64 syscall vs x86 `entry_SYSCALL_64` |
| `kernel/13-b-arm64-memory.md` | Weak memory model, `TTBR0_EL1`/`TTBR1_EL1`, ASID vs PCID |
| `kernel/13-c-arm64-pmu.md` | ARM PMU system registers, `perf_event_open` on ARM64, available events |
| `k8s/13-k8s-connection.md` | Two-node cluster topology, inter-node networking, real node drain |
| `exercises/arm64-sysinfo/` | C: read ARM system registers with `mrs`, CPU identification, cache topology |
| `exercises/cluster-join/` | Go: automate k3s node join, verify node Ready, label with hardware info |

## Hardware Shopping List

| Item | Why | Cost |
|------|-----|------|
| Raspberry Pi 5 8GB (×2) | 8GB needed for k3s + workloads | ~$80 each |
| Official RPi 5A USB-C PSU (×2) | 5V 5A; underpowered PSU causes throttling | ~$12 each |
| Pimoroni NVMe Base or Pineboards HatDrive | PCIe → NVMe; etcd needs fast fsync | ~$20 each |
| NVMe SSD 256GB (×2) | etcd + container images; SD card is too slow | ~$25 each |
| Small Gigabit switch (5-port) | Inter-node Ethernet | ~$15 |
| Ethernet cables (×2) | — | ~$5 |
| Heatsink/fan case (×2) | RPi5 thermal throttles at 85°C under sustained load | ~$10 each |

**Total for two-node cluster: ~$275**

Compare to one cloud VM with equivalent specs for a month: ~$100+. After 3 months the lab pays for itself and you own real hardware.
