# eBPF Cheatsheet for Linux-to-Kubernetes

This cheatsheet provides quick reference and examples for eBPF programming used throughout the course. Topics include BPF program types, map types, libbpf usage, and common patterns for kernel observation and in-kernel processing.

---

## Prerequisites

- **Linux 5.15+** with:
  - CONFIG_BPF=y
  - CONFIG_HAVE_EBPF_JIT=y
  - CONFIG_BPF_SYSCALL=y
  - CONFIG_DEBUG_INFO_BTF=y (for CO-RE)
- **clang/llvm 16+** for BPF compilation
- **libbpf** (>= 0.7) installed and development headers
- **bpftool** for inspection and loading
- **pahole** (from dwarves) for BTF generation

### Installation (Ubuntu 22.04)

```bash
sudo apt-get install -y \
  clang llvm \
  libbpf-dev \
  linux-headers-$(uname -r) \
  bpftool \
  dwarves
```

---

## BPF Kernel Program Types

### Tracepoint Programs

**Triggered by:** Kernel tracepoint (stable, guaranteed ABI).

**Use case:** Trace syscalls, scheduler events, network stack events.

**Example: Trace sys_enter_openat**

```c
/* SPDX-License-Identifier: GPL-2.0 */
#include "vmlinux.h"
#include <bpf/bpf_helpers.h>

/* References: kernel/trace/events/syscalls.h::syscalls */

struct {
	__uint(type, BPF_MAP_TYPE_RINGBUF);
	__uint(max_entries, 256 * 1024);
} events SEC(".maps");

struct syscall_event {
	u64 ts;
	u32 pid;
	char comm[16];
};

SEC("tracepoint/syscalls/sys_enter_openat")
int trace_openat(struct trace_event_raw_sys_enter *ctx)
{
	struct syscall_event *e;

	e = bpf_ringbuf_reserve(&events, sizeof(*e), 0);
	if (!e)
		return 0;

	e->ts = bpf_ktime_get_ns();
	e->pid = bpf_get_current_pid_tgid() & 0xFFFFFFFF;
	bpf_get_current_comm(&e->comm, sizeof(e->comm));

	bpf_ringbuf_submit(e, 0);
	return 0;
}

char LICENSE[] SEC("license") = "GPL";
```

**Compile:**
```bash
clang -O2 -target bpf -c -o trace.o trace.bpf.c
```

**Load and run:**
```bash
bpftool prog load trace.o /sys/fs/bpf/trace type tracepoint
# Read output from userspace via ringbuf reader
```

---

### Kprobe & Kretprobe Programs

**Triggered by:** Entry to or return from any kernel function (no stable ABI).

**Use case:** Trace internal kernel functions, access function arguments.

**Warning:** Kprobes are fragile across kernel versions; function signatures change.

**Example: Trace do_fork entry**

```c
/* References: kernel/fork.c::_do_fork() */

struct fork_event {
	u64 ts;
	u32 parent_pid;
	u32 child_pid;
};

SEC("kprobe/copy_process")
int trace_fork_entry(struct pt_regs *ctx)
{
	struct fork_event e = {};

	e.ts = bpf_ktime_get_ns();
	e.parent_pid = bpf_get_current_pid_tgid() & 0xFFFFFFFF;
	
	/* child_pid not yet assigned; would read from task_struct arg */
	bpf_ringbuf_output(&events, &e, sizeof(e), 0);
	return 0;
}
```

**Load:**
```bash
bpftool prog load fork.o /sys/fs/bpf/fork type kprobe
```

---

### BPF Maps

Maps are key-value stores shared between kernel and userspace.

#### BPF_MAP_TYPE_HASH

Standard hash table. Fast lookups, iteratable from userspace.

