package ebpf

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/linux-to-k8s/kube-inspect/internal/proc"
)

// BPFProg holds parsed information from a BPF program FD's fdinfo entry.
type BPFProg struct {
	PID      int
	FD       int
	ProgType int    `json:"prog_type"`
	ProgID   int    `json:"prog_id"`
	Tag      string `json:"tag"`
	Jited    bool   `json:"jited"`
}

// BPFMap holds parsed information from a BPF map FD's fdinfo entry.
type BPFMap struct {
	PID        int
	FD         int
	MapType    int `json:"map_type"`
	MapID      int `json:"map_id"`
	KeySize    int `json:"key_size"`
	ValueSize  int `json:"value_size"`
	MaxEntries int `json:"max_entries"`
}

// BPFObjects groups all BPF programs and maps found for a pod.
type BPFObjects struct {
	Progs []BPFProg
	Maps  []BPFMap
}

// progTypeName maps BPF program type numbers to their canonical names.
// These values are stable kernel ABI from include/uapi/linux/bpf.h.
var progTypeName = map[int]string{
	0:  "UNSPEC",
	1:  "SOCKET_FILTER",
	2:  "KPROBE",
	3:  "SCHED_CLS",
	4:  "SCHED_ACT",
	5:  "TRACEPOINT",
	6:  "XDP",
	7:  "PERF_EVENT",
	8:  "CGROUP_SKB",
	9:  "CGROUP_SOCK",
	10: "LWT_IN",
	11: "LWT_OUT",
	12: "LWT_XMIT",
	13: "SOCK_OPS",
	14: "SK_SKB",
	15: "CGROUP_DEVICE",
	16: "SK_MSG",
	17: "RAW_TRACEPOINT",
	18: "CGROUP_SOCK_ADDR",
	19: "LWT_SEG6LOCAL",
	20: "LIRC_MODE2",
	21: "SK_REUSEPORT",
	22: "FLOW_DISSECTOR",
	23: "CGROUP_SYSCTL",
	24: "RAW_TRACEPOINT_WRITABLE",
	25: "CGROUP_SOCKOPT",
	26: "TRACING",
	27: "STRUCT_OPS",
	28: "EXT",
	29: "LSM",
	30: "SK_LOOKUP",
	31: "SYSCALL",
}

// mapTypeName maps BPF map type numbers to their canonical names.
// These values are stable kernel ABI from include/uapi/linux/bpf.h.
var mapTypeName = map[int]string{
	0:  "UNSPEC",
	1:  "HASH",
	2:  "ARRAY",
	3:  "PROG_ARRAY",
	4:  "PERF_EVENT_ARRAY",
	5:  "PERCPU_HASH",
	6:  "PERCPU_ARRAY",
	7:  "STACK_TRACE",
	8:  "CGROUP_ARRAY",
	9:  "LRU_HASH",
	10: "LRU_PERCPU_HASH",
	11: "LPM_TRIE",
	12: "ARRAY_OF_MAPS",
	13: "HASH_OF_MAPS",
	14: "DEVMAP",
	15: "SOCKMAP",
	16: "CPUMAP",
	17: "XSKMAP",
	18: "SOCKHASH",
	19: "CGROUP_STORAGE",
	20: "REUSEPORT_SOCKARRAY",
	21: "PERCPU_CGROUP_STORAGE",
	22: "QUEUE",
	23: "STACK",
	24: "SK_STORAGE",
	25: "DEVMAP_HASH",
	26: "STRUCT_OPS",
	27: "RINGBUF",
	28: "INODE_STORAGE",
	29: "TASK_STORAGE",
	30: "BLOOM_FILTER",
	31: "USER_RINGBUF",
	32: "CGROUP_STORAGE_PERCPU",
}

// ProgTypeName returns the human-readable name for a BPF program type.
func ProgTypeName(t int) string {
	if name, ok := progTypeName[t]; ok {
		return name
	}
	return "UNKNOWN"
}

