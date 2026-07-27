# 12-c — virtio: Paravirtualized Devices, `virtqueue`, and `vhost-net`

## The Cost of Pretending to Be Real Hardware

The most straightforward approach to device virtualization is emulation: make the guest believe it has a real Intel e1000 network card, an IDE hard disk controller, an Intel AHCI SATA controller. QEMU implements dozens of these emulated devices. The guest loads its existing driver for the emulated device, the driver issues standard hardware commands, and QEMU intercepts and translates them into host I/O operations.

The problem is cost. An emulated network card operates through MMIO register writes and PCI interrupts — both of which cause VM exits. A single network packet requires the guest driver to write to MMIO registers (VM exit: `EXIT_REASON_EPT_VIOLATION` or MMIO exit), QEMU wakes up to process the write, QEMU copies the packet data, QEMU injects a completion interrupt (VM entry), the guest ISR runs (VM exit to deliver interrupt context). Depending on the emulated device, a single packet can trigger 5–20 VM exits. At 1 Gbps line rate with 1500-byte frames, that is 83,000 packets per second, potentially one million VM exits per second — catastrophic for performance.

Rusty Russell proposed virtio in 2008 as a standard paravirtualization framework: instead of emulating real hardware, define a simple, efficient protocol that both the guest driver and the host backend understand is running in a VM. Replace MMIO register writes with shared ring buffers. Replace interrupt-per-packet with batched notifications. Replace per-packet VM exits with bulk data transfer via DMA into shared memory.

virtio was standardized by the OASIS consortium (virtio 1.0 in 2016, virtio 1.2 in 2022) and is now implemented in every major hypervisor and every major guest OS. Every cloud provider uses virtio for the network and block devices attached to KVM VMs.

## Source Locations

| File | Key Symbols | URL |
|------|-------------|-----|
| `drivers/virtio/virtio.c` | `register_virtio_driver()`, `virtio_device_probe()` | https://elixir.bootlin.com/linux/v6.9/source/drivers/virtio/virtio.c |
| `include/linux/virtio.h` | `struct virtio_device`, `struct virtqueue` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/virtio.h |
| `drivers/net/virtio_net.c` | `virtnet_probe()`, `start_xmit()`, `virtnet_poll()` | https://elixir.bootlin.com/linux/v6.9/source/drivers/net/virtio_net.c |
| `drivers/block/virtio_blk.c` | `virtblk_probe()`, `virtblk_request()` | https://elixir.bootlin.com/linux/v6.9/source/drivers/block/virtio_blk.c |
| `drivers/vhost/net.c` | `vhost_net_open()`, `handle_tx()`, `handle_rx()` | https://elixir.bootlin.com/linux/v6.9/source/drivers/vhost/net.c |

## 1. The `virtqueue` — A Lock-Free Ring Buffer in Shared Memory

The core of virtio is the `virtqueue`: a ring buffer allocated in memory that both the guest and host can access directly. No MMIO. No per-packet VM exits for data transfer. The guest writes descriptors into the ring, notifies the host with a single PCI write (one VM exit per batch, not one per packet), the host processes the descriptors, and marks them used.

```
virtqueue layout (split virtqueue format, simplified):
┌─────────────────────────────────────────────────────┐
│ Descriptor Table   [desc_num entries]               │
│   addr   len   flags  next                          │
│   (GPA)  (bytes) ...  (chain link)                  │
├─────────────────────────────────────────────────────┤
│ Available Ring     [avail_flags, avail_idx, ring[]] │
│   ring[0..N]: descriptor table indices              │
│   ← guest writes here (produces descriptors)        │
├─────────────────────────────────────────────────────┤
│ Used Ring          [used_flags, used_idx, ring[]]   │
│   ring[0..N]: {id, len} — completed descriptors     │
│   ← host writes here (consumes descriptors)         │
└─────────────────────────────────────────────────────┘
```

**Guest TX path (sending a packet):**
1. Guest allocates a descriptor pointing to the packet data (GPA + length)
2. Guest writes descriptor index to `avail_ring.ring[avail_idx % size]`
3. Guest increments `avail_ring.idx`
4. Guest writes to the `VIRTIO_PCI_QUEUE_NOTIFY` MMIO register → **one VM exit per batch**
5. Host reads the available ring, processes descriptors, sends packets
6. Host writes completed descriptor to `used_ring.ring[used_idx % size]`
7. Host sends interrupt to guest → guest ISR reads `used_ring`, frees buffers

Without batching, the notification VM exit still occurs per-packet. With batching (`VIRTIO_F_NOTIFICATION_DATA` feature), the guest can submit many packets before notifying, and the host can process many completions before interrupting — dramatically reducing VM exit rate.

## 2. `struct virtqueue` in the Kernel

```c
// include/linux/virtio.h (Linux 6.9)
struct virtqueue {
    struct list_head    list;        // linked list of all VQs on this device
    void               (*callback)(struct virtqueue *vq);  // called on completion interrupt
    const char         *name;       // "input" / "output" / "request"
    struct virtio_device *vdev;     // back-pointer to the virtio device
    unsigned int        index;      // queue index
    unsigned int        num_free;   // number of free descriptors
    unsigned int        num_max;    // max descriptor count
    bool                reset;      // queue is being reset
    void               *priv;       // internal vring state
};
```

