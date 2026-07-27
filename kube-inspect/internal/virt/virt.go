// Package virt detects KVM hypervisor environment and reports virtualization-
// related metrics: CPU steal time, memory balloon state, and virtio devices.
package virt

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Report holds KVM virtualization status for the current node.
type Report struct {
	Hypervisor    string
	IsVM          bool
	VCPUs         int
	StealPct      float64
	StealAvg1min  float64 // exponential moving average (sampled)
	BalloonPages  uint64
	BalloonBytes  uint64
	BalloonPct    float64 // balloon / total RAM
	MemAvailBytes uint64
	MemTotalBytes uint64
	VirtioDevices []VirtioDevice
	EPTEnabled    bool
	IOMMUActive   bool
	SRIOVVFs      int
}

// VirtioDevice represents a single virtio bus device.
type VirtioDevice struct {
	Name   string // e.g. "virtio0"
	Driver string // e.g. "virtio_net"
	ID     string // hex device ID e.g. "0x0001"
}

// Gather collects KVM virtualization status. Takes two /proc/stat samples
// 1 second apart to compute steal time percentage.
func Gather() (*Report, error) {
	r := &Report{}

	r.Hypervisor, r.IsVM = detectHypervisor()
	r.VCPUs = countCPUs()

	// Two-sample steal time measurement
	s1 := readCPUStat()
	time.Sleep(1 * time.Second)
	s2 := readCPUStat()
	r.StealPct = calcStealPct(s1, s2)

	// Memory balloon
	r.BalloonPages, r.BalloonBytes = readBalloon()
	r.MemTotalBytes = readMemField("MemTotal:")
	r.MemAvailBytes = readMemField("MemAvailable:")
	if r.MemTotalBytes > 0 && r.BalloonBytes > 0 {
		r.BalloonPct = float64(r.BalloonBytes) / float64(r.MemTotalBytes) * 100
	}

	// virtio devices
	r.VirtioDevices = listVirtioDevices()

	// EPT support (Intel VT-x)
	if data, err := os.ReadFile("/sys/module/kvm_intel/parameters/ept"); err == nil {
		r.EPTEnabled = strings.TrimSpace(string(data)) == "Y"
	}

	// IOMMU
	if entries, err := os.ReadDir("/sys/class/iommu"); err == nil {
		r.IOMMUActive = len(entries) > 0
	}

	return r, nil
}

func detectHypervisor() (string, bool) {
	// Check DMI product name for QEMU/KVM
	if data, err := os.ReadFile("/sys/class/dmi/id/sys_vendor"); err == nil {
		vendor := strings.TrimSpace(string(data))
		switch {
		case strings.Contains(vendor, "QEMU"):
			return "KVM/QEMU (" + vendor + ")", true
		case strings.Contains(vendor, "VMware"):
			return "VMware (" + vendor + ")", true
		case strings.Contains(vendor, "Xen"):
			return "Xen (" + vendor + ")", true
		case strings.Contains(vendor, "Microsoft"):
			return "Hyper-V (" + vendor + ")", true
		}
	}

	// virtio bus presence is a strong KVM indicator
	if _, err := os.Stat("/sys/bus/virtio"); err == nil {
		return "KVM (virtio bus detected)", true
	}

	// Check /proc/cpuinfo for hypervisor flag
	f, err := os.Open("/proc/cpuinfo")
	if err == nil {
		defer f.Close()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if strings.Contains(sc.Text(), "hypervisor") {
				return "KVM (CPUID hypervisor flag)", true
			}
		}
	}

	return "none (bare metal)", false
}

func countCPUs() int {
	matches, _ := filepath.Glob("/sys/devices/system/cpu/cpu[0-9]*")
	count := 0
	for _, m := range matches {
		if info, err := os.Stat(m); err == nil && info.IsDir() {
			count++
		}
	}
	return count
}

type cpuStat struct {
	user, nice, system, idle, iowait, irq, softirq, steal uint64
}

func readCPUStat() cpuStat {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return cpuStat{}
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 9 {
			continue
		}
		p := func(s string) uint64 {
			v, _ := strconv.ParseUint(s, 10, 64)
			return v
		}
		return cpuStat{
			user: p(fields[1]), nice: p(fields[2]), system: p(fields[3]),
			idle: p(fields[4]), iowait: p(fields[5]), irq: p(fields[6]),
			softirq: p(fields[7]), steal: p(fields[8]),
		}
	}
	return cpuStat{}
}