```c
struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__type(key, u32);              /* PID */
	__type(value, u64);            /* syscall count */
	__uint(max_entries, 10240);
} syscall_counts SEC(".maps");

/* In BPF program: */
u64 *count = bpf_map_lookup_elem(&syscall_counts, &pid);
if (count)
	__sync_fetch_and_add(count, 1);
else {
	u64 v = 1;
	bpf_map_update_elem(&syscall_counts, &pid, &v, 0);
}
```

#### BPF_MAP_TYPE_ARRAY

Fixed-size array (index 0 to max_entries-1). Fast, per-CPU variant for reducing lock contention.

```c
struct {
	__uint(type, BPF_MAP_TYPE_ARRAY);
	__type(key, u32);
	__type(value, u64);
	__uint(max_entries, 256);      /* Statistics per CPU or event ID */
} stats SEC(".maps");
```

#### BPF_MAP_TYPE_PER_CPU_ARRAY

Per-CPU array; each CPU has its own value, no locking needed.

```c
struct {
	__uint(type, BPF_MAP_TYPE_PER_CPU_ARRAY);
	__type(key, u32);
	__type(value, struct cpu_stats);
	__uint(max_entries, 256);
} cpu_stats SEC(".maps");
```

#### BPF_MAP_TYPE_RINGBUF

Ring buffer for efficient event streaming to userspace.

```c
struct {
	__uint(type, BPF_MAP_TYPE_RINGBUF);
	__uint(max_entries, 256 * 1024);
} events SEC(".maps");

/* In BPF program: */
struct event *e = bpf_ringbuf_reserve(&events, sizeof(*e), 0);
if (e) {
	/* Fill e */
	bpf_ringbuf_submit(e, 0);
}
```

**Read from userspace:**
```c
#include <bpf/libbpf.h>

int handle_event(void *ctx, void *data, size_t sz) {
	struct event *e = data;
	printf("PID: %u\n", e->pid);
	return 0;
}

int main() {
	int fd = bpf_obj_get("/sys/fs/bpf/events");  /* Get map FD */
	struct ring_buffer *rb = ring_buffer__new(fd, handle_event, NULL, NULL);
	if (!rb) {
		perror("ring_buffer__new");
		return 1;
	}
	
	/* Poll events indefinitely */
	ring_buffer__poll(rb, -1);
	ring_buffer__free(rb);
	return 0;
}
```

---

## Compile-Once Run-Everywhere (CO-RE)

**Problem:** Kernel struct layouts differ across kernel versions. Hardcoded offsets break.

**Solution:** CO-RE lets BPF programs portably access struct fields by name, using BTF (BPF Type Format).

### Example: CO-RE Access to task_struct

**Without CO-RE (fragile):**
```c
/* Different kernel versions have different task_struct layouts! */
struct task_struct *task = (struct task_struct *)ctx->regs[...];
u32 pid = *(u32 *)((void *)task + 0x0B9C);  /* Offset changes per version! */
```

**With CO-RE (portable):**
```c
#include "vmlinux.h"  /* Generated from kernel BTF */
#include <bpf/bpf_core_read.h>

struct task_struct *task = (struct task_struct *)ctx->regs[...];
u32 pid = BPF_CORE_READ(task, struct task_struct, tgid);
```

### Generate vmlinux.h

**vmlinux.h** is a header generated from kernel BTF that contains all kernel struct definitions.

```bash
# Generate vmlinux.h from running kernel
bpftool btf dump file /sys/kernel/btf/vmlinux format c > vmlinux.h

# Or from kernel image
bpftool btf dump file /boot/vmlinuz-$(uname -r) format c > vmlinux.h
```

### CO-RE Read Helpers

```c
#include <bpf/bpf_core_read.h>

/* Read a field */
u32 pid = BPF_CORE_READ(task, struct task_struct, tgid);

/* Read through pointer */
struct mm_struct *mm = BPF_CORE_READ(task, struct task_struct, mm);
unsigned long vm_start = BPF_CORE_READ(mm, struct mm_struct, mmap->vm_start);

/* Read kernel string */
char comm[16];
bpf_probe_read_kernel_str(comm, sizeof(comm), &task->comm);

/* Read user string */
char buf[64];
bpf_probe_read_user_str(buf, sizeof(buf), (void *)user_ptr);
```

