// Package arch reports CPU architecture-specific information.
// On ARM64: exception levels, ARM PMU state, memory model, ASID width.
// On x86-64: reports the architecture and key x86 specifics.
package arch

import (
	"bufio"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

// Report holds architecture-specific information.
type Report struct {
	Arch        string // "amd64" or "arm64"
	Kernel      string // uname -r
	NodeName    string
	CPUModel    string
	CPUCores    int
	CPUFreqMHz  int
	TempCelsius int
	IsARM64     bool

	// ARM64-specific
	ARM64 *ARM64Info
}

// ARM64Info holds ARM64/RPi5-specific fields.
type ARM64Info struct {
	Implementer     string // e.g. "ARM Limited (0x41)"
	CPUPart         string // e.g. "Cortex-A76 (0xd0b)"
	CPUArchitecture string // e.g. "8"
	KVMAvailable    bool
	PageSizeBytes   int64
	EBPFJITEnabled  bool
	PMUType         string // e.g. "armv8_pmuv3"
	MemModel        string // "RVWMO (weakly ordered)"
	TTBRScheme      string
	ASIDWidth       string
}

// Gather collects architecture information for the current system.
func Gather() (*Report, error) {
	r := &Report{
		Arch:    runtime.GOARCH,
		IsARM64: runtime.GOARCH == "arm64",
	}

	// uname
	var uts syscall.Utsname
	if err := syscall.Uname(&uts); err == nil {
		r.Kernel = charsToString(uts.Release[:])
		r.NodeName = charsToString(uts.Nodename[:])
	}

	r.CPUCores = countCPUs()
	r.CPUFreqMHz = readCPUFreqMHz()
	r.TempCelsius = readTempCelsius()

	if r.IsARM64 {
		r.ARM64 = gatherARM64()
	}

	r.CPUModel = readCPUModel()
	return r, nil
}

func gatherARM64() *ARM64Info {
	a := &ARM64Info{
		MemModel:   "RVWMO (weakly ordered — barriers required for SMP correctness)",
		TTBRScheme: "TTBR0_EL1 (user 0x0000...) / TTBR1_EL1 (kernel 0xFFFF...)",
		ASIDWidth:  "16-bit (ARMv8.2-A, up to 65535 address spaces)",
		PageSizeBytes: int64(os.Getpagesize()),
	}

	// Parse /proc/cpuinfo for ARM64 fields
	f, err := os.Open("/proc/cpuinfo")
	if err == nil {
		defer f.Close()
		sc := bufio.NewScanner(f)
		var implCode, partCode uint64
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "CPU implementer"):
				val := strings.TrimSpace(strings.SplitN(line, ":", 2)[1])
				implCode, _ = strconv.ParseUint(strings.TrimPrefix(val, "0x"), 16, 64)
			case strings.HasPrefix(line, "CPU part"):
				val := strings.TrimSpace(strings.SplitN(line, ":", 2)[1])
				partCode, _ = strconv.ParseUint(strings.TrimPrefix(val, "0x"), 16, 64)
			case strings.HasPrefix(line, "CPU architecture"):
				a.CPUArchitecture = strings.TrimSpace(strings.SplitN(line, ":", 2)[1])
			}
		}

		// Decode implementer
		switch implCode {
		case 0x41:
			a.Implementer = fmt.Sprintf("ARM Limited (0x%02x)", implCode)
		case 0x42:
			a.Implementer = fmt.Sprintf("Broadcom (0x%02x)", implCode)
		case 0x51:
			a.Implementer = fmt.Sprintf("Qualcomm (0x%02x)", implCode)
		case 0x61:
			a.Implementer = fmt.Sprintf("Apple (0x%02x)", implCode)
		default:
			a.Implementer = fmt.Sprintf("Unknown (0x%02x)", implCode)
		}

		// Decode part number
		switch partCode {
		case 0xd0b:
			a.CPUPart = fmt.Sprintf("Cortex-A76 (0x%03x)", partCode)
		case 0xd08:
			a.CPUPart = fmt.Sprintf("Cortex-A72 (0x%03x)", partCode)
		case 0xd0c:
			a.CPUPart = fmt.Sprintf("Neoverse N1 (0x%03x)", partCode)
		case 0xd49:
			a.CPUPart = fmt.Sprintf("Neoverse N2 (0x%03x)", partCode)
		default:
			a.CPUPart = fmt.Sprintf("Unknown (0x%03x)", partCode)
		}
	}

	// KVM availability (ARM64 uses EL2)
	if _, err := os.Stat("/dev/kvm"); err == nil {
		a.KVMAvailable = true
	}

	// eBPF JIT
	if data, err := os.ReadFile("/proc/sys/net/core/bpf_jit_enable"); err == nil {
		val, _ := strconv.Atoi(strings.TrimSpace(string(data)))
		a.EBPFJITEnabled = val > 0
	}

	// ARM PMU type
	if data, err := os.ReadFile("/sys/bus/event_source/devices/armv8_pmuv3/type"); err == nil {
		a.PMUType = "armv8_pmuv3 (type=" + strings.TrimSpace(string(data)) + ")"
	}

	return a
}

