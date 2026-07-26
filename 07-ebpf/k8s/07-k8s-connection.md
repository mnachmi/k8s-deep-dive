# 07-k8s — eBPF in Kubernetes: Cilium, Tetragon, Hubble, and Falco

## The Infrastructure Beneath the Abstractions

In 2016, kube-proxy on large Kubernetes clusters had become a bottleneck. Every Service endpoint change required regenerating thousands of iptables rules. Rule insertion was O(N) — on a cluster with 10,000 Services, a single endpoint update rewrote the entire ruleset. Latency spikes during Service updates were measured in seconds. The fundamental problem was that iptables, designed in the late 1990s for stateful firewall rulesets of dozens of rules, was being used as a dynamic load balancer for thousands of frequently-changing backends.

The eBPF-based solution is architecturally different. Instead of a linear chain of match-action rules, Cilium maintains hash maps: a map from ClusterIP:port to a list of backend pod IPs, updated incrementally on each endpoint change. A BPF program attached at XDP or TC performs a map lookup — O(1) regardless of cluster size — and redirects the packet directly to the backend. On a cluster with 100,000 endpoints, the lookup time is the same as on a cluster with 10. The Service table can be updated entry by entry rather than regenerated wholesale.

The same architecture that makes networking fast makes observability deep. Tetragon attaches BPF programs to kernel tracepoints and LSM hooks, generating security events — process exec, file access, network connection — with full kernel-level context: cgroup identity, mount namespace ID, capability set at the time of the operation. These are not approximations reconstructed from userspace; they are measurements taken at the kernel boundary where the operation actually occurred. Falco uses the same mechanism. Hubble aggregates the network-level events from Cilium's BPF programs into a flow log that shows every connection attempt in the cluster, with source and destination pod identity derived from the cgroup namespace.

eBPF has become the foundation of modern Kubernetes networking and security tooling. Where kube-proxy once relied on O(N) iptables traversal, Cilium performs O(1) hash map lookups in the kernel data path. Where traditional security tools intercepted syscalls through kernel modules, Tetragon and Falco attach to kernel tracepoints and LSM hooks with full verifier safety. This document traces how each major Kubernetes eBPF project maps to the kernel hook points and map types covered in earlier chapters.

---

## 1. eBPF in Kubernetes — Architecture Overview

| Project | eBPF Hook | Map Types Used | What It Replaces / Adds |
|---------|-----------|----------------|-------------------------|
| Cilium kube-proxy replacement | XDP or TC ingress (`BPF_PROG_TYPE_SCHED_CLS`) | `BPF_MAP_TYPE_HASH`, `BPF_MAP_TYPE_ARRAY` | Replaces kube-proxy iptables DNAT rules; O(1) Service lookup |
| Cilium network policy | TC egress / ingress per endpoint | `BPF_MAP_TYPE_HASH` (policy map), `BPF_MAP_TYPE_LPM_TRIE` | Replaces iptables NetworkPolicy chains; per-pod BPF program pinned under `/sys/fs/bpf/` |
| Tetragon security events | `BPF_PROG_TYPE_TRACING` fentry/fexit on LSM hooks | `BPF_MAP_TYPE_RINGBUF` | Adds kernel-level process and network security tracing with enforcement via `bpf_send_signal` |
| Hubble network observability | Piggybacks on Cilium TC programs | `BPF_MAP_TYPE_PERF_EVENT_ARRAY` | Adds per-flow visibility: source/dest pod, L4 verdict, HTTP metadata |
| Falco eBPF driver | `BPF_PROG_TYPE_TRACEPOINT` on `sys_enter_*` / `sys_exit_*` | `BPF_MAP_TYPE_PERF_EVENT_ARRAY` | Replaces Falco kernel module; production-safe syscall capture |
| bpftrace debugging | kprobes, uprobes, tracepoints, perf events | `BPF_MAP_TYPE_HASH`, `BPF_MAP_TYPE_ARRAY`, histograms | Adds ad-hoc kernel instrumentation without recompilation |

---

## 2. Cilium — eBPF Replaces kube-proxy

Cilium's kube-proxy replacement removes iptables entirely from the Service load-balancing path. Instead, Cilium programs BPF maps and attaches TC BPF programs (or XDP programs on supported drivers) to each network interface. Every packet entering the node is intercepted before it reaches Netfilter.

### Service Maps

Cilium maintains two BPF maps for IPv4 Service resolution:

