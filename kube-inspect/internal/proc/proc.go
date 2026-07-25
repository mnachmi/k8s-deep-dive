package proc

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// Process holds the kernel-visible identity of a process.
type Process struct {
	PID     int
	PPID    int
	Comm    string
	PidNsID uint64 // inode of /proc/<pid>/ns/pid
	CgroupPath string // from /proc/<pid>/cgroup, cgroup v2 unified path
}

func nsInode(pid int) uint64 {
	var stat syscall.Stat_t
	if err := syscall.Stat(fmt.Sprintf("/proc/%d/ns/pid", pid), &stat); err != nil {
		return 0
	}
	return stat.Ino
}

func readComm(pid int) string {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
	if err != nil {
		return "?"
	}
	return strings.TrimSpace(string(data))
}

func readPPID(pid int) int {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "PPid:") {
			if f := strings.Fields(line); len(f) >= 2 {
				v, _ := strconv.Atoi(f[1])
				return v
			}
		}
	}
	return 0
}

// readCgroupV2Path returns the cgroup v2 unified hierarchy path for a PID.
// /proc/<pid>/cgroup contains a single line "0::<path>" for cgroup v2.
func readCgroupV2Path(pid int) string {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "0::") {
			return strings.TrimPrefix(line, "0::")
		}
	}
	return ""
}

// ListPodProcesses returns all processes whose cgroup v2 path contains the
// given pod UID. Kubernetes places pod processes under:
//   /sys/fs/cgroup/kubepods/<qos>/<pod-uid>/...
//
// We identify them by matching the cgroup path read from /proc/<pid>/cgroup.
func ListPodProcesses(podUID string) ([]Process, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, fmt.Errorf("reading /proc: %w", err)
	}

	var result []Process
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		cgroup := readCgroupV2Path(pid)
		if !strings.Contains(cgroup, podUID) {
			continue
		}
		result = append(result, Process{
			PID:        pid,
			PPID:       readPPID(pid),
			Comm:       readComm(pid),
			PidNsID:    nsInode(pid),
			CgroupPath: cgroup,
		})
	}
	return result, nil
}

// NsSymlinks returns the namespace symlink targets for a given PID.
// Maps namespace name (e.g. "pid", "net") to its inode string (e.g. "pid:[4026531836]").
func NsSymlinks(pid int) (map[string]string, error) {
	nsDir := fmt.Sprintf("/proc/%d/ns", pid)
	entries, err := os.ReadDir(nsDir)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", nsDir, err)
	}
	result := make(map[string]string, len(entries))
	for _, e := range entries {
		target, err := os.Readlink(filepath.Join(nsDir, e.Name()))
		if err == nil {
			result[e.Name()] = target
		}
	}
	return result, nil
}

// NsInfo holds the namespace identifiers for one process in a pod.
// The NS map keys are namespace types ("net", "mnt", "pid", etc.) and
// values are inode numbers from /proc/<pid>/ns/.
// Processes with the same inode for a given type share that namespace instance.
// See include/linux/ns_common.h:struct ns_common for the kernel side.
type NsInfo struct {
	PID  int               `json:"pid"`
	Comm string            `json:"comm"`
	NS   map[string]uint64 `json:"ns"`
}

// podNsTypes is the set of namespace types inspected per pod process.
var podNsTypes = []string{"cgroup", "ipc", "mnt", "net", "pid", "user", "uts"}

func readNsInode(pid int, nstype string) uint64 {
	var st syscall.Stat_t
	if err := syscall.Stat(fmt.Sprintf("/proc/%d/ns/%s", pid, nstype), &st); err != nil {
		return 0
	}
	return st.Ino
}

// ListPodNamespaces returns per-process namespace info for all processes
// belonging to the given pod UID (matched via cgroup v2 path).
// It builds on ListPodProcesses and adds namespace inode data per process.
func ListPodNamespaces(podUID string) ([]NsInfo, error) {
	procs, err := ListPodProcesses(podUID)
	if err != nil {
		return nil, fmt.Errorf("listing pod processes: %w", err)
	}
	result := make([]NsInfo, 0, len(procs))
	for _, p := range procs {
		ns := make(map[string]uint64, len(podNsTypes))
		for _, t := range podNsTypes {
			ns[t] = readNsInode(p.PID, t)
		}
		result = append(result, NsInfo{
			PID:  p.PID,
			Comm: p.Comm,
			NS:   ns,
		})
	}
	return result, nil
}
