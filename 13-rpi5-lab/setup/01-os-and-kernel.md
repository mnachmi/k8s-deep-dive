# Setup 01 — OS, Kernel, and System Prerequisites on Raspberry Pi 5

## Why Ubuntu 24.04, Not Raspberry Pi OS

Raspberry Pi OS (formerly Raspbian) is excellent for general-purpose use and first-boot experience. For this course it has two problems. First, the default Raspberry Pi OS kernel is compiled with a non-standard config that disables or modifies several kernel features we need (cgroup v2 enabled by default, eBPF JIT always-on, full kprobe support). Second, k3s on Raspberry Pi OS requires manual cgroup configuration in `cmdline.txt` that varies by OS version and is a common source of installation failures.

Ubuntu 24.04 LTS for Raspberry Pi 5 ships with a mainline-close kernel (6.8+), cgroup v2 enabled by default, eBPF JIT enabled, and the same kernel config you would see on an Ubuntu cloud VM. Everything in this course works without kernel parameter modifications. If you later want to compare with Raspberry Pi OS, the skills transfer directly — the kernel differences are in configuration, not in the kernel concepts themselves.

## Hardware Prerequisites

- Raspberry Pi 5 (8GB model recommended; 4GB is possible but tight for k3s + workloads)
- 32GB+ microSD card (Class 10 A2 rated)
- Official RPi 5A USB-C power supply (5V 5A). Underpowered PSUs cause CPU frequency throttling that makes Kubernetes unreliable.
- USB keyboard + HDMI monitor for first boot (or configure headless before flashing)
- Ethernet cable (WiFi works but Ethernet is strongly preferred for stability)

## 1. Flash Ubuntu 24.04

```bash
# On your laptop: download Ubuntu 24.04 LTS for Raspberry Pi 5
# Download from: https://ubuntu.com/download/raspberry-pi
# Verify SHA256:
sha256sum ubuntu-24.04-preinstalled-server-arm64+raspi.img.xz
# Match against checksum on the download page

# Flash with Raspberry Pi Imager or dd:
xzcat ubuntu-24.04-preinstalled-server-arm64+raspi.img.xz | sudo dd of=/dev/sdX bs=4M status=progress
sync
```

## 2. Headless SSH Setup (Optional)

Edit the `system-boot` partition (FAT32) on the flashed card before first boot:

```bash
# On your laptop, mount the first partition of the SD card
sudo mount /dev/sdX1 /mnt

# Create/modify user-data for cloud-init to set password and enable SSH
cat << 'EOF' | sudo tee /mnt/user-data
#cloud-config
hostname: rpi5-01
users:
  - name: ubuntu
    sudo: ALL=(ALL) NOPASSWD:ALL
    shell: /bin/bash
    ssh_authorized_keys:
      - ssh-ed25519 AAAA... your-public-key-here
packages:
  - linux-tools-common
  - bpftrace
  - strace
  - htop
ssh_pwauth: false
EOF

sudo umount /mnt
```

## 3. First Boot and Verification

```bash
# SSH in after first boot (cloud-init takes ~2 minutes)
ssh ubuntu@rpi5-01.local   # or use IP from your router

# Verify kernel version (6.8+ expected)
uname -r
# → 6.8.0-1013-raspi (or similar)

uname -m
# → aarch64

# Verify ARM architecture details
cat /proc/cpuinfo | grep -E "^CPU|^Hardware|^Model"
# Hardware     : BCM2835  (compatibility name)
# Model        : Raspberry Pi 5 Model B Rev 1.0
# CPU part     : 0xd0b    (Cortex-A76)
# CPU implementer : 0x41  (ARM Ltd.)

# Verify cgroup v2 (must be enabled for k3s)
grep cgroup /proc/mounts | grep -v cgroup1
# → cgroup2 /sys/fs/cgroup cgroup2 rw,nosuid,...
# If you see "cgroup2" in the output, cgroup v2 is active.

stat -f /sys/fs/cgroup | grep Type
# → Type: 0x63677270  (cgroup2 magic number = 0x63677270)

# Alternative cgroup v2 check:
ls /sys/fs/cgroup/
# Should show: cgroup.controllers, cgroup.max.depth, memory.pressure, ...
# NOT: blkio, cpuacct, cpuset (those are cgroup v1 names)
```

## 4. Kernel Parameters (Verify, Not Modify)

Ubuntu 24.04 for RPi5 sets these by default — verify they are active:

