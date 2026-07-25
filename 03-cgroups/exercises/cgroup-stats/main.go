package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Stats holds parsed cgroup v2 resource stats.
type Stats struct {
	Path string

	// memory
	MemCurrent uint64
	MemMax     string
	MemHigh    string
	MemStat    map[string]uint64
	MemEvents  map[string]uint64

	// cpu
	CPUMax    string
	CPUWeight uint64
	CPUStat   map[string]uint64

	// pids
	PidsCurrent uint64
	PidsMax     string
}

func readUint64(path string) (uint64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
}

func readStr(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func readKV(path string) map[string]uint64 {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	m := make(map[string]uint64)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		parts := strings.Fields(sc.Text())
		if len(parts) == 2 {
			if v, err := strconv.ParseUint(parts[1], 10, 64); err == nil {
				m[parts[0]] = v
			}
		}
	}
	return m
}

// cgroupFromPID reads /proc/<pid>/cgroup and returns the cgroup v2 path
// under /sys/fs/cgroup/ by extracting the "0::<path>" line.
func cgroupFromPID(pid int) (string, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
	if err != nil {
		return "", fmt.Errorf("reading /proc/%d/cgroup: %w", pid, err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "0::") {
			rel := strings.TrimPrefix(line, "0::")
			return filepath.Join("/sys/fs/cgroup", rel), nil
		}
	}
	return "", fmt.Errorf("no cgroup v2 entry in /proc/%d/cgroup", pid)
}

func readStats(path string) (*Stats, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("cgroup %s: %w", path, err)
	}
	s := &Stats{Path: path}
	s.MemCurrent, _ = readUint64(filepath.Join(path, "memory.current"))
	s.MemMax = readStr(filepath.Join(path, "memory.max"))
	s.MemHigh = readStr(filepath.Join(path, "memory.high"))
	s.MemStat = readKV(filepath.Join(path, "memory.stat"))
	s.MemEvents = readKV(filepath.Join(path, "memory.events"))
	s.CPUMax = readStr(filepath.Join(path, "cpu.max"))
	s.CPUWeight, _ = readUint64(filepath.Join(path, "cpu.weight"))
	s.CPUStat = readKV(filepath.Join(path, "cpu.stat"))
	s.PidsCurrent, _ = readUint64(filepath.Join(path, "pids.current"))
	s.PidsMax = readStr(filepath.Join(path, "pids.max"))
	return s, nil
}

func fmtBytes(b uint64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.2f GiB (%d B)", float64(b)/(1<<30), b)
	case b >= 1<<20:
		return fmt.Sprintf("%.2f MiB (%d B)", float64(b)/(1<<20), b)
	case b >= 1<<10:
		return fmt.Sprintf("%.2f KiB (%d B)", float64(b)/(1<<10), b)
	default:
		return fmt.Sprintf("%d B", b)
	}
}

func fmtLimit(s string) string {
	if s == "" || s == "max" {
		return s
	}
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return s
	}
	return fmtBytes(v)
}

func printStats(s *Stats) {
	fmt.Printf("Cgroup: %s\n\n", s.Path)

	fmt.Println("=== Memory ===")
	fmt.Printf("  current  : %s\n", fmtBytes(s.MemCurrent))
	fmt.Printf("  max      : %s\n", fmtLimit(s.MemMax))
	fmt.Printf("  high     : %s\n", fmtLimit(s.MemHigh))
	for _, k := range []string{"anon", "file", "kernel", "slab", "sock"} {
		if v, ok := s.MemStat[k]; ok {
			fmt.Printf("  stat.%-9s %s\n", k+":", fmtBytes(v))
		}
	}
	for _, k := range []string{"oom", "oom_kill", "max"} {
		if v, ok := s.MemEvents[k]; ok {
			fmt.Printf("  events.%-6s %d\n", k+":", v)
		}
	}

	fmt.Println("\n=== CPU ===")
	fmt.Printf("  max    : %s\n", s.CPUMax)
	fmt.Printf("  weight : %d\n", s.CPUWeight)
	for _, k := range []string{"usage_usec", "user_usec", "system_usec", "nr_throttled", "throttled_usec"} {
		if v, ok := s.CPUStat[k]; ok {
			fmt.Printf("  stat.%-16s %d\n", k+":", v)
		}
	}

	fmt.Println("\n=== PIDs ===")
	fmt.Printf("  current : %d\n", s.PidsCurrent)
	fmt.Printf("  max     : %s\n", s.PidsMax)
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: cgroup-stats <cgroup-path>")
	fmt.Fprintln(os.Stderr, "       cgroup-stats --pid <pid>")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "examples:")
	fmt.Fprintln(os.Stderr, "  cgroup-stats /sys/fs/cgroup/user.slice")
	fmt.Fprintln(os.Stderr, "  cgroup-stats --pid 1")
	fmt.Fprintln(os.Stderr, "  cgroup-stats --pid $$")
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}

	var cgPath string

	if os.Args[1] == "--pid" {
		if len(os.Args) < 3 {
			usage()
			os.Exit(1)
		}
		pid, err := strconv.Atoi(os.Args[2])
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: invalid PID %q\n", os.Args[2])
			os.Exit(1)
		}
		p, err := cgroupFromPID(pid)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("PID %d → cgroup: %s\n\n", pid, p)
		cgPath = p
	} else {
		cgPath = os.Args[1]
	}

	s, err := readStats(cgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	printStats(s)
}
