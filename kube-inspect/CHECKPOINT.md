# kube-inspect — Checkpoint Log

Each chapter adds capabilities to this tool.

| Chapter | Checkpoint | Package | Status |
|---------|-----------|---------|--------|
| 00 | CLI skeleton, no-op collectors | cmd/ | done |
| 01 | List pod processes via /proc | internal/proc | done |
| 02 | Namespace enumeration per pod | internal/proc | done |
| 03 | cgroup v2 stats per pod | internal/cgroup | done |
| 04 | PSI + OOM events per pod | internal/cgroup | done |
| 05 | Mount ns inspection, overlay layers | internal/proc | done |
| 06 | Network ns interface stats | internal/netns | done |
| 07 | eBPF objects per pod | internal/ebpf | done |
| 08 | CPU affinity + NUMA placement | internal/sched | done |
| 09 | Node pressure + eviction thresholds | internal/kubelet | done |
| 10 | Full perf report — complete tool | internal/metrics | pending |
| 11 | Cluster health + kernel version audit | internal/ | pending |
