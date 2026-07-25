# node-pressure-reader

Reads `/proc/pressure/*` (node PSI) and, optionally, a pod cgroup's
`memory.events` and `memory.pressure` to show resource pressure at both
the node and pod level.

## Build and Run

```
go build -o node-pressure-reader .

# Node-level PSI only
./node-pressure-reader

# Node PSI + pod cgroup stats
./node-pressure-reader --pod <pod-uid>
```

## Sample Output

```
=== Node Pressure (PSI) ===
  memory pressure:
    some  avg10=0.12%  avg60=0.05%  avg300=0.01%  total=123456 µs
    full  avg10=0.04%  avg60=0.01%  avg300=0.00%  total=45678 µs
  cpu pressure:
    some  avg10=2.45%  avg60=1.23%  avg300=0.89%  total=987654 µs
  io pressure:
    some  avg10=0.50%  avg60=0.30%  avg300=0.10%  total=234567 µs
    full  avg10=0.20%  avg60=0.10%  avg300=0.03%  total=89012 µs

=== Pod abc123 ===
  cgroup: /sys/fs/cgroup/kubepods.slice/kubepods-besteffort-podabc123.slice
  memory.events:
    low=0  high=3  max=1  oom=0  oom_kill=0
  memory pressure:
    some  avg10=0.80%  avg60=0.30%  avg300=0.05%  total=56789 µs
    full  avg10=0.20%  avg60=0.05%  avg300=0.01%  total=12345 µs
```

## Kernel Paths

| File | Symbol |
|------|--------|
| `/proc/pressure/*` | `kernel/sched/psi.c:psi_show()` |
| `memory.events` | `mm/memcontrol.c` — incremented by `mem_cgroup_event()` |
| `memory.pressure` | per-cgroup `struct psi_group` |

Sources:
https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/psi.c
https://elixir.bootlin.com/linux/v6.9/source/mm/memcontrol.c

## Exercises

a) Add `--watch` flag that re-reads every 5 seconds and prints the delta
   in PSI totals (to see rate of pressure accumulation).

b) Add parsing for `cpu.pressure` and `io.pressure` from the pod cgroup.

c) Compare the pod's `memory.current` with `memory.max` and print a
   utilization percentage next to the memory.events summary.
