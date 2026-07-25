# References & Resources

This page consolidates the key references used throughout the course: kernel source, Linux kernel documentation, books, articles, and tools.

---

## Kernel Source

### elixir.bootlin.com

**URL:** https://elixir.bootlin.com/linux/latest/source

The primary reference for all kernel source code citations in this course. Provides:
- Full kernel source tree with cross-referencing
- Function and struct definitions with line numbers
- Type definitions across header files
- Search by symbol, function, or struct name

Every kernel reference in this course includes a specific elixir.bootlin.com URL with file path and line number.

**Key directories to explore:**
- `include/linux/` — public kernel headers (structs, macros, syscall numbers)
- `include/uapi/` — userspace API headers (syscall arguments, constants)
- `kernel/` — core kernel subsystems (scheduler, process management, IPC)
- `fs/` — filesystem layer (VFS, specific filesystems, cgroups)
- `net/` — network stack (TCP/IP, netfilter, conntrack, namespaces)
- `mm/` — memory management (page tables, NUMA, memory reclaim)
- `security/` — LSM framework, AppArmor, SELinux

### Official Linux Kernel Documentation

**URL:** https://www.kernel.org/doc/

- Kernel configuration (`Documentation/kconfig/`)
- Subsystem overviews (`Documentation/*/`)
- Device tree and hardware (`Documentation/devicetree/`)

---

## Books

### "Linux Kernel Development" — Robert M. Love (3rd Edition, 2010)

**Best for:** Process model, kernel architecture, system calls, concurrency, memory management

Covers:
- Task scheduling and context switching
- Process creation (`fork`, `clone`, `exec`)
- Signals and process communication
- Memory management fundamentals
- File I/O and the VFS layer
- Concurrency, locking, and synchronization

**Chapters most relevant to this course:**
- Chapter 3: Process Management
- Chapter 4: Process Scheduling
- Chapter 5: Process Synchronization
- Chapter 7: Interrupts and Deferred Work
- Chapter 8: Bottom Halves and Deferrable Functions
- Chapter 10: Kernel Synchronization
- Chapter 13: The Virtual Filesystem

**Limitation:** Published in 2010; does not cover cgroups v2, modern namespace implementations, or eBPF.

---

### "Understanding the Linux Kernel" — Marco Bovet & David Cesati (3rd Edition, 2005)

**Best for:** Deep architectural understanding of memory management, interrupts, and hardware interaction

Covers:
- CPU architecture and MMU integration
- Kernel page table management
- Interrupt and exception handling
- Device drivers and PCI
- Synchronization primitives

**Chapters most relevant:**
- Chapter 3: Processes and Interrupts
- Chapter 4: Interrupts and Exceptions
- Chapter 9: Process Address Space
- Chapter 10: Virtual Memory

**Note:** Published in 2005 for Linux 2.6; outdated but the memory management and MMU sections remain architecturally sound.

---

### "The Linux Programming Interface" — Michael Kerrisk (1st Edition, 2010)

**Best for:** Userspace API, system calls, and how kernel mechanisms are exposed to applications

Covers:
- System calls and signal handling
- File I/O and directory operations
- Processes, process groups, and sessions
- Signals, timers, and clocks
- IPC (pipes, FIFOs, message queues, semaphores, shared memory)
- Network programming (sockets)
- Namespaces and cgroups (detailed coverage)
- Linux security and capabilities

**Chapters most relevant:**
- Chapter 3: System Calls
- Chapter 6: Processes
- Chapter 10: Signals
- Chapter 24: Process Execution
- Chapter 28-32: IPC
- Chapter 44: Virtual Memory Operations
- Chapter 49: Memory Mappings
- Chapter 61: Namespaces

---

### "BPF Performance Tools" — Brendan Gregg (1st Edition, 2019)

**Best for:** eBPF programming, performance profiling, observability at the kernel level

Covers:
- BPF architecture and verifier
- Writing BPF programs (eBPF bytecode, CO-RE)
- libbpf and bpftool
- Performance profiling with BPF (CPU, memory, I/O, network)
- Kernel tracepoints and USDT probes
- On-CPU and off-CPU flame graphs
- Real-world observability examples

**Chapters most relevant:**
- Chapter 1-4: BPF Architecture and Tooling
- Chapter 8: CPU Profiling
- Chapter 9: Off-CPU Analysis
- Chapter 10: Memory Analysis

---

### "Systems Performance" — Brendan Gregg (2nd Edition, 2020)

