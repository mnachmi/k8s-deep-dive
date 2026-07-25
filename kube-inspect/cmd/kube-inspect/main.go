package main

import (
	"flag"
	"fmt"
	"os"
)

var (
	flagPod  = flag.String("pod", "", "Pod UID to inspect (reads from /proc)")
	flagNode = flag.Bool("node", false, "Inspect all pods on this node")
	flagJSON = flag.Bool("json", false, "Output as JSON")
)

func main() {
	flag.Parse()
	if *flagPod == "" && !*flagNode {
		fmt.Fprintln(os.Stderr, "usage: kube-inspect --pod <uid> | --node [--json]")
		os.Exit(1)
	}
	fmt.Println("kube-inspect: checkpoint 00 — skeleton only")
}
