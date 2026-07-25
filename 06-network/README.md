# Chapter 06 — Networking

Every packet entering or leaving a Kubernetes pod passes through the Linux kernel networking stack. A single HTTP request from a pod crosses multiple layers: the container's veth pair, the host bridge or routing table, iptables NAT rules set by kube-proxy, and the physical NIC. This chapter teaches the kernel networking stack from the core data structures (sk_buff, struct sock, struct net) through Netfilter to how Kubernetes assembles pod networks using CNI plugins, iptables DNAT, and conntrack. The same mechanisms power NetworkPolicy enforcement, pod-to-pod routing, and the ServiceIP abstraction.

## Learning Objectives

By the end of this chapter you will be able to:

1. Trace a packet from NIC receive through the TCP/IP stack to a user process.
2. Explain `struct sk_buff`: the packet buffer data structure, headroom/tailroom, and cloning.
3. Explain how network namespaces isolate pod network stacks and how veth pairs connect them to the host.
4. Trace how kube-proxy programs iptables DNAT rules for a ClusterIP Service.
5. Interpret `/proc/pid/net/dev` and `ss` output to diagnose pod network issues.

## Prerequisites

- **Chapter 02** — network namespaces (`clone(CLONE_NEWNET)`), clone flags, and how the kernel assigns each process a `struct net` via its `nsproxy`. Understanding how the kernel associates a process with a network namespace is a prerequisite before the per-namespace routing tables and device lists in this chapter make sense.
- **Chapter 03** — cgroup-based traffic control. The `net_cls` and `net_prio` cgroup subsystems tag `sk_buff` packets so that traffic control qdiscs can apply bandwidth limits per cgroup — bridging the concepts from Chapter 03 directly into the packet path explained here.

## Linux Network Stack — Container Context

```
User Process (container)
  │  write(sock_fd, buf, len)
  ▼
struct socket → struct sock (tcp_sock)
  │  tcp_sendmsg() → ip_queue_xmit()
  ▼
struct sk_buff (allocated, headers pushed)
  │  Routing decision (ip_route_output)
  ▼
Netfilter OUTPUT hooks (iptables OUTPUT chain)
  │  conntrack, DNAT (kube-proxy rules)
  ▼
struct net_device (veth0 in pod netns)
  │  veth_xmit() → peer device (veth1 in host netns)
  ▼
Bridge / routing in host netns
  │  Netfilter POSTROUTING (MASQUERADE for egress)
  ▼
Physical NIC (tx queue → DMA → wire)

Receive path (reverse):
NIC interrupt → NAPI poll → netif_receive_skb()
  → Netfilter PREROUTING → route lookup
  → ip_local_deliver() → tcp_v4_rcv()
  → sock receive queue → process read()
```

## Reading Order

Work through the files in this order. Each document builds on the previous.

| File | Topic |
|------|-------|
| `kernel/06-a-skbuff.md` | `struct sk_buff` and `struct net_device`: the core packet buffer and device abstraction, receive and transmit paths, NAPI |
| `kernel/06-b-socket.md` | `struct socket`, `struct sock`, `tcp_sock`: the socket layer, send/receive buffers, and state machine |
| `kernel/06-c-netns.md` | Network namespaces (`struct net`), per-namespace routing tables, veth pairs, and how pods get isolated stacks |
| `kernel/06-d-netfilter.md` | Netfilter hooks, iptables, conntrack (`nf_conntrack`), and how DNAT/SNAT work at the kernel level |
| `k8s/06-k8s-connection.md` | How CNI plugins wire pod networks, how kube-proxy programs DNAT rules, and how ServiceIP routing works end-to-end |
| `exercises/tcp-echo-demo/` | C program: TCP echo server and client running in separate network namespaces; trace the full send/receive path with bpftrace |
| `exercises/netns-inspector/` | Go tool that reads `/proc/<pid>/net/dev`, resolves veth peer indexes, and renders the full pod-to-node network topology |