**Best for:** Holistic performance analysis, tuning knobs, and understanding the full I/O and CPU stack

Covers:
- Performance methodology (average, percentiles, profiles)
- CPU performance and scheduler tuning
- Memory performance (NUMA, huge pages, swapping)
- Filesystem and I/O performance
- Network performance
- Cloud and container performance (k8s tuning)
- Observability tools (perf, flame graphs, sysctl tuning)

**Chapters most relevant:**
- Chapter 2: Methodology (essential)
- Chapter 6: CPUs
- Chapter 8: Memory
- Chapter 9: Filesystems
- Chapter 10: Disk I/O
- Chapter 11: Network
- Chapter 14: Performance Tuning

---

## Linux Kernel Mailing List (LKML)

**URL:** https://lkml.org/

A searchable archive of discussions, patches, and design decisions on the Linux kernel mailing list. Useful for:
- Understanding the "why" behind kernel design choices
- Reading patch discussions before features landed
- Finding workarounds for known issues
- Understanding subsystem maintainers' design philosophy

---

## LWN.net

**URL:** https://lwn.net/

Weekly Linux kernel news and in-depth articles. Highly recommended for:

- **Weekly kernel updates** — summaries of kernel subsystem changes
- **In-depth LWN articles** — detailed explanations of new kernel features written by experienced kernel developers
- **Original design discussions** — LWN archives the mailing list context around features

**Key topics covered throughout this course:**
- Cgroups v2 introduction and unification
- BPF and CO-RE (Compile Once, Run Everywhere)
- Namespace unification and recent changes
- Memory management updates (PSI, NUMA)
- Scheduler improvements and CFS
- Network stack changes and BPF in networking
- Container and Kubernetes performance

---

## Essential Tools

### Kernel Exploration & Analysis

#### `bpftool`

**Purpose:** Inspect eBPF programs, maps, and kernel tracepoints

**Common usage:**
```bash
# List all loaded BPF programs
bpftool prog list

# Inspect BPF map contents
bpftool map show
bpftool map dump name <name>

# Find tracepoints
bpftool btf dump file /sys/kernel/btf/vmlinux format c | grep tracepoint

# Attach a kprobe to a syscall
bpftool prog attach kprobe <function> <program>
```

**References:**
- Man page: `man bpftool`
- Kernel source: `tools/bpf/bpftool/`

#### `pahole`

**Purpose:** Inspect kernel struct memory layout and DWARF debug information

**Common usage:**
```bash
# Show struct memory layout
pahole -C task_struct /boot/vmlinuz-$(uname -r)

# Generate BTF from kernel image
pahole -J /boot/vmlinuz-$(uname -r)

# Show struct size and alignment
pahole -d task_struct /boot/vmlinuz-$(uname -r)
```

**Part of:** `dwarves` package

---

#### `crash`

**Purpose:** Kernel debugging and live analysis (works on running kernel or kernel core dumps)

**Common usage:**
```bash
# Run interactively on running kernel
crash /boot/vmlinuz-$(uname -r) /proc/kcore

# In crash shell:
> struct task_struct <address>  # Show struct at address
> ps                             # List all tasks
> set <pid>                      # Switch context to a PID
> kmem -f                        # Show memory fragmentation
```

**References:**
- URL: https://people.redhat.com/anderson/crash_whitepaper.html
- GitHub: https://github.com/crash-utility/crash

---

#### `numactl`

**Purpose:** Inspect and manage NUMA topology and memory placement

**Common usage:**
```bash
# Show NUMA topology
numactl --hardware

# Show available NUMA nodes
numactl --show

# Pin process to NUMA node
numactl --membind=0 <command>

# Migrate pages to NUMA node
numactl --migrate --nodes=1 <pid>
```

**References:**
- Man page: `man numactl`
- NUMA tuning guide: https://access.redhat.com/documentation/en-us/red_hat_enterprise_linux/7/html/performance_tuning_guide/chap-system_performance_tuning_guide-numa

---

### Performance Profiling

#### `perf`

**Purpose:** CPU profiling, event counting, tracepoint inspection, and performance analysis

**Common usage:**
```bash
# Sample CPU usage (flame graph input)
perf record -g -F 99 <command>
perf script > out.perf
stackcollapse-perf.pl out.perf | flamegraph.pl > flame.svg

# Count syscalls
perf trace <command>

# Profile specific events
perf stat -e cycles,instructions,cache-misses <command>

# Record with LBR (for off-CPU analysis on Intel CPUs)
perf record -g --call-graph lbr <command>

# List available tracepoints
perf list tracepoint
```