// MapTypeName returns the human-readable name for a BPF map type.
func MapTypeName(t int) string {
	if name, ok := mapTypeName[t]; ok {
		return name
	}
	return "UNKNOWN"
}

// parseFDInfo reads /proc/<pid>/fdinfo/<fd> and returns key-value pairs.
func parseFDInfo(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	fields := make(map[string]string)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		parts := strings.SplitN(line, ":", 2)
		if len(parts) == 2 {
			key := strings.TrimSpace(parts[0])
			val := strings.TrimSpace(parts[1])
			fields[key] = val
		}
	}
	return fields, scanner.Err()
}

// parseInt looks up a key in the fields map and parses it as int; returns 0 on failure.
func parseInt(fields map[string]string, key string) int {
	s, ok := fields[key]
	if !ok {
		return 0
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return v
}

// scanPIDFDs walks /proc/<pid>/fd/ and returns all BPF programs and maps found.
func scanPIDFDs(pid int) ([]BPFProg, []BPFMap, error) {
	fdDir := fmt.Sprintf("/proc/%d/fd", pid)
	entries, err := os.ReadDir(fdDir)
	if err != nil {
		// The process may have exited between listing and reading; skip quietly.
		return nil, nil, nil
	}

	var progs []BPFProg
	var maps []BPFMap

	for _, entry := range entries {
		fdStr := entry.Name()
		fd, err := strconv.Atoi(fdStr)
		if err != nil {
			continue
		}

		fdinfoPath := filepath.Join(fmt.Sprintf("/proc/%d/fdinfo", pid), fdStr)
		fields, err := parseFDInfo(fdinfoPath)
		if err != nil {
			// FD may have closed; skip.
			continue
		}

		if _, hasProg := fields["prog_type"]; hasProg {
			jited := parseInt(fields, "prog_jited") != 0
			progs = append(progs, BPFProg{
				PID:      pid,
				FD:       fd,
				ProgType: parseInt(fields, "prog_type"),
				ProgID:   parseInt(fields, "prog_id"),
				Tag:      fields["prog_tag"],
				Jited:    jited,
			})
		} else if _, hasMap := fields["map_type"]; hasMap {
			maps = append(maps, BPFMap{
				PID:        pid,
				FD:         fd,
				MapType:    parseInt(fields, "map_type"),
				MapID:      parseInt(fields, "map_id"),
				KeySize:    parseInt(fields, "key_size"),
				ValueSize:  parseInt(fields, "value_size"),
				MaxEntries: parseInt(fields, "max_entries"),
			})
		}
	}

	return progs, maps, nil
}

// ListPodBPFObjects enumerates BPF programs and maps held open by all processes
// in the given pod (identified by pod UID in their cgroup v2 path).
// Objects are deduplicated by prog_id / map_id so the same kernel object
// appearing in multiple processes is only reported once.
func ListPodBPFObjects(podUID string) (BPFObjects, error) {
	processes, err := proc.ListPodProcesses(podUID)
	if err != nil {
		return BPFObjects{}, fmt.Errorf("listing pod processes: %w", err)
	}
	if len(processes) == 0 {
		return BPFObjects{}, fmt.Errorf("no processes found for pod %s", podUID)
	}

	seenProgs := make(map[int]bool)
	seenMaps := make(map[int]bool)
	var result BPFObjects

	for _, p := range processes {
		progs, maps, err := scanPIDFDs(p.PID)
		if err != nil {
			// Non-fatal; process may have exited.
			continue
		}
		for _, prog := range progs {
			if prog.ProgID != 0 && seenProgs[prog.ProgID] {
				continue
			}
			seenProgs[prog.ProgID] = true
			result.Progs = append(result.Progs, prog)
		}
		for _, m := range maps {
			if m.MapID != 0 && seenMaps[m.MapID] {
				continue
			}
			seenMaps[m.MapID] = true
			result.Maps = append(result.Maps, m)
		}
	}

	return result, nil
}
