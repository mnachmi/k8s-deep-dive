package sched

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/linux-to-k8s/kube-inspect/internal/proc"
)

// PodSchedInfo holds CPU and NUMA affinity for a pod.
type PodSchedInfo struct {
	PodUID            string
	CgroupCPUs        string // cpuset.cpus from the pod's cgroup v2 path (empty if not found)
	CgroupMems        string // cpuset.mems from the pod's cgroup v2 path (empty if not found)
	CPUWeight         string // cpu.weight (empty if not found)
	CPUMax            string // cpu.max (empty if not found)
	ProcessAffinities []ProcessAffinity
}

// ProcessAffinity holds per-process affinity from /proc/<pid>/status.
type ProcessAffinity struct {
	PID             int
	Comm            string
	CpusAllowedList string
	MemsAllowedList string
}

// cgroupPath returns the cgroup v2 path for the given pod UID.
// It searches under /sys/fs/cgroup/kubepods.slice for a directory
// matching *pod<uid>*.
func cgroupPath(podUID string) (string, error) {
	base := "/sys/fs/cgroup/kubepods.slice"
	pattern := fmt.Sprintf("*pod%s*", podUID)
	matches, err := filepath.Glob(filepath.Join(base, "*", pattern))
	if err != nil {
		return "", err
	}
	// Also try direct path (no QoS subdirectory) for some kubelet configurations.
	direct, _ := filepath.Glob(filepath.Join(base, pattern))
	matches = append(matches, direct...)
	if len(matches) == 0 {
		return "", nil
	}
	return matches[0], nil
}

// readCgroupFile reads a single-line cgroup file, returning "" if missing.
func readCgroupFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// parseStatusAffinity reads Cpus_allowed_list and Mems_allowed_list from
// /proc/<pid>/status, along with the process name.
func parseStatusAffinity(pid int) ProcessAffinity {
	path := fmt.Sprintf("/proc/%d/status", pid)
	f, err := os.Open(path)
	if err != nil {
		return ProcessAffinity{PID: pid}
	}
	defer f.Close()

	pa := ProcessAffinity{PID: pid}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		parts := strings.SplitN(scanner.Text(), ":", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])
		switch key {
		case "Name":
			pa.Comm = val
		case "Cpus_allowed_list":
			pa.CpusAllowedList = val
		case "Mems_allowed_list":
			pa.MemsAllowedList = val
		}
	}
	return pa
}

// GetPodSchedInfo returns CPU/NUMA affinity for all processes in the pod
// plus the pod-level cgroup cpuset and cpu.weight / cpu.max.
func GetPodSchedInfo(podUID string) (PodSchedInfo, error) {
	info := PodSchedInfo{PodUID: podUID}

	cgPath, err := cgroupPath(podUID)
	if err != nil {
		return info, fmt.Errorf("cgroup lookup: %w", err)
	}
	if cgPath != "" {
		info.CgroupCPUs = readCgroupFile(filepath.Join(cgPath, "cpuset.cpus"))
		info.CgroupMems = readCgroupFile(filepath.Join(cgPath, "cpuset.mems"))
		info.CPUWeight = readCgroupFile(filepath.Join(cgPath, "cpu.weight"))
		info.CPUMax = readCgroupFile(filepath.Join(cgPath, "cpu.max"))
	}

	processes, err := proc.ListPodProcesses(podUID)
	if err != nil {
		return info, fmt.Errorf("listing processes: %w", err)
	}

	seen := make(map[string]bool)
	for _, p := range processes {
		pa := parseStatusAffinity(p.PID)
		key := pa.CpusAllowedList + "|" + pa.MemsAllowedList
		if seen[key] {
			continue
		}
		seen[key] = true
		info.ProcessAffinities = append(info.ProcessAffinities, pa)
	}

	return info, nil
}
