# Setup 04 — Hardware Guide: NVMe, Cooling, Power, and Network

## Why NVMe Matters for Kubernetes

The SD card in a Raspberry Pi 5 has two problems for Kubernetes: sequential write throughput (~20-40 MB/s) and, critically, random write IOPS (~500-1000 IOPS). etcd — the backing store for k3s's server state — is write-intensive. Every Kubernetes resource creation, update, and watch event writes to etcd. On an SD card, etcd fsync latency is measured in hundreds of milliseconds. Kubernetes sets a default etcd heartbeat interval of 250ms and an election timeout of 1250ms. If etcd fsync regularly takes 400ms, the cluster considers itself unhealthy and the control plane becomes unstable.

The RPi5 has a PCIe 2.0 x1 interface, usable via an M.2 HAT. An NVMe SSD on that interface delivers:
- Sequential write: 400-600 MB/s
- Random 4KB write IOPS: 50,000-100,000
- fsync latency: 0.1-0.5ms

etcd fsync on an NVMe SSD is 100-1000× faster than on an SD card. The control plane operates within its timing parameters and the cluster is stable under load.

## 1. NVMe HAT Options

| HAT | PCIe | Interface | Price | Notes |
|-----|------|-----------|-------|-------|
| Pimoroni NVMe Base | 2.0 x1 | M.2 2242/2280 | ~$20 | Solid build, flush mount |
| Pineboards HatDrive! Bottom | 2.0 x1 | M.2 2242/2280 | ~$25 | Supports 2242 and 2280 |
| Official RPi M.2 HAT+ | 2.0 x1 | M.2 2230/2242 | ~$15 | Only 2230/2242 (shorter) |

Any M.2 2280 NVMe SSD rated for 50,000+ random write IOPS works. Recommended budget options:
- WD Blue SN570 256GB (~$25-30)
- Kingston NV3 256GB (~$20-25)
- Samsung 980 256GB (~$35)

## 2. NVMe Installation and Configuration

```bash
# After physical installation:
# Enable PCIe in /boot/firmware/config.txt (RPi5 requires explicit PCIe enable)
sudo bash -c 'cat >> /boot/firmware/config.txt << "EOF"

# Enable PCIe for NVMe HAT
dtparam=pciex1
# Force PCIe Gen 2 speed (more stable than Gen 3 on some HATs)
dtparam=pciex1_gen=2
EOF'

sudo reboot

# After reboot: verify NVMe is detected
lsblk
# NAME         MAJ:MIN RM   SIZE RO TYPE MOUNTPOINT
# nvme0n1      259:0    0 238.5G  0 disk
# mmcblk0      179:0    0  29.7G  0 disk /

# Verify PCIe speed
sudo dmesg | grep -i pci
# pcie0000:00: PCIe Gen2 link detected

# Benchmark NVMe (compare to SD card baseline):
# Random 4KB writes (etcd-relevant):
sudo fio --filename=/dev/nvme0n1 --direct=1 --rw=randwrite --bs=4k \
  --ioengine=libaio --iodepth=1 --numjobs=1 --runtime=10 \
  --group_reporting --name=randwrite-test
# Expected NVMe: 50,000+ IOPS, 0.02ms latency
# Expected SD card: 500-2000 IOPS, 0.5-2ms latency

# Benchmark for fsync (most relevant for etcd):
sudo fio --filename=/dev/nvme0n1 --direct=1 --rw=write --bs=4k \
  --ioengine=sync --fsync=1 --numjobs=1 --runtime=10 \
  --name=fsync-test
```

## 3. Mount NVMe for k3s Data

```bash
# Partition and format NVMe
sudo parted /dev/nvme0n1 mklabel gpt
sudo parted /dev/nvme0n1 mkpart primary ext4 0% 100%
sudo mkfs.ext4 /dev/nvme0n1p1 -L k3s-data

# Get UUID for stable fstab entry
NVME_UUID=$(sudo blkid /dev/nvme0n1p1 -s UUID -o value)

# Add to fstab
echo "UUID=${NVME_UUID} /var/lib/rancher ext4 defaults,noatime 0 2" | \
  sudo tee -a /etc/fstab

# Create mount point and mount
sudo mkdir -p /var/lib/rancher
sudo mount -a
df -h /var/lib/rancher
# Filesystem      Size  Used Avail Use% Mounted on
# /dev/nvme0n1p1  235G  0    235G  0%   /var/lib/rancher
```