---

## Common Patterns

### Syscall Counting by PID

**Trace every syscall, count by process:**

```c
SEC("tracepoint/syscalls/sys_enter")
int count_syscalls(struct trace_event_raw_sys_enter *ctx)
{
	u32 pid = bpf_get_current_pid_tgid() & 0xFFFFFFFF;
	u64 *count = bpf_map_lookup_elem(&syscall_counts, &pid);

	if (count)
		__sync_fetch_and_add(count, 1);
	else {
		u64 v = 1;
		bpf_map_update_elem(&syscall_counts, &pid, &v, 0);
	}
	return 0;
}
```

**Read from userspace:**
```c
int map_fd = bpf_obj_get("/sys/fs/bpf/syscall_counts");
map_fd = bpf_map__fd(obj->maps.syscall_counts);  /* Or via libbpf object */

/* Iterate over all entries */
u32 key = 0, next_key;
u64 count;
while (bpf_map_get_next_key(map_fd, &key, &next_key) == 0) {
	bpf_map_lookup_elem(map_fd, &next_key, &count);
	printf("PID %u: %lu syscalls\n", next_key, count);
	key = next_key;
}
```

---

### Per-Pod Observation (Kubernetes Context)

**Monitor syscalls for a specific pod via cgroup:**

```c
/* BPF program attached to cgroup. Traces syscalls within that cgroup. */

#include "vmlinux.h"
#include <bpf/bpf_helpers.h>

struct {
	__uint(type, BPF_MAP_TYPE_RINGBUF);
	__uint(max_entries, 256 * 1024);
} syscall_log SEC(".maps");

struct syscall_log_entry {
	u64 ts;
	u32 pid;
	char comm[16];
	u32 syscall_nr;
	char pod_name[64];
};

SEC("tracepoint/syscalls/sys_enter")
int trace_syscall(struct trace_event_raw_sys_enter *ctx)
{
	struct syscall_log_entry *e;
	
	e = bpf_ringbuf_reserve(&syscall_log, sizeof(*e), 0);
	if (!e)
		return 0;

	e->ts = bpf_ktime_get_ns();
	e->pid = bpf_get_current_pid_tgid() & 0xFFFFFFFF;
	bpf_get_current_comm(&e->comm, sizeof(e->comm));
	e->syscall_nr = ctx->id;
	
	/* Pod name would be injected from userspace via BPF map on pod lookup */
	
	bpf_ringbuf_submit(e, 0);
	return 0;
}

char LICENSE[] SEC("license") = "GPL";
```

---

### Memory Allocation Tracing (malloc/free)

**Trace userspace allocations via libc uprobes:**

```c
/* Trace malloc calls via USDT or uprobe */

SEC("uprobe//lib/x86_64-linux-gnu/libc.so.6:malloc")
int trace_malloc(struct pt_regs *ctx)
{
	u64 size = ctx->rdi;  /* First arg to malloc */
	u64 ret = ctx->rax;   /* Return value (in rax after call) */
	
	struct alloc_event e = {
		.ts = bpf_ktime_get_ns(),
		.pid = bpf_get_current_pid_tgid() & 0xFFFFFFFF,
		.size = size,
		.ptr = ret,
	};
	
	bpf_ringbuf_output(&alloc_events, &e, sizeof(e), 0);
	return 0;
}
```

