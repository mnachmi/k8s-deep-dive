// vcpu-inspector: detect KVM hypervisor, report steal time, balloon pages, virtio devices.
// Run from inside a KVM guest (cloud node or local VM).
// Build: go build -o vcpu-inspector ./cmd/vcpu-inspector
// Run:   sudo ./vcpu-inspector

package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type StealSample struct {
	total uint64
	steal uint64
	ts    time.Time
}

type VirtioDevice struct {
	name    string
	subsystem string
	driver  string
}

func main() {
	fmt.Println("=== vCPU Inspector — KVM Guest Analysis ===")
	fmt.Println()

	hypervisor := detectHypervisor()
	fmt.Printf("Hypervisor      : %s\n", hypervisor)
	if hypervisor == "none (bare metal)" {
		fmt.Println("Not running inside a KVM VM. Exiting.")
		fmt.Println()
		fmt.Println("Tip: run this on a cloud VM or a KVM guest created with:")
		fmt.Println("  virt-install --name test --memory 2048 --vcpus 2 --import --disk ...")
		return
	}
	fmt.Println()

	// vCPU count
	vcpus := countVCPUs()
	fmt.Printf("vCPUs           : %d\n", vcpus)

	// Steal time — two samples 2 seconds apart
	s1 := readProcStatCPU()
	time.Sleep(2 * time.Second)
	s2 := readProcStatCPU()

	stealPct := calcStealPct(s1, s2)
	stealIndicator := ""
	switch {
	case stealPct >= 10.0:
		stealIndicator = "🔴 CRITICAL — host severely overcommitted"
	case stealPct >= 5.0:
		stealIndicator = "⚠  WARNING — host overcommitted"
	case stealPct >= 1.0:
		stealIndicator = "✓  OK"
	default:
		stealIndicator = "✓  Excellent"
	}
	fmt.Printf("CPU Steal Time  : %.2f%% %s\n", stealPct, stealIndicator)
	fmt.Println()

	// Balloon driver
	balloonPages, balloonBytes := readBalloonPages()
	if balloonPages > 0 {
		totalMem := readTotalMemory()
		pct := 0.0
		if totalMem > 0 {
			pct = float64(balloonBytes) / float64(totalMem) * 100
		}
		indicator := "✓"
		if pct > 25 {
			indicator = "🔴"
		} else if pct > 10 {
			indicator = "⚠ "
		}
		fmt.Printf("Balloon Pages   : %d pages (%s MB held) %s %.1f%% of RAM\n",
			balloonPages, bytesToMiB(balloonBytes), indicator, pct)
	} else {
		fmt.Println("Balloon Pages   : 0 (balloon driver not inflated)")
	}

	// MemAvailable
	memAvail := readMemAvailable()
	fmt.Printf("MemAvailable    : %s MB\n", bytesToMiB(memAvail))
	fmt.Println()

	// virtio devices
	devices := listVirtioDevices()
	if len(devices) > 0 {
		fmt.Printf("Virtio Devices  : %d found\n", len(devices))
		for _, d := range devices {
			fmt.Printf("  %-20s driver=%-20s subsystem=%s\n", d.name, d.driver, d.subsystem)
		}
	} else {
		fmt.Println("Virtio Devices  : none detected")
	}
	fmt.Println()

	// KVM debug stats (requires root + debugfs)
	printKVMDebugStats()
}

// detectHypervisor reads CPUID hypervisor leaf via /proc/cpuinfo
// or checks /sys/hypervisor / systemd-detect-virt style via /proc/1/environ.
func detectHypervisor() string {
	// Check /proc/cpuinfo for hypervisor flag
	f, err := os.Open("/proc/cpuinfo")
	if err == nil {
		defer f.Close()
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "flags") && strings.Contains(line, "hypervisor") {
				// flags include 'hypervisor' — we're in a VM. Check vendor.
				break
			}
		}
	}

	// Check /sys/hypervisor/type (Xen) or /sys/class/dmi/id/product_name
	if data, err := os.ReadFile("/sys/class/dmi/id/product_name"); err == nil {
		name := strings.TrimSpace(string(data))
		if strings.Contains(strings.ToLower(name), "virtual") ||
			strings.Contains(strings.ToLower(name), "kvm") {
			return "KVM (via DMI: " + name + ")"
		}
	}

	// Check /proc/1/status for indication
	if data, err := os.ReadFile("/sys/class/dmi/id/sys_vendor"); err == nil {
		vendor := strings.TrimSpace(string(data))
		switch {
		case strings.Contains(vendor, "QEMU"):
			return "KVM/QEMU (" + vendor + ")"
		case strings.Contains(vendor, "VMware"):
			return "VMware (" + vendor + ")"
		case strings.Contains(vendor, "Xen"):
			return "Xen (" + vendor + ")"
		case strings.Contains(vendor, "Microsoft"):
			return "Hyper-V (" + vendor + ")"
		}
	}

	// Check /proc/cpuinfo for hypervisor flag (fallback for ARM/non-DMI)
	if f2, err := os.Open("/proc/cpuinfo"); err == nil {
		defer f2.Close()
		scanner := bufio.NewScanner(f2)
		for scanner.Scan() {
			if strings.Contains(scanner.Text(), "hypervisor") {
				return "KVM (CPUID hypervisor flag)"
			}
		}
	}

	// Check if virtio bus exists — strong indicator of KVM
	if _, err := os.Stat("/sys/bus/virtio"); err == nil {
		return "KVM (virtio bus present)"
	}

	return "none (bare metal)"
}