**`cilium_lb4_services`** (`BPF_MAP_TYPE_HASH`) maps a `(ClusterIP, port, protocol)` tuple to a service entry that contains the number of backends and a revision counter. The key is a packed struct; the value includes the `count` of available backends and a `flags` field indicating the Service type (ClusterIP, NodePort, ExternalIP).

**`cilium_lb4_backends`** (`BPF_MAP_TYPE_ARRAY`) maps an integer backend ID to a `(pod IP, port, flags)` entry. Array indexing makes backend selection by ID an O(1) direct lookup rather than a hash probe.

### Packet Walk — XDP / TC DNAT

When a packet arrives destined for a ClusterIP:

1. The TC ingress BPF program receives the packet as an `__sk_buff` context.
2. It extracts `(dst_ip, dst_port, protocol)` from the packet headers.
3. It calls `bpf_map_lookup_elem` on `cilium_lb4_services` with the extracted key.
4. On a hit, it selects a backend ID using consistent hashing over the `count` field.
5. It calls `bpf_map_lookup_elem` on `cilium_lb4_backends` with the backend ID.
6. It rewrites `__sk_buff->remote_ip4` and the destination port directly in the packet — no Netfilter DNAT rule traversal occurs.
7. The packet is forwarded to the backend pod through the normal routing path.

The entire lookup is O(1): one hash map probe for the Service, one array index for the backend. In contrast, kube-proxy programs one iptables DNAT rule per backend, and each packet must traverse the entire chain in O(N) until a matching rule fires.

### Conntrack Without nf_conn

Cilium maintains its own conntrack state in **`cilium_ct4_global`** (`BPF_MAP_TYPE_HASH`), keyed on the 5-tuple `(src_ip, dst_ip, src_port, dst_port, protocol)`. This replaces the kernel's `nf_conn` table managed by `nf_conntrack_hash_check_insert`. Reply packets are reverse-NATed using the conntrack entry without invoking Netfilter at all.

### Per-Endpoint Programs

Cilium generates a unique BPF program for each pod endpoint and pins it under `/sys/fs/bpf/tc/globals/`. The program encodes that pod's egress and ingress network policy, identity label set, and allowed peer CIDRs. When Kubernetes NetworkPolicy changes, Cilium regenerates and reloads only the affected endpoint programs, leaving all other pods' data paths untouched.

```bash
# List Cilium's pinned BPF objects:
ls /sys/fs/bpf/tc/globals/

# Inspect the lb4 services map:
bpftool map dump pinned /sys/fs/bpf/tc/globals/cilium_lb4_services

# Inspect the lb4 backends map:
bpftool map dump pinned /sys/fs/bpf/tc/globals/cilium_lb4_backends

# Show conntrack entries:
bpftool map dump pinned /sys/fs/bpf/tc/globals/cilium_ct4_global | head -40

# Confirm kube-proxy is not running (Cilium replacement mode):
kubectl -n kube-system get pods | grep kube-proxy
iptables -t nat -L KUBE-SERVICES 2>/dev/null || echo "No KUBE-SERVICES chain — iptables replacement active"
```

---

## 3. Tetragon — Kernel-Level Security Tracing

Tetragon is a Cilium sub-project that attaches BPF programs to kernel security hooks using `BPF_PROG_TYPE_TRACING` with fentry/fexit semantics. This gives it access to typed kernel arguments directly, without parsing raw register dumps.

### LSM Hook Attachment Points

**`security_bprm_check`** — called by the kernel during `execve` before the binary's credentials are committed. Tetragon's fentry program receives `struct linux_binprm *bprm`, from which it reads the binary path, the calling PID, and the credential set (`uid`, `gid`, `cap_effective`). This covers every process execution across all containers on the node.

**`security_socket_connect`** — called when a socket initiates a connection. Tetragon reads `struct sockaddr *address` to obtain the remote IP and port, and reads the calling task's cgroup ID to identify the Kubernetes pod. This captures every outbound TCP connection regardless of the application's language or runtime.

**`security_file_open`** — called on every file open. Tetragon reads `struct file *file` to obtain the inode number, resolved path, and open flags. Sensitive file access (e.g., `/etc/shadow`, container secrets mounted at `/var/run/secrets/`) can be detected and reported.

### Event Pipeline

```
kernel LSM hook
    └─ BPF fentry program runs (BPF_PROG_TYPE_TRACING)
         └─ bpf_ringbuf_submit() → BPF_MAP_TYPE_RINGBUF
              └─ Tetragon userspace daemon (Go)
                   └─ gRPC stream → Tetragon server
                        └─ Kubernetes-aware events (pod name, namespace, labels, process tree)
```

