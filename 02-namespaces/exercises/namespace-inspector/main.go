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

// NsInfo holds all namespace identifiers for one process.
type NsInfo struct {
	PID  int
	Comm string
	NS   map[string]uint64 // namespace type → inode number
}

// nsTypes is the ordered set of namespace symlinks we inspect.
// These match the entries in /proc/<pid>/ns/.
var nsTypes = []string{
	"cgroup", "ipc", "mnt", "net", "pid",
	"pid_for_children", "time", "time_for_children", "user", "uts",
}

func readComm(pid int) string {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
	if err != nil {
		return "?"
	}
	return strings.TrimSpace(string(data))
}

// nsInode returns the inode of /proc/<pid>/ns/<nstype>, or 0 on error.
// The inode is the canonical namespace identity: two processes with the same
// inode for a given type are in the same namespace instance.
// See fs/nsfs.c:ns_get_path() in the Linux kernel source.
func nsInode(pid int, nstype string) uint64 {
	var st syscall.Stat_t
	if err := syscall.Stat(fmt.Sprintf("/proc/%d/ns/%s", pid, nstype), &st); err != nil {
		return 0
	}
	return st.Ino
}

func nsSymlink(pid int, nstype string) string {
	target, err := os.Readlink(fmt.Sprintf("/proc/%d/ns/%s", pid, nstype))
	if err != nil {
		return "?"
	}
	return target
}

func listAllProcesses() []NsInfo {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var result []NsInfo
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		ns := make(map[string]uint64, len(nsTypes))
		for _, t := range nsTypes {
			ns[t] = nsInode(pid, t)
		}
		result = append(result, NsInfo{
			PID:  pid,
			Comm: readComm(pid),
			NS:   ns,
		})
	}
	return result
}

func inspectPID(pid int, allProcs []NsInfo) {
	comm := readComm(pid)
	fmt.Printf("PID %d (%s) namespace map:\n", pid, comm)
	fmt.Printf("  %-22s  %-32s  %s\n", "TYPE", "SYMLINK TARGET", "INODE")
	fmt.Printf("  %-22s  %-32s  %s\n",
		strings.Repeat("-", 22), strings.Repeat("-", 32), strings.Repeat("-", 18))
	for _, t := range nsTypes {
		sym := nsSymlink(pid, t)
		ino := nsInode(pid, t)
		inoStr := "-"
		if ino != 0 {
			inoStr = strconv.FormatUint(ino, 10)
		}
		fmt.Printf("  %-22s  %-32s  %s\n", t, sym, inoStr)
	}

	if len(allProcs) == 0 {
		return
	}

	// Find which other processes share each namespace with this PID.
	var target NsInfo
	for _, p := range allProcs {
		if p.PID == pid {
			target = p
			break
		}
	}
	if target.PID == 0 {
		return
	}

	fmt.Printf("\nProcesses sharing namespaces with PID %d:\n", pid)
	for _, t := range []string{"net", "mnt", "pid", "ipc", "uts", "cgroup"} {
		targetIno := target.NS[t]
		if targetIno == 0 {
			continue
		}
		var peers []string
		for _, p := range allProcs {
			if p.PID != pid && p.NS[t] == targetIno {
				peers = append(peers, fmt.Sprintf("%d(%s)", p.PID, p.Comm))
			}
		}
		if len(peers) == 0 {
			continue
		}
		if len(peers) <= 8 {
			fmt.Printf("  %-22s  %s\n", t, strings.Join(peers, ", "))
		} else {
			fmt.Printf("  %-22s  %d processes (host namespace)\n", t, len(peers))
		}
	}
}

func showSharedGroups(procs []NsInfo) {
	// For each namespace type, group PIDs by inode — show groups of 2–8
	// (groups larger than 8 are almost certainly the host namespace).
	fmt.Printf("Shared namespace groups (2–8 members, likely container groups):\n")
	printed := false
	for _, t := range []string{"net", "mnt", "pid", "ipc", "uts", "cgroup"} {
		groups := make(map[uint64][]NsInfo)
		for _, p := range procs {
			if ino := p.NS[t]; ino != 0 {
				groups[ino] = append(groups[ino], p)
			}
		}
		var inodes []uint64
		for ino, members := range groups {
			if len(members) >= 2 && len(members) <= 8 {
				inodes = append(inodes, ino)
			}
		}
		if len(inodes) == 0 {
			continue
		}
		sort.Slice(inodes, func(i, j int) bool { return inodes[i] < inodes[j] })
		for _, ino := range inodes {
			group := groups[ino]
			sort.Slice(group, func(i, j int) bool { return group[i].PID < group[j].PID })
			pids := make([]string, len(group))
			for i, p := range group {
				pids[i] = fmt.Sprintf("%d(%s)", p.PID, p.Comm)
			}
			fmt.Printf("  %s inode %-12d  %s\n", t, ino, strings.Join(pids, ", "))
			printed = true
		}
	}
	if !printed {
		fmt.Println("  (no container namespace groups found — are containers running?)")
	}
}

func showNsCounts(procs []NsInfo) {
	fmt.Printf("\nDistinct namespace instances per type:\n")
	for _, t := range []string{"net", "mnt", "pid", "ipc", "uts", "cgroup", "user", "time"} {
		seen := make(map[uint64]struct{})
		for _, p := range procs {
			if ino := p.NS[t]; ino != 0 {
				seen[ino] = struct{}{}
			}
		}
		fmt.Printf("  %-22s  %d\n", t, len(seen))
	}
}

func main() {
	if len(os.Args) >= 2 {
		pid, err := strconv.Atoi(os.Args[1])
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: invalid PID %q\n", os.Args[1])
			os.Exit(1)
		}
		procs := listAllProcesses()
		inspectPID(pid, procs)
		return
	}

	procs := listAllProcesses()
	fmt.Printf("Scanned %d processes from /proc\n\n", len(procs))

	showSharedGroups(procs)
	showNsCounts(procs)

	fmt.Printf("\nYour shell's namespace map:\n")
	inspectPID(os.Getpid(), nil)

	fmt.Printf("\n/proc/self/ns/ contents:\n")
	links, _ := filepath.Glob("/proc/self/ns/*")
	for _, l := range links {
		target, err := os.Readlink(l)
		if err == nil {
			fmt.Printf("  %s -> %s\n", filepath.Base(l), target)
		}
	}
}
