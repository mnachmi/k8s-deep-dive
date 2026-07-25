# cgroup-stats

A Go tool that reads cgroup v2 resource statistics directly from the kernel's cgroupfs virtual filesystem — no external dependencies, stdlib only.

## What It Demonstrates

- **Parsing cgroup v2 file formats**: single-value files (`memory.current`, `cpu.weight`) and key-value stat files (`memory.stat`, `cpu.stat`) using only the Go stdlib
- **Resolving a process's cgroup path** from `/proc/<pid>/cgroup` — the `0::<path>` line maps to `/sys/fs/cgroup/<path>`
- **Human-readable formatting** of byte values (GiB/MiB/KiB)
- **Reading live kernel resource accounting data** directly from the cgroupfs virtual filesystem

## Build and Run

```bash
make build
```

### Read a specific cgroup path

```bash
./cgroup-stats /sys/fs/cgroup/user.slice
```

### Read by PID (no root required for your own processes)

```bash
./cgroup-stats --pid 1          # systemd's cgroup
make run-self                   # your current shell's cgroup
```

### Read a Kubernetes pod's cgroup

```bash
POD_UID=$(kubectl get pod nginx -o jsonpath='{.metadata.uid}')
CGROUP=$(find /sys/fs/cgroup -type d -name "pod${POD_UID}" 2>/dev/null | head -1)
./cgroup-stats "$CGROUP"
```

## Expected Output (`make run-self`)

```
PID 12345 → cgroup: /sys/fs/cgroup/user.slice/user-1000.slice/session-5.scope

Cgroup: /sys/fs/cgroup/user.slice/user-1000.slice/session-5.scope

=== Memory ===
  current  : 14.23 MiB (14921728 B)
  max      : max
  high     : max
  stat.anon:     8.45 MiB (8859648 B)
  stat.file:     5.78 MiB (6062080 B)
  events.oom:    0
  events.oom_kill: 0

=== CPU ===
  max    : max 100000
  weight : 100
  stat.usage_usec:         1234567
  stat.user_usec:          789012
  stat.system_usec:        445555
  stat.nr_throttled:       0
  stat.throttled_usec:     0

=== PIDs ===
  current : 4
  max     : max
```

## Kernel References

- `memory.current` ← `mem_cgroup.memory.usage.counter` in `mm/memcontrol.c`
- `cpu.stat.throttled_usec` ← `cfs_bandwidth.throttled_time` in `kernel/sched/fair.c`
- `pids.current` ← `pids_cgroup.counter` in `kernel/cgroup/pids.c`
- `/proc/<pid>/cgroup` format defined in `kernel/cgroup/cgroup.c:cgroup_pidlist_show()`

## Exercises

1. **Stress test**: run `stress-ng --vm 1 --vm-bytes 100M --timeout 30s` and monitor its cgroup with `watch -n 0.5 ./cgroup-stats --pid $(pgrep stress-ng)`. Watch `memory.current` and `cpu.stat` change in real time.
2. **Add `--watch` flag**: make the tool loop with a 1-second interval, clearing the screen between iterations (use `fmt.Print("\033[H\033[2J")` to clear).
3. **Add `--json` flag**: output the stats as JSON using `encoding/json`. Useful for piping to `jq`.
4. **Walk the hierarchy**: add a subcommand that walks all cgroups under `/sys/fs/cgroup/kubepods/` and prints a sorted table of memory usage per pod.

## K8s Connection

This tool is a simplified version of what `kube-inspect --cgroup` does. The exact same file reads (`memory.current`, `cpu.stat`, `pids.current`) are how Kubernetes monitoring tools (Prometheus node-exporter, cadvisor, kubelet itself) collect per-container resource metrics.
