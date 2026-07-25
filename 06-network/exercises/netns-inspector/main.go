package main

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// NetInterface holds the parsed per-interface statistics from one line of
// /proc/<pid>/net/dev.
//
// The kernel populates this file via dev_seq_printf_stats() at:
// https://elixir.bootlin.com/linux/v6.9/source/net/core/net-procfs.c
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

// readNetDev reads and parses /proc/<pid>/net/dev, returning one NetInterface
// per non-loopback and loopback interface found.
//
// File format (from the kernel's dev_seq_printf_stats):
//
//	Inter-|   Receive                                                |  Transmit
//	 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
//	    lo:       0       0    0    0    0     0          0         0        0       0    0    0    0     0       0          0
//	  eth0: 1234567    8901    0    0    0     0          0         0   567890    1234    0    0    0     0       0          0
//
// The first two lines are headers and are skipped. Each subsequent line
// contains the interface name (with trailing ":") followed by 16 uint64
// counter values in receive then transmit order.
func readNetDev(pid int) ([]NetInterface, error) {
	path := fmt.Sprintf("/proc/%d/net/dev", pid)
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	defer f.Close()

	var ifaces []NetInterface
	scanner := bufio.NewScanner(f)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		// Skip the two header lines.
		if lineNum <= 2 {
			continue
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		iface, err := parseNetDevLine(line)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: skipping line: %v\n", err)
			continue
		}
		ifaces = append(ifaces, iface)
	}
	return ifaces, scanner.Err()
}

// parseNetDevLine parses a single data line from /proc/<pid>/net/dev.
//
// Each line has the form:
//
//	<iface>: rx_bytes rx_packets rx_errs rx_drop rx_fifo rx_frame rx_comp rx_multi tx_bytes tx_packets tx_errs tx_drop tx_fifo tx_colls tx_carr tx_comp
//
// We capture fields [0..3] from the rx side and fields [8..11] from the tx
// side (indices within the 16-field counter block).
func parseNetDevLine(line string) (NetInterface, error) {
	// Split on ":" first to separate name from counters.
	colonIdx := strings.Index(line, ":")
	if colonIdx < 0 {
		return NetInterface{}, fmt.Errorf("no ':' found in net/dev line: %q", line)
	}
	name := strings.TrimSpace(line[:colonIdx])
	rest := strings.TrimSpace(line[colonIdx+1:])

	fields := strings.Fields(rest)
	// We need at least 16 fields: 8 rx + 8 tx.
	if len(fields) < 16 {
		return NetInterface{}, fmt.Errorf("net/dev line for %q has %d fields, want >= 16", name, len(fields))
	}

	parse := func(s string) (uint64, error) {
		v, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("parsing %q: %w", s, err)
		}
		return v, nil
	}

	rxBytes, err := parse(fields[0])
	if err != nil {
		return NetInterface{}, err
	}
	rxPackets, err := parse(fields[1])
	if err != nil {
		return NetInterface{}, err
	}
	rxErrors, err := parse(fields[2])
	if err != nil {
		return NetInterface{}, err
	}
	rxDropped, err := parse(fields[3])
	if err != nil {
		return NetInterface{}, err
	}

	// Fields 4-7 are rx_fifo, rx_frame, rx_compressed, rx_multicast — skipped.

	txBytes, err := parse(fields[8])
	if err != nil {
		return NetInterface{}, err
	}
	txPackets, err := parse(fields[9])
	if err != nil {
		return NetInterface{}, err
	}
	txErrors, err := parse(fields[10])
	if err != nil {
		return NetInterface{}, err
	}
	txDropped, err := parse(fields[11])
	if err != nil {
		return NetInterface{}, err
	}

	return NetInterface{
		Name:      name,
		RxBytes:   rxBytes,
		RxPackets: rxPackets,
		RxErrors:  rxErrors,
		RxDropped: rxDropped,
		TxBytes:   txBytes,
		TxPackets: txPackets,
		TxErrors:  txErrors,
		TxDropped: txDropped,
	}, nil
}