func countVCPUs() int {
	count := 0
	pattern := "/sys/devices/system/cpu/cpu[0-9]*"
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return 0
	}
	for _, m := range matches {
		info, err := os.Stat(m)
		if err == nil && info.IsDir() {
			count++
		}
	}
	return count
}

type cpuStat struct {
	user, nice, system, idle, iowait, irq, softirq, steal uint64
}

func readProcStatCPU() cpuStat {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return cpuStat{}
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 9 {
			continue
		}
		parse := func(s string) uint64 {
			v, _ := strconv.ParseUint(s, 10, 64)
			return v
		}
		return cpuStat{
			user:    parse(fields[1]),
			nice:    parse(fields[2]),
			system:  parse(fields[3]),
			idle:    parse(fields[4]),
			iowait:  parse(fields[5]),
			irq:     parse(fields[6]),
			softirq: parse(fields[7]),
			steal:   parse(fields[8]),
		}
	}
	return cpuStat{}
}

func calcStealPct(s1, s2 cpuStat) float64 {
	stealDelta := s2.steal - s1.steal
	total1 := s1.user + s1.nice + s1.system + s1.idle + s1.iowait + s1.irq + s1.softirq + s1.steal
	total2 := s2.user + s2.nice + s2.system + s2.idle + s2.iowait + s2.irq + s2.softirq + s2.steal
	totalDelta := total2 - total1
	if totalDelta == 0 {
		return 0
	}
	return float64(stealDelta) / float64(totalDelta) * 100.0
}

func readBalloonPages() (pages uint64, bytes uint64) {
	// virtio_balloon exposes its page count via sysfs
	// Modern kernels: /sys/bus/virtio/devices/virtioN/virtio_balloon/
	// Alternative: parse /proc/meminfo for Balloon field
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "Balloon:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				kb, _ := strconv.ParseUint(fields[1], 10, 64)
				// Balloon field is in kB
				bytes = kb * 1024
				pages = bytes / 4096
				return pages, bytes
			}
		}
	}
	return 0, 0
}

func readTotalMemory() uint64 {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "MemTotal:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				kb, _ := strconv.ParseUint(fields[1], 10, 64)
				return kb * 1024
			}
		}
	}
	return 0
}

func readMemAvailable() uint64 {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "MemAvailable:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				kb, _ := strconv.ParseUint(fields[1], 10, 64)
				return kb * 1024
			}
		}
	}
	return 0
}

func listVirtioDevices() []VirtioDevice {
	var devices []VirtioDevice
	base := "/sys/bus/virtio/devices"
	entries, err := os.ReadDir(base)
	if err != nil {
		return devices
	}

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		devPath := filepath.Join(base, e.Name())
		d := VirtioDevice{name: e.Name()}

		// Read driver
		driverLink := filepath.Join(devPath, "driver")
		if target, err := os.Readlink(driverLink); err == nil {
			d.driver = filepath.Base(target)
		}

		// Read subsystem (device class)
		if data, err := os.ReadFile(filepath.Join(devPath, "device")); err == nil {
			d.subsystem = strings.TrimSpace(string(data))
		}

		devices = append(devices, d)
	}
	return devices
}

func printKVMDebugStats() {
	debugBase := "/sys/kernel/debug/kvm"
	info, err := os.Stat(debugBase)
	if err != nil || !info.IsDir() {
		fmt.Println("KVM Debug Stats : /sys/kernel/debug/kvm not accessible")
		fmt.Println("                  (run on the KVM HOST, or mount debugfs: mount -t debugfs none /sys/kernel/debug)")
		return
	}

	stats := []struct {
		file  string
		label string
	}{
		{"exits", "Total VM exits"},
		{"io_exits", "I/O port exits"},
		{"mmio_exits", "MMIO exits"},
		{"halt_exits", "HLT exits (guest idle)"},
		{"irq_exits", "Interrupt exits"},
		{"mmu_shadow_zapped", "Shadow page invalidations"},
		{"remote_tlb_flush", "Remote TLB flushes"},
	}

	fmt.Println("KVM Debug Stats (host-side, requires debugfs):")
	for _, s := range stats {
		path := filepath.Join(debugBase, s.file)
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		val := strings.TrimSpace(string(data))
		fmt.Printf("  %-35s : %s\n", s.label, val)
	}
}

func bytesToMiB(b uint64) string {
	return fmt.Sprintf("%d", b/1024/1024)
}
