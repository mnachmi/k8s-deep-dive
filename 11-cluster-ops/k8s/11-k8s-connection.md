# 11 — Kubernetes Bridge: Cluster Operations and Kernel Internals

## 1. Architecture Overview

| Event | Kernel Signal | K8s Response |
|-------|--------------|--------------|
| Kernel panic | `panic()` → halt/reboot | kubelet dies → NodeNotReady after 40s |
| Soft lockup | `kernel.softlockup_panic=1` → panic | Same as panic path |
| OOM kill | `memory.events oom_kill` | Pod evicted; node pressure taint if sustained |
| NMI watchdog | hardlockup → reboot (if `panic=N`) | NodeNotReady; node re-registers after reboot |
| kexec reboot | `machine_kexec()` → crash kernel | Node offline; rejoins after kdump + restart |
| Tainted kernel | `/proc/sys/kernel/tainted` ≠ 0 | No automatic action; operator audit only |

## 2. NodeNotReady Lifecycle

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
     │    node.kubernetes.io/not-ready:NoExecute
     │    node.kubernetes.io/unreachable:NoExecute (effect after 5s default)
     ▼
Pods with no toleration: evicted after tolerationSeconds (default 300s)
     │
     ▼
Node rejoins: kubelet re-registers → heartbeat resumes
     │  Taints removed automatically by node-lifecycle-controller
     │  Pods rescheduled on node (if not evicted to other nodes)
```

## 3. Rolling Upgrade — Kernel Perspective

A rolling node upgrade (drain → upgrade kernel → reboot → uncordon) involves:

1. `kubectl drain <node>` — sets `node.kubernetes.io/unschedulable:NoSchedule`, evicts all evictable pods
2. `apt upgrade linux-image-6.9` + `reboot` — the kernel performs orderly shutdown:
   - `kernel_restart()` → `migrate_to_reboot_cpu()` (migrate all tasks to CPU 0)
   - `device_shutdown()` (flush I/O, unmount filesystems)
   - `machine_restart()` → `reboot` syscall or ACPI reset
3. Node reboots, kubelet starts, re-registers with API server
4. `kubectl uncordon <node>` — removes unschedulable taint, allows scheduling

## 4. etcd Backup — Kernel Connection

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

## 5. Node Health Audit

```bash
# Kernel version and taint state
uname -r
cat /proc/sys/kernel/tainted
cat /proc/version

# Kernel parameters relevant to node health
sysctl kernel.panic kernel.panic_on_oops kernel.nmi_watchdog kernel.watchdog_thresh

# kdump readiness
cat /sys/kernel/kexec_crash_loaded
cat /proc/cmdline | grep crashkernel

# Check for past oops/panics
dmesg -T | grep -E 'BUG:|Oops|WARN|Call Trace|Kernel panic'
journalctl -k --since "1 day ago" | grep -E 'panic|BUG|lockup'

# Pod failure rates due to node issues
kubectl get events --field-selector type=Warning --all-namespaces | grep -i 'oom\|evict\|notready'
```

## 6. Common Failure Patterns

| Symptom | Kernel Cause | Diagnosis |
|---------|-------------|-----------|
| Node reboots every few hours | NMI watchdog hardlockup | `dmesg | grep watchdog` |
| Node NotReady, stays up | kubelet OOM-killed | `journalctl -u kubelet` + `oom_score_adj` |
| Node tainted (bit 9 = WARN) | Driver BUG_ON/WARN | `dmesg | grep WARN`; check modules |
| Pods evicted during upgrade | PodDisruptionBudget not set | Set `minAvailable` in PDB |
| etcd latency spikes | Kernel writeback competing | `iostat -x` + `vm.dirty_background_ratio` |
| No crash dump after panic | kdump not configured | `cat /sys/kernel/kexec_crash_loaded` |

## 7. Verification Commands

```bash
# Full node health snapshot
echo "=== Kernel Version ===" && uname -r && cat /proc/version
echo "=== Taint State ===" && cat /proc/sys/kernel/tainted
echo "=== Watchdog ===" && sysctl kernel.nmi_watchdog kernel.watchdog_thresh kernel.softlockup_panic
echo "=== Kdump ===" && cat /sys/kernel/kexec_crash_loaded
echo "=== Panic Config ===" && sysctl kernel.panic kernel.panic_on_oops

# Recent kernel warnings
dmesg -T | tail -100 | grep -c WARN
```

## 8. Key Kernel References

| Symbol | File | URL |
|--------|------|-----|
| `panic()` | `kernel/panic.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/panic.c |
| `TAINT_*` flags | `include/linux/panic.h` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/panic.h |
| `watchdog_overflow_callback()` | `kernel/watchdog_hld.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/watchdog_hld.c |
| `crash_kexec()` | `kernel/kexec_core.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/kexec_core.c |
| `machine_restart()` | `arch/x86/kernel/reboot.c` | https://elixir.bootlin.com/linux/v6.9/source/arch/x86/kernel/reboot.c |
| `kernel_restart()` | `kernel/reboot.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/reboot.c |
