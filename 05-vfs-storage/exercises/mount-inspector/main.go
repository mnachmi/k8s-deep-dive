package main

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// MountInfo holds the parsed fields from one line of /proc/<pid>/mountinfo.
//
// The kernel emits these fields in show_mountinfo() at:
// https://elixir.bootlin.com/linux/v6.9/source/fs/proc_namespace.c
type MountInfo struct {
	MountID        int
	ParentID       int
	Major, Minor   int
	Root           string
	MountPoint     string
	MountOptions   string
	OptionalFields []string
	FSType         string
	Source         string
	SuperOptions   string
}

// parseMountInfo parses a single line from /proc/<pid>/mountinfo.
//
// Line format (from kernel fs/proc_namespace.c show_mountinfo()):
//
//	<MountID> <ParentID> <Major>:<Minor> <Root> <MountPoint> <MountOptions> [optfields...] - <FSType> <Source> <SuperOptions>
//
// The optional fields section is terminated by a lone "-".
func parseMountInfo(line string) (*MountInfo, error) {
	fields := strings.Fields(line)
	// Minimum viable line: 6 fixed fields + "-" + FSType + Source + SuperOptions = 10 fields
	if len(fields) < 10 {
		return nil, fmt.Errorf("mountinfo line too short: %q", line)
	}

	mi := &MountInfo{}

	// Field 0: MountID
	v, err := strconv.Atoi(fields[0])
	if err != nil {
		return nil, fmt.Errorf("bad MountID %q: %w", fields[0], err)
	}
	mi.MountID = v

	// Field 1: ParentID
	v, err = strconv.Atoi(fields[1])
	if err != nil {
		return nil, fmt.Errorf("bad ParentID %q: %w", fields[1], err)
	}
	mi.ParentID = v

	// Field 2: Major:Minor
	mm := strings.SplitN(fields[2], ":", 2)
	if len(mm) != 2 {
		return nil, fmt.Errorf("bad Major:Minor %q", fields[2])
	}
	mi.Major, err = strconv.Atoi(mm[0])
	if err != nil {
		return nil, fmt.Errorf("bad Major %q: %w", mm[0], err)
	}
	mi.Minor, err = strconv.Atoi(mm[1])
	if err != nil {
		return nil, fmt.Errorf("bad Minor %q: %w", mm[1], err)
	}

	// Field 3: Root
	mi.Root = fields[3]

	// Field 4: MountPoint
	mi.MountPoint = fields[4]

	// Field 5: MountOptions
	mi.MountOptions = fields[5]

	// Fields 6..N-4: optional fields, terminated by a lone "-"
	i := 6
	for i < len(fields) && fields[i] != "-" {
		mi.OptionalFields = append(mi.OptionalFields, fields[i])
		i++
	}

	// fields[i] must be "-"
	if i >= len(fields) || fields[i] != "-" {
		return nil, fmt.Errorf("missing '-' separator in mountinfo line: %q", line)
	}
	i++ // skip "-"

	// After "-": FSType, Source, SuperOptions
	if i+2 >= len(fields) {
		return nil, fmt.Errorf("missing post-separator fields in mountinfo line: %q", line)
	}
	mi.FSType = fields[i]
	mi.Source = fields[i+1]
	mi.SuperOptions = fields[i+2]

	return mi, nil
}

// readMountInfo reads and parses all lines from /proc/<pid>/mountinfo.
func readMountInfo(pid int) ([]*MountInfo, error) {
	path := fmt.Sprintf("/proc/%d/mountinfo", pid)
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	defer f.Close()

	var mounts []*MountInfo
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		mi, err := parseMountInfo(line)
		if err != nil {
			// Skip unparseable lines rather than aborting
			fmt.Fprintf(os.Stderr, "warning: skipping line: %v\n", err)
			continue
		}
		mounts = append(mounts, mi)
	}
	return mounts, scanner.Err()
}

// overlayLayerCount counts the number of lower layers in an overlay mount.
// SuperOptions contains "lowerdir=<a>:<b>:<c>,..."; counting ':' separators
// in the lowerdir value and adding 1 gives the layer count.
func overlayLayerCount(superOptions string) int {
	for _, opt := range strings.Split(superOptions, ",") {
		if strings.HasPrefix(opt, "lowerdir=") {
			val := strings.TrimPrefix(opt, "lowerdir=")
			return strings.Count(val, ":") + 1
		}
	}
	return 0
}

