# Chapter 11 — Cluster Operations Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build Chapter 11 covering the kernel mechanisms that govern node health and recovery — kernel panic, NMI/watchdog, kexec/kdump — and how Kubernetes responds to node failures, performs rolling upgrades, and how operators audit kernel version and taint state.

**Architecture:** Same structure as previous chapters: `kernel/` (3 deep-dive docs), `k8s/` (1 connection doc), `exercises/` (C + Go), `kube-inspect/` (checkpoint 11). C exercise reads kernel health state from procfs/sysctl (no module required). Go exercise parses `/proc/sys/kernel/tainted`, `/proc/version`, and kernel message ring. kube-inspect checkpoint 11 adds `internal/health` + `--health` flag.

**Tech Stack:** Markdown, C (gcc, glibc only), Go 1.22, Linux 6.9 kernel source via elixir.bootlin.com

## Global Constraints

- All kernel struct fields/functions must be accurate for Linux 6.9
- All elixir.bootlin.com URLs: bare format (`https://elixir.bootlin.com/linux/v6.9/source/...`), never `[text](url)`
- C compile: `gcc -Wall -Wextra -Werror -o <name> <name>.c`
- Go: `go build ./...` + `go vet ./...` must pass
- Go module for exercises: `github.com/linux-to-k8s/<exercise-name>`, go 1.22
- No placeholder text (no TBD, TODO, etc.)
- Every kernel doc has a `## Key Kernel References` table with ≥ 5 entries (bare URLs)
- Reading order: 11-a → 11-b → 11-c → k8s-connection
- Prerequisites: Ch01 (task_struct), Ch03 (cgroups), Ch08 (scheduler/watchdog context)

---

## Task 1: README + `11-a-panic.md` — kernel panic, die notifiers, taint flags, oops

**Files:**
- Modify: `11-cluster-ops/README.md` (replace stub)
- Create: `11-cluster-ops/kernel/11-a-panic.md`

### `11-cluster-ops/README.md`

Replace the stub with:

```markdown
# Chapter 11 — Cluster Operations

A Kubernetes node is a Linux machine. When it fails — kernel panic, lockup, OOM, or hardware error — the recovery path starts inside the kernel before Kubernetes even notices. This chapter covers the kernel mechanisms that govern node health: how panics propagate through the notification chain, how the NMI watchdog catches CPU lockups, how kdump/kexec preserves a memory snapshot for post-mortem analysis, and how Kubernetes detects and recovers from node failures. The final kube-inspect checkpoint reads kernel version, taint flags, and watchdog state to give operators a single tool for node health auditing.

## Learning Objectives

1. Understand the Linux kernel panic path: `panic()`, `die()`, `struct die_args`, and panic notifiers
2. Understand kernel taint flags and how to interpret `/proc/sys/kernel/tainted`
3. Understand NMI watchdog: softlockup vs hardlockup, PMU-triggered NMIs, `/proc/sys/kernel/nmi_watchdog`
4. Understand kexec/kdump: `kexec_load(2)`, crash kernel memory reservation, `/proc/vmcore`
5. Diagnose node failures in Kubernetes: NodeNotReady lifecycle, taint-based eviction, rolling upgrades

## Prerequisites

- Chapter 01 — Process Model (task_struct, kernel threads)
- Chapter 03 — cgroups (kubelet resource model)
- Chapter 08 — Scheduler (CPU context, watchdog kthreads)

## Reading Order

| File | Topic |
|------|-------|
| `kernel/11-a-panic.md` | kernel panic(), die notifiers, taint flags, oops handling |
| `kernel/11-b-nmi-watchdog.md` | NMI, hardlockup/softlockup watchdog, register_nmi_handler |
| `kernel/11-c-kexec.md` | kexec_load(2), kdump, crash kernel, /proc/vmcore |
| `k8s/11-k8s-connection.md` | NodeNotReady lifecycle, rolling upgrades, etcd backup, node audit |
| `exercises/kernel-health-reader/` | C: read panic, taint, watchdog sysctl state |
| `exercises/node-health-reader/` | Go: /proc/version, taint flags, /dev/kmsg OOM scan |
| `kube-inspect` checkpoint 11 | Node health audit: kernel version, taint, watchdog state |

## Kernel to K8s Bridge

```
Hardware fault / BUG() / NULL deref
    │
    ▼
die() → die_chain notifiers → oops_enter()
    │  if panic_on_oops=1 or unrecoverable:
    ▼
panic()
    │  smp_send_stop (halt other CPUs)
    │  panic_notifier_list callbacks
    │  if kexec_loaded: machine_kexec() → crash kernel boots
    ▼
