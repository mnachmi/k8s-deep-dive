# Contributing to the Linux-to-Kubernetes Course

This document covers build requirements, code style, exercise verification, and how to add new chapters.

---

## Build Requirements

To work on this course, you need:

- **Linux 5.15+** kernel with:
  - CONFIG_BPF=y
  - CONFIG_HAVE_EBPF_JIT=y
  - CONFIG_BPF_SYSCALL=y
  - CONFIG_CGROUP_V2=y (for cgroups chapters)
- **gcc** 9+ with `-Wall -Wextra -Werror` support
- **clang/llvm 16+** (for eBPF compilation)
- **Go 1.22+** with `go vet` and `gofmt`
- **libbpf-dev** (>= 0.7)
- **linux-headers** matching your kernel version
- **make** (GNU make)
- **bpftool**, **perf**, **numactl**, **pahole** (for kernel struct inspection)
- **kind** or **k3s** for Kubernetes cluster testing

### Installation (Ubuntu 22.04 LTS)

```bash
sudo apt-get update
sudo apt-get install -y \
  build-essential gcc clang llvm \
  linux-headers-$(uname -r) \
  libbpf-dev \
  golang-1.22 \
  make \
  bpftool linux-tools-generic \
  numactl \
  dwarves \
  git

# Verify Go installation
/usr/lib/go-1.22/bin/go version
```

---

## C Code Style

All C exercises must:

1. **Compile cleanly** with flags: `gcc -Wall -Wextra -Werror -o <program> <program>.c`
2. **Follow K&R style** (same as Linux kernel):
   - Tabs (not spaces) for indentation
   - Opening brace on same line: `if (x) {`
   - Closing brace on new line at same indentation as statement
   - 80-character line limit (or 100 for legibility)
3. **Include kernel source references** as comments:
   ```c
   /* kernel/sched/core.c::do_sched_yield() */
   syscall(__NR_sched_yield);
   ```
4. **Include a comment block** explaining what the program does and what kernel mechanism it demonstrates

Example:

```c
/*
 * Demonstrates clone(2) and PID namespace isolation.
 * References: kernel/fork.c::_do_fork()
 *            include/uapi/linux/sched.h (CLONE_* flags)
 */

#define _GNU_SOURCE
#include <sched.h>
#include <stdio.h>
#include <unistd.h>
#include <sys/wait.h>

int child_fn(void *arg)
{
	printf("Child PID: %d\n", getpid());
	return 0;
}

int main(void)
{
	char stack[4096];
	pid_t pid = clone(child_fn, stack + 4096,
			  CLONE_NEWPID | SIGCHLD, NULL);
	if (pid < 0) {
		perror("clone");
		return 1;
	}
	waitpid(pid, NULL, 0);
	return 0;
}
```

---

## Go Code Style

All Go exercises and the `kube-inspect` project must:

1. **Pass `go vet ./...`** with zero warnings
2. **Be formatted** with `gofmt` (standard Go style)
3. **Include kernel source references** in comments:
   ```go
   // References: kernel/sched/core.c::do_sched_yield()
   syscall.Syscall(syscall.SYS_SCHED_YIELD, 0, 0, 0)
   ```
4. **Include package and function documentation** per Go conventions
5. **Use error handling** consistently (check all errors)

Example:

```go
// Package proc provides utilities for reading /proc and PID namespace information.
// References: fs/proc/base.c
package proc

import (
	"os"
	"path/filepath"
)

// ListProcesses returns PIDs in the current namespace.
func ListProcesses() ([]int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var pids []int
	for _, e := range entries {
		// Parse PID from directory name
		// References: kernel/pid.c::alloc_pid()
		// ... implementation
	}
	return pids, nil
}
```

---

## Verifying Exercises Compile

Every exercise directory has a `Makefile` with three targets:

```bash
make build    # Compile the exercise
make run      # Run the exercise
make clean    # Remove built artifacts
```

To verify all exercises compile:

```bash
# For C exercises
cd 00-prologue/exercises/syscall-tracer
make build

# For Go exercises
cd 01-process-model/exercises/proc-walker
make build

# Full project
cd kube-inspect
make build
```

### Makefile Template for C Exercise

```makefile
PROGRAM := syscall-tracer
CFLAGS  := -Wall -Wextra -Werror -std=c11
SRC     := main.c
OBJ     := $(SRC:.c=.o)

.PHONY: build run clean

build: $(PROGRAM)

$(PROGRAM): $(OBJ)
	gcc $(CFLAGS) -o $@ $^

%.o: %.c
	gcc $(CFLAGS) -c -o $@ $^

run: build
	./$(PROGRAM)

clean:
	rm -f $(PROGRAM) $(OBJ)
```

