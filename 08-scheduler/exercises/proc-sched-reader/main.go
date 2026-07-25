// proc-sched-reader: parses /proc/<pid>/sched and /proc/<pid>/status
// to extract CPU affinity, NUMA affinity, and CFS scheduling statistics.
//
// Usage: proc-sched-reader <pid> [pid...]
// Build: go build -o proc-sched-reader .
//
// Kernel paths:
//   /proc/<pid>/sched     — kernel/sched/debug.c:proc_sched_show_task()
//   /proc/<pid>/status    — fs/proc/array.c:task_status()
//
// Source:
//   https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/debug.c
//   https://elixir.bootlin.com/linux/v6.9/source/fs/proc/array.c
package main

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// SchedInfo holds fields parsed from /proc/<pid>/sched.
type SchedInfo struct {
	PID                   int
	Comm                  string
	VrunTime              float64 // se.vruntime (ns)
	SumExecRuntime        float64 // se.sum_exec_runtime (ns)
	NrVoluntarySwitches   int
	NrInvoluntarySwitches int
	PrioValue             int // prio (dynamic priority)
	PolicyValue           int // sched policy number
}

// StatusInfo holds scheduling-relevant fields from /proc/<pid>/status.
type StatusInfo struct {
	PID              int
	Name             string
	CpusAllowedList  string // e.g. "0-3" or "0,2,4-7"
	MemsAllowedList  string // NUMA nodes, e.g. "0-1"
	VoluntaryCtxt    int
	NonvoluntaryCtxt int
}

// parseSchedFile reads /proc/<pid>/sched and returns a SchedInfo.
// The file has a header line "task_name (pid, #threads: N)" then
// key-value lines: "field.name  :   value".
func parseSchedFile(pid int) (SchedInfo, error) {
	path := fmt.Sprintf("/proc/%d/sched", pid)
	f, err := os.Open(path)
	if err != nil {
		return SchedInfo{}, err
	}
	defer f.Close()

	info := SchedInfo{PID: pid}
	scanner := bufio.NewScanner(f)
	firstLine := true
	for scanner.Scan() {
		line := scanner.Text()
		if firstLine {
			// header: "nginx (1234, #threads: 1)"
			firstLine = false
			if idx := strings.Index(line, " ("); idx > 0 {
				info.Comm = line[:idx]
			}
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])
		switch key {
		case "se.vruntime":
			info.VrunTime, _ = strconv.ParseFloat(val, 64)
		case "se.sum_exec_runtime":
			info.SumExecRuntime, _ = strconv.ParseFloat(val, 64)
		case "nr_voluntary_switches":
			info.NrVoluntarySwitches, _ = strconv.Atoi(val)
		case "nr_involuntary_switches":
			info.NrInvoluntarySwitches, _ = strconv.Atoi(val)
		case "prio":
			info.PrioValue, _ = strconv.Atoi(val)
		case "policy":
			info.PolicyValue, _ = strconv.Atoi(val)
		}
	}
	return info, scanner.Err()
}

// parseStatusFile reads scheduling-relevant fields from /proc/<pid>/status.
func parseStatusFile(pid int) (StatusInfo, error) {
	path := fmt.Sprintf("/proc/%d/status", pid)
	f, err := os.Open(path)
	if err != nil {
		return StatusInfo{}, err
	}
	defer f.Close()

	info := StatusInfo{PID: pid}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])
		switch key {
		case "Name":
			info.Name = val
		case "Cpus_allowed_list":
			info.CpusAllowedList = val
		case "Mems_allowed_list":
			info.MemsAllowedList = val
		case "voluntary_ctxt_switches":
			info.VoluntaryCtxt, _ = strconv.Atoi(val)
		case "nonvoluntary_ctxt_switches":
			info.NonvoluntaryCtxt, _ = strconv.Atoi(val)
		}
	}
	return info, scanner.Err()
}

func policyName(policy int) string {
	switch policy {
	case 0:
		return "SCHED_NORMAL"
	case 1:
		return "SCHED_FIFO"
	case 2:
		return "SCHED_RR"
	case 3:
		return "SCHED_BATCH"
	case 5:
		return "SCHED_IDLE"
	case 6:
		return "SCHED_DEADLINE"
	default:
		return fmt.Sprintf("UNKNOWN(%d)", policy)
	}
}

func printPID(pid int) {
	sched, err := parseSchedFile(pid)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pid %d: %v\n", pid, err)
		return
	}
	status, err := parseStatusFile(pid)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pid %d: %v\n", pid, err)
		return
	}

	fmt.Printf("PID %d  (%s)\n", pid, sched.Comm)
	fmt.Printf("  Policy         : %s\n", policyName(sched.PolicyValue))
	fmt.Printf("  Prio (dynamic) : %d  (100-139=CFS, 0-99=RT)\n", sched.PrioValue)
	fmt.Printf("  vruntime       : %.3f ms\n", sched.VrunTime/1e6)
	fmt.Printf("  sum_exec       : %.3f ms\n", sched.SumExecRuntime/1e6)
	fmt.Printf("  voluntary sw   : %d\n", sched.NrVoluntarySwitches)
	fmt.Printf("  involuntary sw : %d\n", sched.NrInvoluntarySwitches)
	fmt.Printf("  CPUs allowed   : %s\n", status.CpusAllowedList)
	fmt.Printf("  NUMA nodes     : %s\n", status.MemsAllowedList)
	fmt.Println()
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "usage: %s <pid> [pid...]\n", os.Args[0])
		os.Exit(1)
	}
	for _, arg := range os.Args[1:] {
		pid, err := strconv.Atoi(arg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "invalid pid %q: %v\n", arg, err)
			continue
		}
		printPID(pid)
	}
}