/proc/vmcore (crash kernel reads previous kernel's memory)
makedumpfile → vmcore.flat → crash tool

Kubernetes:
    kubelet health check fails → NodeNotReady
    → node.kubernetes.io/not-ready taint added
    → pods evicted after tolerationSeconds
    → node rejoins: taint cleared
```
```

### `11-cluster-ops/kernel/11-a-panic.md`

Full Data Structure Deep Dive. Required sections:

**1. Source Locations**

| File | Key Symbols | URL |
|------|-------------|-----|
| `kernel/panic.c` | `panic()`, `oops_enter()`, `oops_exit()`, `panic_notifier_list` | https://elixir.bootlin.com/linux/v6.9/source/kernel/panic.c |
| `include/linux/kdebug.h` | `struct die_args`, `enum die_val`, `register_die_notifier()` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/kdebug.h |
| `include/linux/panic.h` | `TAINT_*` flags, `add_taint()`, `test_taint()` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/panic.h |
| `kernel/notifier.c` | `atomic_notifier_call_chain()`, `blocking_notifier_call_chain()` | https://elixir.bootlin.com/linux/v6.9/source/kernel/notifier.c |
| `arch/x86/kernel/dumpstack.c` | `die()`, `show_regs()`, x86 oops output | https://elixir.bootlin.com/linux/v6.9/source/arch/x86/kernel/dumpstack.c |

**2. `struct die_args`**

```c
// include/linux/kdebug.h (Linux 6.9)
struct die_args {
    struct pt_regs  *regs;      // CPU register state at time of exception
    const char      *str;       // description string (e.g. "general protection fault")
    long             err;       // error code (e.g. segment fault error code)
    int              trapnr;    // hardware trap number (e.g. X86_TRAP_GP = 13)
    int              signr;     // signal to deliver (e.g. SIGSEGV)
};
```

Used by `die()` to pass context to all registered die notifiers. Notifiers return `NOTIFY_STOP` to suppress further handling, `NOTIFY_OK` to continue.

**3. The panic() Path**

```
BUG() / NULL deref / oops_exit() + panic_on_oops
    │
    ▼
panic(const char *fmt, ...)                   // kernel/panic.c
    │
    ├─ 1. bust_spinlocks(1)                   // disable lockdep, make printk work
    ├─ 2. smp_send_stop()                     // IPI to halt all other CPUs
    ├─ 3. atomic_notifier_call_chain(          // call panic_notifier_list
    │       &panic_notifier_list, PANIC_ACTION, buf)
    ├─ 4. kmsg_dump(KMSG_DUMP_PANIC)          // flush ring buffer to persistent storage
    ├─ 5. if (kexec_crash_loaded())           // kdump path
    │       crash_kexec(NULL)                 //   → machine_kexec() → crash kernel
    │
    ├─ 6. if (panic_timeout > 0)              // reboot after N seconds
    │       reboot_wait()
    │   elif (panic_timeout < 0)              // reboot immediately
    │       emergency_restart()
    │
    └─ 7. hlt_loop()                          // halt forever (panic_timeout=0)
```

The `panic_notifier_list` is an `atomic_notifier_head`. It uses `atomic_notifier_call_chain()` — safe to call from any context (spinlock-safe, no sleeping).

**4. Taint Flags**

`/proc/sys/kernel/tainted` is a bitmask. Each bit records a permanent kernel state change:

```c
// include/linux/panic.h (Linux 6.9) — selected flags
#define TAINT_PROPRIETARY_MODULE    0   // (P) out-of-tree proprietary module loaded
#define TAINT_FORCED_MODULE         1   // (F) module force-loaded
#define TAINT_CPU_OUT_OF_SPEC       2   // (S) SMP kernel on non-SMP hardware
#define TAINT_FORCED_RMMOD          3   // (R) module force-unloaded
#define TAINT_MACHINE_CHECK         4   // (M) MCE (hardware error) occurred
#define TAINT_BAD_PAGE              5   // (B) bad page accessed
#define TAINT_USER                  6   // (U) userspace set taint
#define TAINT_DIE                   7   // (D) kernel died (oops/BUG)
#define TAINT_OVERRIDDEN_ACPI_TABLE 8   // (A) ACPI table overridden
#define TAINT_WARN                  9   // (W) kernel WARN_ON fired
#define TAINT_CRAP                 10   // (C) staging driver loaded
#define TAINT_FIRMWARE_WORKAROUND  11   // (I) firmware bug workaround applied
#define TAINT_OOT_MODULE           12   // (O) out-of-tree module (non-proprietary)
#define TAINT_UNSIGNED_MODULE      13   // (E) unsigned module loaded
#define TAINT_SOFTLOCKUP           14   // (L) soft lockup detected
#define TAINT_LIVEPATCH            15   // (K) live kernel patch applied
#define TAINT_AUX                  16   // (X) auxiliary taint (driver-defined)
#define TAINT_RANDSTRUCT           17   // (T) struct layout was randomized
#define TAINT_TEST                 18   // (N) test module loaded
```

`add_taint(flag, lockdep_ok)` sets a bit. `test_taint(flag)` tests it. On Kubernetes nodes, key flags to watch: bit 9 (WARN), bit 7 (DIE/oops), bit 4 (MCE), bit 14 (soft lockup), bit 15 (livepatch).

**5. oops_enter() and oops_exit()**

An "oops" is a recoverable kernel fault (process killed, kernel continues). An "oops" becomes a panic if:
- `CONFIG_PANIC_ON_OOPS=y`
- `sysctl kernel.panic_on_oops=1`
- The fault occurs in interrupt context (no process to kill)
- The fault is in a kernel thread (no user process to kill)

```c
// kernel/panic.c
void oops_enter(void)
{
    tracing_off();          // stop ftrace
    /* if in_interrupt(): cannot kill a process → will become panic */
    if (in_interrupt())
        panic("Fatal exception in interrupt");
}

void oops_exit(void)
{
    tracing_on();
    print_oops_end_marker();
    if (panic_on_oops)
        panic("Fatal exception");
}
```

**6. Live Observation**

```bash
# Read current taint state
cat /proc/sys/kernel/tainted        # decimal bitmask, 0 = clean kernel

# Decode taint flags (kernel provides this)
awk 'NR==1{t=$1; for(i=0;i<19;i++){if(and(t,2^i))printf "bit %d set\n",i}}' \
    /proc/sys/kernel/tainted

# Simulate a taint (user-settable bit 6)
echo 64 > /proc/sys/kernel/tainted   # requires CAP_SYS_ADMIN

# Configure panic behavior
sysctl kernel.panic_on_oops          # 0 or 1
sysctl kernel.panic                  # reboot timeout in seconds (0=halt, <0=immediate)

# Watch kernel messages for WARN/BUG
dmesg -T -w | grep -E 'BUG:|Oops|WARN|RIP:|Call Trace'

# bpftrace: trace panic() entry
bpftrace -e 'kprobe:panic { printf("PANIC: cpu=%d pid=%d comm=%s\n", cpu, pid, comm); }'
```

**7. Key Kernel References**

| Symbol | File | URL |
|--------|------|-----|
| `panic()` | `kernel/panic.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/panic.c |
| `struct die_args` | `include/linux/kdebug.h` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/kdebug.h |
| `TAINT_*` flags | `include/linux/panic.h` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/panic.h |
| `add_taint()` | `kernel/panic.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/panic.c |
| `register_die_notifier()` | `kernel/notifier.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/notifier.c |
| `oops_enter()` | `kernel/panic.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/panic.c |

- [ ] **Step 1: Read the existing ch11 README stub**
- [ ] **Step 2: Write `11-cluster-ops/README.md`** — replace stub with full chapter intro
- [ ] **Step 3: Write `11-cluster-ops/kernel/11-a-panic.md`** — all 7 sections
- [ ] **Step 4: Verify no placeholder text, all URLs bare v6.9, ≥ 5 Key References**
- [ ] **Step 5: Commit**

```bash
git add 11-cluster-ops/README.md 11-cluster-ops/kernel/11-a-panic.md
git commit -m "docs(ch11): README + 11-a kernel panic, die notifiers, taint flags, oops path"
```

---

## Task 2: `11-b-nmi-watchdog.md` — NMI, hardlockup, softlockup watchdog

**Files:**
- Create: `11-cluster-ops/kernel/11-b-nmi-watchdog.md`

Full Data Structure Deep Dive. Required sections:

**1. Source Locations**

| File | Key Symbols | URL |
|------|-------------|-----|
| `arch/x86/include/asm/nmi.h` | `nmi_handler_t`, `register_nmi_handler()`, `NMI_LOCAL`, `NMI_UNKNOWN` | https://elixir.bootlin.com/linux/v6.9/source/arch/x86/include/asm/nmi.h |
| `arch/x86/kernel/nmi.c` | `do_nmi()`, `nmi_handle()`, NMI handler dispatch | https://elixir.bootlin.com/linux/v6.9/source/arch/x86/kernel/nmi.c |
| `kernel/watchdog.c` | `watchdog_enable()`, softlockup watchdog kthread | https://elixir.bootlin.com/linux/v6.9/source/kernel/watchdog.c |
| `kernel/watchdog_hld.c` | hardlockup detector: `watchdog_overflow_callback()` | https://elixir.bootlin.com/linux/v6.9/source/kernel/watchdog_hld.c |
| `include/linux/nmi.h` | `touch_nmi_watchdog()`, `touch_softlockup_watchdog()` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/nmi.h |

**2. NMI Overview**

The Non-Maskable Interrupt cannot be blocked by `cli` (the `IF` flag in RFLAGS does not mask NMIs). On x86, NMI sources include:
- Hardware errors (ECC memory errors, PCI SERR)
- Watchdog timer overflow (PMU → perf_event overflow → NMI)
- IPMI/BMC management interrupts
- `int 2` instruction (software NMI for testing)

NMI vector: `X86_TRAP_NMI = 2`. Entry point: `do_nmi()` in `arch/x86/kernel/nmi.c`.

**3. NMI Handler Registration**

```c
// arch/x86/include/asm/nmi.h (Linux 6.9)
typedef int (*nmi_handler_t)(unsigned int type, struct pt_regs *regs);

// Handler types
#define NMI_LOCAL    0   // local (in-CPU) NMI
#define NMI_UNKNOWN  1   // NMI from unknown source
#define NMI_SERR     2   // PCI SERR
#define NMI_IO_CHECK 3   // I/O check

int register_nmi_handler(unsigned int type, nmi_handler_t handler,
                         unsigned long flags, const char *name);
void unregister_nmi_handler(unsigned int type, const char *name);
```

All NMI handlers must complete in < ~100 µs. No sleeping, no locks that might be held by interrupted code.

**4. Softlockup Watchdog**

The softlockup detector uses a per-CPU high-priority kthread (`watchdog/N`, SCHED_FIFO priority 99). The system timer tick function `update_process_times()` stamps a per-CPU `watchdog_touch_ts` timestamp via `touch_softlockup_watchdog()`. The watchdog kthread resets `watchdog_report_ts`. If the kthread hasn't run for `2 × watchdog_thresh` seconds, a soft lockup is reported:

```
BUG: soft lockup - CPU#0 stuck for 22s! [nginx:1234]
```

Key sysctls:
- `kernel.watchdog_thresh` (default 10s) — trigger at `2 × thresh`
- `kernel.softlockup_panic` — 0/1 (default 0: print only)
- `kernel.nmi_watchdog` — 0=disabled, 1=enabled (hardlockup)

**5. Hardlockup Watchdog**

The hardlockup detector uses the CPU's PMU (Performance Monitoring Unit) to generate an NMI every `watchdog_thresh` seconds. If the NMI fires and the softlockup `hrtimer_interrupts` counter hasn't changed (meaning no hrtimer interrupts, meaning the CPU was completely locked), it reports a hardlockup:

```
NMI watchdog: Watchdog detected hard LOCKUP on cpu 0
```

Implementation in `kernel/watchdog_hld.c`:
```c
// Per-CPU perf_event configured as:
//   type = PERF_TYPE_HARDWARE
//   config = PERF_COUNT_HW_CPU_CYCLES (or NMI-capable PMU counter)
//   sample_period = hw_nmi_get_sample_period(watchdog_thresh)
//   overflow_handler = watchdog_overflow_callback

static void watchdog_overflow_callback(struct perf_event *event,
                                       struct perf_hw_data *data,
                                       struct pt_regs *regs)
{
    if (is_hardlockup())
        watchdog_hardlockup_check(smp_processor_id(), regs);
}
```

The connection to Chapter 10: the hardlockup watchdog uses `perf_event_open`-equivalent infrastructure internally — a PMU counter configured to overflow at a fixed rate, generating an NMI.

**6. Live Observation**

```bash
# Check watchdog state
cat /proc/sys/kernel/nmi_watchdog        # 1=enabled
cat /proc/sys/kernel/watchdog_thresh     # 10 (seconds)
cat /proc/sys/kernel/softlockup_panic    # 0 or 1

# Temporarily disable watchdog (for long-running kernel operations)
sysctl kernel.nmi_watchdog=0
sysctl kernel.watchdog=0

# Check for past lockup events
dmesg -T | grep -E 'lockup|NMI|watchdog'

# bpftrace: count softlockup touches per CPU
bpftrace -e 'kprobe:touch_softlockup_watchdog { @[cpu]++; }
interval:s:5 { print(@); clear(@); }'

# Check per-CPU watchdog perf_event fds
ls /proc/$(pgrep -n watchdog)/fd 2>/dev/null
```

**7. Key Kernel References**

| Symbol | File | URL |
|--------|------|-----|
| `register_nmi_handler()` | `arch/x86/include/asm/nmi.h` | https://elixir.bootlin.com/linux/v6.9/source/arch/x86/include/asm/nmi.h |
| `do_nmi()` | `arch/x86/kernel/nmi.c` | https://elixir.bootlin.com/linux/v6.9/source/arch/x86/kernel/nmi.c |
| `watchdog_overflow_callback()` | `kernel/watchdog_hld.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/watchdog_hld.c |
| `touch_softlockup_watchdog()` | `include/linux/nmi.h` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/nmi.h |
| `watchdog_enable()` | `kernel/watchdog.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/watchdog.c |
| `is_hardlockup()` | `kernel/watchdog_hld.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/watchdog_hld.c |

- [ ] **Step 1: Write `11-cluster-ops/kernel/11-b-nmi-watchdog.md`** — all 7 sections
- [ ] **Step 2: Verify no placeholder text, all URLs bare v6.9, ≥ 5 Key References**
- [ ] **Step 3: Commit**

```bash
git add 11-cluster-ops/kernel/11-b-nmi-watchdog.md
git commit -m "docs(ch11): 11-b NMI, hardlockup/softlockup watchdog, register_nmi_handler"
```

---

## Task 3: `11-c-kexec.md` — kexec_load, kdump, crash kernel, /proc/vmcore

**Files:**
- Create: `11-cluster-ops/kernel/11-c-kexec.md`

Full Data Structure Deep Dive. Required sections:

**1. Source Locations**

| File | Key Symbols | URL |
|------|-------------|-----|
| `include/uapi/linux/kexec.h` | `kexec_load(2)` flags: `KEXEC_ON_CRASH`, `KEXEC_SEGMENT_*`, `struct kexec_segment` | https://elixir.bootlin.com/linux/v6.9/source/include/uapi/linux/kexec.h |
| `kernel/kexec_core.c` | `machine_kexec()`, `kexec_load_purgatory()`, `kimage` struct | https://elixir.bootlin.com/linux/v6.9/source/kernel/kexec_core.c |
| `include/linux/kexec.h` | `struct kimage`, `struct kexec_segment`, `kexec_crash_loaded()` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/kexec.h |
| `arch/x86/kernel/machine_kexec_64.c` | x86_64 `machine_kexec()` — disables paging, jumps to new kernel | https://elixir.bootlin.com/linux/v6.9/source/arch/x86/kernel/machine_kexec_64.c |
| `fs/proc/vmcore.c` | `/proc/vmcore` — ELF core dump from crashed kernel | https://elixir.bootlin.com/linux/v6.9/source/fs/proc/vmcore.c |

**2. `kexec_load(2)` Syscall**

```c
// include/uapi/linux/kexec.h
long kexec_load(
    unsigned long entry,   // entry point address in new kernel
    unsigned long nr_segments,
    struct kexec_segment *segments,  // array of memory segments
    unsigned long flags    // KEXEC_ON_CRASH for kdump; 0 for regular kexec
);

struct kexec_segment {
    const void __user *buf;    // source data in userspace
    size_t             bufsz;  // size of source data
    const void __user *mem;    // target physical address
    size_t             memsz;  // size of target region
};
```

`KEXEC_ON_CRASH`: loads the new kernel into the memory region reserved by the `crashkernel=` boot parameter. This memory is kept separate from the running kernel — it cannot be allocated by the normal allocator.

**3. `struct kimage`**

```c
// include/linux/kexec.h (selected fields, Linux 6.9)
struct kimage {
    kimage_entry_t  *entry;         // start of kimage entry list
    kimage_entry_t  *last_free;     // last free entry in list
    kimage_entry_t  *next_entry;    // current pointer
    struct list_head control_pages; // pages used for control code
    struct list_head dest_pages;    // pages to copy to their final location
    struct list_head unusable_pages;// pages that cannot be used

    unsigned long   start;          // entry point (physical address)
    struct page    *control_code_page; // page containing assembly trampoline
    unsigned long   nr_segments;    // number of loaded segments
    struct kexec_segment segment[KEXEC_SEGMENT_MAX]; // segments (up to 16)

    unsigned int    type;           // KEXEC_TYPE_DEFAULT or KEXEC_TYPE_CRASH
    // ...
};
```

**4. kdump Boot Sequence**

```
Panic in production kernel
    │
    ▼
crash_kexec(regs)         // kernel/kexec_core.c
    │  machine_kexec()    // arch/x86/kernel/machine_kexec_64.c
    │    - disables paging, IRQs, APIC
    │    - copies control code to identity-mapped page
    │    - jumps to crash kernel entry point
    ▼
Crash kernel boots (smaller, no KASLR)
    │  uses crashkernel= reserved memory only
    │  runs makedumpfile OR copies /proc/vmcore
    ▼
/proc/vmcore
    │  ELF core file: PT_LOAD segments map production kernel's physical memory
    │  PT_NOTE: contains ELF notes with register state, kernel version
    ▼
`crash` tool:  crash vmlinux /proc/vmcore
    │  bt    → backtrace at time of crash
    │  log   → dmesg from crashed kernel
    │  ps    → task list
```

**5. `/proc/vmcore`**

`/proc/vmcore` is an ELF core file implemented in `fs/proc/vmcore.c`. It presents the crashed kernel's memory as an ELF PT_LOAD map. The crash kernel reads the previous kernel's memory using physical addresses saved in the ELF notes.

Key ELF notes in `/proc/vmcore`:
- `VMCOREINFO` note: kernel version, page size, symbol offsets, struct sizes — everything `makedumpfile` and `crash` need to parse the kernel state
- `NT_PRSTATUS` per-CPU: register state of each CPU at crash time

```bash
# On crash kernel (after kdump):
makedumpfile -c -d 31 /proc/vmcore /var/crash/vmcore.flat
crash /boot/vmlinux-$(uname -r) /proc/vmcore
```

**6. Live Observation**

```bash
# Check if crash kernel is loaded
cat /sys/kernel/kexec_loaded       # 1 = crash kernel loaded, 0 = not loaded
cat /sys/kernel/kexec_crash_size   # size reserved for crash kernel

# Check crashkernel parameter
cat /proc/cmdline | grep -o 'crashkernel=[^ ]*'

# Check reserved memory region
cat /proc/iomem | grep "Crash kernel"

# Configure panic-to-kdump
sysctl kernel.panic_on_oops=1
sysctl kernel.panic=10            # reboot after 10s (kdump must complete first)

# bpftrace: trace kexec_load syscall
bpftrace -e 'tracepoint:syscalls:sys_enter_kexec_load {
    printf("kexec_load: pid=%d flags=%lx\n", pid, args->flags);
}'
```

**7. Key Kernel References**

| Symbol | File | URL |
|--------|------|-----|
| `struct kimage` | `include/linux/kexec.h` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/kexec.h |
| `struct kexec_segment` | `include/uapi/linux/kexec.h` | https://elixir.bootlin.com/linux/v6.9/source/include/uapi/linux/kexec.h |
| `machine_kexec()` | `arch/x86/kernel/machine_kexec_64.c` | https://elixir.bootlin.com/linux/v6.9/source/arch/x86/kernel/machine_kexec_64.c |
| `crash_kexec()` | `kernel/kexec_core.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/kexec_core.c |
| `/proc/vmcore` handler | `fs/proc/vmcore.c` | https://elixir.bootlin.com/linux/v6.9/source/fs/proc/vmcore.c |
| `kexec_crash_loaded()` | `include/linux/kexec.h` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/kexec.h |