```bash
# eBPF JIT compilation enabled
cat /proc/sys/net/core/bpf_jit_enable
# → 1 (should be 1 or 2)

# If 0, enable:
sudo sysctl -w net.core.bpf_jit_enable=1
sudo bash -c 'echo "net.core.bpf_jit_enable=1" >> /etc/sysctl.d/99-k8s.conf'

# kprobes enabled
cat /sys/kernel/debug/kprobes/enabled
# → enabled=1

# If debugfs not mounted:
sudo mount -t debugfs none /sys/kernel/debug
echo "debugfs /sys/kernel/debug debugfs defaults 0 0" | sudo tee -a /etc/fstab

# perf_event_paranoid (allow perf without root)
cat /proc/sys/kernel/perf_event_paranoid
# → 2 (default; allows only root to profile kernel)
# For development, lower to 1:
sudo sysctl -w kernel.perf_event_paranoid=1
sudo bash -c 'echo "kernel.perf_event_paranoid=1" >> /etc/sysctl.d/99-k8s.conf'
```

## 5. Install Required Tools

```bash
# Core tools for this course
sudo apt-get update
sudo apt-get install -y \
  linux-tools-$(uname -r) \  # perf for this kernel
  linux-tools-common \
  bpftrace \
  strace \
  ltrace \
  lsof \
  sysstat \
  htop \
  iotop \
  numactl \
  ethtool \
  iproute2 \
  tcpdump \
  conntrack \
  build-essential \
  golang-go \
  clang \
  libbpf-dev \
  bpfcc-tools \
  git

# Verify perf works
perf stat -e cycles,instructions -- sleep 1
# Should show cycle and instruction counts for Cortex-A76

# Verify bpftrace works (requires root for most probes)
sudo bpftrace -e 'tracepoint:syscalls:sys_enter_read { @[comm] = count(); } interval:s:3 { print(@); exit(); }'
```

## 6. Verify ARM64 Kernel Features

```bash
# ARM PMU — verify hardware performance counters work
perf stat -e L1-dcache-load-misses,cpu-cycles -- dd if=/dev/zero of=/dev/null count=100000
# Should show non-zero cache miss and cycle counts

# ARM64 syscall tracing
strace -e trace=read ls /tmp 2>&1 | head
# Syscall numbers will be ARM64 values (read = 63, write = 64, etc.)

# eBPF JIT on ARM64
# JIT support was added for ARM64 in Linux 3.18 (2014)
bpftrace --info 2>&1 | grep -i jit
# → JIT compiled

# kprobes on ARM64
# ARM64 kprobes use single-step mechanism (MDSCR_EL1.SS)
# different from x86 int3 breakpoint
sudo bpftrace -e 'kprobe:do_sys_open { printf("open: %s\n", str(arg1)); }' &
ls /tmp
kill %1

# conntrack (for Kubernetes networking verification)
sudo conntrack -L 2>/dev/null | head -5
# If not found, load module:
sudo modprobe nf_conntrack
```

## 7. Static IP Configuration (Recommended for k3s)

k3s nodes need stable IPs for the cluster token join. Set a static IP via netplan:

```yaml
# /etc/netplan/01-static.yaml
network:
  version: 2
  renderer: networkd
  ethernets:
    eth0:
      addresses:
        - 192.168.1.101/24   # choose an IP not in your DHCP range
      routes:
        - to: default
          via: 192.168.1.1
      nameservers:
        addresses:
          - 8.8.8.8
          - 1.1.1.1
```

```bash
sudo netplan apply
ip addr show eth0
# Verify the static IP is active
```

## 8. Thermal and Power Verification

```bash
# Check CPU temperature (RPi5 throttles at 85°C)
cat /sys/class/thermal/thermal_zone0/temp
# → 47000 (millidegrees Celsius = 47°C — healthy range)

# Check CPU frequency (should be 2400000 = 2.4GHz when not throttled)
cat /sys/devices/system/cpu/cpu0/cpufreq/scaling_cur_freq
# → 2400000

# Check for thermal throttle events
vcgencmd measure_throttled 2>/dev/null || \
  cat /sys/devices/platform/soc/soc:firmware/raspberrypi-hwmon/hwmon/hwmon*/in0_label 2>/dev/null
# 0x0: no throttling has occurred (good)
# 0x50000: ARM frequency capped and throttled (bad — check cooling)

# Power supply check — inadequate PSU causes undervoltage throttle
dmesg | grep -i "volt\|throttl\|power"
```

When the above checks pass, your RPi5 is ready for k3s installation. Continue to `setup/02-k3s-single.md`.
