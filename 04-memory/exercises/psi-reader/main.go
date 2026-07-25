package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// PSILine holds the parsed metrics from one "some" or "full" line in a PSI file.
type PSILine struct {
	Avg10  float64
	Avg60  float64
	Avg300 float64
	Total  uint64
}

// PSIStats holds parsed PSI data for one resource type.
type PSIStats struct {
	Resource string // "cpu", "memory", or "io"
	Some     PSILine
	Full     PSILine
}

// parsePSILine parses a line like:
//
//	"some avg10=0.00 avg60=0.01 avg300=0.02 total=12345"
func parsePSILine(line string) (string, PSILine, error) {
	fields := strings.Fields(line)
	if len(fields) < 5 {
		return "", PSILine{}, fmt.Errorf("unexpected PSI line format: %q", line)
	}
	kind := fields[0] // "some" or "full"
	var p PSILine
	for _, f := range fields[1:] {
		kv := strings.SplitN(f, "=", 2)
		if len(kv) != 2 {
			continue
		}
		switch kv[0] {
		case "avg10":
			p.Avg10, _ = strconv.ParseFloat(kv[1], 64)
		case "avg60":
			p.Avg60, _ = strconv.ParseFloat(kv[1], 64)
		case "avg300":
			p.Avg300, _ = strconv.ParseFloat(kv[1], 64)
		case "total":
			p.Total, _ = strconv.ParseUint(kv[1], 10, 64)
		}
	}
	return kind, p, nil
}

// readPSI reads and parses a PSI file (e.g., /proc/pressure/memory).
func readPSI(path string) (*PSIStats, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	defer f.Close()

	resource := filepath.Base(path)
	// Strip ".pressure" suffix for cgroup files like "memory.pressure"
	resource = strings.TrimSuffix(resource, ".pressure")

	stats := &PSIStats{Resource: resource}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		kind, parsed, err := parsePSILine(line)
		if err != nil {
			continue
		}
		switch kind {
		case "some":
			stats.Some = parsed
		case "full":
			stats.Full = parsed
		}
	}
	return stats, scanner.Err()
}

func printPSILine(label string, l PSILine) {
	fmt.Printf("  %-5s  avg10=%.2f  avg60=%.2f  avg300=%.2f  total=%dµs\n",
		label, l.Avg10, l.Avg60, l.Avg300, l.Total)
}

func printPSIStats(s *PSIStats) {
	printPSILine("some", s.Some)
	printPSILine("full", s.Full)
}

func showSystemPSI() {
	fmt.Println("=== System-wide PSI (/proc/pressure/) ===")
	for _, res := range []string{"cpu", "memory", "io"} {
		path := "/proc/pressure/" + res
		stats, err := readPSI(path)
		if err != nil {
			fmt.Printf("\n  %s pressure: unavailable (%v)\n", res, err)
			continue
		}
		fmt.Printf("\n  %s pressure:\n", res)
		printPSIStats(stats)
	}
}

func showCgroupPSI(cgPath string) {
	fmt.Printf("\n=== Cgroup PSI: %s ===\n", cgPath)
	for _, res := range []string{"cpu", "memory", "io"} {
		path := filepath.Join(cgPath, res+".pressure")
		stats, err := readPSI(path)
		if err != nil {
			fmt.Printf("\n  %s.pressure: unavailable (%v)\n", res, err)
			continue
		}
		fmt.Printf("\n  %s pressure:\n", res)
		printPSIStats(stats)
	}
}

// cgroupFromPID resolves a PID's cgroup v2 path from /proc/<pid>/cgroup.
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
	return "", fmt.Errorf("no cgroup v2 entry (0::) found for PID %d", pid)
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: psi-reader")
	fmt.Fprintln(os.Stderr, "       psi-reader --cgroup <path>")
	fmt.Fprintln(os.Stderr, "       psi-reader --pid <pid>")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "examples:")
	fmt.Fprintln(os.Stderr, "  psi-reader                                    # system-wide PSI")
	fmt.Fprintln(os.Stderr, "  psi-reader --cgroup /sys/fs/cgroup/user.slice  # + cgroup PSI")
	fmt.Fprintln(os.Stderr, "  psi-reader --pid 1                             # systemd's cgroup PSI")
}

func main() {
	switch {
	case len(os.Args) == 1:
		showSystemPSI()

	case len(os.Args) == 3 && os.Args[1] == "--cgroup":
		showSystemPSI()
		showCgroupPSI(os.Args[2])

	case len(os.Args) == 3 && os.Args[1] == "--pid":
		pid, err := strconv.Atoi(os.Args[2])
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: invalid PID %q\n", os.Args[2])
			os.Exit(1)
		}
		cgPath, err := cgroupFromPID(pid)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("PID %d -> cgroup: %s\n\n", pid, cgPath)
		showSystemPSI()
		showCgroupPSI(cgPath)

	default:
		usage()
		os.Exit(1)
	}
}
