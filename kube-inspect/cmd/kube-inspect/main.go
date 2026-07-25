package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/linux-to-k8s/kube-inspect/internal/cgroup"
	"github.com/linux-to-k8s/kube-inspect/internal/proc"
)

var (
	flagPod        = flag.String("pod", "", "Pod UID to inspect")
	flagNode       = flag.Bool("node", false, "Inspect all pods on this node")
	flagJSON       = flag.Bool("json", false, "Output as JSON")
	flagNamespaces = flag.Bool("namespaces", false, "Show namespace inodes per process (requires --pod)")
	flagCgroup     = flag.Bool("cgroup", false, "Show cgroup v2 resource stats (requires --pod)")
)

func main() {
	flag.Parse()
	if *flagPod == "" && !*flagNode {
		fmt.Fprintln(os.Stderr, "usage: kube-inspect --pod <uid> [--namespaces] [--cgroup] [--json]")
		os.Exit(1)
	}

	if *flagPod != "" {
		procs, err := proc.ListPodProcesses(*flagPod)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		if *flagJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			enc.Encode(procs)
			return
		}
		fmt.Printf("Processes for pod %s:\n", *flagPod)
		fmt.Printf("  %-8s %-8s %-20s %-18s %s\n", "PID", "PPID", "COMM", "PID-NS-INODE", "CGROUP")
		for _, p := range procs {
			fmt.Printf("  %-8d %-8d %-20s %-18d %s\n",
				p.PID, p.PPID, p.Comm, p.PidNsID, p.CgroupPath)
		}
		fmt.Printf("Total: %d processes\n", len(procs))

		if *flagNamespaces {
			nsInfos, err := proc.ListPodNamespaces(*flagPod)
			if err != nil {
				fmt.Fprintf(os.Stderr, "namespaces error: %v\n", err)
			} else {
				fmt.Printf("\nNamespace inodes for pod %s:\n", *flagPod)
				fmt.Printf("  %-8s %-20s %-10s %-10s %-10s %-10s %-10s %-10s %-10s\n",
					"PID", "COMM", "cgroup", "ipc", "mnt", "net", "pid", "user", "uts")
				for _, n := range nsInfos {
					fmt.Printf("  %-8d %-20s %-10d %-10d %-10d %-10d %-10d %-10d %-10d\n",
						n.PID, n.Comm,
						n.NS["cgroup"], n.NS["ipc"], n.NS["mnt"],
						n.NS["net"], n.NS["pid"], n.NS["user"], n.NS["uts"])
				}
			}
		}

		if *flagCgroup {
			stats, err := cgroup.ReadPodStats(*flagPod)
			if err != nil {
				fmt.Fprintf(os.Stderr, "cgroup error: %v\n", err)
			} else {
				fmt.Printf("\nCgroup stats for pod %s:\n", *flagPod)
				fmt.Printf("  Path:             %s\n", stats.CgroupPath)
				fmt.Printf("  memory.current:   %d bytes\n", stats.MemoryCurrent)
				fmt.Printf("  memory.max:       %s\n", stats.MemoryMax)
				if stats.MemoryEvents != nil {
					fmt.Printf("  memory.oom_kill:  %d\n", stats.MemoryEvents["oom_kill"])
				}
				fmt.Printf("  cpu.max:          %s\n", stats.CPUMax)
				if stats.CPUStat != nil {
					fmt.Printf("  cpu.nr_throttled: %d\n", stats.CPUStat["nr_throttled"])
					fmt.Printf("  cpu.throttled_us: %d\n", stats.CPUStat["throttled_usec"])
				}
				fmt.Printf("  pids.current:     %d\n", stats.PidsCurrent)
				fmt.Printf("  pids.max:         %s\n", stats.PidsMax)
			}
		}
	}
}