- [ ] **Step 1: Write `11-cluster-ops/kernel/11-c-kexec.md`** — all 7 sections
- [ ] **Step 2: Verify no placeholder text, all URLs bare v6.9, ≥ 5 Key References**
- [ ] **Step 3: Commit**

```bash
git add 11-cluster-ops/kernel/11-c-kexec.md
git commit -m "docs(ch11): 11-c kexec_load, kdump, struct kimage, /proc/vmcore"
```

---

## Task 4: `11-k8s-connection.md` — NodeNotReady lifecycle, rolling upgrades, etcd backup, node audit

**Files:**
- Create: `11-cluster-ops/k8s/11-k8s-connection.md`

Required sections:

**1. Architecture Overview**

| Event | Kernel Signal | K8s Response |
|-------|--------------|--------------|
| Kernel panic | `panic()` → halt/reboot | kubelet dies → NodeNotReady after 40s |
| Soft lockup | `kernel.softlockup_panic=1` → panic | Same as panic path |
| OOM kill | `memory.events oom_kill` | Pod evicted; node pressure taint if sustained |
| NMI watchdog | hardlockup → reboot (if `panic=N`) | NodeNotReady; node re-registers after reboot |
| kexec reboot | `machine_kexec()` → crash kernel | Node offline; rejoins after kdump + restart |
| Tainted kernel | `/proc/sys/kernel/tainted` ≠ 0 | No automatic action; operator audit only |