// printTable prints a formatted table of interface statistics.
func printTable(pid int, ifaces []NetInterface) {
	fmt.Printf("Network interfaces for PID %d:\n", pid)
	fmt.Printf("  %-12s  %12s  %10s  %8s  %8s  %12s  %10s  %8s  %8s\n",
		"IFACE", "RX-BYTES", "RX-PKTS", "RX-ERR", "RX-DROP",
		"TX-BYTES", "TX-PKTS", "TX-ERR", "TX-DROP")
	for _, iface := range ifaces {
		fmt.Printf("  %-12s  %12d  %10d  %8d  %8d  %12d  %10d  %8d  %8d\n",
			iface.Name,
			iface.RxBytes, iface.RxPackets, iface.RxErrors, iface.RxDropped,
			iface.TxBytes, iface.TxPackets, iface.TxErrors, iface.TxDropped)
	}
}

// printDeltaTable prints per-second delta rates between two snapshots.
func printDeltaTable(pid int, prev, curr []NetInterface, elapsed time.Duration) {
	// Build a map from name -> stats for prev.
	prevMap := make(map[string]NetInterface, len(prev))
	for _, iface := range prev {
		prevMap[iface.Name] = iface
	}

	secs := elapsed.Seconds()
	if secs <= 0 {
		secs = 1
	}

	fmt.Printf("\nNetwork interface rates for PID %d (per second, interval %.1fs):\n", pid, secs)
	fmt.Printf("  %-12s  %14s  %12s  %14s  %12s\n",
		"IFACE", "RX-BYTES/s", "RX-PKTS/s", "TX-BYTES/s", "TX-PKTS/s")
	for _, c := range curr {
		p, ok := prevMap[c.Name]
		if !ok {
			p = NetInterface{}
		}
		rxBps := float64(c.RxBytes-p.RxBytes) / secs
		rxPps := float64(c.RxPackets-p.RxPackets) / secs
		txBps := float64(c.TxBytes-p.TxBytes) / secs
		txPps := float64(c.TxPackets-p.TxPackets) / secs
		fmt.Printf("  %-12s  %14.1f  %12.1f  %14.1f  %12.1f\n",
			c.Name, rxBps, rxPps, txBps, txPps)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: netns-inspector")
	fmt.Fprintln(os.Stderr, "       netns-inspector --pid <pid>")
	fmt.Fprintln(os.Stderr, "       netns-inspector --pid <pid> --watch")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "examples:")
	fmt.Fprintln(os.Stderr, "  netns-inspector                # current process network namespace")
	fmt.Fprintln(os.Stderr, "  netns-inspector --pid 1        # PID 1 (init/systemd) network namespace")
	fmt.Fprintln(os.Stderr, "  netns-inspector --pid 1 --watch  # refresh every 2s with delta rates")
}

func main() {
	pid := os.Getpid()
	watch := false

	args := os.Args[1:]
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--pid":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "error: --pid requires an argument")
				os.Exit(1)
			}
			i++
			v, err := strconv.Atoi(args[i])
			if err != nil {
				fmt.Fprintf(os.Stderr, "error: invalid PID %q\n", args[i])
				os.Exit(1)
			}
			pid = v
		case "--watch":
			watch = true
		case "--help", "-h":
			usage()
			os.Exit(0)
		default:
			fmt.Fprintf(os.Stderr, "error: unknown flag %q\n", args[i])
			usage()
			os.Exit(1)
		}
	}

	if !watch {
		ifaces, err := readNetDev(pid)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		printTable(pid, ifaces)
		return
	}

	// Watch mode: read initial snapshot, then loop every 2 seconds printing
	// per-second delta rates.
	prev, err := readNetDev(pid)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	prevTime := time.Now()

	// Print initial absolute snapshot so the user can see starting values.
	printTable(pid, prev)

	interval := 2 * time.Second
	for {
		time.Sleep(interval)

		curr, err := readNetDev(pid)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		now := time.Now()
		elapsed := now.Sub(prevTime)

		printDeltaTable(pid, prev, curr, elapsed)

		prev = curr
		prevTime = now
	}
}