**References:**
- Linux foundation guide: https://www.kernel.org/doc/html/latest/tools/perf/index.html
- BPF Performance Tools book (Chapter 8-9)

---

#### Flame Graphs

**Purpose:** Visualize CPU time spent in kernel and userspace

**How to generate:**
```bash
perf record -g -F 99 -a sleep 10  # Profile system-wide for 10s
perf script > out.perf
cd /path/to/FlameGraph  # https://github.com/brendangregg/FlameGraph
./stackcollapse-perf.pl out.perf | ./flamegraph.pl > flame.svg
# Open flame.svg in a web browser
```

**Interpreted by:**
- X-axis: sorted alphabetically (not time-ordered, just for visualization)
- Y-axis: stack depth
- Height: width is proportional to time spent in that function

---

### Network & Namespace Tools

#### `ss` (socket statistics)

**Purpose:** Query socket, connection, and network statistics

**Common usage:**
```bash
# List all TCP connections
ss -tanp

# Show socket options
ss -o

# Count connections by state
ss -s

# Show specific namespace
ip netns exec <namespace> ss -tanp
```

**References:**
- Man page: `man ss`
- Replaces older `netstat` tool

---

#### `ip` (IP configuration)

**Purpose:** Manage network interfaces, routing, and namespaces

**Common usage:**
```bash
# Create a network namespace
ip netns add test-ns

# List network interfaces in a namespace
ip netns exec test-ns ip link show

# Inspect routing table
ip netns exec test-ns ip route show

# Show network statistics
ip -s link

# Dump routing policy database
ip rule show
```

**References:**
- Man page: `man ip-netns`, `man ip-link`, `man ip-route`

---

### Cgroup & Resource Inspection

#### `/proc` filesystem

**Key files for this course:**
- `/proc/pid/status` — process memory and state
- `/proc/pid/maps` — virtual memory layout
- `/proc/pid/smaps` — detailed per-vma memory stats
- `/proc/pid/ns/` — namespace links
- `/proc/pid/cgroup` — cgroup membership
- `/proc/stat` — system-wide CPU stats
- `/proc/meminfo` — system memory info
- `/proc/slabinfo` — kernel memory allocator stats

---

#### `/sys/fs/cgroup/` (cgroups v2)

**Key hierarchy:**
```
/sys/fs/cgroup/
├── cpuset.*          # CPU and memory node constraints
├── cpu.max           # CPU bandwidth limits (usec/period)
├── memory.max        # Memory hard limit
├── memory.current    # Current memory usage
├── io.max            # I/O bandwidth limits
├── pids.max          # PID limit
└── memory.events     # OOM and memory pressure events
```

**Common inspection:**
```bash
# Show all cgroup controllers
cat /sys/fs/cgroup/cgroup.controllers

# Show cgroup limits for a container
cat /sys/fs/cgroup/docker/<container-id>/memory.max
cat /sys/fs/cgroup/docker/<container-id>/cpu.max

# Show PSI (Pressure Stall Information)
cat /proc/pressure/cpu
cat /proc/pressure/memory
cat /proc/pressure/io
```

---

## Reference Summary by Chapter

### Chapter 00: Prologue — The Gap
- **Book:** Love chapters 1-2 (kernel overview)
- **Tools:** `objdump`, `readelf` (to inspect syscall table)
- **LWN:** "A decade of Linux kernel community development" (2015)

### Chapter 01: Process Model
- **Book:** Love chapters 3-5 (process model, scheduling)
- **Book:** Kerrisk chapters 3-6, 24
- **Kernel:** `include/linux/sched.h::task_struct`, `kernel/fork.c::_do_fork()`
- **Tools:** `ps`, `/proc/pid/status`, `/proc/pid/maps`, `crash`

### Chapter 02: Namespaces
- **Book:** Kerrisk chapter 61 (comprehensive namespace coverage)
- **Kernel:** `include/linux/nsproxy.h`, `kernel/nsproxy.c`
- **Tools:** `ip netns`, `lsns`, `nsenter`
- **LWN:** "Namespaces in operation" series (2013-2020)

### Chapter 03: cgroups v2
- **Book:** None (cgroups v2 too recent for older books)
- **Kernel:** `kernel/cgroup/`, `include/linux/cgroup-defs.h`
- **LWN:** "Cgroups v2" (2016-2021 coverage)
- **Tools:** `bpftool`, `/sys/fs/cgroup/`, `cat /proc/pid/cgroup`
- **Docs:** https://docs.kernel.org/admin-guide/cgroup-v2.html