**2. NodeNotReady Lifecycle**

```
Node healthy: kubelet sends NodeReady=True heartbeat every 10s to API server
     │
     │  Kernel panic / kubelet crash / network partition
     ▼
API server: last heartbeat > node-monitor-grace-period (default 40s)
     │
     ▼
node-lifecycle-controller sets NodeReady=Unknown
     │  Adds taints:
     │    node.kubernetes.io/not-ready:NoSchedule
     │    node.kubernetes.io/unreachable:NoExecute (effect after 5s default)
     ▼
Pods with no toleration: evicted after tolerationSeconds (default 300s)
     │
     ▼
Node rejoins: kubelet re-registers → heartbeat resumes
     │  Taints removed automatically by node-lifecycle-controller
     │  Pods rescheduled on node (if not evicted to other nodes)
```

**3. Rolling Upgrade — Kernel Perspective**

A rolling node upgrade (drain → upgrade kernel → reboot → uncordon) involves:

1. `kubectl drain <node>` — sets `node.kubernetes.io/unschedulable:NoSchedule`, evicts all evictable pods
2. `apt upgrade linux-image-6.9` + `reboot` — the kernel performs orderly shutdown:
   - `kernel_restart()` → `migrate_to_reboot_cpu()` (migrate all tasks to CPU 0)
   - `device_shutdown()` (flush I/O, unmount filesystems)
   - `machine_restart()` → `reboot` syscall or ACPI reset
3. Node reboots, kubelet starts, re-registers with API server
4. `kubectl uncordon <node>` — removes unschedulable taint, allows scheduling

**4. etcd Backup — Kernel Connection**

etcd is a Raft-based distributed KV store running in user space. The kernel mechanisms that matter for etcd reliability:

| Kernel Mechanism | etcd Impact |
|-----------------|-------------|
| `fsync(2)` / `fdatasync(2)` | WAL durability: etcd calls `fdatasync` after each WAL entry |
| Direct I/O (O_DIRECT) | Bypasses page cache for predictable write latency |
| `/proc/<pid>/io` wchar field | Monitor etcd write throughput |
| `vm.dirty_background_ratio` | Avoid page cache flushes competing with etcd writes |

Backup command (no kernel involvement — pure API):
```bash
etcdctl snapshot save /backup/etcd-$(date +%Y%m%d).db \
    --endpoints=https://127.0.0.1:2379 \
    --cacert=/etc/kubernetes/pki/etcd/ca.crt \
    --cert=/etc/kubernetes/pki/etcd/healthcheck-client.crt \
    --key=/etc/kubernetes/pki/etcd/healthcheck-client.key
```

**5. Node Health Audit**

```bash
# Kernel version and taint state
uname -r
cat /proc/sys/kernel/tainted
cat /proc/version

# Kernel parameters relevant to node health
sysctl kernel.panic kernel.panic_on_oops kernel.nmi_watchdog kernel.watchdog_thresh

# kdump readiness
cat /sys/kernel/kexec_loaded
cat /proc/cmdline | grep crashkernel

# Check for past oops/panics
dmesg -T | grep -E 'BUG:|Oops|WARN|Call Trace|Kernel panic'
journalctl -k --since "1 day ago" | grep -E 'panic|BUG|lockup'

# Pod failure rates due to node issues
kubectl get events --field-selector type=Warning --all-namespaces | grep -i 'oom\|evict\|notready'
```

**6. Common Failure Patterns**

| Symptom | Kernel Cause | Diagnosis |
|---------|-------------|-----------|
| Node reboots every few hours | NMI watchdog hardlockup | `dmesg | grep watchdog` |
| Node NotReady, stays up | kubelet OOM-killed | `journalctl -u kubelet` + `oom_score_adj` |
| Node tainted (bit 9 = WARN) | Driver BUG_ON/WARN | `dmesg | grep WARN`; check modules |
| Pods evicted during upgrade | PodDisruptionBudget not set | Set `minAvailable` in PDB |
| etcd latency spikes | Kernel writeback competing | `iostat -x` + `vm.dirty_background_ratio` |
| No crash dump after panic | kdump not configured | `cat /sys/kernel/kexec_loaded` |

