package kubelet

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// NodePressure holds PSI data from /proc/pressure/*.
type NodePressure struct {
	MemorySomeAvg10 float64
	MemoryFullAvg10 float64
	CPUSomeAvg10    float64
	IOSomeAvg10     float64
	IOFullAvg10     float64
}

// PodMemEvents holds memory.events counters from a pod cgroup.
type PodMemEvents struct {
	PodUID  string
	CgPath  string
	Low     uint64
	High    uint64
	Max     uint64
	OOM     uint64
	OOMKill uint64
}

// parsePSIAvg10 reads a PSI file and returns the "some" and "full" avg10 values.
func parsePSIAvg10(path string) (someAvg10, fullAvg10 float64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		kind := fields[0]
		for _, kv := range fields[1:] {
			parts := strings.SplitN(kv, "=", 2)
			if len(parts) == 2 && parts[0] == "avg10" {
				v, _ := strconv.ParseFloat(parts[1], 64)
				if kind == "some" {
					someAvg10 = v
				} else if kind == "full" {
					fullAvg10 = v
				}
			}
		}
	}
	return someAvg10, fullAvg10, scanner.Err()
}

// GetNodePressure reads /proc/pressure/{memory,cpu,io} and returns avg10 values.
func GetNodePressure() (NodePressure, error) {
	var np NodePressure
	var err error

	np.MemorySomeAvg10, np.MemoryFullAvg10, err = parsePSIAvg10("/proc/pressure/memory")
	if err != nil {
		return np, fmt.Errorf("memory pressure: %w", err)
	}
	np.CPUSomeAvg10, _, err = parsePSIAvg10("/proc/pressure/cpu")
	if err != nil {
		return np, fmt.Errorf("cpu pressure: %w", err)
	}
	np.IOSomeAvg10, np.IOFullAvg10, err = parsePSIAvg10("/proc/pressure/io")
	if err != nil {
		return np, fmt.Errorf("io pressure: %w", err)
	}
	return np, nil
}

// parseMemoryEvents reads a cgroup memory.events file.
func parseMemoryEvents(path string) (low, high, max, oom, oomKill uint64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, 0, 0, 0, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		parts := strings.Fields(scanner.Text())
		if len(parts) != 2 {
			continue
		}
		v, _ := strconv.ParseUint(parts[1], 10, 64)
		switch parts[0] {
		case "low":
			low = v
		case "high":
			high = v
		case "max":
			max = v
		case "oom":
			oom = v
		case "oom_kill":
			oomKill = v
		}
	}
	return low, high, max, oom, oomKill, scanner.Err()
}

// GetPodMemEvents reads memory.events for the given pod UID's cgroup.
func GetPodMemEvents(podUID string) (PodMemEvents, error) {
	ev := PodMemEvents{PodUID: podUID}

	base := "/sys/fs/cgroup/kubepods.slice"
	pattern := fmt.Sprintf("*pod%s*", podUID)
	matches, _ := filepath.Glob(filepath.Join(base, "*", pattern))
	direct, _ := filepath.Glob(filepath.Join(base, pattern))
	all := append(matches, direct...)
	if len(all) == 0 {
		return ev, fmt.Errorf("cgroup not found for pod %s", podUID)
	}
	ev.CgPath = all[0]

	var err error
	ev.Low, ev.High, ev.Max, ev.OOM, ev.OOMKill, err =
		parseMemoryEvents(filepath.Join(ev.CgPath, "memory.events"))
	return ev, err
}
