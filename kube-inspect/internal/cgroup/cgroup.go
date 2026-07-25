package cgroup

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// PodStats holds cgroup v2 resource stats for a pod.
// Fields are read from /sys/fs/cgroup/kubepods/pod<uid>/ and sub-directories.
type PodStats struct {
	CgroupPath string `json:"cgroup_path"`

	// memory (from memory.*)
	MemoryCurrent uint64            `json:"memory_current"`
	MemoryMax     string            `json:"memory_max"`
	MemoryHigh    string            `json:"memory_high"`
	MemoryStat    map[string]uint64 `json:"memory_stat,omitempty"`
	MemoryEvents  map[string]uint64 `json:"memory_events,omitempty"`

	// cpu (from cpu.*)
	CPUStat   map[string]uint64 `json:"cpu_stat,omitempty"`
	CPUMax    string            `json:"cpu_max"`
	CPUWeight uint64            `json:"cpu_weight"`

	// pids (from pids.*)
	PidsCurrent uint64 `json:"pids_current"`
	PidsMax     string `json:"pids_max"`
}

func readUint64(path string) (uint64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
}

func readString(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func readKV(path string) map[string]uint64 {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	result := make(map[string]uint64)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 2 {
			if v, err := strconv.ParseUint(fields[1], 10, 64); err == nil {
				result[fields[0]] = v
			}
		}
	}
	return result
}

// findPodCgroup searches /sys/fs/cgroup/kubepods/ for a directory
// whose name contains the given pod UID.
func findPodCgroup(podUID string) (string, error) {
	root := "/sys/fs/cgroup/kubepods"
	var found string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable directories
		}
		if d.IsDir() && strings.Contains(d.Name(), podUID) {
			found = path
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("searching %s: %w", root, err)
	}
	if found == "" {
		return "", fmt.Errorf("pod cgroup for %s not found under %s", podUID, root)
	}
	return found, nil
}

// ReadPodStats returns cgroup v2 resource stats for the given pod UID.
// It locates the pod's cgroup directory under /sys/fs/cgroup/kubepods/
// by searching for a directory matching the pod UID.
func ReadPodStats(podUID string) (*PodStats, error) {
	cgPath, err := findPodCgroup(podUID)
	if err != nil {
		return nil, err
	}

	s := &PodStats{CgroupPath: cgPath}

	s.MemoryCurrent, _ = readUint64(filepath.Join(cgPath, "memory.current"))
	s.MemoryMax = readString(filepath.Join(cgPath, "memory.max"))
	s.MemoryHigh = readString(filepath.Join(cgPath, "memory.high"))
	s.MemoryStat = readKV(filepath.Join(cgPath, "memory.stat"))
	s.MemoryEvents = readKV(filepath.Join(cgPath, "memory.events"))

	s.CPUStat = readKV(filepath.Join(cgPath, "cpu.stat"))
	s.CPUMax = readString(filepath.Join(cgPath, "cpu.max"))
	s.CPUWeight, _ = readUint64(filepath.Join(cgPath, "cpu.weight"))

	s.PidsCurrent, _ = readUint64(filepath.Join(cgPath, "pids.current"))
	s.PidsMax = readString(filepath.Join(cgPath, "pids.max"))

	return s, nil
}