`BPF_MAP_TYPE_RINGBUF` is preferred over `PERF_EVENT_ARRAY` because it provides a single, ordered ring buffer shared across CPUs, eliminating the per-CPU ordering issues that affect perf buffers under high event rates.

### In-Kernel Enforcement

When a TracingPolicy rule specifies enforcement (e.g., block execution of `/bin/bash` in a production namespace), Tetragon's BPF program calls:

```c
bpf_send_signal(SIGKILL);
```

This kills the offending process from within the kernel, before the syscall returns to userspace. Enforcement latency is bounded by the time to run the BPF program — typically under one microsecond — compared to the round-trip through a userspace daemon required by ptrace-based tools.

```bash
# View Tetragon security events in real time:
kubectl exec -n kube-system ds/tetragon -c tetragon -- \
  tetra getevents --output compact

# Check Tetragon's BPF programs:
bpftool prog list | grep tetragon

# List TracingPolicy objects in the cluster:
kubectl get tracingpolicies.cilium.io
```

---

## 4. Hubble — eBPF Network Observability

Hubble provides Kubernetes-aware network flow visibility without adding any BPF programs of its own. It sits on top of Cilium's existing TC BPF data path and reads events that Cilium already emits.

### Event Collection

Every Cilium TC BPF program emits a flow event to a **`BPF_MAP_TYPE_PERF_EVENT_ARRAY`** when it makes a policy decision. The event record contains the raw 5-tuple, the policy verdict (`FORWARDED`, `DROPPED`, `REDIRECTED`), the interface index, and a timestamp. Because the map is a perf ring buffer, the Hubble daemon can consume events from all CPUs through a single file descriptor per CPU.

### Kubernetes Metadata Enrichment

The flow event contains only kernel-level data: IP addresses, ports, and interface indices. The Hubble daemon enriches each event using its local cache of Kubernetes API objects:

1. It resolves the source IP to a pod name, namespace, and label set by watching the Kubernetes API (or querying Cilium's identity cache).
2. It resolves the destination IP similarly, handling cases where the destination is a Service ClusterIP (pre-DNAT) or a pod IP (post-DNAT).
3. If Cilium's L7 proxy (Envoy) is active, additional HTTP/gRPC metadata (method, URL, status code) is appended.

### Flow Event Fields

| Field | Source | Example |
|-------|--------|---------|
| `source.pod` | K8s API | `default/frontend-7d4b9c-xjk2p` |
| `destination.pod` | K8s API | `default/backend-55f8d-mn9q1` |
| `destination.service` | K8s API | `default/backend-svc:8080` |
| `l4.tcp` | Perf event | `src_port=48231, dst_port=8080` |
| `verdict` | Cilium BPF | `FORWARDED` |
| `drop_reason` | Cilium BPF | `POLICY_DENIED` (if dropped) |

```bash
# Query Hubble flows for a specific pod:
hubble observe --pod default/frontend --follow

# Filter for dropped flows across the cluster:
hubble observe --verdict DROPPED

# Show flows to a specific service:
hubble observe --to-service default/backend-svc

# Access Hubble UI (if deployed):
kubectl port-forward -n kube-system svc/hubble-ui 12000:80
```

---

## 5. Falco — eBPF Driver

Falco is a CNCF runtime security project that detects anomalous behavior by monitoring system calls. It supports two collection mechanisms: a kernel module driver and an eBPF driver. The eBPF driver is the production-recommended choice because it does not require loading an unsigned kernel module.

### eBPF Driver Architecture

When started with `--driver ebpf`, Falco loads a set of BPF programs of type **`BPF_PROG_TYPE_TRACEPOINT`** attached to `sys_enter_*` and `sys_exit_*` raw tracepoints. These tracepoints fire on entry and exit of every syscall, providing Falco with:

- The syscall number and all arguments (from `struct pt_regs`)
- The calling PID, TID, UID, and GID
- The cgroup hierarchy (used to map to a container/pod)
- The return value (on `sys_exit_*`)

Captured events are written to a **`BPF_MAP_TYPE_PERF_EVENT_ARRAY`**, one ring buffer per CPU. The Falco userspace engine consumes events from all CPUs, reconstructs syscall argument strings (file paths, network addresses), evaluates them against rules written in the Falco rule language, and emits alerts.

### Comparison to Kernel Module Driver

| Aspect | eBPF Driver | Kernel Module Driver |
|--------|-------------|---------------------|
| Kernel module loading | Not required | Required (`insmod`) |
| Kernel version support | Requires kernel ≥ 4.14 with BTF for CO-RE | Works on older kernels |
| Safety | BPF verifier enforces memory safety | Full kernel access; bugs can panic the node |
| Upgrade path | eBPF bytecode reloaded; no reboot | Module reinsert may require reboot |
| Performance | Slightly higher per-event overhead (ring buffer copy) | Slightly lower per-event overhead |

```bash
# Start Falco with the eBPF driver:
falco --driver ebpf

# Verify which driver Falco is using:
falco --version

# List loaded Falco BPF programs:
bpftool prog list | grep falco

# Watch Falco alerts:
kubectl logs -n falco ds/falco -f
```

---

## 6. Common Failure Patterns

| Symptom | Likely Cause | Diagnosis Command |
|---------|-------------|-------------------|
| `libbpf: load bpf program failed: Permission denied` or `bpf() syscall failed: EPERM` on Cilium/Tetragon startup | BPF program rejected by verifier, or process lacks `CAP_BPF` / `CAP_NET_ADMIN` | `bpftool prog load <obj>.o /sys/fs/bpf/test 2>&1`; check `dmesg | grep BPF` for verifier log |
| Cilium map insert returns `-E2BIG`; pod networking silently degrades | BPF map `max_entries` reached; often `cilium_ct4_global` filling up under high connection rate | `bpftool map show pinned /sys/fs/bpf/tc/globals/cilium_ct4_global`; check `Used` vs `Max entries`; tune `--bpf-ct-global-max` |
| ClusterIP traffic works but `iptables -t nat -L` shows KUBE-SERVICES chains still present | Both kube-proxy and Cilium in kube-proxy-replacement mode are active; conflicting DNAT | `kubectl -n kube-system get pods | grep kube-proxy`; `cilium status | grep "KubeProxy replacement"`; remove kube-proxy DaemonSet if using full replacement |
| Hubble or Falco shows `N events dropped` in perf buffer stats | Userspace consumer is too slow; perf ring buffer overruns | `cat /sys/kernel/debug/tracing/per_cpu/cpu0/stats | grep overrun`; increase ring buffer size via `--perf-buffer-size`; check userspace CPU usage |
| Tetragon or Falco fails to load BPF programs on kernel upgrade: `BTF not found` or `CO-RE relocation failed` | Kernel does not expose BTF via `/sys/kernel/btf/vmlinux`, or BTF version mismatch | `ls -lh /sys/kernel/btf/vmlinux`; `bpftool btf dump file /sys/kernel/btf/vmlinux | head`; ensure `CONFIG_DEBUG_INFO_BTF=y` in kernel config |
| Pod with `privileged: true` or `hostNetwork: true` bypasses Cilium NetworkPolicy | eBPF policy programs are attached per-endpoint at the veth host side; privileged pods on the host network bypass the veth entirely | `bpftool net show dev eth0`; verify Cilium has programs attached on `hostNetwork` pod interfaces; use admission controllers or OPA/Gatekeeper to block privileged pods |

---

## 7. Key Kernel References

| Symbol | File | URL |
|--------|------|-----|
| `bpf_send_signal` helper (used by Tetragon enforcement) | `kernel/bpf/helpers.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/bpf/helpers.c |
| `bpf_map_update_elem` helper (used by all map write paths) | `kernel/bpf/helpers.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/bpf/helpers.c |
| `bpf_prog_run_xdp` (XDP program dispatch, used by Cilium XDP mode) | `net/core/filter.c` | https://elixir.bootlin.com/linux/v6.9/source/net/core/filter.c |
| `security_bprm_check` (LSM hook for execve, Tetragon attachment point) | `security/security.c` | https://elixir.bootlin.com/linux/v6.9/source/security/security.c |
| `security_socket_connect` (LSM hook for TCP connect, Tetragon attachment point) | `security/security.c` | https://elixir.bootlin.com/linux/v6.9/source/security/security.c |
| `nf_conntrack_hash_check_insert` (kernel conntrack insert, replaced by Cilium's own BPF conntrack) | `net/netfilter/nf_conntrack_core.c` | https://elixir.bootlin.com/linux/v6.9/source/net/netfilter/nf_conntrack_core.c |
| `trace_call_bpf` (perf event / tracepoint BPF dispatch, used by Falco tracepoint programs) | `kernel/trace/bpf_trace.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/trace/bpf_trace.c |
| `bpf_ringbuf_submit` (ring buffer commit, used by Tetragon event pipeline) | `kernel/bpf/ringbuf.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/bpf/ringbuf.c |
