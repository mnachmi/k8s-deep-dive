package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/linux-to-k8s/kube-inspect/internal/cgroup"
	"github.com/linux-to-k8s/kube-inspect/internal/ebpf"
	"github.com/linux-to-k8s/kube-inspect/internal/health"
	"github.com/linux-to-k8s/kube-inspect/internal/kubelet"
	"github.com/linux-to-k8s/kube-inspect/internal/metrics"
	"github.com/linux-to-k8s/kube-inspect/internal/netns"
	"github.com/linux-to-k8s/kube-inspect/internal/proc"
	"github.com/linux-to-k8s/kube-inspect/internal/sched"
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
	flagSched      = flag.Bool("sched", false, "Show CPU/NUMA affinity and cgroup cpu.weight/cpu.max for the pod (requires --pod)")
	flagPressure   = flag.Bool("pressure", false, "Show node PSI and pod memory.events (requires --pod for pod events)")
	flagPerf       = flag.Bool("perf", false, "Show CPU throttle stats and scheduler latency per process (requires --pod)")
	flagHealth     = flag.Bool("health", false, "Show kernel version, taint flags, watchdog and panic config, kdump state")
)

func main() {
	flag.Parse()
	if *flagPod == "" && !*flagNode && !*flagHealth {
		fmt.Fprintln(os.Stderr, "usage: kube-inspect --pod <uid> [--namespaces] [--cgroup] [--psi] [--mounts] [--netns] [--ebpf] [--sched] [--pressure] [--perf] [--health] [--json]")
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

		if *flagSched {
			info, err := sched.GetPodSchedInfo(*flagPod)
			if err != nil {
				fmt.Fprintf(os.Stderr, "sched: %v\n", err)
			} else {
				fmt.Printf("Scheduler info for pod %s:\n\n", info.PodUID)
				if info.CgroupCPUs != "" {
					fmt.Printf("  cgroup cpuset.cpus  : %s\n", info.CgroupCPUs)
					fmt.Printf("  cgroup cpuset.mems  : %s\n", info.CgroupMems)
					fmt.Printf("  cgroup cpu.weight   : %s\n", info.CPUWeight)
					fmt.Printf("  cgroup cpu.max      : %s\n", info.CPUMax)
				} else {
					fmt.Printf("  (cgroup path not found for pod %s)\n", info.PodUID)
				}
				if len(info.ProcessAffinities) > 0 {
					fmt.Printf("\n  Process affinities (deduplicated):\n")
					for _, pa := range info.ProcessAffinities {
						fmt.Printf("    PID=%-6d  comm=%-16s  cpus=%-10s  mems=%s\n",
							pa.PID, pa.Comm, pa.CpusAllowedList, pa.MemsAllowedList)
					}
				}
				fmt.Println()
			}
		}

		if *flagPressure {
			np, err := kubelet.GetNodePressure()
			if err != nil {
				fmt.Fprintf(os.Stderr, "pressure: %v\n", err)
			} else {
				fmt.Printf("Node pressure (PSI avg10):\n")
				fmt.Printf("  memory some=%.2f%%  full=%.2f%%\n", np.MemorySomeAvg10, np.MemoryFullAvg10)
				fmt.Printf("  cpu    some=%.2f%%\n", np.CPUSomeAvg10)
				fmt.Printf("  io     some=%.2f%%  full=%.2f%%\n", np.IOSomeAvg10, np.IOFullAvg10)
				fmt.Println()
			}
			ev, err := kubelet.GetPodMemEvents(*flagPod)
			if err != nil {
				fmt.Fprintf(os.Stderr, "pod memory events: %v\n", err)
			} else {
				fmt.Printf("Pod %s memory.events:\n", ev.PodUID)
				fmt.Printf("  low=%d  high=%d  max=%d  oom=%d  oom_kill=%d\n",
					ev.Low, ev.High, ev.Max, ev.OOM, ev.OOMKill)
				fmt.Println()
			}
		}

		if *flagPerf {
			report, err := metrics.GetPodPerfReport(*flagPod)
			if err != nil {
				fmt.Fprintf(os.Stderr, "perf: %v\n", err)
			} else {
				t := report.Throttle
				fmt.Printf("CPU throttle for pod %s:\n", report.PodUID)
				fmt.Printf("  cgroup:         %s\n", t.CgPath)
				fmt.Printf("  usage_usec:     %d\n", t.UsageUS)
				fmt.Printf("  nr_periods:     %d\n", t.NrPeriods)
				fmt.Printf("  nr_throttled:   %d\n", t.NrThrottled)
				fmt.Printf("  throttled_usec: %d\n", t.ThrottledUS)
				fmt.Printf("  throttle_ratio: %.1f%%\n", t.ThrottleRatio()*100)
				fmt.Println()
				if len(report.Processes) > 0 {
					fmt.Printf("Scheduler latency (top processes by wait ratio):\n")
					fmt.Printf("  %-8s %-16s %12s %12s %8s %10s\n",
						"PID", "COMM", "RUNTIME_MS", "WAIT_MS", "WAIT%", "SWITCHES")
					for _, p := range report.Processes {
						fmt.Printf("  %-8d %-16s %12.1f %12.1f %7.1f%% %10d\n",
							p.PID, p.Comm,
							float64(p.RuntimeNS)/1e6,
							float64(p.WaitNS)/1e6,
							p.WaitRatio()*100,
							p.NrSwitches)
					}
					fmt.Println()
				}
			}
		}
	}

	if *flagHealth {
		nh, err := health.GetNodeHealth()
		if err != nil {
			fmt.Fprintf(os.Stderr, "health: %v\n", err)
		} else {
			fmt.Printf("Node kernel health:\n")
			fmt.Printf("  release:          %s\n", nh.Release)
			if nh.TaintRaw == 0 {
				fmt.Printf("  tainted:          0 (clean)\n")
			} else {
				fmt.Printf("  tainted:          %d\n", nh.TaintRaw)
				for _, t := range nh.ActiveTaints {
					fmt.Printf("    bit %2d (%s): %s\n", t.Bit, t.Code, t.Meaning)
				}
			}
			fmt.Printf("  panic:            %d  panic_on_oops: %d  softlockup_panic: %d\n",
				nh.PanicTimeout, nh.PanicOnOops, nh.SoftlockupPanic)
			fmt.Printf("  nmi_watchdog:     %d  watchdog_thresh: %ds\n",
				nh.NMIWatchdog, nh.WatchdogThresh)
			if nh.KexecLoaded == 1 {
				fmt.Printf("  kdump:            ready (crashkernel=%s)\n", nh.CrashKernel)
			} else {
				fmt.Printf("  kdump:            not loaded\n")
			}
			fmt.Println()
		}
	}
}
