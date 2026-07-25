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
	flagPSI        = flag.Bool("psi", false, "Show PSI pressure metrics and OOM events (requires --pod)")
	flagMounts     = flag.Bool("mounts", false, "Show mount namespace table and overlay layer count (requires --pod)")
)

func main() {
	flag.Parse()
	if *flagPod == "" && !*flagNode {
		fmt.Fprintln(os.Stderr, "usage: kube-inspect --pod <uid> [--namespaces] [--cgroup] [--psi] [--mounts] [--json]")
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

		if *flagPSI {
			psi, err := cgroup.ReadPodPSI(*flagPod)
			if err != nil {
				fmt.Fprintf(os.Stderr, "psi error: %v\n", err)
			} else {
				fmt.Printf("\nPSI pressure for pod %s:\n", *flagPod)
				fmt.Printf("  Path:              %s\n", psi.CgroupPath)
				fmt.Printf("  memory.some  avg10=%.2f avg60=%.2f avg300=%.2f\n",
					psi.MemorySome.Avg10, psi.MemorySome.Avg60, psi.MemorySome.Avg300)
				fmt.Printf("  memory.full  avg10=%.2f avg60=%.2f avg300=%.2f\n",
					psi.MemoryFull.Avg10, psi.MemoryFull.Avg60, psi.MemoryFull.Avg300)
				fmt.Printf("  cpu.some     avg10=%.2f avg60=%.2f avg300=%.2f\n",
					psi.CPUSome.Avg10, psi.CPUSome.Avg60, psi.CPUSome.Avg300)
				fmt.Printf("  io.some      avg10=%.2f avg60=%.2f avg300=%.2f\n",
					psi.IOSome.Avg10, psi.IOSome.Avg60, psi.IOSome.Avg300)
				fmt.Printf("  oom_events:  %d  oom_kills: %d\n",
					psi.OOMCount, psi.OOMKillCount)
			}
		}

		if *flagMounts {
			mounts, err := proc.ListPodMounts(*flagPod)
			if err != nil {
				fmt.Fprintf(os.Stderr, "mounts error: %v\n", err)
			} else {
				fmt.Printf("\nMounts for pod %s:\n", *flagPod)
				fmt.Printf("  %-6s  %-6s  %-11s %-20s %s\n",
					"ID", "PARENT", "FSTYPE", "SOURCE", "MOUNTPOINT")
				for _, m := range mounts {
					fmt.Printf("  %-6d  %-6d  %-11s %-20s %s\n",
						m.MountID, m.ParentID, m.FSType, m.Source, m.MountPoint)
					if m.FSType == "overlay" && m.MountPoint == "/" {
						layers, lerr := proc.CountOverlayLayers(*flagPod)
						if lerr != nil {
							fmt.Fprintf(os.Stderr, "overlay layers error: %v\n", lerr)
						} else {
							fmt.Printf("    overlay layers: %d\n", layers)
						}
					}
				}
			}
		}
	}
}