### Chapter 04: Memory Management
- **Book:** Bovet/Cesati chapters 2-4, 9-10 (mmu, paging)
- **Book:** Love chapter 11-15 (kernel memory, slab allocator)
- **Book:** Gregg "Systems Performance" chapter 8 (memory tuning)
- **Tools:** `pahole`, `numactl`, `/proc/meminfo`, `/proc/slabinfo`, `perf`

### Chapter 05: VFS & Storage
- **Book:** Love chapter 13 (VFS layer)
- **Book:** Bovet/Cesati chapter 11-12 (filesystems)
- **Kernel:** `fs/`, `include/linux/fs.h::inode`
- **Tools:** `bpftool`, `strace`, `debugfs`

### Chapter 06: Network Stack
- **Book:** Gregg "Systems Performance" chapter 11 (network tuning)
- **Kernel:** `net/`, `include/linux/skbuff.h::sk_buff`
- **Tools:** `ss`, `ip`, `bpftool`, `tcpdump`, `perf`
- **LWN:** Ongoing network stack optimization articles

### Chapter 07: eBPF
- **Book:** Gregg "BPF Performance Tools" chapters 1-4
- **Kernel:** `kernel/bpf/`, `tools/bpf/`
- **Tools:** `bpftool`, `libbpf`, `clang/llvm`
- **LWN:** "A thorough introduction to eBPF" (2021)

### Chapter 08: CPU Scheduler
- **Book:** Love chapter 4 (scheduler overview)
- **Kernel:** `kernel/sched/`, `include/linux/sched.h::sched_entity`
- **Tools:** `perf`, flame graphs, `bpftool`
- **LWN:** Ongoing scheduler optimization articles

### Chapter 09: K8s Internals
- **Book:** Kerrisk chapters 19-22 (signals, IPC, networking)
- **Kubernetes Docs:** https://kubernetes.io/docs/concepts/architecture/
- **Tools:** `strace`, `perf`, `kubectl`

### Chapter 10: Performance Profiling
- **Book:** Gregg "BPF Performance Tools" (full book)
- **Book:** Gregg "Systems Performance" (full book)
- **Tools:** `perf`, `bpftool`, flame graphs, on-CPU/off-CPU analysis

### Chapter 11: Cluster Operations
- **Kernel:** `kernel/panic.c`, `arch/x86/kernel/nmi.c` (NMI, kdump)
- **Tools:** `crash`, `perf`, kernel panic analysis
- **Docs:** https://docs.kernel.org/admin-guide/kdump/

---

## Online Resources

### Kernel Documentation & Guides

- **Kernel Source:** https://elixir.bootlin.com/linux/latest/source
- **Official Docs:** https://www.kernel.org/doc/
- **Arch Wiki:** https://wiki.archlinux.org/title/Kernel (performance tuning)
- **Red Hat Guides:** https://access.redhat.com/documentation/ (performance, tuning, troubleshooting)

### Kubernetes & Container Insights

- **Kubernetes Docs:** https://kubernetes.io/docs/
- **Containerd Docs:** https://containerd.io/docs/
- **OpenContainers Spec:** https://github.com/opencontainers/runtime-spec
- **Cilium Project:** https://cilium.io/ (eBPF + networking in k8s)

### Performance & Observability

- **Brendan Gregg's Blog:** http://www.brendangregg.com/ (performance methodology, tools, flame graphs)
- **FlameGraph Project:** https://github.com/brendangregg/FlameGraph
- **Linux Perf Wiki:** https://perf.wiki.kernel.org/

---

## Search Strategy

When looking for kernel information:

1. **Start with elixir.bootlin.com** — search by struct name, function name, or file path
2. **Cross-reference with LWN.net** — understand the design philosophy
3. **Check the kernel source directly** — read the comments and git log
4. **Consult LKML** — if a feature seems undocumented, search LKML for the original patch discussion
5. **Use `perf`, `bpftool`, `crash`** — observe the mechanism live in your running kernel

---

## Version Notes

This course targets:
- **Linux kernel:** 5.15+ (LTS) or latest stable (currently 6.x)
- **Go:** 1.22+
- **clang/llvm:** 16+
- **Kubernetes:** 1.28+ (for kind testing)

Some examples use features available only in 5.15+. Older kernels may not support cgroups v2, eBPF CO-RE, or PSI metrics.
