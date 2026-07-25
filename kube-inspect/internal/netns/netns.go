package netns

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/linux-to-k8s/kube-inspect/internal/proc"
)

// NetInterface holds per-interface counters from /proc/<pid>/net/dev.
// Fields mirror the rx/tx columns in the kernel's seq_file output for
// struct net_device — see net/core/net-procfs.c:dev_seq_printf_stats().
type NetInterface struct {
	Name      string
	RxBytes   uint64
	RxPackets uint64
	RxErrors  uint64
	RxDropped uint64
	TxBytes   uint64
	TxPackets uint64
	TxErrors  uint64
	TxDropped uint64
}

// ListPodInterfaces returns network interface stats for the given pod.
// It locates the pod's first process via proc.ListPodProcesses, then
// reads /proc/<pid>/net/dev which is scoped to that process's net namespace.
func ListPodInterfaces(podUID string) ([]NetInterface, error) {
	procs, err := proc.ListPodProcesses(podUID)
	if err != nil || len(procs) == 0 {
		return nil, fmt.Errorf("no processes for pod %s: %w", podUID, err)
	}
	return readNetDev(procs[0].PID)
}

// readNetDev parses /proc/<pid>/net/dev and returns one NetInterface per row.
//
// File format (two header lines followed by data):
//
//	Inter-|   Receive                                                |  Transmit
//	 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
//	    lo: 1024      12    0    0    0     0          0         0     1024      12    0    0    0     0       0          0
//
// Fields after the ":" are space-separated. The 16 values map to:
//
//	RX: [0]=bytes [1]=packets [2]=errs [3]=drop [4]=fifo [5]=frame [6]=compressed [7]=multicast
//	TX: [8]=bytes [9]=packets [10]=errs [11]=drop [12]=fifo [13]=colls [14]=carrier [15]=compressed
func readNetDev(pid int) ([]NetInterface, error) {
	path := fmt.Sprintf("/proc/%d/net/dev", pid)
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	defer f.Close()

	var ifaces []NetInterface
	lineNum := 0
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		lineNum++
		if lineNum <= 2 {
			// skip both header lines
			continue
		}
		line := scanner.Text()
		// Split on ":" to separate interface name from counters
		colonIdx := strings.Index(line, ":")
		if colonIdx == -1 {
			continue
		}
		name := strings.TrimSpace(line[:colonIdx])
		rest := strings.TrimSpace(line[colonIdx+1:])
		fields := strings.Fields(rest)
		if len(fields) < 16 {
			continue
		}
		parse := func(s string) uint64 {
			v, _ := strconv.ParseUint(s, 10, 64)
			return v
		}
		ifaces = append(ifaces, NetInterface{
			Name:      name,
			RxBytes:   parse(fields[0]),
			RxPackets: parse(fields[1]),
			RxErrors:  parse(fields[2]),
			RxDropped: parse(fields[3]),
			TxBytes:   parse(fields[8]),
			TxPackets: parse(fields[9]),
			TxErrors:  parse(fields[10]),
			TxDropped: parse(fields[11]),
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scanning %s: %w", path, err)
	}
	return ifaces, nil
}
