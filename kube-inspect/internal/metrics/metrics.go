package metrics

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ThrottleStats holds cpu.stat throttle fields for a pod cgroup.
type ThrottleStats struct {
	PodUID      string
	CgPath      string
	NrPeriods   uint64
	NrThrottled uint64
	ThrottledUS uint64
	UsageUS     uint64
}

// ThrottleRatio returns the fraction of periods that were throttled (0.0–1.0).
func (t ThrottleStats) ThrottleRatio() float64 {
	if t.NrPeriods == 0 {
		return 0
	}
	return float64(t.NrThrottled) / float64(t.NrPeriods)
}

// ProcessSchedStat holds /proc/<pid>/schedstat for one process in a pod.
type ProcessSchedStat struct {
	PID        int
	Comm       string
	RuntimeNS  uint64
	WaitNS     uint64
	NrSwitches uint64
}

// WaitRatio returns wait/(wait+runtime). Returns 0 if both zero.
func (p ProcessSchedStat) WaitRatio() float64 {
	total := p.WaitNS + p.RuntimeNS
	if total == 0 {
		return 0
	}
	return float64(p.WaitNS) / float64(total)
}

// PodPerfReport aggregates throttle stats and per-process sched latency.
type PodPerfReport struct {
	PodUID    string
	Throttle  ThrottleStats
	Processes []ProcessSchedStat
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

func parseCPUStat(path string) (ThrottleStats, error) {
	f, err := os.Open(path)
	if err != nil {
		return ThrottleStats{}, err
	}
	defer f.Close()

	var s ThrottleStats
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

func podPIDs(podUID string) []int {
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
		s := string(cgData)
		if strings.Contains(s, "pod"+podUID) ||
			strings.Contains(s, strings.ReplaceAll(podUID, "-", "")) {
			pids = append(pids, pid)
		}
	}
	return pids
}

func parseSchedStat(pid int) (ProcessSchedStat, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/schedstat", pid))
	if err != nil {
		return ProcessSchedStat{}, err
	}
	fields := strings.Fields(strings.TrimSpace(string(data)))
	if len(fields) < 3 {
		return ProcessSchedStat{}, fmt.Errorf("unexpected schedstat format")
	}
	rt, _ := strconv.ParseUint(fields[0], 10, 64)
	wait, _ := strconv.ParseUint(fields[1], 10, 64)
	sw, _ := strconv.ParseUint(fields[2], 10, 64)
	comm := "(unknown)"
	if cb, err2 := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid)); err2 == nil {
		comm = strings.TrimSpace(string(cb))
	}
	return ProcessSchedStat{PID: pid, Comm: comm, RuntimeNS: rt, WaitNS: wait, NrSwitches: sw}, nil
}

// GetPodPerfReport reads cpu.stat throttle metrics and per-process schedstat
// for all processes belonging to the given pod UID.
func GetPodPerfReport(podUID string) (PodPerfReport, error) {
	report := PodPerfReport{PodUID: podUID}

	cgPath, err := findPodCgroup(podUID)
	if err != nil {
		return report, err
	}
	ts, err := parseCPUStat(filepath.Join(cgPath, "cpu.stat"))
	if err != nil {
		return report, fmt.Errorf("cpu.stat: %w", err)
	}
	ts.PodUID = podUID
	ts.CgPath = cgPath
	report.Throttle = ts

	for _, pid := range podPIDs(podUID) {
		ps, err := parseSchedStat(pid)
		if err != nil {
			continue
		}
		report.Processes = append(report.Processes, ps)
	}
	return report, nil
}
