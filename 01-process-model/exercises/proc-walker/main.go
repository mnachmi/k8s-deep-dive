package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

type Process struct {
	PID     int
	PPID    int
	Comm    string
	PidNsID uint64 // inode of /proc/<pid>/ns/pid — unique namespace identifier
}

// readComm reads /proc/<pid>/comm
func readComm(pid int) string {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
	if err != nil {
		return "?"
	}
	return strings.TrimSpace(string(data))
}

// readPPID reads the PPID from /proc/<pid>/status
func readPPID(pid int) int {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "PPid:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				ppid, _ := strconv.Atoi(fields[1])
				return ppid
			}
		}
	}
	return 0
}

// nsInode returns the inode number of the PID namespace symlink.
// This inode is unique per namespace — processes sharing an inode share a namespace.
func nsInode(pid int) uint64 {
	var stat syscall.Stat_t
	path := fmt.Sprintf("/proc/%d/ns/pid", pid)
	if err := syscall.Stat(path, &stat); err != nil {
		return 0
	}
	return stat.Ino
}

func listProcesses() ([]Process, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}

	var procs []Process
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue // not a PID directory
		}
		procs = append(procs, Process{
			PID:     pid,
			PPID:    readPPID(pid),
			Comm:    readComm(pid),
			PidNsID: nsInode(pid),
		})
	}
	return procs, nil
}

func main() {
	procs, err := listProcesses()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error reading /proc: %v\n", err)
		os.Exit(1)
	}

	// Group by PID namespace inode
	byNS := make(map[uint64][]Process)
	for _, p := range procs {
		byNS[p.PidNsID] = append(byNS[p.PidNsID], p)
	}

	// Sort namespace IDs for stable output
	var nsIDs []uint64
	for id := range byNS {
		nsIDs = append(nsIDs, id)
	}
	sort.Slice(nsIDs, func(i, j int) bool { return nsIDs[i] < nsIDs[j] })

	hostNS := nsInode(os.Getpid())

	for _, nsID := range nsIDs {
		group := byNS[nsID]
		label := fmt.Sprintf("ns-inode:%d", nsID)
		if nsID == hostNS {
			label += " (host)"
		}
		fmt.Printf("\n=== PID namespace %s ===\n", label)
		fmt.Printf("  %-8s %-8s %s\n", "PID", "PPID", "COMM")
		sort.Slice(group, func(i, j int) bool { return group[i].PID < group[j].PID })
		for _, p := range group {
			fmt.Printf("  %-8d %-8d %s\n", p.PID, p.PPID, p.Comm)
		}
	}

	// Print namespace summary
	fmt.Printf("\n--- Summary ---\n")
	fmt.Printf("Total processes: %d\n", len(procs))
	fmt.Printf("Distinct PID namespaces: %d\n", len(byNS))
	if len(byNS) > 1 {
		fmt.Printf("Non-host namespaces: %d (likely containers)\n", len(byNS)-1)
	}

	// Show the path to see namespace symlinks for a given PID
	if len(os.Args) > 1 {
		pid, err := strconv.Atoi(os.Args[1])
		if err == nil {
			fmt.Printf("\n--- Namespace symlinks for PID %d ---\n", pid)
			nsDir := fmt.Sprintf("/proc/%d/ns", pid)
			links, _ := filepath.Glob(nsDir + "/*")
			for _, l := range links {
				target, err := os.Readlink(l)
				if err == nil {
					fmt.Printf("  %s -> %s\n", filepath.Base(l), target)
				}
			}
		}
	}
}
