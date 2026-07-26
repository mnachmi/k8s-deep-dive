// node-health-reader: reads Linux node health state for Kubernetes operators.
//
// Reads:
//   - /proc/version and /proc/sys/kernel/osrelease (kernel version)
//   - /proc/sys/kernel/tainted (decode taint flags)
//   - /proc/sys/kernel/panic, panic_on_oops, nmi_watchdog, watchdog_thresh
//   - /sys/kernel/kexec_crash_loaded (kdump readiness)
//   - /dev/kmsg (scan recent kernel messages for OOM kills and BUG/WARN)
//
// Usage:
//
//	node-health-reader                    print full health report
//	node-health-reader --kmsg             also scan /dev/kmsg for OOM/BUG/WARN
//	node-health-reader --json             output as JSON
//
// Build: go build -o node-health-reader .
//
// Kernel paths:
//
//	/proc/sys/kernel/tainted → include/linux/panic.h TAINT_* flags
//	/dev/kmsg               → kernel/printk/printk.c — structured ring buffer
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// TaintFlag describes one kernel taint bit.
type TaintFlag struct {
	Bit     int    `json:"bit"`
	Code    string `json:"code"`
	Meaning string `json:"meaning"`
}

// taintTable maps bit index to taint description.
// Source: include/linux/panic.h (Linux 6.9)
var taintTable = []TaintFlag{
	{0, "P", "proprietary module loaded"},
	{1, "F", "module force-loaded"},
	{2, "S", "CPU out of spec (overclocking, thermals, hw errata)"},
	{3, "R", "module force-removed"},
	{4, "M", "machine check error"},
	{5, "B", "bad page accessed"},
	{6, "U", "userspace set taint"},
	{7, "D", "kernel oops/BUG fired"},
	{8, "A", "ACPI table overridden"},
	{9, "W", "WARN_ON fired"},
	{10, "C", "staging driver loaded"},
	{11, "I", "firmware workaround"},
	{12, "O", "out-of-tree module"},
	{13, "E", "unsigned module"},
	{14, "L", "soft lockup detected"},
	{15, "K", "livepatch applied"},
	{16, "X", "auxiliary taint"},
	{17, "T", "randstruct layout"},
	{18, "N", "test module"},
}

// KernelHealth holds all node health state.
type KernelHealth struct {
	Version         string      `json:"version"`
	Release         string      `json:"release"`
	TaintRaw        int64       `json:"taint_raw"`
	TaintClean      bool        `json:"taint_clean"`
	ActiveTaints    []TaintFlag `json:"active_taints,omitempty"`
	PanicTimeout    int         `json:"panic_timeout_s"`
	PanicOnOops     int         `json:"panic_on_oops"`
	NMIWatchdog     int         `json:"nmi_watchdog"`
	WatchdogThresh  int         `json:"watchdog_thresh_s"`
	SoftlockupPanic int         `json:"softlockup_panic"`
	KexecLoaded     int         `json:"kexec_loaded"`
	CrashKernel     string      `json:"crash_kernel_param"`
}

// KmsgEvent holds a parsed /dev/kmsg entry.
type KmsgEvent struct {
	Level   int    `json:"level"`
	Message string `json:"message"`
}

func readSysctl(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func readSysctlInt(path string) int {
	s := readSysctl(path)
	v, _ := strconv.Atoi(s)
	return v
}

func decodeTaint(raw int64) (bool, []TaintFlag) {
	if raw == 0 {
		return true, nil
	}
	var active []TaintFlag
	for _, f := range taintTable {
		if raw&(1<<f.Bit) != 0 {
			active = append(active, f)
		}
	}
	return false, active
}

func parseCmdlineParam(param string) string {
	data, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return ""
	}
	for _, field := range strings.Fields(string(data)) {
		if strings.HasPrefix(field, param+"=") {
			return strings.TrimPrefix(field, param+"=")
		}
	}
	return ""
}

func collectHealth() KernelHealth {
	taintRaw := int64(readSysctlInt("/proc/sys/kernel/tainted"))
	clean, active := decodeTaint(taintRaw)
	return KernelHealth{
		Version:         readSysctl("/proc/version"),
		Release:         readSysctl("/proc/sys/kernel/osrelease"),
		TaintRaw:        taintRaw,
		TaintClean:      clean,
		ActiveTaints:    active,
		PanicTimeout:    readSysctlInt("/proc/sys/kernel/panic"),
		PanicOnOops:     readSysctlInt("/proc/sys/kernel/panic_on_oops"),
		NMIWatchdog:     readSysctlInt("/proc/sys/kernel/nmi_watchdog"),
		WatchdogThresh:  readSysctlInt("/proc/sys/kernel/watchdog_thresh"),
		SoftlockupPanic: readSysctlInt("/proc/sys/kernel/softlockup_panic"),
		KexecLoaded:     readSysctlInt("/sys/kernel/kexec_crash_loaded"),
		CrashKernel:     parseCmdlineParam("crashkernel"),
	}
}

