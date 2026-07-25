// sched-latency-reader: reads scheduler latency stats from
// /proc/<pid>/schedstat and CPU throttle stats from a pod cgroup's cpu.stat.
//
// Usage:
//   sched-latency-reader --pid <pid>           print schedstat for one process
//   sched-latency-reader --pod <uid>           print throttle report for pod
//   sched-latency-reader --pod <uid> --all     also show per-process schedstat
//
// Build: go build -o sched-latency-reader .
//
// Kernel paths:
//   /proc/<pid>/schedstat  → fs/proc/base.c (proc_pid_schedstat)
//   cgroup cpu.stat        → kernel/sched/fair.c (struct cfs_bandwidth)
//
// Sources:
//   https://elixir.bootlin.com/linux/v6.9/source/fs/proc/base.c
//   https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/fair.c
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// SchedStat holds /proc/<pid>/schedstat fields.
type SchedStat struct {
	PID           int
	Comm          string
	RuntimeNS     uint64 // time running on CPU (nanoseconds)
	WaitNS        uint64 // time waiting on runqueue (nanoseconds)
	NrSwitches    uint64 // total context switches
}

// CPUStat holds relevant fields from cgroup cpu.stat.
type CPUStat struct {
	UsageUS      uint64
	NrPeriods    uint64
	NrThrottled  uint64
	ThrottledUS  uint64
}

// ThrottleRatio returns throttled fraction (0.0–1.0). Returns 0 if no periods.
func (s CPUStat) ThrottleRatio() float64 {
	if s.NrPeriods == 0 {
		return 0
	}
	return float64(s.NrThrottled) / float64(s.NrPeriods)
}

// WaitRatio returns wait_ns / (wait_ns + runtime_ns). Returns 0 if both zero.
func (s SchedStat) WaitRatio() float64 {
	total := s.WaitNS + s.RuntimeNS
	if total == 0 {
		return 0
	}
	return float64(s.WaitNS) / float64(total)
}

// parseSchedStat reads /proc/<pid>/schedstat.
func parseSchedStat(pid int) (SchedStat, error) {
	path := fmt.Sprintf("/proc/%d/schedstat", pid)
	data, err := os.ReadFile(path)
	if err != nil {
		return SchedStat{}, err
	}
	fields := strings.Fields(strings.TrimSpace(string(data)))
	if len(fields) < 3 {
		return SchedStat{}, fmt.Errorf("%s: unexpected format %q", path, data)
	}
	rt, _ := strconv.ParseUint(fields[0], 10, 64)
	wait, _ := strconv.ParseUint(fields[1], 10, 64)
	sw, _ := strconv.ParseUint(fields[2], 10, 64)

	comm := "(unknown)"
	if cb, err2 := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid)); err2 == nil {
		comm = strings.TrimSpace(string(cb))
	}
	return SchedStat{PID: pid, Comm: comm, RuntimeNS: rt, WaitNS: wait, NrSwitches: sw}, nil
}

// parseCPUStat reads cgroup cpu.stat.
func parseCPUStat(path string) (CPUStat, error) {
	f, err := os.Open(path)
	if err != nil {
		return CPUStat{}, err
	}
	defer f.Close()

	var s CPUStat
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		parts := strings.Fields(scanner.Text())
		if len(parts) != 2 {
			continue
		}
		v, _ := strconv.ParseUint(parts[1], 10, 64)
		switch parts[0] {
		case "usage_usec":
			s.UsageUS = v
		case "nr_periods":
			s.NrPeriods = v
		case "nr_throttled":
			s.NrThrottled = v
		case "throttled_usec":
			s.ThrottledUS = v
		}
	}
	return s, scanner.Err()
}

func findPodCgroup(podUID string) (string, error) {
	base := "/sys/fs/cgroup/kubepods.slice"
	pattern := fmt.Sprintf("*pod%s*", podUID)
	matches, _ := filepath.Glob(filepath.Join(base, "*", pattern))
	direct, _ := filepath.Glob(filepath.Join(base, pattern))
	all := append(matches, direct...)
	if len(all) == 0 {
		return "", fmt.Errorf("cgroup not found for pod %s", podUID)
	}
	return all[0], nil
}

// listPodPIDs returns PIDs whose /proc/<pid>/cgroup mentions the pod UID.
func listPodPIDs(podUID string) []int {
	var pids []int
	entries, _ := os.ReadDir("/proc")
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		cgData, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
		if err != nil {
			continue
		}
		if strings.Contains(string(cgData), "pod"+podUID) ||
			strings.Contains(string(cgData), strings.ReplaceAll(podUID, "-", "")) {
			pids = append(pids, pid)
		}
	}
	return pids
}

func printSchedStat(s SchedStat) {
	fmt.Printf("  pid=%-6d  comm=%-16s  runtime=%6.1fms  wait=%6.1fms  wait_ratio=%4.1f%%  switches=%d\n",
		s.PID, s.Comm,
		float64(s.RuntimeNS)/1e6,
		float64(s.WaitNS)/1e6,
		s.WaitRatio()*100,
		s.NrSwitches)
}

func runPID(pid int) {
	s, err := parseSchedStat(pid)
	if err != nil {
		fmt.Fprintf(os.Stderr, "schedstat: %v\n", err)
		return
	}
	fmt.Printf("=== /proc/%d/schedstat ===\n", pid)
	printSchedStat(s)
	fmt.Printf("\nKernel path: /proc/%d/schedstat → fs/proc/base.c (proc_pid_schedstat)\n", pid)
	fmt.Printf("Source: https://elixir.bootlin.com/linux/v6.9/source/fs/proc/base.c\n")
}

func runPod(podUID string, showAll bool) {
	cgPath, err := findPodCgroup(podUID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cgroup: %v\n", err)
	} else {
		fmt.Printf("=== CPU throttle for pod %s ===\n", podUID)
		fmt.Printf("  cgroup: %s\n", cgPath)
		cs, err := parseCPUStat(filepath.Join(cgPath, "cpu.stat"))
		if err != nil {
			fmt.Fprintf(os.Stderr, "  cpu.stat: %v\n", err)
		} else {
			fmt.Printf("  usage_usec:     %d\n", cs.UsageUS)
			fmt.Printf("  nr_periods:     %d\n", cs.NrPeriods)
			fmt.Printf("  nr_throttled:   %d\n", cs.NrThrottled)
			fmt.Printf("  throttled_usec: %d\n", cs.ThrottledUS)
			fmt.Printf("  throttle_ratio: %.1f%%\n", cs.ThrottleRatio()*100)
		}
		fmt.Println()
	}

	if showAll {
		fmt.Printf("=== Scheduler latency for pod %s processes ===\n", podUID)
		pids := listPodPIDs(podUID)
		if len(pids) == 0 {
			fmt.Println("  (no processes found for this pod UID)")
			return
		}
		for _, pid := range pids {
			s, err := parseSchedStat(pid)
			if err != nil {
				continue
			}
			printSchedStat(s)
		}
	}
}

func main() {
	pid := flag.Int("pid", 0, "PID to read schedstat for")
	pod := flag.String("pod", "", "Pod UID for cgroup cpu.stat throttle report")
	all := flag.Bool("all", false, "Also show per-process schedstat for --pod")
	flag.Parse()

	if *pid == 0 && *pod == "" {
		fmt.Fprintln(os.Stderr, "usage: sched-latency-reader --pid <pid> | --pod <uid> [--all]")
		os.Exit(1)
	}
	if *pid != 0 {
		runPID(*pid)
	}
	if *pod != "" {
		runPod(*pod, *all)
	}
}
