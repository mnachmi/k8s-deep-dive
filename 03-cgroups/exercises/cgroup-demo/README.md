# cgroup-demo — cgroup v2 memory and PID limits in C

A minimal C program that creates a cgroup v2 hierarchy, sets `memory.max` and `pids.max` limits, forks a child into it, and watches the kernel enforce those limits by OOM-killing the child when it allocates past the cap.

## What It Demonstrates

- Creating a cgroup v2 directory programmatically via filesystem writes (no special API — cgroupfs IS the API)
- Setting `memory.max` and `pids.max` limits
- Moving a process into a cgroup by writing its PID to `cgroup.procs`
- Observing the kernel enforce the memory limit: the child process is OOM-killed (SIGKILL) when it exceeds `memory.max`
- Reading `memory.events` to see OOM kill counters

## Requirements

- Root access (to write to `/sys/fs/cgroup/` and create subdirectories)
- Linux with cgroup v2 unified hierarchy mounted at `/sys/fs/cgroup/` — verify:

```bash
mount | grep cgroup2
```

## Build and Run

```bash
make build
sudo make run
```

## Expected Output

```
=== cgroup v2 demo (requires root) ===

[setup] /sys/fs/cgroup/cgroup-demo-test
[setup] memory.max = 32 MiB
[setup] pids.max   = 10

[parent] forking child
[parent] waiting for pid 54321
[child pid=54321] joined /sys/fs/cgroup/cgroup-demo-test
[child] /proc/self/cgroup: 0::/cgroup-demo-test
[child] allocating in 4 MiB steps (limit: 32 MiB)
[child] +4 MiB → total ~4 MiB
[child] +4 MiB → total ~8 MiB
[child] +4 MiB → total ~12 MiB
[child] +4 MiB → total ~16 MiB
[child] +4 MiB → total ~20 MiB
[child] +4 MiB → total ~24 MiB
[child] +4 MiB → total ~28 MiB
[child] +4 MiB → total ~32 MiB
[parent] child killed by signal 9 (SIGKILL — OOM killed by cgroup memory controller)

[stats]
  memory.current: 0
  pids.current  : 0
  memory.events : oom 1
                  oom_kill 1

[cleanup] removed /sys/fs/cgroup/cgroup-demo-test
```

Note: the exact step count before OOM kill may vary depending on kernel and system memory pressure. The key observation is SIGKILL from signal 9.

## Observing in Real Time

Open a second terminal while the demo runs:

```bash
# Watch memory usage climb:
watch -n 0.2 cat /sys/fs/cgroup/cgroup-demo-test/memory.current

# Watch for OOM events:
watch -n 0.2 cat /sys/fs/cgroup/cgroup-demo-test/memory.events
```

## Kernel References

| Call / struct | File | Link |
|---|---|---|
| `mkdir()` on cgroupfs | `kernel/cgroup/cgroup.c:cgroup_mkdir()` | https://elixir.bootlin.com/linux/v6.9/source/kernel/cgroup/cgroup.c |
| `write("cgroup.procs")` | `kernel/cgroup/cgroup.c:cgroup_procs_write()` | https://elixir.bootlin.com/linux/v6.9/source/kernel/cgroup/cgroup.c |
| `write("memory.max")` | `mm/memcontrol.c:mem_cgroup_write()` | https://elixir.bootlin.com/linux/v6.9/source/mm/memcontrol.c |
| `struct mem_cgroup` | `mm/memcontrol.h` | https://elixir.bootlin.com/linux/v6.9/source/mm/memcontrol.h |
| OOM kill path | `mm/oom_kill.c:oom_kill_process()` | https://elixir.bootlin.com/linux/v6.9/source/mm/oom_kill.c |

## Exercises

1. **Change the limit**: set `MEM_MAX` to `64 * 1024 * 1024` (64 MiB) and observe more steps succeed before OOM.
2. **PID limit**: add a fork loop to the child that spawns more than 10 processes. Observe `EAGAIN` from fork when `pids.max` is hit.
3. **CPU throttling**: create `cpu.max` with `echo "10000 100000" > /sys/fs/cgroup/cgroup-demo-test/cpu.max` (10% CPU), run a CPU-burning child (`while (1) {}`) and observe `cpu.stat` throttled values.
4. **Watch `memory.stat`**: instead of reading `memory.current`, read `memory.stat` and trace how `anon` (anonymous pages from malloc) grows with each allocation step.

## K8s Connection

When Kubernetes writes `resources.limits.memory: 32Mi` to a container, the kubelet performs exactly steps 1-3 of this demo (mkdir, write memory.max, write pids to cgroup.procs). The OOM kill behavior you observe here is identical to what happens when a production container exceeds its memory limit — the kernel sends SIGKILL, kubelet records `OOMKilled`, and the container restarts.
