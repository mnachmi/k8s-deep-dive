// node-pressure-reader: reads PSI files from /proc/pressure/* and
// memory.events from pod cgroup paths.
//
// Usage:
//   node-pressure-reader               — print node-level PSI
//   node-pressure-reader --pod <uid>   — also print pod cgroup memory.events
//
// Build: go build -o node-pressure-reader .
//
// Kernel paths:
//   /proc/pressure/{cpu,memory,io}  — kernel/sched/psi.c:psi_show()
//   cgroup memory.events             — mm/memcontrol.c
//
// Source:
//   https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/psi.c
//   https://elixir.bootlin.com/linux/v6.9/source/mm/memcontrol.c
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

// PSIStats holds parsed data from a /proc/pressure/* or cgroup *.pressure file.
type PSIStats struct {
	Resource string
	SomeAvg10  float64
	SomeAvg60  float64
	SomeAvg300 float64
	SomeTotal  uint64 // microseconds
	FullAvg10  float64
	FullAvg60  float64
	FullAvg300 float64
	FullTotal  uint64 // microseconds
	HasFull    bool   // false for CPU
}

// MemoryEvents holds parsed data from cgroup memory.events.
type MemoryEvents struct {
	Low      uint64
	High     uint64
	Max      uint64
	OOM      uint64
	OOMKill  uint64
}

// parsePSIFile reads a PSI file and returns a PSIStats.
// Format per line: "some avg10=X avg60=X avg300=X total=Y"
func parsePSIFile(path, resource string) (PSIStats, error) {
	f, err := os.Open(path)
	if err != nil {
		return PSIStats{}, err
	}
	defer f.Close()

	stats := PSIStats{Resource: resource}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		kind := fields[0] // "some" or "full"
		vals := make(map[string]string)
		for _, f := range fields[1:] {
			kv := strings.SplitN(f, "=", 2)
			if len(kv) == 2 {
				vals[kv[0]] = kv[1]
			}
		}
		avg10, _ := strconv.ParseFloat(vals["avg10"], 64)
		avg60, _ := strconv.ParseFloat(vals["avg60"], 64)
		avg300, _ := strconv.ParseFloat(vals["avg300"], 64)
		total, _ := strconv.ParseUint(vals["total"], 10, 64)

		switch kind {
		case "some":
			stats.SomeAvg10 = avg10
			stats.SomeAvg60 = avg60
			stats.SomeAvg300 = avg300
			stats.SomeTotal = total
		case "full":
			stats.HasFull = true
			stats.FullAvg10 = avg10
			stats.FullAvg60 = avg60
			stats.FullAvg300 = avg300
			stats.FullTotal = total
		}
	}
	return stats, scanner.Err()
}

// parseMemoryEvents reads cgroup memory.events.
func parseMemoryEvents(path string) (MemoryEvents, error) {
	f, err := os.Open(path)
	if err != nil {
		return MemoryEvents{}, err
	}
	defer f.Close()

	var ev MemoryEvents
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		parts := strings.Fields(scanner.Text())
		if len(parts) != 2 {
			continue
		}
		v, _ := strconv.ParseUint(parts[1], 10, 64)
		switch parts[0] {
		case "low":
			ev.Low = v
		case "high":
			ev.High = v
		case "max":
			ev.Max = v
		case "oom":
			ev.OOM = v
		case "oom_kill":
			ev.OOMKill = v
		}
	}
	return ev, scanner.Err()
}

func printPSI(s PSIStats) {
	fmt.Printf("  %s pressure:\n", s.Resource)
	fmt.Printf("    some  avg10=%.2f%%  avg60=%.2f%%  avg300=%.2f%%  total=%d µs\n",
		s.SomeAvg10, s.SomeAvg60, s.SomeAvg300, s.SomeTotal)
	if s.HasFull {
		fmt.Printf("    full  avg10=%.2f%%  avg60=%.2f%%  avg300=%.2f%%  total=%d µs\n",
			s.FullAvg10, s.FullAvg60, s.FullAvg300, s.FullTotal)
	}
}

func printNodePressure() {
	fmt.Println("=== Node Pressure (PSI) ===")
	resources := []struct{ name, path string }{
		{"memory", "/proc/pressure/memory"},
		{"cpu", "/proc/pressure/cpu"},
		{"io", "/proc/pressure/io"},
	}
	for _, r := range resources {
		s, err := parsePSIFile(r.path, r.name)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  %s: %v\n", r.path, err)
			continue
		}
		printPSI(s)
	}
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

func printPodStats(podUID string) {
	fmt.Printf("\n=== Pod %s ===\n", podUID)
	cgPath, err := findPodCgroup(podUID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  %v\n", err)
		return
	}
	fmt.Printf("  cgroup: %s\n", cgPath)

	// memory.events
	ev, err := parseMemoryEvents(filepath.Join(cgPath, "memory.events"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "  memory.events: %v\n", err)
	} else {
		fmt.Printf("  memory.events:\n")
		fmt.Printf("    low=%d  high=%d  max=%d  oom=%d  oom_kill=%d\n",
			ev.Low, ev.High, ev.Max, ev.OOM, ev.OOMKill)
	}

	// memory.pressure
	mp, err := parsePSIFile(filepath.Join(cgPath, "memory.pressure"), "memory")
	if err != nil {
		fmt.Fprintf(os.Stderr, "  memory.pressure: %v\n", err)
	} else {
		printPSI(mp)
	}
}

func main() {
	podUID := flag.String("pod", "", "Pod UID to inspect (optional)")
	flag.Parse()

	printNodePressure()

	if *podUID != "" {
		printPodStats(*podUID)
	}
}