**7. Verification Commands**

```bash
# Full node health snapshot
echo "=== Kernel Version ===" && uname -r && cat /proc/version
echo "=== Taint State ===" && cat /proc/sys/kernel/tainted
echo "=== Watchdog ===" && sysctl kernel.nmi_watchdog kernel.watchdog_thresh kernel.softlockup_panic
echo "=== Kdump ===" && cat /sys/kernel/kexec_loaded
echo "=== Panic Config ===" && sysctl kernel.panic kernel.panic_on_oops

# Recent kernel warnings
dmesg -T | tail -100 | grep -c WARN
```

**8. Key Kernel References**

| Symbol | File | URL |
|--------|------|-----|
| `panic()` | `kernel/panic.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/panic.c |
| `TAINT_*` flags | `include/linux/panic.h` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/panic.h |
| `watchdog_overflow_callback()` | `kernel/watchdog_hld.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/watchdog_hld.c |
| `crash_kexec()` | `kernel/kexec_core.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/kexec_core.c |
| `machine_restart()` | `arch/x86/kernel/reboot.c` | https://elixir.bootlin.com/linux/v6.9/source/arch/x86/kernel/reboot.c |
| `kernel_restart()` | `kernel/reboot.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/reboot.c |

- [ ] **Step 1: Write `11-cluster-ops/k8s/11-k8s-connection.md`** — all 8 sections
- [ ] **Step 2: Verify all facts, no placeholder text, all URLs bare v6.9**
- [ ] **Step 3: Commit**

```bash
git add 11-cluster-ops/k8s/11-k8s-connection.md
git commit -m "docs(ch11): 11-k8s-connection NodeNotReady lifecycle, rolling upgrade, etcd backup, node audit"
```

---

## Task 5: C exercise — `kernel-health-reader`

**Files:**
- Create: `11-cluster-ops/exercises/kernel-health-reader/kernel_health_reader.c`
- Create: `11-cluster-ops/exercises/kernel-health-reader/Makefile`
- Create: `11-cluster-ops/exercises/kernel-health-reader/README.md`

### `kernel_health_reader.c`

```c
/*
 * kernel_health_reader.c — read kernel health state from procfs and sysctl.
 *
 * Demonstrates access to:
 *   a) Kernel version from /proc/version
 *   b) Taint flags from /proc/sys/kernel/tainted (decode each bit)
 *   c) Watchdog state from /proc/sys/kernel/nmi_watchdog and watchdog_thresh
 *   d) Panic configuration from /proc/sys/kernel/panic and panic_on_oops
 *   e) kdump readiness from /sys/kernel/kexec_loaded and /proc/cmdline
 *
 * Build:  gcc -Wall -Wextra -Werror -o kernel_health_reader kernel_health_reader.c
 * Run:    ./kernel_health_reader
 *
 * Kernel paths:
 *   /proc/sys/kernel/tainted  → include/linux/panic.h TAINT_* flags
 *   /sys/kernel/kexec_loaded  → kernel/kexec_core.c kexec_crash_loaded()
 */

#define _GNU_SOURCE
#include <stdio.h>
#include <string.h>
#include <unistd.h>

/* taint flag names — index matches bit position (TAINT_* in include/linux/panic.h) */
static const char *taint_names[] = {
    "P: proprietary module",     /*  0 */
    "F: forced module load",     /*  1 */
    "S: SMP on non-SMP CPU",     /*  2 */
    "R: forced module rmmod",    /*  3 */
    "M: machine check error",    /*  4 */
    "B: bad page accessed",      /*  5 */
    "U: userspace set taint",    /*  6 */
    "D: kernel oops/BUG",        /*  7 */
    "A: ACPI table overridden",  /*  8 */
    "W: WARN_ON fired",          /*  9 */
    "C: staging driver",         /* 10 */
    "I: firmware workaround",    /* 11 */
    "O: out-of-tree module",     /* 12 */
    "E: unsigned module",        /* 13 */
    "L: soft lockup",            /* 14 */
    "K: livepatch applied",      /* 15 */
    "X: auxiliary taint",        /* 16 */
    "T: randstruct",             /* 17 */
    "N: test module",            /* 18 */
};
#define NTAINT_FLAGS ((int)(sizeof(taint_names) / sizeof(taint_names[0])))

static void read_file(const char *path, char *buf, size_t len)
{
    FILE *f = fopen(path, "r");
    if (!f) {
        snprintf(buf, len, "(unreadable)");
        return;
    }
    if (fgets(buf, (int)len, f) == NULL)
        snprintf(buf, len, "(empty)");
    /* strip trailing newline */
    char *nl = strchr(buf, '\n');
    if (nl) *nl = '\0';
    fclose(f);
}

static void part_a(void)
{
    char ver[256];
    printf("=== Part a: Kernel version (/proc/version) ===\n");
    read_file("/proc/version", ver, sizeof(ver));
    printf("  %s\n", ver);
}

static void part_b(void)
{
    char tbuf[32];
    printf("\n=== Part b: Taint flags (/proc/sys/kernel/tainted) ===\n");
    read_file("/proc/sys/kernel/tainted", tbuf, sizeof(tbuf));
    long taint = strtol(tbuf, NULL, 10);
    printf("  raw value: %ld\n", taint);
    if (taint == 0) {
        printf("  kernel is CLEAN (no taints)\n");
    } else {
        printf("  active taints:\n");
        for (int i = 0; i < NTAINT_FLAGS; i++) {
            if (taint & (1L << i))
                printf("    bit %2d — %s\n", i, taint_names[i]);
        }
    }
}

static void part_c(void)
{
    char nmi[16], thresh[16];
    printf("\n=== Part c: Watchdog state ===\n");
    read_file("/proc/sys/kernel/nmi_watchdog", nmi, sizeof(nmi));
    read_file("/proc/sys/kernel/watchdog_thresh", thresh, sizeof(thresh));
    printf("  nmi_watchdog: %s  (0=disabled, 1=enabled)\n", nmi);
    printf("  watchdog_thresh: %ss  (softlockup at %s×2s, hardlockup at ~%ss)\n",
           thresh, thresh, thresh);
}

static void part_d(void)
{
    char panic_timeout[16], panic_on_oops[16], softlockup_panic[16];
    printf("\n=== Part d: Panic configuration ===\n");
    read_file("/proc/sys/kernel/panic", panic_timeout, sizeof(panic_timeout));
    read_file("/proc/sys/kernel/panic_on_oops", panic_on_oops, sizeof(panic_on_oops));
    read_file("/proc/sys/kernel/softlockup_panic", softlockup_panic, sizeof(softlockup_panic));
    printf("  kernel.panic:           %s  (0=halt, >0=reboot after N secs, <0=immediate)\n",
           panic_timeout);
    printf("  kernel.panic_on_oops:   %s  (0=kill process, 1=panic)\n", panic_on_oops);
    printf("  kernel.softlockup_panic:%s  (0=print only, 1=panic)\n", softlockup_panic);
}

static void part_e(void)
{
    char loaded[16], cmdline[512];
    printf("\n=== Part e: kdump readiness ===\n");
    read_file("/sys/kernel/kexec_loaded", loaded, sizeof(loaded));
    read_file("/proc/cmdline", cmdline, sizeof(cmdline));
    printf("  kexec_loaded: %s  (1=crash kernel ready, 0=no kdump)\n", loaded);
    /* find crashkernel= parameter */
    char *ck = strstr(cmdline, "crashkernel=");
    if (ck) {
        char *end = strchr(ck, ' ');
        if (end) *end = '\0';
        printf("  crashkernel param: %s\n", ck);
    } else {
        printf("  crashkernel param: (not present — kdump not configured)\n");
    }
}

int main(void)
{
    printf("kernel-health-reader — Linux node health state\n\n");
    part_a();
    part_b();
    part_c();
    part_d();
    part_e();
    return 0;
}
```