func readCPUModel() string {
	f, err := os.Open("/proc/cpuinfo")
	if err != nil {
		return ""
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "model name") || strings.HasPrefix(line, "Model name") {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 {
				return strings.TrimSpace(parts[1])
			}
		}
		// ARM64 uses "Hardware" field
		if strings.HasPrefix(line, "Hardware") {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 {
				return strings.TrimSpace(parts[1])
			}
		}
	}
	return ""
}

func countCPUs() int {
	f, err := os.Open("/proc/cpuinfo")
	if err != nil {
		return 0
	}
	defer f.Close()

	count := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "processor") {
			count++
		}
	}
	return count
}

func readCPUFreqMHz() int {
	data, err := os.ReadFile("/sys/devices/system/cpu/cpu0/cpufreq/scaling_cur_freq")
	if err != nil {
		return 0
	}
	khz, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	return khz / 1000
}

func readTempCelsius() int {
	data, err := os.ReadFile("/sys/class/thermal/thermal_zone0/temp")
	if err != nil {
		return 0
	}
	milli, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	return milli / 1000
}

func charsToString(ca []int8) string {
	b := make([]byte, 0, len(ca))
	for _, c := range ca {
		if c == 0 {
			break
		}
		b = append(b, byte(c))
	}
	return string(b)
}

// Print renders the arch report in human-readable format.
func (r *Report) Print() {
	const sep = "══════════════════════════════════════════════════════"
	fmt.Println("CPU Architecture Status")
	fmt.Println(sep)
	fmt.Printf("Architecture       : %s\n", r.Arch)
	fmt.Printf("Kernel             : %s\n", r.Kernel)
	fmt.Printf("Node               : %s\n", r.NodeName)
	if r.CPUModel != "" {
		fmt.Printf("CPU                : %s\n", r.CPUModel)
	}
	fmt.Printf("CPU Cores          : %d\n", r.CPUCores)
	if r.CPUFreqMHz > 0 {
		fmt.Printf("CPU Frequency      : %d MHz\n", r.CPUFreqMHz)
	}
	if r.TempCelsius > 0 {
		tempIndicator := "✓"
		if r.TempCelsius >= 80 {
			tempIndicator = "🔴 CRITICAL (>80°C)"
		} else if r.TempCelsius >= 70 {
			tempIndicator = "⚠  WARNING (>70°C)"
		}
		fmt.Printf("Temperature        : %d°C  %s\n", r.TempCelsius, tempIndicator)
	}

	if r.IsARM64 && r.ARM64 != nil {
		a := r.ARM64
		fmt.Println()
		fmt.Println("ARM64 Specifics:")
		fmt.Printf("  Implementer      : %s\n", a.Implementer)
		fmt.Printf("  CPU Part         : %s\n", a.CPUPart)
		fmt.Printf("  Architecture     : ARMv%s\n", a.CPUArchitecture)
		fmt.Printf("  Page size        : %d bytes\n", a.PageSizeBytes)

		fmt.Println()
		fmt.Println("Exception Levels:")
		fmt.Printf("  EL0 (user)       : active (this process)\n")
		fmt.Printf("  EL1 (kernel)     : Linux %s\n", r.Kernel)
		if a.KVMAvailable {
			fmt.Printf("  EL2 (hypervisor) : KVM available (/dev/kvm)\n")
		} else {
			fmt.Printf("  EL2 (hypervisor) : not active\n")
		}
		fmt.Printf("  EL3 (secure mon) : TrustZone firmware\n")
		fmt.Printf("  Syscall entry    : SVC #0 → el0_svc (arch/arm64/kernel/entry.S)\n")

		fmt.Println()
		fmt.Println("Memory System:")
		fmt.Printf("  TTBR scheme      : %s\n", a.TTBRScheme)
		fmt.Printf("  ASID width       : %s\n", a.ASIDWidth)
		fmt.Printf("  Memory model     : %s\n", a.MemModel)

		fmt.Println()
		fmt.Println("Performance:")
		if a.PMUType != "" {
			fmt.Printf("  ARM PMU          : %s\n", a.PMUType)
		}
		jitStr := "disabled"
		if a.EBPFJITEnabled {
			jitStr = "enabled"
		}
		fmt.Printf("  eBPF JIT         : %s (arm64 native code generation)\n", jitStr)

		fmt.Println()
		fmt.Println("Cross-Chapter Verification (ARM64 = same as x86 for these):")
		fmt.Printf("  cgroup v2        : architecture-independent (same /sys/fs/cgroup)\n")
		fmt.Printf("  namespaces       : architecture-independent (same clone(2) flags)\n")
		fmt.Printf("  eBPF maps        : architecture-independent (same bpf(2) syscall)\n")
		fmt.Printf("  kprobes          : ARM64 uses MDSCR_EL1.SS (hardware single-step)\n")
		fmt.Printf("                     x86 uses int3 breakpoint — different impl, same API\n")
	}

	fmt.Println(sep)
}