**Load uprobes with libbpf:**
```c
#include <bpf/libbpf.h>

int main() {
	struct bpf_object *obj = bpf_object__open("malloc_trace.o");
	bpf_object__load(obj);
	
	/* Attach uprobe to malloc */
	struct bpf_program *prog = bpf_object__find_program_by_name(obj, "trace_malloc");
	struct bpf_link *link = bpf_program__attach_uprobe(prog, false, -1 /* self */,
				"libc.so.6", 0 /* offset or symbol lookup */);
	
	/* Wait for events */
	ring_buffer__poll(rb, -1);
	return 0;
}
```

---

## libbpf Usage

### Load and Attach a BPF Object

```c
#include <bpf/libbpf.h>

int main() {
	/* Open and load BPF object */
	struct bpf_object *obj = bpf_object__open("prog.bpf.o");
	if (!obj) {
		perror("bpf_object__open");
		return 1;
	}
	
	int err = bpf_object__load(obj);
	if (err) {
		fprintf(stderr, "bpf_object__load: %d\n", err);
		return 1;
	}
	
	/* Find and attach a tracepoint program */
	struct bpf_program *prog = bpf_object__find_program_by_name(obj, "trace_syscall");
	struct bpf_link *link = bpf_program__attach(prog);
	if (!link) {
		perror("bpf_program__attach");
		return 1;
	}
	
	printf("BPF program attached. Press Ctrl+C to exit.\n");
	
	/* Keep running; events flow to userspace */
	pause();
	
	bpf_link__destroy(link);
	bpf_object__close(obj);
	return 0;
}
```

### Access Maps

```c
/* Get map by name */
struct bpf_map *map = bpf_object__find_map_by_name(obj, "syscall_counts");
int map_fd = bpf_map__fd(map);

/* Lookup */
u32 pid = 1234;
u64 *count = bpf_map_lookup_elem(map_fd, &pid);
if (count)
	printf("PID %u: %lu syscalls\n", pid, *count);

/* Update */
u64 new_count = 42;
bpf_map_update_elem(map_fd, &pid, &new_count, 0);

/* Delete */
bpf_map_delete_elem(map_fd, &pid);

/* Iterate */
u32 key = 0, next_key;
while (bpf_map_get_next_key(map_fd, &key, &next_key) == 0) {
	bpf_map_lookup_elem(map_fd, &next_key, count);
	printf("PID %u: %lu\n", next_key, *count);
	key = next_key;
}
```

---

## Kernel Source References

### Core BPF Implementation

| Component | File | Reference |
|-----------|------|-----------|
| BPF syscall | `kernel/bpf/syscall.c` | `__sys_bpf()` |
| BPF verifier | `kernel/bpf/verifier.c` | `do_check()` |
| BPF JIT compiler (x86) | `arch/x86/net/bpf_jit_comp.c` | `bpf_int_jit_compile()` |
| Maps | `kernel/bpf/hashtab.c`, etc | Map operations |
| Tracepoints | `include/trace/` | TP definitions |
| Kprobes | `kernel/kprobes.c` | Probe attachment |
| BTF | `kernel/bpf/btf.c` | BTF validation |

### Helper Functions

| Helper | Kernel File | Reference |
|--------|-------------|-----------|
| `bpf_get_current_pid_tgid()` | `kernel/bpf/helpers.c` | Helper implementation |
| `bpf_ktime_get_ns()` | `kernel/bpf/helpers.c` | Current time in nanoseconds |
| `bpf_get_current_comm()` | `kernel/bpf/helpers.c` | Process comm (16 bytes) |
| `bpf_map_lookup_elem()` | `kernel/bpf/helpers.c` | Map lookup (BPF side) |
| `bpf_probe_read_kernel()` | `kernel/bpf/helpers.c` | Safe kernel memory read |
| `bpf_probe_read_user()` | `kernel/bpf/helpers.c` | Safe userspace memory read |

---

## Building a BPF Program

### Makefile Template

