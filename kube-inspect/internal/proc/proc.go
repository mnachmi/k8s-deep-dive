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

// MountInfo represents one line from /proc/pid/mountinfo.
// Format: <MountID> <ParentID> <Major>:<Minor> <Root> <MountPoint> <Options> [optfields...] - <FSType> <Source> <SuperOpts>
// See kernel/05-b-mount.md for field-by-field explanation.
type MountInfo struct {
	MountID    int    `json:"mount_id"`
	ParentID   int    `json:"parent_id"`
	Root       string `json:"root"`
	MountPoint string `json:"mount_point"`
	Options    string `json:"options"`
	FSType     string `json:"fstype"`
	Source     string `json:"source"`
	SuperOpts  string `json:"super_opts"`
}

// ListPodMounts returns the mount table for the first process belonging to
// the given pod UID, read from /proc/<pid>/mountinfo.
func ListPodMounts(podUID string) ([]MountInfo, error) {
	procs, err := ListPodProcesses(podUID)
	if err != nil {
		return nil, fmt.Errorf("listing pod processes: %w", err)
	}
	if len(procs) == 0 {
		return nil, fmt.Errorf("no processes found for pod %s", podUID)
	}
	pid := procs[0].PID
	path := fmt.Sprintf("/proc/%d/mountinfo", pid)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var mounts []MountInfo
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		m, err := parseMountInfoLine(line)
		if err != nil {
			continue
		}
		mounts = append(mounts, m)
	}
	return mounts, nil
}

// parseMountInfoLine parses a single line from /proc/<pid>/mountinfo.
// Format: mountID parentID major:minor root mountPoint options [optfields...] - fstype source superOpts
func parseMountInfoLine(line string) (MountInfo, error) {
	fields := strings.Fields(line)
	// Need at least: 0=mountID 1=parentID 2=major:minor 3=root 4=mountPoint 5=options then "-" fstype source superOpts
	if len(fields) < 7 {
		return MountInfo{}, fmt.Errorf("mountinfo: too few fields: %q", line)
	}
	mountID, err := strconv.Atoi(fields[0])
	if err != nil {
		return MountInfo{}, fmt.Errorf("mountinfo: bad mount_id %q: %w", fields[0], err)
	}
	parentID, err := strconv.Atoi(fields[1])
	if err != nil {
		return MountInfo{}, fmt.Errorf("mountinfo: bad parent_id %q: %w", fields[1], err)
	}
	root := fields[3]
	mountPoint := fields[4]
	options := fields[5]

	// Scan for the separator "-" which marks the start of the per-filesystem fields.
	sepIdx := -1
	for i := 6; i < len(fields); i++ {
		if fields[i] == "-" {
			sepIdx = i
			break
		}
	}
	if sepIdx == -1 || len(fields) < sepIdx+4 {
		return MountInfo{}, fmt.Errorf("mountinfo: missing '-' separator in %q", line)
	}
	fstype := fields[sepIdx+1]
	source := fields[sepIdx+2]
	superOpts := fields[sepIdx+3]

	return MountInfo{
		MountID:    mountID,
		ParentID:   parentID,
		Root:       root,
		MountPoint: mountPoint,
		Options:    options,
		FSType:     fstype,
		Source:     source,
		SuperOpts:  superOpts,
	}, nil
}

// CountOverlayLayers returns the number of lower layers in the overlay mount
// at "/" for the given pod. It calls ListPodMounts and inspects the SuperOpts
// field for "lowerdir=<a>:<b>:..." to count the colon-separated entries.
// Returns 0, nil if no overlay mount on "/" is found.
func CountOverlayLayers(podUID string) (int, error) {
	mounts, err := ListPodMounts(podUID)
	if err != nil {
		return 0, fmt.Errorf("listing pod mounts: %w", err)
	}
	for _, m := range mounts {
		if m.FSType != "overlay" || m.MountPoint != "/" {
			continue
		}
		for _, opt := range strings.Split(m.SuperOpts, ",") {
			if strings.HasPrefix(opt, "lowerdir=") {
				val := strings.TrimPrefix(opt, "lowerdir=")
				return strings.Count(val, ":") + 1, nil
			}
		}
		// overlay mount found but no lowerdir — treat as 1 layer
		return 1, nil
	}
	return 0, nil
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