func calcStealPct(s1, s2 cpuStat) float64 {
	stealDelta := s2.steal - s1.steal
	t1 := s1.user + s1.nice + s1.system + s1.idle + s1.iowait + s1.irq + s1.softirq + s1.steal
	t2 := s2.user + s2.nice + s2.system + s2.idle + s2.iowait + s2.irq + s2.softirq + s2.steal
	totalDelta := t2 - t1
	if totalDelta == 0 {
		return 0
	}
	return float64(stealDelta) / float64(totalDelta) * 100.0
}

func readBalloon() (pages, bytes uint64) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "Balloon:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) >= 2 {
			kb, _ := strconv.ParseUint(fields[1], 10, 64)
			bytes = kb * 1024
			pages = bytes / 4096
			return pages, bytes
		}
	}
	return 0, 0
}

func readMemField(prefix string) uint64 {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) >= 2 {
			kb, _ := strconv.ParseUint(fields[1], 10, 64)
			return kb * 1024
		}
	}
	return 0
}

func listVirtioDevices() []VirtioDevice {
	var devs []VirtioDevice
	base := "/sys/bus/virtio/devices"
	entries, err := os.ReadDir(base)
	if err != nil {
		return devs
	}

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		d := VirtioDevice{Name: e.Name()}

		devPath := filepath.Join(base, e.Name())
		if data, err := os.ReadFile(filepath.Join(devPath, "device")); err == nil {
			d.ID = strings.TrimSpace(string(data))
		}
		driverLink := filepath.Join(devPath, "driver")
		if target, err := os.Readlink(driverLink); err == nil {
			d.Driver = filepath.Base(target)
		}
		devs = append(devs, d)
	}
	return devs
}

// Print renders the virt report in human-readable format.
func (r *Report) Print() {
	const sep = "══════════════════════════════════════════════════════"
	fmt.Println("KVM Virtualization Status")
	fmt.Println(sep)

	if !r.IsVM {
		fmt.Printf("Hypervisor         : %s\n", r.Hypervisor)
		fmt.Printf("Note               : Not running inside a VM.\n")
		fmt.Printf("                     Steal time and balloon metrics are not applicable.\n")
		fmt.Println(sep)
		return
	}

	fmt.Printf("Hypervisor         : %s\n", r.Hypervisor)
	fmt.Printf("vCPUs              : %d\n", r.VCPUs)

	// Steal time
	stealIndicator := "✓  OK"
	switch {
	case r.StealPct >= 10:
		stealIndicator = "🔴 CRITICAL"
	case r.StealPct >= 5:
		stealIndicator = "⚠  WARNING"
	}
	fmt.Printf("CPU Steal Time     : %.2f%%  %s\n", r.StealPct, stealIndicator)

	// Balloon
	balloonMiB := r.BalloonBytes / 1024 / 1024
	if r.BalloonBytes > 0 {
		bi := "⚠  WARNING"
		if r.BalloonPct > 25 {
			bi = "🔴 CRITICAL"
		}
		fmt.Printf("Balloon Pages Held : %d pages (%d MB)  %s %.1f%% of RAM\n",
			r.BalloonPages, balloonMiB, bi, r.BalloonPct)
	} else {
		fmt.Printf("Balloon Pages Held : 0 (balloon not inflated)\n")
	}
	fmt.Printf("MemAvailable       : %d MB\n", r.MemAvailBytes/1024/1024)

	// virtio devices
	if len(r.VirtioDevices) > 0 {
		fmt.Printf("Virtio Devices     : %d\n", len(r.VirtioDevices))
		for _, d := range r.VirtioDevices {
			fmt.Printf("  %-20s driver=%-22s id=%s\n", d.Name, d.Driver, d.ID)
		}
	}

	// Hardware features
	if r.EPTEnabled {
		fmt.Printf("EPT                : enabled\n")
	}
	if r.IOMMUActive {
		fmt.Printf("IOMMU              : active\n")
	}

	// Recommendations
	hasWarnings := r.StealPct >= 5 || r.BalloonPct > 10
	if hasWarnings {
		fmt.Println()
		fmt.Println("Recommendations:")
		if r.StealPct >= 5 {
			fmt.Printf("  • CPU steal %.1f%%: host is overcommitting vCPUs.\n", r.StealPct)
			fmt.Printf("    Check steal vs throttle: if cpu.stat nr_throttled=0, this is steal (not CFS).\n")
		}
		if r.BalloonPct > 10 {
			fmt.Printf("  • Balloon %.1f%%: host is reclaiming guest memory.\n", r.BalloonPct)
			fmt.Printf("    Risk: kubelet eviction trigger (MemAvailable < eviction threshold).\n")
		}
	}

	fmt.Println(sep)
}
