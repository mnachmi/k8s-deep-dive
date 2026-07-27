# 13 — ARM64 to Kubernetes: Architecture Portability and Two-Node Cluster Realities

## The Abstraction Test

Every chapter in this course described a Linux kernel mechanism and then explained how Kubernetes uses it. The connection was always: the kernel provides the primitive, Kubernetes invokes the primitive through a stable API (cgroup v2, netlink, inotify, eBPF), and the cloud provider's x86 hardware runs it. Running Kubernetes on ARM64 RPi5 hardware is a test of whether those abstractions genuinely hold — whether the Kubernetes layer is truly portable, or whether there are hidden x86 assumptions in the stack.

The answer, for chapters 01-12, is that the abstractions hold completely. `clone(2)` with `CLONE_NEWPID` creates a PID namespace on ARM64 the same way it does on x86. The cgroup v2 hierarchy under `/sys/fs/cgroup` is identical. eBPF bytecode compiles to ARM64 native code via the ARM64 JIT. The scheduler's red-black tree for vruntime exists on both architectures. `perf_event_open(2)` works — the events measured differ, but the interface is the same.

What differs is the hardware implementation underneath each abstraction — and understanding those differences is the final layer of architectural insight this course provides.

## 1. ARM64 Kernel Boot: Different Path, Same Kernel

On x86, the Linux kernel boots through a 16-bit real mode → 32-bit protected mode → 64-bit long mode transition. Multiple bootloaders (GRUB, syslinux) manage this. The boot protocol is defined by `Documentation/x86/boot.rst`.

On ARM64, the kernel boots at EL2 (hypervisor level) or EL1 (if not booting as a hypervisor guest), with the MMU off, via a handoff from the bootloader (U-Boot on RPi5) that passes a DTB (Device Tree Blob) describing the hardware. There is no real/protected mode dance.

```bash
# RPi5 boot chain:
# 1. ROM bootloader (in BCM2712 SoC)
# 2. EEPROM firmware (SD card or NVMe)
# 3. U-Boot (universal bootloader)
# 4. Linux kernel (arm64/head.S at EL2)
# 5. Device tree blob (describes BCM2712 hardware to the kernel)

# View the device tree in the running system:
dtc -I fs /proc/device-tree 2>/dev/null | head -50
# or
cat /proc/device-tree/compatible
# → raspberrypi,5-model-b brcm,bcm2712

# Compare to x86: no device tree — hardware enumerated via ACPI or PCI probing
```

## 2. Container Runtime: Same runc, ARM64 Images

Containerd on ARM64 pulls and runs `linux/arm64` container images. Docker Hub and major registries provide multi-architecture manifests — `nginx:alpine` resolves to the `linux/arm64` variant automatically.

```bash
# Verify containerd is pulling ARM64 images
sudo ctr -n k8s.io images list | head -5
# All images should show platform: linux/arm64

# Check that pods are running ARM64 binaries:
kubectl exec -it nginx-pod -- uname -m
# → aarch64

# Mixed-arch inspection (if you add an x86 image accidentally):
kubectl get pods -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.status.containerStatuses[0].image}{"\n"}{end}'
```

## 3. Flannel VXLAN on ARM64: Identical to x86

Chapter 06 covered how Flannel assigns a /24 CIDR per node and uses VXLAN (UDP 8472) to encapsulate inter-node pod traffic. This is architecture-independent — the VXLAN implementation is in the kernel's `drivers/net/vxlan.c`, which compiles cleanly on ARM64.

The one ARM64 difference: VXLAN packet processing on ARM64 without checksum offload requires CPU-based checksum computation. The Cortex-A76 is efficient at this, but for extremely high throughput (>10 Gbps) on ARM64, this can become a bottleneck — not relevant at Gigabit RPi5 speeds.

```bash
# Verify Flannel on ARM64 behaves identically:
# 1. Check VXLAN interface
ip -d link show flannel.1
# → vxlan id 1 local 192.168.1.101 dev eth0 srcport 0 0 dstport 8472

# 2. Capture inter-node pod traffic (ARM64 and x86 produce identical VXLAN encapsulation)
sudo tcpdump -i eth0 -n -v 'udp port 8472' -c 5

# 3. bpftrace for Flannel packet flow (ARM64 eBPF works identically):
sudo bpftrace -e '
  kprobe:vxlan_xmit { @vxlan_tx++ }
  kprobe:vxlan_rcv  { @vxlan_rx++ }
  interval:s:5 { print(@vxlan_tx); print(@vxlan_rx); clear(@vxlan_tx); clear(@vxlan_rx); }
'
```

## 4. kube-inspect `--arch` Flag: ARM64-Specific Information

The `--arch` flag surfaces information specific to the running CPU architecture. On ARM64 it reads system registers, exception level detection, ARM PMU state, and ASID usage:

```
kube-inspect --arch
ARM64 Architecture Status
══════════════════════════════════════════════════════
CPU Architecture     : aarch64 (ARMv8.2-A)
SoC                  : BCM2712 (Raspberry Pi 5)
CPU Part             : 0xd0b (Cortex-A76) × 4 cores
CPU Frequency        : 2400 MHz (no throttle)
Temperature          : 52°C (healthy, threshold 85°C)

Exception Levels:
  EL0 (user)         : active (this process)
  EL1 (kernel)       : Linux 6.8.0
  EL2 (hypervisor)   : KVM (available via /dev/kvm)
  EL3 (secure mon)   : TrustZone (firmware only)

Memory:
  TTBR0_EL1 scheme   : per-process (current PID 12345)
  TTBR1_EL1 scheme   : fixed kernel mapping
  ASID width         : 16-bit (65535 ASIDs)
  Page size          : 4KB
  VA range           : 48-bit (256TB)

ARM PMU:
  PMU version        : PMUv3 (ARMv8.2-A)
  Event counters     : 6 per-CPU
  Cycle counter      : PMCCNTR_EL0 (64-bit)
  User-space access  : enabled (PMUSERENR_EL0.EN=1)

Memory Model:
  Ordering           : RVWMO (weakly ordered)
  Barriers needed    : yes (vs x86 TSO — see 13-b-arm64-memory.md)
  smp_mb()           : compiled to dmb(ish)

Cross-Chapter Verification:
  cgroup v2          : ✓ (same as x86 — architecture independent)
  eBPF JIT           : ✓ arm64 JIT enabled
  kprobes            : ✓ hardware single-step (MDSCR_EL1.SS)
  perf events        : ✓ PMUv3 (CPU_CYCLES, L1D_CACHE_REFILL, ...)
══════════════════════════════════════════════════════
```

## 5. Two-Node Topology and Kubernetes Scheduling

With two nodes, Kubernetes scheduling decisions become observable in hardware:

**Node affinity and topology:**

```bash
# Label nodes by hardware (useful for real heterogeneous clusters)
kubectl label node rpi5-01 hardware=rpi5 rack=shelf-1
kubectl label node rpi5-02 hardware=rpi5 rack=shelf-1

# Schedule only on specific node:
kubectl run debug-pod --image=nginx:alpine --overrides='
{
  "spec": {
    "nodeSelector": {"kubernetes.io/hostname": "rpi5-02"}
  }
}'
kubectl get pod debug-pod -o wide
# → NODE: rpi5-02
```

**Real HA behavior — control plane on rpi5-01:**

```bash
# k3s agent (rpi5-02) will reconnect if control plane is temporarily unavailable
# Pods already running on rpi5-02 continue running during a rpi5-01 restart
# This is the behavior kubelet is designed for: autonomy during network partition

# Simulate control plane restart:
sudo systemctl restart k3s
# rpi5-02 agent loses connection, logs reconnect attempts
# Existing pods on rpi5-02 continue running (kubelet local state)
# New scheduling is blocked until rpi5-01 reconnects
```

## 6. Resource Contention on Low-Memory Nodes

An RPi5 with 8GB RAM running k3s + containerd + 4 pods uses ~3-4GB of RAM for infrastructure. This leaves 4-5GB for workloads. Kubernetes eviction behavior on resource-constrained nodes is more visible here than on cloud VMs:

```bash
# Watch PSI (pressure) on RPi5 under load
watch -n 2 'cat /proc/pressure/memory'
# some avg10=0.00 avg60=0.00 avg300=0.00 total=0
# full avg10=0.00 avg60=0.00 avg300=0.00 total=0

# Run a memory-intensive pod to trigger pressure
kubectl run memhog --image=polinux/stress --restart=Never -- \
  stress --vm 1 --vm-bytes 2G --vm-keep

# Watch PSI rise
watch -n 1 'cat /proc/pressure/memory'
# some avg10=12.3 ← memory pressure rising

# Watch kubelet eviction decisions
sudo journalctl -u k3s -f | grep -i evict
```

## 7. eBPF Performance on Cortex-A76

eBPF JIT compilation to ARM64 produces efficient machine code. Cortex-A76's wide out-of-order pipeline handles eBPF helper calls well. For most monitoring workloads (counting syscalls, tracing scheduler events, measuring latency), eBPF on RPi5 has negligible overhead (<1% CPU).

The exception: kretprobes on very-high-frequency functions (e.g., `kretprobe:vfs_read` on a disk-intensive workload) can reach 2-5% overhead on 4 cores at 2.4GHz. This is consistent with x86 behavior — kretprobes are expensive regardless of architecture.

```bash
# Measure eBPF overhead on RPi5:
# Baseline (no eBPF):
dd if=/dev/zero of=/dev/null count=1000000 bs=4k
# eBPF overhead (with kprobe on vfs_read):
sudo bpftrace -e 'kprobe:vfs_read { @++ }' &
dd if=/dev/zero of=/dev/null count=1000000 bs=4k
# Compare throughput — typically <2% difference
kill %1
```
