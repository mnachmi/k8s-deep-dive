package health

import (
	"os"
	"strconv"
	"strings"
)

// TaintBit describes one active taint flag.
type TaintBit struct {
	Bit     int
	Code    string
	Meaning string
}

var taintTable = []TaintBit{
	{0, "P", "proprietary module"},
	{1, "F", "forced module load"},
	{2, "S", "SMP on non-SMP CPU"},
	{3, "R", "forced module rmmod"},
	{4, "M", "machine check error"},
	{5, "B", "bad page"},
	{6, "U", "userspace taint"},
	{7, "D", "kernel oops/BUG"},
	{8, "A", "ACPI table overridden"},
	{9, "W", "WARN_ON fired"},
	{10, "C", "staging driver"},
	{11, "I", "firmware workaround"},
	{12, "O", "out-of-tree module"},
	{13, "E", "unsigned module"},
	{14, "L", "soft lockup"},
	{15, "K", "livepatch"},
	{16, "X", "auxiliary taint"},
	{17, "T", "randstruct"},
	{18, "N", "test module"},
}

// NodeHealth holds kernel health state for a node.
type NodeHealth struct {
	Release         string
	TaintRaw        int64
	ActiveTaints    []TaintBit
	PanicTimeout    int
	PanicOnOops     int
	NMIWatchdog     int
	WatchdogThresh  int
	SoftlockupPanic int
	KexecLoaded     int
	CrashKernel     string
}

func readSysctl(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func readSysctlInt(path string) int {
	v, _ := strconv.Atoi(readSysctl(path))
	return v
}

func cmdlineParam(param string) string {
	data, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return ""
	}
	for _, f := range strings.Fields(string(data)) {
		if strings.HasPrefix(f, param+"=") {
			return strings.TrimPrefix(f, param+"=")
		}
	}
	return ""
}

// GetNodeHealth reads kernel version, taint state, panic config,
// watchdog settings, and kdump readiness from procfs/sysfs.
func GetNodeHealth() (NodeHealth, error) {
	taintRaw := int64(readSysctlInt("/proc/sys/kernel/tainted"))
	var active []TaintBit
	for _, t := range taintTable {
		if taintRaw&(1<<t.Bit) != 0 {
			active = append(active, t)
		}
	}
	return NodeHealth{
		Release:         readSysctl("/proc/sys/kernel/osrelease"),
		TaintRaw:        taintRaw,
		ActiveTaints:    active,
		PanicTimeout:    readSysctlInt("/proc/sys/kernel/panic"),
		PanicOnOops:     readSysctlInt("/proc/sys/kernel/panic_on_oops"),
		NMIWatchdog:     readSysctlInt("/proc/sys/kernel/nmi_watchdog"),
		WatchdogThresh:  readSysctlInt("/proc/sys/kernel/watchdog_thresh"),
		SoftlockupPanic: readSysctlInt("/proc/sys/kernel/softlockup_panic"),
		KexecLoaded:     readSysctlInt("/sys/kernel/kexec_loaded"),
		CrashKernel:     cmdlineParam("crashkernel"),
	}, nil
}
