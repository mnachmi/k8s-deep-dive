# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this repository is

A 14-chapter course (`00-prologue` … `13-rpi5-lab`) that teaches Linux kernel internals from source and maps each mechanism onto Kubernetes. Deliverables are Markdown deep dives, standalone C and Go exercises, and `kube-inspect/`, a Go diagnostic CLI that grows by one flag per chapter. The primary product is prose with source-level precision, so technical accuracy of kernel claims matters more than anything else here. Read `README.md` for the chapter index and `CONTRIBUTING.md` for the style rules and chapter-authoring checklist.

## Commands

There is no test suite anywhere in the repo. Verification means "it compiles cleanly and `go vet` is silent".

```bash
# kube-inspect (module: github.com/linux-to-k8s/kube-inspect, Go 1.23, stdlib only, no cgo)
cd kube-inspect && make build          # -> bin/kube-inspect (bin/ is gitignored)
cd kube-inspect && go vet ./... && go build ./...
sudo ./bin/kube-inspect --pod <uid> --cgroup --psi --sched --perf   # needs root + a real node
./bin/kube-inspect --health            # node-level flags (--health/--virt/--arch) need no --pod

# C exercise (chapters 00-11 have Makefiles; targets: build, run, clean)
cd 03-cgroups/exercises/cgroup-demo && make build
# Equivalent manual compile; -Werror is mandatory for every C exercise
gcc -Wall -Wextra -Werror -o cgroup_demo cgroup_demo.c

# Go exercise (each is its own module with its own go.mod; not part of kube-inspect)
cd 03-cgroups/exercises/cgroup-stats && make build      # or: go vet ./... && go build -o cgroup-stats .

# Chapters 08-13 exercises have no Makefile; follow the "Build and Run" block in each README
cd 12-kvm-virtualization/exercises/vcpu-inspector && go build -o vcpu-inspector .
```

Most exercises and most `kube-inspect` flags read `/proc`, `/sys/fs/cgroup`, or `/sys/kernel/debug` and need root or a Kubernetes node to produce real output. Compiling never needs root.

## Repository layout and content conventions

Every chapter has three tracks, and a change to one usually implies a change to the others:

```
NN-name/
├── README.md               # objectives, prerequisites, reading-order table, kube-inspect checkpoint
├── kernel/NN-{a,b,c}-*.md  # kernel mechanism taught from source
├── k8s/NN-k8s-connection.md# K8s API field -> kernel data structure / cgroup file
└── exercises/<name>/       # C PoC (snake_case binary) and Go tool (kebab-case binary), each with README
```

Chapter 13 additionally has `setup/` (RPi5 OS + k3s install guides). Chapter READMEs and each exercise README are the only places build instructions live.

**Kernel doc skeleton** (see `03-cgroups/kernel/03-a-cgroup-struct.md` as the reference example): a problem-first narrative opener in the style of Robert Love ("The Problem X Solves", historical context) → `Source Location(s)` table → one `## struct foo` section per key struct with the struct quoted field-by-field → `Lifecycle` → `Locking Discipline` → `Object Graph` → `Live Observation` (bpftrace / bpftool / `/sys` commands) → `Key Kernel References`. The struct deep dive is mandatory: every field gets type, purpose, who sets it, who reads it. Do not add a struct without its lifecycle and locking sections.

**K8s doc skeleton**: narrative opener → numbered sections mapping YAML fields to kernel files (`resources.limits.cpu` → `cpu.max`) → failure modes → verification commands → a `kube-inspect Integration` section describing the chapter's flag.

**Exercises** must be standalone (no import of kube-inspect), cite kernel source in comments as `// kernel/sched/core.c::do_sched_yield()`, and have a README with expected output.

## kube-inspect architecture

`cmd/kube-inspect/main.go` is a flat `flag`-based dispatcher: each `--flag` calls exactly one exported function in one `internal/` package and prints a text block (or JSON with `--json`). Adding a chapter checkpoint means: new function (or package) under `internal/`, one new `flag.Bool`, one `if *flagX {}` block in `main`, a row in `kube-inspect/CHECKPOINT.md`, and a row in the README flag table.

Pod discovery is the shared primitive. `proc.ListPodProcesses(podUID)` walks `/proc`, reads `/proc/<pid>/cgroup`, and keeps PIDs whose cgroup v2 path contains the pod UID. `netns`, `ebpf`, and `sched` build on it (they are the only intra-module imports). `cgroup`, `sched`, `metrics`, and `kubelet` each locate the pod's cgroup directory independently, and they disagree on the root: `internal/cgroup` walks `/sys/fs/cgroup/kubepods` (cgroupfs driver layout) while `sched`, `metrics`, and `kubelet` walk `/sys/fs/cgroup/kubepods.slice` (systemd driver layout). Be aware of this when a flag reports "cgroup path not found" on one node type but not another, and do not introduce a third variant.

`bpf/` contains only a `.gitkeep`. The `--ebpf` flag reads `/proc/<pid>/fdinfo` for BPF fds; there is no libbpf or eBPF program in the tool despite what the design spec sketches. Keep the module dependency-free unless a chapter explicitly calls for otherwise.

## Technical accuracy rules

- Every kernel source citation links to **`https://elixir.bootlin.com/linux/v6.9/source/<path>`** with a file path, function or struct name, and line number. The whole corpus is pinned to v6.9 (over a thousand links); do not use `latest` or a different version in chapter content.
- Every claim about kernel behavior needs a source citation, an LWN link, or a book reference. Every Kubernetes claim needs a `kubectl` or kernel-tool command that verifies it.
- bpftrace probes must exist in modern kernels: use `kprobe:kernel_clone` (never `do_fork`/`_do_fork`, removed in 5.10), and `copy_process` receives `struct kernel_clone_args *` as **arg3**. Past peer reviews found errors chiefly in probe names, struct field names that moved between versions (e.g. `mnt_userns` → `mnt_idmap`, `__i_atime`), and function locations (`do_filp_open` is in `fs/namei.c`). Verify struct fields against v6.9 before quoting them.
- No placeholder text, no "TODO", no "coming soon" in completed chapters.

## Git conventions

- Conventional commits with the chapter or component as scope: `feat(ch11): …`, `docs(ch03): …`, `fix(ch10): …`, `feat(kube-inspect): checkpoint 11 — …`, `chore: …`.
- **Compiled binaries must never be committed.** Every exercise directory that produces a binary gets its own `.gitignore` listing that binary (or an entry in the root `.gitignore`). Check `git status` before committing an exercise; large Go binaries have slipped in before.
- `.superpowers/sdd/` is a gitignored working directory for subagent-driven development (task briefs, review diffs, `progress.md` ledger). `docs/superpowers/specs/` holds the approved course design and `docs/superpowers/plans/` the per-chapter implementation plans.

## When adding or revising a chapter

Follow "How to Add a New Chapter" in `CONTRIBUTING.md`. In addition to the chapter directory itself, these files must be updated together or the course drifts: the root `README.md` chapter index, flag reference table, and diagnostic reference table; `kube-inspect/CHECKPOINT.md`; and `docs/ebpf-cheatsheet.md` if the chapter introduces new bpftrace one-liners.
