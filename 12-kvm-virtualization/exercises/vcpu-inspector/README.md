# Exercise: vCPU Inspector

Detect KVM hypervisor, read steal time from `/proc/stat`, and report virtio device inventory from inside a KVM guest.

## What You Will See

```
=== vCPU Inspector — KVM Guest Analysis ===

Hypervisor      : KVM/QEMU (QEMU Standard PC)
vCPUs           : 4
CPU Steal Time  : 3.7%  ⚠  WARNING — host overcommitted

Balloon Pages   : 131072 pages (512 MB held)  ⚠  12.5% of RAM
MemAvailable    : 3412 MB

Virtio Devices  : 4 found
  virtio0          driver=virtio_net            subsystem=0x0001
  virtio1          driver=virtio_blk            subsystem=0x0002
  virtio2          driver=virtio_balloon        subsystem=0x0005
  virtio3          driver=virtio_rng            subsystem=0x0004

KVM Debug Stats (host-side, requires debugfs):
  Total VM exits                    : 4829103
  I/O port exits                    : 102847
  MMIO exits                        : 7342
  HLT exits (guest idle)            : 4201034
  Interrupt exits                   : 515880
```

## Build and Run

```bash
# Build (inside the guest or on the host for cross-build)
go build -o vcpu-inspector .

# Run (no root needed for most features)
./vcpu-inspector

# KVM debug stats require root + debugfs mounted
sudo mount -t debugfs none /sys/kernel/debug 2>/dev/null || true
sudo ./vcpu-inspector
```

## What to Observe

**Steal time above 5%:** The host is overcommitting vCPUs. Your Kubernetes pods experience latency that looks like application slowness but is actually hypervisor scheduling. `cpu.stat nr_throttled` will show zero — this is not CFS throttle.

**Balloon pages > 0:** The host is reclaiming guest memory. Run `grep -E "MemAvailable|Balloon" /proc/meminfo` to see the live balance. High balloon + low MemAvailable = imminent kubelet evictions unrelated to pod behavior.

**virtio device count:** Normal KVM guest: net, blk, balloon, rng. If you see `virtio_net` replaced by a PCI device with `i40evf` or `ixgbevf` driver, you have an SR-IOV VF for that NIC — near-native network performance.

## Kernel Connections

| Observed behavior | Kernel mechanism | Source |
|------------------|-----------------|--------|
| Steal time in `/proc/stat` | `account_steal_time()` | `kernel/sched/cputime.c` |
| Balloon field in `/proc/meminfo` | `fill_balloon()` | `drivers/virtio/virtio_balloon.c` |
| `/sys/bus/virtio/devices/` | `register_virtio_device()` | `drivers/virtio/virtio.c` |
| KVM debug stats | `kvm_vcpu_stats_*` | `virt/kvm/kvm_main.c` |

## Go to Chapter

- `kernel/12-a-kvm-arch.md` — `struct kvm_vcpu` and why vCPU = `task_struct`
- `kernel/12-d-steal-balloon.md` — steal time and balloon driver internals
- `kernel/12-c-virtio.md` — virtio device enumeration via sysfs
