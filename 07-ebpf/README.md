# Chapter 07 — eBPF

For two decades, extending the Linux kernel with custom logic required one of three approaches: modify the kernel and submit a patch (6-12 month upstream process), write a kernel module (native code, one bug causes a panic), or use ptrace and system call interposition (50× overhead, misses kernel-internal events). None of these options were viable for production observability or networking tools that needed to run on any Linux kernel version without rebooting.

eBPF changed this. In 2014, Alexei Starovoitov rewrote the classic BPF packet filter as a general-purpose in-kernel virtual machine with 11 64-bit registers, a verifier that proves program safety at load time, and a JIT compiler that translates BPF bytecode to native machine code. For the first time, userspace could inject logic into the kernel at runtime — attaching programs to arbitrary function calls, tracepoints, network interfaces, and socket operations — with the verifier guaranteeing that the program would terminate, not crash the kernel, and not access memory out of bounds.

The Kubernetes ecosystem was transformed by this capability. Cilium replaced kube-proxy's O(N) iptables rules with O(1) BPF map lookups. Falco and Tetragon replaced fragile kernel-module-based syscall interception with safe, stable tracepoint hooks. Hubble attached to the Cilium BPF data path to build a cluster-wide connection audit log without any packet copying. The production infrastructure of major cloud providers — Facebook, Google, Cloudflare — is built on eBPF programs that would have required kernel modifications five years ago.

eBPF turns the Linux kernel into a programmable platform: user-supplied programs are verified, JIT-compiled to native code, and attached to hundreds of hook points without kernel recompilation or module loading. This chapter goes from the BPF instruction set architecture through maps, tracing probes, and network data-path hooks — and shows how Cilium, Tetragon, and Hubble build production Kubernetes infrastructure on top.

## Learning Objectives

1. Understand the BPF ISA (registers, instruction encoding, helper call convention) and how the verifier proves safety
2. Read and write BPF maps (hash, array, ringbuf) using the raw `bpf(2)` syscall
3. Attach eBPF programs via kprobes, tracepoints, fentry/fexit, and understand CO-RE
4. Trace XDP and TC hook points in the network data path
5. Understand how Cilium replaces kube-proxy and how Tetragon provides kernel-level security observability

## Prerequisites

- Chapter 02 — Namespaces (network namespaces, veth pairs)
- Chapter 06 — Networking (sk_buff, Netfilter hooks, kube-proxy)
- Familiarity with bpftrace (used throughout for live observation)

## Reading Order

| File | Topic |
|------|-------|
| `kernel/07-a-ebpf-arch.md` | BPF ISA, `struct bpf_prog`, verifier, JIT |
| `kernel/07-b-maps.md` | Map types, `struct bpf_htab`, ringbuf, BTF |
| `kernel/07-c-tracing.md` | kprobes, tracepoints, fentry/fexit, CO-RE |
| `kernel/07-d-network.md` | XDP, TC, sockops — eBPF in the data path |
| `k8s/07-k8s-connection.md` | Cilium, Tetragon, Hubble |
| `exercises/bpf-syscall-demo/` | C: raw `bpf(2)` — map create, prog load, socket filter |
| `exercises/bpf-fdinfo-reader/` | Go: list BPF FDs per process from `/proc/fdinfo` |
| `kube-inspect` checkpoint 07 | Enumerate BPF objects per pod |

## Network Stack Placement

```
User space: bpf() syscall ─────────────────────────────────────┐
                                                                 │
Kernel:                                                          ▼
  XDP hook (driver layer)        NIC driver → xdp_frame         │
  ↓                                                              │
  TC ingress (qdisc layer)       skb arrives → cls_bpf          │
  ↓                                                              │
  Netfilter hooks (NF_INET_*)    nf_hook_ops (ch06)             │
  ↓                                                              │
  Socket layer                   sock_ops / sk_msg               │
  ↓                                                              │
  System calls                   kprobe / tracepoint / fentry   │
                                                                  ▼
                                 BPF maps (shared data plane) ◄──┘
```
