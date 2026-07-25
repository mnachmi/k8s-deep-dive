package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/linux-to-k8s/kube-inspect/internal/cgroup"
	"github.com/linux-to-k8s/kube-inspect/internal/ebpf"
	"github.com/linux-to-k8s/kube-inspect/internal/netns"
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
	flagNetNS      = flag.Bool("netns", false, "Show network namespace interface stats (requires --pod)")
	flagEBPF       = flag.Bool("ebpf", false, "Show BPF objects (programs and maps) per pod (requires --pod)")
)

func main() {
	flag.Parse()
	if *flagPod == "" && !*flagNode {
		fmt.Fprintln(os.Stderr, "usage: kube-inspect --pod <uid> [--namespaces] [--cgroup] [--psi] [--mounts] [--netns] [--ebpf] [--json]")
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
						layers := 0
						for _, opt := range strings.Split(m.SuperOpts, ",") {
							if strings.HasPrefix(opt, "lowerdir=") {
								val := strings.TrimPrefix(opt, "lowerdir=")
								layers = strings.Count(val, ":") + 1
							}
						}
						if layers > 0 {
							fmt.Printf("    overlay layers: %d\n", layers)
						}
					}
				}
			}
		}

		if *flagNetNS {
			ifaces, err := netns.ListPodInterfaces(*flagPod)
			if err != nil {
				fmt.Fprintf(os.Stderr, "netns error: %v\n", err)
			} else {
				fmt.Printf("\nNetwork interfaces for pod %s:\n", *flagPod)
				fmt.Printf("  %-12s %-12s %-8s %-8s %-8s %-12s %-8s %-8s %-8s\n",
					"INTERFACE", "RX-BYTES", "RX-PKTS", "RX-ERR", "RX-DROP",
					"TX-BYTES", "TX-PKTS", "TX-ERR", "TX-DROP")
				for _, iface := range ifaces {
					fmt.Printf("  %-12s %-12d %-8d %-8d %-8d %-12d %-8d %-8d %-8d\n",
						iface.Name,
						iface.RxBytes, iface.RxPackets, iface.RxErrors, iface.RxDropped,
						iface.TxBytes, iface.TxPackets, iface.TxErrors, iface.TxDropped)
				}
			}
		}

		if *flagEBPF {
			objs, err := ebpf.ListPodBPFObjects(*flagPod)
			if err != nil {
				fmt.Fprintf(os.Stderr, "ebpf error: %v\n", err)
			} else {
				fmt.Printf("\nBPF objects for pod %s:\n", *flagPod)
				fmt.Printf("\n  Programs (%d):\n", len(objs.Progs))
				if len(objs.Progs) == 0 {
					fmt.Println("    (none)")
				} else {
					for _, p := range objs.Progs {
						fmt.Printf("    PID=%-6d fd=%-4d type=%d (%s)  id=%-6d tag=%-16s  jited=%v\n",
							p.PID, p.FD, p.ProgType, ebpf.ProgTypeName(p.ProgType),
							p.ProgID, p.Tag, p.Jited)
					}
				}
				fmt.Printf("\n  Maps (%d):\n", len(objs.Maps))
				if len(objs.Maps) == 0 {
					fmt.Println("    (none)")
				} else {
					for _, m := range objs.Maps {
						fmt.Printf("    PID=%-6d fd=%-4d type=%d (%s)  id=%-6d key=%-4d value=%-4d max=%d\n",
							m.PID, m.FD, m.MapType, ebpf.MapTypeName(m.MapType),
							m.MapID, m.KeySize, m.ValueSize, m.MaxEntries)
					}
				}
			}
		}
	}
}