Now reinstall k3s after mounting (k3s data will land on NVMe):

```bash
# If k3s was installed before NVMe setup, uninstall and reinstall:
sudo /usr/local/bin/k3s-uninstall.sh 2>/dev/null || true

# Reinstall with NVMe backing k3s data directory
curl -sfL https://get.k3s.io | INSTALL_K3S_EXEC="server \
  --node-ip=192.168.1.101 \
  --advertise-address=192.168.1.101 \
  --write-kubeconfig-mode=644 \
  --disable=traefik" sh -
```

## 4. Cooling

The Cortex-A76 in BCM2712 throttles at 85°C. Under sustained Kubernetes load (multi-pod scheduling, eBPF JIT compilation, disk-intensive workloads), the RPi5 without active cooling can reach thermal throttle in 5-10 minutes.

Recommended cases with cooling:
| Option | Cooling | Price | Notes |
|--------|---------|-------|-------|
| Argon NEO 5 | Passive heatsink + case | ~$15 | Good for light workloads |
| Argon ONE V3 | Active fan + heatsink | ~$25 | Best for sustained load |
| Pimoroni Heatsink Case | Passive aluminum | ~$10 | Minimal footprint |

Fan control (for active cooling cases):

```bash
# Check current temperature
cat /sys/class/thermal/thermal_zone0/temp
# 47000 → 47°C

# Monitor temperature during sustained load
watch -n 1 cat /sys/class/thermal/thermal_zone0/temp

# Check for throttle events (RPi5 specific):
vcgencmd measure_throttled
# 0x0: no throttling
# 0x50000: throttled (reduce workload or add cooling)

# Check current CPU frequency
cat /sys/devices/system/cpu/cpu0/cpufreq/scaling_cur_freq
# 2400000 → 2.4GHz (full speed, no throttle)
# 1800000 → 1.8GHz (throttling)
```

## 5. Power Supply

RPi5 requires 5V 5A (25W) for full-speed operation with NVMe HAT. Insufficient current causes CPU frequency throttling identical in appearance to thermal throttling.

Always use:
- Official Raspberry Pi 27W USB-C Power Supply (~$12)
- CanaKit 25W USB-C (~$12)

Avoid:
- Phone chargers (most are limited to 5V 3A = 15W)
- Laptop USB-C ports (may not negotiate 5A)
- USB hubs (add significant voltage drop)

```bash
# Detect undervoltage in dmesg:
dmesg | grep -i "voltage\|under"
# [   12.345] raspberrypi-fw-defs: Voltage too low for full performance

# Check firmware throttle register:
vcgencmd get_throttled
# bit 0 = 1: undervoltage detected (CURRENTLY)
# bit 16 = 1: undervoltage has occurred since last reboot
```

## 6. Network Switch

For two-node cluster, any Gigabit switch works. Recommended for clean cable management:
- TP-Link TL-SG105 5-port Gigabit (~$15)
- Netgear GS305 5-port Gigabit (~$20)

Connect:
- RPi5 #1 eth0 → switch port 1
- RPi5 #2 eth0 → switch port 2
- Laptop/router → switch port 3

Both RPi5s should see Gigabit link:

```bash
ethtool eth0 | grep -i speed
# Speed: 1000Mb/s

# Verify full-duplex (required for Flannel VXLAN performance)
ethtool eth0 | grep -i duplex
# Duplex: Full
```

## 7. Complete Two-Node Hardware Checklist

```
□ RPi5 #1 (8GB) + official PSU + heatsink case
□ RPi5 #2 (8GB) + official PSU + heatsink case
□ NVMe HAT × 2 (Pimoroni or Pineboards)
□ NVMe SSD 256GB × 2 (WD SN570 or equivalent)
□ MicroSD 32GB × 2 (for OS boot, k3s data on NVMe)
□ USB-C to USB-C cable × 2 (to PSUs)
□ Gigabit switch (5-port minimum)
□ Ethernet cable × 2 (Cat5e or better)
□ HDMI micro to HDMI full × 1 (for first-boot console if needed)

Total cost estimate:
  RPi5 × 2:          $160
  PSU × 2:           $24
  NVMe HAT × 2:      $40
  NVMe SSD × 2:      $50
  MicroSD × 2:       $16
  Cases × 2:         $30
  Switch:            $15
  Cables:            $10
  ─────────────────
  Total:            ~$345
```
