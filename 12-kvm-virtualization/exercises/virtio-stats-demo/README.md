# Exercise: virtio Stats Demo

Read virtio device statistics via sysfs from inside a KVM guest. Demonstrates the sysfs interface to virtio bus devices, network statistics, balloon state, and block I/O counters.

## What You Will See

```
virtio Device Statistics — KVM Guest
──────────────────────────────────────────────────────────

Device: virtio0
  Type   : virtio-net (ID 0x0001)
  Driver : virtio_net
  Network Interface: eth0
    RX packets          : 2847103
    TX packets          : 1923847
    RX bytes            : 3721048576
    TX bytes            : 892034892
    RX dropped          : 0
    TX dropped          : 0
    RX errors           : 0
    TX errors           : 0

Device: virtio1
  Type   : virtio-blk (ID 0x0002)
  Driver : virtio_blk
  Block Device : /dev/vda
    Read  I/Os  : 48201 (384808 sectors, 12034 ms)
    Write I/Os  : 203847 (1630776 sectors, 89203 ms)

Device: virtio2
  Type   : virtio-balloon (ID 0x0005)
  Driver : virtio_balloon
  Balloon pages held : 131072 kB (128 MB)
  MemAvailable       : 3841024 kB (3751 MB)
  Balloon/Total RAM  : 3.1%  ✓  OK

Device: virtio3
  Type   : virtio-rng (ID 0x0004)
  Driver : virtio_rng
  (no additional stats available for this device type)
```

## Build and Run

```bash
# Build
gcc -O2 -Wall -o virtio_stats virtio_stats.c

# Run (no root needed)
./virtio_stats
```

## Kernel Source Connections

The program reads from sysfs paths that the virtio bus infrastructure populates:

```
/sys/bus/virtio/devices/virtioN/
    device    → virtio device ID (0x0001=net, 0x0002=blk, 0x0005=balloon)
    driver    → symlink to bound driver (virtio_net, virtio_blk, etc.)
    features  → negotiated feature bits (VIRTIO_NET_F_*, etc.)
```

These sysfs files are created by `register_virtio_device()` in `drivers/virtio/virtio.c`. The device ID values are defined in the virtio spec; the kernel header is `include/linux/virtio_ids.h`.

The network statistics come from `/sys/class/net/<ifname>/statistics/` which is populated by the kernel's network stats infrastructure — the same counters accessible via `ip -s link show`.

| Sysfs path | Kernel source | URL |
|-----------|--------------|-----|
| `/sys/bus/virtio/devices/` | `drivers/virtio/virtio.c` | https://elixir.bootlin.com/linux/v6.9/source/drivers/virtio/virtio.c |
| `virtio_ids.h` | `include/linux/virtio_ids.h` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/virtio_ids.h |
| Network stats | `net/core/net-sysfs.c` | https://elixir.bootlin.com/linux/v6.9/source/net/core/net-sysfs.c |
| Block stats | `block/genhd.c` | https://elixir.bootlin.com/linux/v6.9/source/block/genhd.c |

## What to Observe

- **Balloon pages held:** Anything above 0 means the host is reclaiming memory from this guest. High balloon + low MemAvailable = risk of kubelet pod eviction driven by hypervisor, not pod workload.

- **Block write latency:** High `wr_ticks` relative to `wr_ios` means block I/O is slow. On KVM, virtio-blk uses the host's storage — a shared NFS or slow disk on the host causes latency that appears to be a Kubernetes persistent volume problem.

- **Network RX/TX errors > 0:** Rare in practice with virtio but can indicate virtqueue ring overflow if the guest is not polling fast enough (high interrupt latency).