### Makefile Template for Go Exercise

```makefile
.PHONY: build run clean

build:
	go vet ./...
	go build -o main ./cmd/main/main.go

run: build
	./main

clean:
	rm -f main
```

---

## How to Add a New Chapter

Follow this template to add chapter N (e.g., 12-security-contexts):

1. **Create the directory structure:**
   ```bash
   mkdir -p 12-security-contexts/{kernel,k8s,exercises}
   ```

2. **Add chapter README.md** at `12-security-contexts/README.md`:
   - Title: `# Security Contexts` (or your chapter topic)
   - Objectives: bullet list of what the student will learn
   - Reading order: which kernel/ and k8s/ docs to read first
   - Exercise directory links
   - Brief mention of kube-inspect checkpoint
   - Status: mark as `(in progress)` or `(complete)`

3. **Write kernel deep dives** in `kernel/`:
   - One `12-*.md` file per major kernel mechanism
   - Follow the Data Structure Deep Dive convention (see README.md section 4)
   - Every struct cited from kernel source must include:
     - elixir.bootlin.com URL with exact file path
     - Line number in the kernel version being taught (usually latest stable)
     - Full field-by-field explanation
     - Memory layout (use `pahole` output)
     - Lifecycle: allocation, initialization, modification, deallocation
     - Locking discipline
     - Object graph relationships
     - How to observe live with kernel tools

4. **Write K8s connection doc** at `k8s/12-k8s-connection.md`:
   - Which Kubernetes component owns this mechanism
   - How to observe it with `kubectl`, `/sys/fs/cgroup`, `ip netns`, `bpftool`
   - Tuning knobs exposed by Kubernetes
   - Common failure modes and diagnostics

5. **Add exercises** in `exercises/`:
   - Folder per exercise: `exercise-name-{c,go}/`
   - Each with `README.md`, `Makefile`, source code
   - Make sure they compile and run cleanly
   - Include comments citing kernel source

6. **Update kube-inspect checkpoint**:
   - Add one new capability to `kube-inspect` related to this chapter
   - Update `kube-inspect/CHECKPOINT.md` with what was added
   - Update `kube-inspect/internal/` with new package or function

7. **Update the main README.md**:
   - Add row to chapter table
   - Change status from "coming soon" to "in progress" or "complete"

8. **Commit**:
   ```bash
   git add 12-security-contexts/ kube-inspect/
   git commit -m "chore: add chapter 12 — security contexts"
   ```

---

## Kernel Source References

Always cite specific file paths and line numbers from:
- **https://elixir.bootlin.com/linux/latest/source** (default: latest stable kernel)
- Include both the file path and function/struct name

Example commit message:
```
kernel: add task_struct deep dive

References:
- include/linux/sched.h::task_struct (line 748)
- kernel/sched/core.c::copy_process() (line 2275)
- fs/proc/base.c::proc_pid_cmdline_read() (line 445)
```

---

## Documentation Standards

- **No placeholder text** — every section must contain real, substantive content
- **No "TODO" or "coming soon"** sections in completed chapters
- **Every kernel claim** must cite source or book/LWN reference
- **Every Kubernetes example** must be verifiable with `kubectl` or kernel tools
- **Code examples** must compile and run

---

## Testing & Quality Checklist

Before submitting a chapter:

- [ ] All `.md` files read cleanly (no syntax errors)
- [ ] All C exercises compile with `gcc -Wall -Wextra -Werror`
- [ ] All Go exercises pass `go vet ./...`
- [ ] All exercises have `Makefile` with `build`, `run`, `clean` targets
- [ ] All kernel source citations include elixir.bootlin.com URL + file path + line number
- [ ] All Kubernetes examples have `kubectl` commands or kernel tool output to verify
- [ ] No placeholder text anywhere
- [ ] kube-inspect builds cleanly: `cd kube-inspect && make build`
- [ ] Main README.md chapter table is updated

---

## CI/CD (Future)

Once infrastructure is available, this repo will run:
- `make build` and `make run` in every exercise directory
- `go vet ./...` on all Go code
- Lint checks on Markdown (mdlint) and code (golangci-lint for Go)
- Integration tests with a local kind cluster for K8s examples

For now, manual verification per the checklist above.

---

## Questions?

Refer to the course design spec at `docs/superpowers/specs/2026-07-25-linux-to-k8s-course-design.md` for the full philosophy, architecture, and quality standards.
