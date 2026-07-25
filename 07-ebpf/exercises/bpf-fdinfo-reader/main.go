package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// BPFProgInfo holds parsed information from a BPF program FD's fdinfo entry.
type BPFProgInfo struct {
	FD       int
	ProgType int
	ProgID   int
	Tag      string
	Jited    bool
}

// BPFMapInfo holds parsed information from a BPF map FD's fdinfo entry.
type BPFMapInfo struct {
	FD         int
	MapType    int
	MapID      int
	KeySize    int
	ValueSize  int
	MaxEntries int
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

// progTypeLookup returns the name for a prog type, or "UNKNOWN" if not found.
func progTypeLookup(t int) string {
	if name, ok := progTypeName[t]; ok {
		return name
	}
	return "UNKNOWN"
}

// mapTypeLookup returns the name for a map type, or "UNKNOWN" if not found.
func mapTypeLookup(t int) string {
	if name, ok := mapTypeName[t]; ok {
		return name
	}
	return "UNKNOWN"
}

// parseFDInfo reads /proc/<pid>/fdinfo/<fd> and returns parsed key-value pairs
// along with the raw lines for verbose output.
func parseFDInfo(path string) (map[string]string, []string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}

	fields := make(map[string]string)
	var rawLines []string

	for _, line := range strings.Split(string(data), "\n") {
		if line == "" {
			continue
		}
		rawLines = append(rawLines, line)
		parts := strings.SplitN(line, ":", 2)
		if len(parts) == 2 {
			key := strings.TrimSpace(parts[0])
			val := strings.TrimSpace(parts[1])
			fields[key] = val
		}
	}
	return fields, rawLines, nil
}

// parseInt parses a string field into an int, returning 0 on failure.
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

// scanBPFFDs walks /proc/<pid>/fd/ and returns all BPF programs and maps found.
func scanBPFFDs(pid int, verbose bool) ([]BPFProgInfo, []BPFMapInfo, error) {
	fdDir := fmt.Sprintf("/proc/%d/fd", pid)
	entries, err := os.ReadDir(fdDir)
	if err != nil {
		return nil, nil, fmt.Errorf("cannot read %s: %w", fdDir, err)
	}

	var progs []BPFProgInfo
	var maps []BPFMapInfo

	for _, entry := range entries {
		fdStr := entry.Name()
		fd, err := strconv.Atoi(fdStr)
		if err != nil {
			continue
		}

		// Resolve symlink to confirm it's a real fd (ignore errors)
		fdPath := filepath.Join(fdDir, fdStr)
		target, _ := os.Readlink(fdPath)
		_ = target // we use fdinfo, not the symlink target

		fdinfoPath := fmt.Sprintf("/proc/%d/fdinfo/%s", pid, fdStr)
		fields, rawLines, err := parseFDInfo(fdinfoPath)
		if err != nil {
			// Skip FDs we can't read (e.g., closed between listing and reading)
			continue
		}

		if verbose {
			fmt.Printf("  --- fdinfo for fd=%d ---\n", fd)
			for _, l := range rawLines {
				fmt.Printf("    %s\n", l)
			}
		}

		if _, hasProg := fields["prog_type"]; hasProg {
			jited := parseInt(fields, "prog_jited") != 0
			info := BPFProgInfo{
				FD:       fd,
				ProgType: parseInt(fields, "prog_type"),
				ProgID:   parseInt(fields, "prog_id"),
				Tag:      fields["prog_tag"],
				Jited:    jited,
			}
			progs = append(progs, info)
		} else if _, hasMap := fields["map_type"]; hasMap {
			info := BPFMapInfo{
				FD:         fd,
				MapType:    parseInt(fields, "map_type"),
				MapID:      parseInt(fields, "map_id"),
				KeySize:    parseInt(fields, "key_size"),
				ValueSize:  parseInt(fields, "value_size"),
				MaxEntries: parseInt(fields, "max_entries"),
			}
			maps = append(maps, info)
		}
	}

	return progs, maps, nil
}

func main() {
	pid := flag.Int("pid", 0, "PID to inspect (required)")
	verbose := flag.Bool("verbose", false, "print raw fdinfo lines alongside parsed output")
	flag.Parse()

	if *pid == 0 {
		fmt.Fprintln(os.Stderr, "error: --pid is required")
		flag.Usage()
		os.Exit(1)
	}

	fmt.Printf("BPF objects for PID %d:\n", *pid)

	progs, maps, err := scanBPFFDs(*pid, *verbose)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	fmt.Println()
	fmt.Println("Programs:")
	if len(progs) == 0 {
		fmt.Println("  (none)")
	} else {
		for _, p := range progs {
			fmt.Printf("  fd=%-4d prog_type=%-2d (%s)  prog_id=%-6d tag=%-16s  jited=%v\n",
				p.FD, p.ProgType, progTypeLookup(p.ProgType),
				p.ProgID, p.Tag, p.Jited)
		}
	}

	fmt.Println()
	fmt.Println("Maps:")
	if len(maps) == 0 {
		fmt.Println("  (none)")
	} else {
		for _, m := range maps {
			fmt.Printf("  fd=%-4d map_type=%-2d (%s)  map_id=%-6d key_size=%-4d value_size=%-4d max_entries=%d\n",
				m.FD, m.MapType, mapTypeLookup(m.MapType),
				m.MapID, m.KeySize, m.ValueSize, m.MaxEntries)
		}
	}
}