The kernel's `virtqueue_add_sgs()` adds scatter-gather descriptors to the available ring. `virtqueue_kick()` sends the notification. `virtqueue_get_buf()` pulls completed descriptors from the used ring. The callback registered at queue creation runs in the interrupt handler when the host notifies the guest of completions.

## 3. `vhost-net`: Moving the Host Backend into the Kernel

Early QEMU virtio backends ran entirely in QEMU userspace. For the TX path: guest notifies QEMU (VM exit → context switch to QEMU), QEMU reads from the virtqueue (user memory access), QEMU calls `write()` on a TAP fd (syscall), the kernel moves the packet from QEMU's buffer to the TAP device. The packet crosses user/kernel boundaries twice and involves at least one syscall beyond the VM exit.

`vhost-net` (merged in Linux 2.6.33, 2010) moves the host virtio-net backend into the kernel. QEMU configures vhost, then a dedicated kernel thread (`vhost-N`) handles TX and RX for that virtqueue:

```
Guest TX path with vhost-net:
  guest writes to avail_ring
  guest writes to NOTIFY register → VM exit → KVM
  KVM delivers event to vhost-net kernel thread (eventfd)
  vhost-net thread: read from virtqueue descriptors (GPA via IOMMU/EPT)
  vhost-net thread: write to TAP fd → packet enters host network stack
  (zero context switches to QEMU userspace)
```

**The performance difference is substantial:** QEMU userspace backend: ~600,000 pps at saturation. vhost-net kernel backend: ~1,400,000 pps. With SR-IOV bypassing virtio entirely: wire rate (~1.5M pps for 1 Gbps).

## 4. `virtio-blk`: Block Device Path

`virtio-blk` uses the same virtqueue mechanism for block I/O:

```c
// drivers/block/virtio_blk.c — request structure
struct virtblk_req {
    struct virtio_blk_outhdr out_hdr;   // type (read/write/flush), sector
    u8                       status;    // completion status (success/failure)
    struct sg_table          sg_table;  // scatter-gather list of data pages
};
```

Guest sends: `[out_hdr descriptor] → [data page descriptors] → [status descriptor]`

The host reads the sector, fills the data pages, writes the status. The virtqueue is the I/O submission and completion ring — structurally similar to NVMe's submission/completion queue model (NVMe was designed with lessons from virtio in mind).

## 5. Pod Network Traffic Flow Through virtio

When a pod in a Kubernetes VM sends a network packet:

```
Pod process
  │ write() / sendmsg()
  ▼
Guest kernel network stack
  │ sk_buff allocation, TCP/IP processing, Netfilter
  ▼
virtio-net driver
  │ virtqueue_add_sgs() — add packet descriptor to TX virtqueue
  │ virtqueue_kick() — notify host (VM exit)
  ▼
vhost-net kernel thread (host)
  │ read packet from guest memory via EPT mapping
  │ write to TAP device fd
  ▼
Host kernel network stack
  │ TAP → bridge/routing → physical NIC
  ▼
Physical network
```

The VM exit at `virtqueue_kick()` is the performance boundary. Everything inside the guest runs at native speed. The notification crosses the VM boundary once per batch. This is why virtio achieves near-native throughput for batch-oriented workloads but adds latency for latency-sensitive single-packet workloads.

## 6. Live Observation

```bash
# From inside a KVM guest: identify virtio devices
ls /sys/bus/virtio/devices/
# → virtio0 (net), virtio1 (blk), virtio2 (balloon), virtio3 (rng)

# virtio-net statistics
cat /sys/class/net/eth0/statistics/rx_packets
cat /sys/class/net/eth0/statistics/tx_packets
ethtool -S eth0 | grep -E "queue|virtqueue"

# Check virtio feature flags negotiated with host
cat /sys/bus/virtio/devices/virtio0/features
# Bits correspond to VIRTIO_NET_F_* feature flags

# vhost-net thread on the host
ps aux | grep vhost
# → vhost-12345 — one thread per virtqueue per VM

# Measure VM exit rate due to virtio notification (from host)
cat /sys/kernel/debug/kvm/exits | head
# High io_exits count = many MMIO register writes (emulated devices)
# Lower on virtio because batching reduces per-packet exits

# perf: count VM exits from virtio-net kick (inside guest)
perf stat -e kvm:kvm_exit -- ping -c 1000 192.168.1.1 2>&1 | grep kvm_exit
```

## 7. Key Kernel References

| Symbol | File | URL |
|--------|------|-----|
| `struct virtqueue` | `include/linux/virtio.h` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/virtio.h |
| `virtqueue_add_sgs()` | `drivers/virtio/virtio_ring.c` | https://elixir.bootlin.com/linux/v6.9/source/drivers/virtio/virtio_ring.c |
| `virtnet_probe()` | `drivers/net/virtio_net.c` | https://elixir.bootlin.com/linux/v6.9/source/drivers/net/virtio_net.c |
| `vhost_net_open()` | `drivers/vhost/net.c` | https://elixir.bootlin.com/linux/v6.9/source/drivers/vhost/net.c |
| `virtblk_request()` | `drivers/block/virtio_blk.c` | https://elixir.bootlin.com/linux/v6.9/source/drivers/block/virtio_blk.c |