### `Makefile`

```makefile
CC      = gcc
CFLAGS  = -Wall -Wextra -Werror
TARGET  = kernel_health_reader

all: $(TARGET)

$(TARGET): kernel_health_reader.c
	$(CC) $(CFLAGS) -o $@ $<

run: $(TARGET)
	./$(TARGET)

clean:
	rm -f $(TARGET)

.PHONY: all run clean
```

### `README.md`

```markdown
# kernel-health-reader

Reads kernel health state from `/proc` and `/sys` to answer the most common
node audit questions an operator needs before a production incident.

## What It Shows

| Part | Source | What you learn |
|------|--------|---------------|
| a | `/proc/version` | Kernel version string |
| b | `/proc/sys/kernel/tainted` | Active taint flags (decoded per-bit) |
| c | `/proc/sys/kernel/nmi_watchdog` | Watchdog enabled + threshold |
| d | `/proc/sys/kernel/panic*` | Panic reboot and oops-panic config |
| e | `/sys/kernel/kexec_loaded` + `/proc/cmdline` | kdump readiness |

## Build and Run

```
make
./kernel_health_reader
```

## Kernel Paths

```
/proc/sys/kernel/tainted
  → include/linux/panic.h — TAINT_* bit definitions
  → kernel/panic.c — add_taint()

/sys/kernel/kexec_loaded
  → kernel/kexec_core.c — kexec_crash_loaded()

/proc/sys/kernel/nmi_watchdog
  → kernel/watchdog.c — watchdog_enable()
```

Sources:
https://elixir.bootlin.com/linux/v6.9/source/include/linux/panic.h
https://elixir.bootlin.com/linux/v6.9/source/kernel/kexec_core.c
https://elixir.bootlin.com/linux/v6.9/source/kernel/watchdog.c

## Exercises

a) Add Part f: read `/proc/sys/kernel/dmesg_restrict` and
   `/proc/sys/kernel/perf_event_paranoid`. Print their values and explain
   the security implications for an unprivileged container.

b) Add taint reason detection: for bit 7 (D: oops), scan `dmesg` for the
   most recent "Oops:" line and print the timestamp.

c) Extend Part e to also read `/proc/iomem` and print the "Crash kernel"
   reserved memory range if present.
```
```

- [ ] **Step 1: Write all three files exactly as specified**
- [ ] **Step 2: Build: `gcc -Wall -Wextra -Werror -o kernel_health_reader kernel_health_reader.c`** — must compile clean
- [ ] **Step 3: Commit**

```bash
git add 11-cluster-ops/exercises/kernel-health-reader/
git commit -m "feat(ch11): C exercise kernel-health-reader — taint flags, watchdog, panic config, kdump state"
```

---

## Task 6: Go exercise — `node-health-reader`

**Files:**
- Create: `11-cluster-ops/exercises/node-health-reader/main.go`
- Create: `11-cluster-ops/exercises/node-health-reader/go.mod`
- Create: `11-cluster-ops/exercises/node-health-reader/README.md`

### `main.go`

```go
// node-health-reader: reads Linux node health state for Kubernetes operators.
//
// Reads:
//   - /proc/version and /proc/sys/kernel/osrelease (kernel version)
//   - /proc/sys/kernel/tainted (decode taint flags)
//   - /proc/sys/kernel/panic, panic_on_oops, nmi_watchdog, watchdog_thresh
//   - /sys/kernel/kexec_loaded (kdump readiness)
//   - /dev/kmsg (scan recent kernel messages for OOM kills and BUG/WARN)
//
// Usage:
//   node-health-reader                    print full health report
//   node-health-reader --kmsg             also scan /dev/kmsg for OOM/BUG/WARN
//   node-health-reader --json             output as JSON
//
// Build: go build -o node-health-reader .
//
// Kernel paths:
//   /proc/sys/kernel/tainted → include/linux/panic.h TAINT_* flags
//   /dev/kmsg               → kernel/printk/printk.c — structured ring buffer
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// TaintFlag describes one kernel taint bit.
type TaintFlag struct {
	Bit     int    `json:"bit"`
	Code    string `json:"code"`
	Meaning string `json:"meaning"`
}

// taintTable maps bit index to taint description.
// Source: include/linux/panic.h (Linux 6.9)
var taintTable = []TaintFlag{
	{0, "P", "proprietary module loaded"},
	{1, "F", "module force-loaded"},
	{2, "S", "SMP on non-SMP CPU"},
	{3, "R", "module force-removed"},
	{4, "M", "machine check error"},
	{5, "B", "bad page accessed"},
	{6, "U", "userspace set taint"},
	{7, "D", "kernel oops/BUG fired"},
	{8, "A", "ACPI table overridden"},
	{9, "W", "WARN_ON fired"},
	{10, "C", "staging driver loaded"},
	{11, "I", "firmware workaround"},
	{12, "O", "out-of-tree module"},
	{13, "E", "unsigned module"},
	{14, "L", "soft lockup detected"},
	{15, "K", "livepatch applied"},
	{16, "X", "auxiliary taint"},
	{17, "T", "randstruct layout"},
	{18, "N", "test module"},
}

// KernelHealth holds all node health state.
type KernelHealth struct {
	Version         string      `json:"version"`
	Release         string      `json:"release"`
	TaintRaw        int64       `json:"taint_raw"`
	TaintClean      bool        `json:"taint_clean"`
	ActiveTaints    []TaintFlag `json:"active_taints,omitempty"`
	PanicTimeout    int         `json:"panic_timeout_s"`
	PanicOnOops     int         `json:"panic_on_oops"`
	NMIWatchdog     int         `json:"nmi_watchdog"`
	WatchdogThresh  int         `json:"watchdog_thresh_s"`
	SoftlockupPanic int         `json:"softlockup_panic"`
	KexecLoaded     int         `json:"kexec_loaded"`
	CrashKernel     string      `json:"crash_kernel_param"`
}

// KmsgEvent holds a parsed /dev/kmsg entry.
type KmsgEvent struct {
	Level   int    `json:"level"`
	Message string `json:"message"`
}

func readSysctl(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func readSysctlInt(path string) int {
	s := readSysctl(path)
	v, _ := strconv.Atoi(s)
	return v
}

func decodeTaint(raw int64) (bool, []TaintFlag) {
	if raw == 0 {
		return true, nil
	}
	var active []TaintFlag
	for _, f := range taintTable {
		if raw&(1<<f.Bit) != 0 {
			active = append(active, f)
		}
	}
	return false, active
}

func parseCmdlineParam(param string) string {
	data, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return ""
	}
	for _, field := range strings.Fields(string(data)) {
		if strings.HasPrefix(field, param+"=") {
			return strings.TrimPrefix(field, param+"=")
		}
	}
	return ""
}

func collectHealth() KernelHealth {
	taintRaw := int64(readSysctlInt("/proc/sys/kernel/tainted"))
	clean, active := decodeTaint(taintRaw)
	return KernelHealth{
		Version:         readSysctl("/proc/version"),
		Release:         readSysctl("/proc/sys/kernel/osrelease"),
		TaintRaw:        taintRaw,
		TaintClean:      clean,
		ActiveTaints:    active,
		PanicTimeout:    readSysctlInt("/proc/sys/kernel/panic"),
		PanicOnOops:     readSysctlInt("/proc/sys/kernel/panic_on_oops"),
		NMIWatchdog:     readSysctlInt("/proc/sys/kernel/nmi_watchdog"),
		WatchdogThresh:  readSysctlInt("/proc/sys/kernel/watchdog_thresh"),
		SoftlockupPanic: readSysctlInt("/proc/sys/kernel/softlockup_panic"),
		KexecLoaded:     readSysctlInt("/sys/kernel/kexec_loaded"),
		CrashKernel:     parseCmdlineParam("crashkernel"),
	}
}

