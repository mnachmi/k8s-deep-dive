# bpf-fdinfo-reader

A Go tool that discovers BPF objects (programs and maps) held open by any process by reading `/proc/<pid>/fdinfo/*`. No root required. Uses only the Go standard library.

---

## 1. What It Demonstrates

Linux exposes BPF object metadata through the proc filesystem. Every open file descriptor has a corresponding `/proc/<pid>/fdinfo/<fd>` entry. When the FD refers to a BPF program or map, the kernel populates additional fields — `prog_type`, `prog_id`, `prog_tag`, `map_type`, `map_id`, and so on — via `bpf_prog_show_fdinfo()` and `bpf_map_show_fdinfo()` in `kernel/bpf/syscall.c`.

This tool walks `/proc/<pid>/fd/`, reads each fdinfo entry, and identifies BPF file descriptors without making any `bpf(2)` syscall and without root privileges. It demonstrates that BPF object inspection is possible entirely from userspace using proc, which is how tools like `bpftool` gather information at a lower level.

---

## 2. Build and Run

```bash
# Build the binary
go build -o bpf-fdinfo-reader .

# Run against a specific PID
go run . --pid <pid>

# Run against the current shell's PID
go run . --pid $PPID

# Print raw fdinfo lines alongside parsed output
go run . --pid <pid> --verbose

# Using the Makefile
make build
make run     # uses $PPID (current shell)
make clean
```

Example output when a process holds BPF objects:

```
BPF objects for PID 42:

Programs:
  fd=5    prog_type=6  (XDP)            prog_id=12     tag=abcdef1234567890  jited=true

Maps:
  fd=6    map_type=1   (HASH)           map_id=7       key_size=4    value_size=8    max_entries=1024
```

If the process holds no BPF file descriptors, both sections print `(none)`.

---

## 3. How to Find a PID with BPF FDs

**Using bpftool (requires root or CAP_BPF):**

```bash
# List all loaded BPF programs — the 'pid' column shows which process loaded them
sudo bpftool prog list

# List all loaded BPF maps
sudo bpftool map list
```

**Manual proc scan (no root needed for processes you own):**

```bash
# Find which FDs in a process are BPF programs
ls /proc/<pid>/fd/ | xargs -I{} sh -c 'grep -l prog_type /proc/<pid>/fdinfo/{} 2>/dev/null'

# Inline one-liner: scan all accessible PIDs for any BPF program FD
for pid in /proc/[0-9]*/fdinfo/*; do
    grep -l prog_type "$pid" 2>/dev/null
done
```

**Well-known processes that typically hold BPF FDs on a Kubernetes node:**

- `cilium-agent` — XDP and TC BPF programs for network policy
- `tetragon` — tracing/LSM BPF programs for runtime security
- `bpftrace` — kprobe/tracepoint programs while a script is running
- `systemd` — may hold cgroup BPF programs for resource control

---

## 4. /proc/fdinfo BPF Fields

When an FD refers to a BPF program, `/proc/<pid>/fdinfo/<fd>` contains:

| Field        | Type   | Description                                                |
|--------------|--------|------------------------------------------------------------|
| `prog_type`  | int    | BPF program type (see `BPF_PROG_TYPE_*` in bpf.h)         |
| `prog_jited` | 0 or 1 | Whether the program has been JIT-compiled to native code   |
| `prog_tag`   | hex    | 8-byte SHA1-based fingerprint of the BPF bytecode          |
| `memlock`    | bytes  | Memory locked for this program (contributes to RLIMIT_MEMLOCK) |
| `prog_id`    | int    | Kernel-assigned unique ID; matches `bpftool prog list`     |
| `frozen`     | 0 or 1 | Whether the program is frozen (immutable)                  |

When an FD refers to a BPF map:

| Field         | Type  | Description                                               |
|---------------|-------|-----------------------------------------------------------|
| `map_type`    | int   | BPF map type (see `BPF_MAP_TYPE_*` in bpf.h)             |
| `key_size`    | bytes | Size of each map key in bytes                             |
| `value_size`  | bytes | Size of each map value in bytes                           |
| `max_entries` | int   | Maximum number of entries the map can hold                |
| `map_id`      | int   | Kernel-assigned unique ID; matches `bpftool map list`     |
| `frozen`      | 0 or 1| Whether the map is frozen (read-only values)              |

Common fields present on all FDs (not BPF-specific):

| Field    | Description                                      |
|----------|--------------------------------------------------|
| `pos`    | Current file offset (always 0 for BPF objects)  |
| `flags`  | Open flags (octal)                               |
| `mnt_id` | Mount namespace ID of the file                   |
| `ino`    | Inode number                                     |

---

## 5. Kernel Path

The BPF fdinfo fields are written by two functions in the kernel BPF subsystem:

- `bpf_prog_show_fdinfo()` — populates `prog_type`, `prog_jited`, `prog_tag`, `prog_id`, `memlock`, and `frozen` for program FDs
- `bpf_map_show_fdinfo()` — populates `map_type`, `key_size`, `value_size`, `max_entries`, `map_id`, `memlock`, and `frozen` for map FDs

Both functions are defined in:

https://elixir.bootlin.com/linux/v6.9/source/kernel/bpf/syscall.c

They are registered as `.show_fdinfo` callbacks on the `bpf_prog_fops` and `bpf_map_fops` file operation structs. The VFS calls these callbacks when any process reads `/proc/<pid>/fdinfo/<fd>` for an FD backed by those file operations — which is how this tool obtains the data without any `bpf(2)` syscall.

---

## 6. Kubernetes Connection

In a Kubernetes cluster running Cilium, BPF maps are pinned to the BPF filesystem:

```
/sys/fs/bpf/tc/globals/
```

These pinned paths are accessible from any process on the host that can read the BPF filesystem mount — no special capability is required to see the filenames. The `cilium-agent` pod holds open FDs to these maps while it is running.

This tool can be run against the `cilium-agent` PID (find it with `pgrep cilium-agent`) to enumerate every BPF map and program it holds open — including map types, sizes, and IDs. The same applies to `tetragon`, which uses tracing and LSM BPF programs for runtime security enforcement.

Cross-referencing the `map_id` values this tool reports with `sudo bpftool map list` confirms that the kernel-assigned IDs are the same regardless of whether you discover them via fdinfo or the `bpf(2)` syscall — they identify the same in-kernel object.

---

## 7. Exercises

**(a) Run against PID 1**

```bash
go run . --pid 1
```

PID 1 is `systemd` on most Linux systems. Report what BPF programs it holds — on modern systems with systemd-bpf, you may see cgroup BPF programs of type `CGROUP_SOCK` or `CGROUP_SKB`.

**(b) Combine with bpf-syscall-demo**

Load the socket filter from the `bpf-syscall-demo` exercise in one terminal:

```bash
cd ../bpf-syscall-demo
sudo ./bpf_syscall_demo &
BPF_PID=$!
```

Then in another terminal, run this tool against that PID:

```bash
go run . --pid $BPF_PID --verbose
```

You should see a `SOCKET_FILTER` program entry (prog_type=1) with its tag and ID, along with any maps it created.

**(c) Cross-reference with bpftool**

Run `sudo bpftool prog list` and note the program IDs. Then run this tool against any process you know loads BPF programs and compare the `prog_id` values. The IDs are the same — they are the kernel's global BPF object identifiers, visible both via `bpf(2)` (which requires privileges) and via `/proc/fdinfo` (which does not, for FDs you own).