func printDefaultTable(pid int, mounts []*MountInfo) {
	fmt.Printf("Mounts for PID %d:\n", pid)
	fmt.Printf("  %-6s  %-6s  %-12s  %-12s  %-24s  %s\n",
		"ID", "PARENT", "MAJOR:MINOR", "FSTYPE", "SOURCE", "MOUNTPOINT")
	for _, m := range mounts {
		mm := fmt.Sprintf("%d:%d", m.Major, m.Minor)
		fmt.Printf("  %-6d  %-6d  %-12s  %-12s  %-24s  %s\n",
			m.MountID, m.ParentID, mm, m.FSType, m.Source, m.MountPoint)
	}
}

func printOverlayTable(pid int, mounts []*MountInfo) {
	fmt.Printf("Overlay mounts for PID %d:\n", pid)
	fmt.Printf("  %-6s  %-6s  %-12s  %-10s  %-20s  %s\n",
		"ID", "PARENT", "FSTYPE", "SOURCE", "MOUNTPOINT", "LAYERS")
	found := false
	for _, m := range mounts {
		if m.FSType != "overlay" {
			continue
		}
		found = true
		layers := overlayLayerCount(m.SuperOptions)
		fmt.Printf("  %-6d  %-6d  %-12s  %-10s  %-20s  %d\n",
			m.MountID, m.ParentID, m.FSType, m.Source, m.MountPoint, layers)
	}
	if !found {
		fmt.Println("  (no overlay mounts found)")
	}
}

func printTypeTable(pid int, mounts []*MountInfo, fstype string) {
	fmt.Printf("Mounts of type %q for PID %d:\n", fstype, pid)
	fmt.Printf("  %-6s  %-6s  %-12s  %-12s  %-24s  %s\n",
		"ID", "PARENT", "MAJOR:MINOR", "FSTYPE", "SOURCE", "MOUNTPOINT")
	found := false
	for _, m := range mounts {
		if m.FSType != fstype {
			continue
		}
		found = true
		mm := fmt.Sprintf("%d:%d", m.Major, m.Minor)
		fmt.Printf("  %-6d  %-6d  %-12s  %-12s  %-24s  %s\n",
			m.MountID, m.ParentID, mm, m.FSType, m.Source, m.MountPoint)
	}
	if !found {
		fmt.Printf("  (no mounts of type %q found)\n", fstype)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: mount-inspector")
	fmt.Fprintln(os.Stderr, "       mount-inspector --pid <pid>")
	fmt.Fprintln(os.Stderr, "       mount-inspector --pid <pid> --overlay")
	fmt.Fprintln(os.Stderr, "       mount-inspector --pid <pid> --type <fstype>")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "examples:")
	fmt.Fprintln(os.Stderr, "  mount-inspector                          # current process mounts")
	fmt.Fprintln(os.Stderr, "  mount-inspector --pid 1                  # PID 1 (init/systemd) mounts")
	fmt.Fprintln(os.Stderr, "  mount-inspector --pid 1 --overlay        # overlay mounts only, with layer count")
	fmt.Fprintln(os.Stderr, "  mount-inspector --pid 1 --type tmpfs     # tmpfs mounts only")
}

func main() {
	pid := os.Getpid()
	overlay := false
	fstype := ""

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
		case "--overlay":
			overlay = true
		case "--type":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "error: --type requires an argument")
				os.Exit(1)
			}
			i++
			fstype = args[i]
		case "--help", "-h":
			usage()
			os.Exit(0)
		default:
			fmt.Fprintf(os.Stderr, "error: unknown flag %q\n", args[i])
			usage()
			os.Exit(1)
		}
	}

	if overlay && fstype != "" {
		fmt.Fprintln(os.Stderr, "error: --overlay and --type are mutually exclusive")
		os.Exit(1)
	}

	mounts, err := readMountInfo(pid)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	switch {
	case overlay:
		printOverlayTable(pid, mounts)
	case fstype != "":
		printTypeTable(pid, mounts, fstype)
	default:
		printDefaultTable(pid, mounts)
	}
}