```makefile
CLANG ?= clang
LLC ?= llc
STRIP ?= llvm-strip
BPFTOOL ?= bpftool

VMLINUX_BTF := /sys/kernel/btf/vmlinux
VMLINUX_H := vmlinux.h

INCLUDES := -I/usr/include/bpf -I.
CFLAGS := -O2 -target bpf -D__KERNEL__ -D__BPF_TRACING__ $(INCLUDES)

.PHONY: generate build clean

generate: $(VMLINUX_H)

$(VMLINUX_H):
	@echo "Generating vmlinux.h..."
	bpftool btf dump file $(VMLINUX_BTF) format c > $@

build: generate prog.bpf.o

prog.bpf.o: prog.bpf.c $(VMLINUX_H)
	$(CLANG) $(CFLAGS) -c -o $@ $<
	$(STRIP) -g $@

clean:
	rm -f prog.bpf.o vmlinux.h
```

---

## Debugging BPF Programs

### View Loaded Programs

```bash
# List all loaded BPF programs
bpftool prog list

# Show program details
bpftool prog show id <ID>

# Dump disassembly
bpftool prog dump xlated id <ID>

# View verifier log
bpftool prog show id <ID> log
```

---

### Inspect Maps

```bash
# List all maps
bpftool map list

# Dump map contents
bpftool map dump name <NAME>

# Dump specific map by ID
bpftool map dump id <ID>

# Show map statistics
bpftool map show id <ID>
```

---

### Trace Program Execution

```bash
# Enable BPF tracing (logs messages via bpf_printk)
cat /sys/kernel/debug/tracing/trace_pipe

# In BPF program: bpf_printk("msg: %d", value);
# Output appears in trace_pipe
```

---

## Common Pitfalls

### 1. Map Size Too Small

**Problem:** BPF_MAP_TYPE_HASH with small `max_entries` causes evictions under load.

**Solution:** Size maps 10x–100x expected entries for workload.

```c
/* Bad: 100 entries for 1000 PIDs */
struct { __uint(max_entries, 100); } syscall_counts;

/* Good: 10000 entries for 1000 PIDs */
struct { __uint(max_entries, 10000); } syscall_counts;
```

---

### 2. Reading User Strings Without bpf_probe_read_user_str

**Problem:** Direct dereference of user pointers crashes the kernel.

**Solution:** Always use safe read helpers.

```c
/* WRONG: Kernel panic! */
char *user_str = (char *)user_ptr;
printf("%s", user_str[0]);

/* CORRECT: Safe read */
char buf[64];
bpf_probe_read_user_str(buf, sizeof(buf), user_ptr);
printf("%s", buf);
```

---

### 3. Verifier Rejects Valid Program

**Problem:** BPF verifier is conservative; rejects programs with potential paths to bad behavior.

**Solution:**
- Initialize all variables
- Use bounds checks on array access
- Avoid unbounded loops
- Use ` __attribute__((noinline))` to prevent inlining of helper calls

```c
/* WRONG: Verifier rejects (potential uninit) */
u32 key;
bpf_map_update_elem(&map, &key, &value, 0);

/* CORRECT: Explicit init */
u32 key = 0;
bpf_map_update_elem(&map, &key, &value, 0);
```

---

### 4. Lost Events from RINGBUF

**Problem:** Ringbuf entries overwritten if userspace reader is too slow.

**Solution:** Use larger ringbuf or sample events (filter in kernel).

```c
/* Sample 1 in 100 syscalls */
if ((bpf_get_current_pid_tgid() * 31) % 100 != 0)
	return 0;

e = bpf_ringbuf_reserve(&events, sizeof(*e), 0);
```

---

## Further Reading

- **BPF and XDP Documentation:** https://www.kernel.org/doc/html/latest/bpf/
- **libbpf GitHub:** https://github.com/libbpf/libbpf
- **bpftool documentation:** https://github.com/libbpf/bpftool
- **BPF Performance Tools (Book):** Chapter 1-4 for architecture, 5-7 for programming
- **LWN eBPF articles:** https://lwn.net/ (search "ebpf")