// scanKmsg reads /dev/kmsg and returns events containing OOM/BUG/WARN/panic.
// /dev/kmsg lines: "<priority>,<seq>,<timestamp_us>,-;<message>"
func scanKmsg(max int) ([]KmsgEvent, error) {
	f, err := os.OpenFile("/dev/kmsg", os.O_RDONLY|os.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var events []KmsgEvent
	scanner := bufio.NewScanner(f)
	for scanner.Scan() && len(events) < max {
		line := scanner.Text()
		// format: "priority,seq,ts,-;message"
		idx := strings.Index(line, ";")
		if idx < 0 {
			continue
		}
		meta := line[:idx]
		msg := line[idx+1:]
		// check for keywords
		lower := strings.ToLower(msg)
		if !strings.Contains(lower, "oom") &&
			!strings.Contains(lower, "bug:") &&
			!strings.Contains(lower, "warn") &&
			!strings.Contains(lower, "panic") &&
			!strings.Contains(lower, "lockup") {
			continue
		}
		lvl := 0
		parts := strings.SplitN(meta, ",", 2)
		if len(parts) > 0 {
			lvl, _ = strconv.Atoi(parts[0])
		}
		events = append(events, KmsgEvent{Level: lvl, Message: msg})
	}
	return events, nil
}

func printHealth(h KernelHealth) {
	fmt.Println("=== Kernel Health Report ===")
	fmt.Printf("  release:          %s\n", h.Release)
	fmt.Printf("  version:          %s\n", h.Version)
	fmt.Println()
	fmt.Println("=== Taint State ===")
	if h.TaintClean {
		fmt.Printf("  tainted: 0 (CLEAN)\n")
	} else {
		fmt.Printf("  tainted: %d\n", h.TaintRaw)
		for _, t := range h.ActiveTaints {
			fmt.Printf("    bit %2d (%s): %s\n", t.Bit, t.Code, t.Meaning)
		}
	}
	fmt.Println()
	fmt.Println("=== Panic Configuration ===")
	fmt.Printf("  kernel.panic:             %d  (0=halt, >0=reboot after Ns)\n", h.PanicTimeout)
	fmt.Printf("  kernel.panic_on_oops:     %d  (1=oops triggers panic)\n", h.PanicOnOops)
	fmt.Printf("  kernel.softlockup_panic:  %d  (1=soft lockup triggers panic)\n", h.SoftlockupPanic)
	fmt.Println()
	fmt.Println("=== Watchdog State ===")
	fmt.Printf("  kernel.nmi_watchdog:      %d  (1=hardlockup detection on)\n", h.NMIWatchdog)
	fmt.Printf("  kernel.watchdog_thresh:   %ds (softlockup at %ds, hardlockup at ~%ds)\n",
		h.WatchdogThresh, h.WatchdogThresh*2, h.WatchdogThresh)
	fmt.Println()
	fmt.Println("=== kdump Readiness ===")
	fmt.Printf("  kexec_loaded:  %d  (1=crash kernel loaded)\n", h.KexecLoaded)
	if h.CrashKernel != "" {
		fmt.Printf("  crashkernel:   %s\n", h.CrashKernel)
	} else {
		fmt.Printf("  crashkernel:   (not set — kdump not configured)\n")
	}
}

func main() {
	kmsgFlag := flag.Bool("kmsg", false, "Scan /dev/kmsg for OOM/BUG/WARN/panic events")
	jsonFlag := flag.Bool("json", false, "Output as JSON")
	flag.Parse()

	h := collectHealth()

	if *jsonFlag {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(h)
		return
	}

	printHealth(h)

	if *kmsgFlag {
		fmt.Println("=== Recent Kernel Messages (OOM/BUG/WARN/panic/lockup) ===")
		events, err := scanKmsg(50)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  kmsg: %v\n", err)
		} else if len(events) == 0 {
			fmt.Println("  (none found)")
		} else {
			for _, e := range events {
				fmt.Printf("  [%d] %s\n", e.Level, e.Message)
			}
		}
	}
}
```

### `go.mod`

```
module github.com/linux-to-k8s/node-health-reader

go 1.22
```

### `README.md`

```markdown
# node-health-reader

Reads Linux kernel health state for Kubernetes node audit: version, taint
flags, watchdog config, panic settings, and kdump readiness.

## Build and Run

```
go build -o node-health-reader .

# Full health report
./node-health-reader

# Include /dev/kmsg scan for OOM/BUG/WARN events
./node-health-reader --kmsg

# JSON output (for integration with monitoring)
./node-health-reader --json
```

## Sample Output

```
=== Kernel Health Report ===
  release:          6.9.3-cloud-amd64
  version:          Linux version 6.9.3 ...

=== Taint State ===
  tainted: 512
    bit  9 (W): WARN_ON fired

=== Panic Configuration ===
  kernel.panic:             10  (0=halt, >0=reboot after Ns)
  kernel.panic_on_oops:     1   (1=oops triggers panic)
  kernel.softlockup_panic:  0   (1=soft lockup triggers panic)

=== Watchdog State ===
  kernel.nmi_watchdog:      1   (1=hardlockup detection on)
  kernel.watchdog_thresh:   10s (softlockup at 20s, hardlockup at ~10s)

=== kdump Readiness ===
  kexec_loaded:  1  (1=crash kernel loaded)
  crashkernel:   512M
```

## Kernel Paths

| Source | Kernel Reference |
|--------|-----------------|
| `/proc/sys/kernel/tainted` | `include/linux/panic.h` — `TAINT_*` flags |
| `/sys/kernel/kexec_loaded` | `kernel/kexec_core.c` — `kexec_crash_loaded()` |
| `/dev/kmsg` | `kernel/printk/printk.c` — structured ring buffer |
| `/proc/sys/kernel/nmi_watchdog` | `kernel/watchdog.c` |

Sources:
https://elixir.bootlin.com/linux/v6.9/source/include/linux/panic.h
https://elixir.bootlin.com/linux/v6.9/source/kernel/kexec_core.c

## Exercises

a) Add `--taint <bit>` flag that explains what a specific taint bit means
   and shows how to check if it is set.

b) Extend `scanKmsg` to parse the timestamp field and show relative time
   (e.g., "3m ago") for each event.

c) Add a `--watch` flag that re-runs the health check every 30 seconds
   and prints a diff of what changed.
```
```

- [ ] **Step 1: Write all three files exactly as specified**
- [ ] **Step 2: Build: `go build ./...` + `go vet ./...`** — must pass clean
- [ ] **Step 3: Commit**

```bash
git add 11-cluster-ops/exercises/node-health-reader/
git commit -m "feat(ch11): Go exercise node-health-reader — taint flags, watchdog, kdump readiness, /dev/kmsg scan"
```

---

## Task 7: kube-inspect checkpoint 11 — Node health audit: kernel version, taint, watchdog

**Files:**
- Create: `kube-inspect/internal/health/health.go`
- Modify: `kube-inspect/cmd/kube-inspect/main.go` (add `--health` flag)
- Modify: `kube-inspect/CHECKPOINT.md` (mark checkpoint 11 done)

### `kube-inspect/internal/health/health.go`