// scanKmsg reads /dev/kmsg and returns events containing OOM/BUG/WARN/panic.
// /dev/kmsg lines: "<priority>,<seq>,<timestamp_us>,-;<message>"
func scanKmsg(max int) ([]KmsgEvent, error) {
	f, err := os.OpenFile("/dev/kmsg", os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var events []KmsgEvent
	scanner := bufio.NewScanner(f)
	for scanner.Scan() && len(events) < max {
		line := scanner.Text()
		// format: "priority,seq,ts,-;message"
		idx := strings.Index(line, ";")
		if idx < 0 {
			continue
		}
		meta := line[:idx]
		msg := line[idx+1:]
		// check for keywords
		lower := strings.ToLower(msg)
		if !strings.Contains(lower, "oom") &&
			!strings.Contains(lower, "bug:") &&
			!strings.Contains(lower, "warn") &&
			!strings.Contains(lower, "panic") &&
			!strings.Contains(lower, "lockup") {
			continue
		}
		lvl := 0
		parts := strings.SplitN(meta, ",", 2)
		if len(parts) > 0 {
			lvl, _ = strconv.Atoi(parts[0])
		}
		events = append(events, KmsgEvent{Level: lvl, Message: msg})
	}
	return events, nil
}

func printHealth(h KernelHealth) {
	fmt.Println("=== Kernel Health Report ===")
	fmt.Printf("  release:          %s\n", h.Release)
	fmt.Printf("  version:          %s\n", h.Version)
	fmt.Println()
	fmt.Println("=== Taint State ===")
	if h.TaintClean {
		fmt.Printf("  tainted: 0 (CLEAN)\n")
	} else {
		fmt.Printf("  tainted: %d\n", h.TaintRaw)
		for _, t := range h.ActiveTaints {
			fmt.Printf("    bit %2d (%s): %s\n", t.Bit, t.Code, t.Meaning)
		}
	}
	fmt.Println()
	fmt.Println("=== Panic Configuration ===")
	fmt.Printf("  kernel.panic:             %d  (0=halt, >0=reboot after Ns)\n", h.PanicTimeout)
	fmt.Printf("  kernel.panic_on_oops:     %d  (1=oops triggers panic)\n", h.PanicOnOops)
	fmt.Printf("  kernel.softlockup_panic:  %d  (1=soft lockup triggers panic)\n", h.SoftlockupPanic)
	fmt.Println()
	fmt.Println("=== Watchdog State ===")
	fmt.Printf("  kernel.nmi_watchdog:      %d  (1=hardlockup detection on)\n", h.NMIWatchdog)
	fmt.Printf("  kernel.watchdog_thresh:   %ds (softlockup at %ds, hardlockup at ~%ds)\n",
		h.WatchdogThresh, h.WatchdogThresh*2, h.WatchdogThresh)
	fmt.Println()
	fmt.Println("=== kdump Readiness ===")
	fmt.Printf("  kexec_crash_loaded:  %d  (1=crash kernel loaded)\n", h.KexecLoaded)
	if h.CrashKernel != "" {
		fmt.Printf("  crashkernel:   %s\n", h.CrashKernel)
	} else {
		fmt.Printf("  crashkernel:   (not set — kdump not configured)\n")
	}
}

func main() {
	kmsgFlag := flag.Bool("kmsg", false, "Scan /dev/kmsg for OOM/BUG/WARN/panic events")
	jsonFlag := flag.Bool("json", false, "Output as JSON")
	flag.Parse()

	h := collectHealth()

	if *jsonFlag {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(h)
		return
	}

	printHealth(h)

	if *kmsgFlag {
		fmt.Println("=== Recent Kernel Messages (OOM/BUG/WARN/panic/lockup) ===")
		events, err := scanKmsg(50)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  kmsg: %v\n", err)
		} else if len(events) == 0 {
			fmt.Println("  (none found)")
		} else {
			for _, e := range events {
				fmt.Printf("  [%d] %s\n", e.Level, e.Message)
			}
		}
	}
}