```go
package health

import (
	"os"
	"strconv"
	"strings"
)

// TaintBit describes one active taint flag.
type TaintBit struct {
	Bit     int
	Code    string
	Meaning string
}

var taintTable = []TaintBit{
	{0, "P", "proprietary module"},
	{1, "F", "forced module load"},
	{2, "S", "SMP on non-SMP CPU"},
	{3, "R", "forced module rmmod"},
	{4, "M", "machine check error"},
	{5, "B", "bad page"},
	{6, "U", "userspace taint"},
	{7, "D", "kernel oops/BUG"},
	{8, "A", "ACPI table overridden"},
	{9, "W", "WARN_ON fired"},
	{10, "C", "staging driver"},
	{11, "I", "firmware workaround"},
	{12, "O", "out-of-tree module"},
	{13, "E", "unsigned module"},
	{14, "L", "soft lockup"},
	{15, "K", "livepatch"},
	{16, "X", "auxiliary taint"},
	{17, "T", "randstruct"},
	{18, "N", "test module"},
}

// NodeHealth holds kernel health state for a node.
type NodeHealth struct {
	Release         string
	TaintRaw        int64
	ActiveTaints    []TaintBit
	PanicTimeout    int
	PanicOnOops     int
	NMIWatchdog     int
	WatchdogThresh  int
	SoftlockupPanic int
	KexecLoaded     int
	CrashKernel     string
}

func readSysctl(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func readSysctlInt(path string) int {
	v, _ := strconv.Atoi(readSysctl(path))
	return v
}

func cmdlineParam(param string) string {
	data, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return ""
	}
	for _, f := range strings.Fields(string(data)) {
		if strings.HasPrefix(f, param+"=") {
			return strings.TrimPrefix(f, param+"=")
		}
	}
	return ""
}

// GetNodeHealth reads kernel version, taint state, panic config,
// watchdog settings, and kdump readiness from procfs/sysfs.
func GetNodeHealth() (NodeHealth, error) {
	taintRaw := int64(readSysctlInt("/proc/sys/kernel/tainted"))
	var active []TaintBit
	for _, t := range taintTable {
		if taintRaw&(1<<t.Bit) != 0 {
			active = append(active, t)
		}
	}
	return NodeHealth{
		Release:         readSysctl("/proc/sys/kernel/osrelease"),
		TaintRaw:        taintRaw,
		ActiveTaints:    active,
		PanicTimeout:    readSysctlInt("/proc/sys/kernel/panic"),
		PanicOnOops:     readSysctlInt("/proc/sys/kernel/panic_on_oops"),
		NMIWatchdog:     readSysctlInt("/proc/sys/kernel/nmi_watchdog"),
		WatchdogThresh:  readSysctlInt("/proc/sys/kernel/watchdog_thresh"),
		SoftlockupPanic: readSysctlInt("/proc/sys/kernel/softlockup_panic"),
		KexecLoaded:     readSysctlInt("/sys/kernel/kexec_loaded"),
		CrashKernel:     cmdlineParam("crashkernel"),
	}, nil
}
```

### `--health` flag in `main.go`

Add flag declaration (after `flagPressure`, before closing paren):
```go
flagHealth = flag.Bool("health", false, "Show kernel version, taint flags, watchdog and panic config, kdump state")
```

Add `[--health]` to the usage string after `[--perf]`.

Add output block at the bottom of the `if *flagPod != ""` block (but note: `--health` is node-level, not pod-level — it should work even without `--pod`). Actually, add it OUTSIDE the `if *flagPod != ""` block, right before the closing brace of `main()`:

```go
if *flagHealth {
    nh, err := health.GetNodeHealth()
    if err != nil {
        fmt.Fprintf(os.Stderr, "health: %v\n", err)
    } else {
        fmt.Printf("Node kernel health:\n")
        fmt.Printf("  release:          %s\n", nh.Release)
        if nh.TaintRaw == 0 {
            fmt.Printf("  tainted:          0 (clean)\n")
        } else {
            fmt.Printf("  tainted:          %d\n", nh.TaintRaw)
            for _, t := range nh.ActiveTaints {
                fmt.Printf("    bit %2d (%s): %s\n", t.Bit, t.Code, t.Meaning)
            }
        }
        fmt.Printf("  panic:            %d  panic_on_oops: %d  softlockup_panic: %d\n",
            nh.PanicTimeout, nh.PanicOnOops, nh.SoftlockupPanic)
        fmt.Printf("  nmi_watchdog:     %d  watchdog_thresh: %ds\n",
            nh.NMIWatchdog, nh.WatchdogThresh)
        if nh.KexecLoaded == 1 {
            fmt.Printf("  kdump:            ready (crashkernel=%s)\n", nh.CrashKernel)
        } else {
            fmt.Printf("  kdump:            not loaded\n")
        }
        fmt.Println()
    }
}
```

Import: `"github.com/linux-to-k8s/kube-inspect/internal/health"`

### Important: `--health` flag placement in main()

The `--health` flag is node-level (does not require `--pod`). Place the output block OUTSIDE the `if *flagPod != ""` block. The existing structure of `main()` is:

```go
flag.Parse()
if *flagPod == "" && !*flagNode {
    // usage error
}
if *flagPod != "" {
    // all pod-level flags...
}
// --health block goes here (node level, no pod required)
```

Also update the usage check: the tool should NOT error when only `--health` is given (no `--pod`, no `--node`). Update the usage check condition to:
```go
if *flagPod == "" && !*flagNode && !*flagHealth {
    fmt.Fprintln(os.Stderr, "usage: ...")
    os.Exit(1)
}
```

### `CHECKPOINT.md`

Change `| 11 | Cluster health + kernel version audit | internal/ | pending |` to `| 11 | Cluster health + kernel version audit | internal/health | done |`

- [ ] **Step 1: Read `kube-inspect/cmd/kube-inspect/main.go`** — note current import block and main() structure
- [ ] **Step 2: Read `kube-inspect/CHECKPOINT.md`** — note current row 11
- [ ] **Step 3: Create `kube-inspect/internal/health/health.go`** exactly as specified
- [ ] **Step 4: Modify `kube-inspect/cmd/kube-inspect/main.go`** — add `--health` flag, output block OUTSIDE pod block, update usage check
- [ ] **Step 5: Modify `kube-inspect/CHECKPOINT.md`** — mark row 11 done
- [ ] **Step 6: Build and vet**

```bash
cd kube-inspect
go build ./...
go vet ./...
```
Expected: clean.

- [ ] **Step 7: Commit**

```bash
cd ..
git add kube-inspect/internal/health/health.go kube-inspect/cmd/kube-inspect/main.go kube-inspect/CHECKPOINT.md
git commit -m "feat(kube-inspect): checkpoint 11 — node health audit: kernel version, taint, watchdog, kdump"
```

---

## Self-Review

**Spec coverage:**
- ✅ `11-cluster-ops/README.md` — objectives, prerequisites, reading order, kernel-to-k8s bridge diagram
- ✅ `kernel/11-a-panic.md` — struct die_args, panic() path, taint flags, oops_enter/exit (7 sections)
- ✅ `kernel/11-b-nmi-watchdog.md` — NMI, softlockup, hardlockup with PMU connection (7 sections)
- ✅ `kernel/11-c-kexec.md` — kexec_load syscall, struct kimage, kdump sequence, /proc/vmcore (7 sections)
- ✅ `k8s/11-k8s-connection.md` — NodeNotReady lifecycle, rolling upgrade, etcd, node audit (8 sections)
- ✅ `exercises/kernel-health-reader/` — C exercise reading taint/panic/watchdog/kdump
- ✅ `exercises/node-health-reader/` — Go exercise with JSON output and kmsg scanning
- ✅ `kube-inspect/internal/health/` — GetNodeHealth(), --health flag, works without --pod

**Placeholder scan:** All sections contain real content. No TBD/TODO.

**Type consistency:**
- `health.NodeHealth.ActiveTaints` is `[]TaintBit` — printed in main.go with `t.Bit`, `t.Code`, `t.Meaning` — matches struct fields
- `health.GetNodeHealth() (NodeHealth, error)` called correctly in main.go
- `--health` flag placed OUTSIDE `if *flagPod != ""` — node-level, no pod required
- Usage check updated to allow `--health` alone (`!*flagHealth` added to error condition)
