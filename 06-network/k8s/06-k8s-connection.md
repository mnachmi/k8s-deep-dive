# 06-k8s — Kubernetes Networking: How Pods and Services Connect

Kubernetes networking is built entirely on Linux kernel primitives: network namespaces, veth pairs, bridges, routing tables, Netfilter, conntrack, and (optionally) eBPF. This document traces the complete path a packet takes from one pod to a Service and back, and explains how each Kubernetes networking concept maps to a specific kernel mechanism.

---

## 1. Pod Networking Model

| Mechanism | Kernel Component | Purpose |
|-----------|-----------------|---------|
| Pod IP assignment | `struct net` + veth pair | Each pod gets its own network namespace with a unique IP |
| Pod-to-pod (same node) | Bridge (`cni0`) + routing | Packets travel via the host bridge or direct host routes |
| Pod-to-pod (cross node) | Overlay (VXLAN/IPIP) or BGP | CNI plugin encapsulates or routes packets between nodes |
| Service ClusterIP | iptables DNAT + conntrack | kube-proxy programs DNAT rules; conntrack handles reply translation |
| Service NodePort | iptables DNAT + MASQUERADE | External traffic DNATed to pod; MASQUERADE rewrites src for hairpin NAT |
| NetworkPolicy | iptables/nftables/eBPF | CNI plugin enforces L3/L4 ingress/egress policy per pod |
| DNS (CoreDNS) | UDP socket, `struct sock` | In-cluster DNS resolves Service names to ClusterIPs |

---

## 2. CNI Plugin Lifecycle

The Container Network Interface (CNI) standard defines how kubelet delegates pod network setup and teardown to a plugin binary. The lifecycle follows five steps:

1. **Pod scheduled**: kubelet calls the CRI (containerd/CRI-O), which creates a pod sandbox — a new network namespace (`struct net`) — before any containers start.
2. **CNI ADD called**: kubelet executes the CNI plugin binary, passing the pod network namespace file descriptor and pod metadata (name, namespace, container ID).
3. **Network wired**: the CNI plugin creates a veth pair, moves one end (`eth0`) into the pod network namespace, assigns the pod IP address and a default route, and connects the host end to the node bridge or routes it directly.
4. **IP returned**: the CNI plugin writes the assigned IP and interface details as JSON to stdout. kubelet stores the IP in the pod's status in the API server.
5. **Pod deletion (CNI DEL)**: when the pod is terminated, kubelet calls the CNI plugin with the `DEL` command; the plugin removes the veth pair, releases the IP back to IPAM, and cleans up routes.

### Verification Commands

```bash
# Find the PID of a process inside the pod (replace with actual container ID):
CPID=$(crictl inspect --output go-template --template '{{.info.pid}}' <container-id>)

# Show the pod's network namespace inode (matches /proc/<pid>/ns/net):
ls -la /proc/$CPID/ns/net

# Identify network namespace by PID:
ip netns identify $CPID

# Show interfaces inside the pod network namespace:
nsenter -t $CPID -n ip link

# Find the host-side veth peer (note the index from the above command, e.g., 5):
ip link show | grep -A1 "^5:"

# Show routes inside the pod:
nsenter -t $CPID -n ip route show
```

---

## 3. Service ClusterIP Packet Walk

A ClusterIP Service is a virtual IP that exists only in iptables (or IPVS) rules — no socket listens on it in the kernel. The following traces a complete connection from a client pod to a Service and back.

**Setup**: client pod IP `10.244.0.3`, Service ClusterIP `10.96.0.1:443`, backend pod IP `10.244.1.5:443`.

1. **Client sends SYN** to `10.96.0.1:443`. The packet leaves the pod network namespace via `eth0` → veth → host bridge.
2. **PREROUTING hook fires**. Conntrack sees a new flow and allocates a `struct nf_conn` entry; the entry has no flags set yet (specifically, it lacks `IPS_CONFIRMED` until the packet actually leaves the box).
3. **KUBE-SERVICES chain matches** the destination `10.96.0.1:443` and jumps to the per-Service chain `KUBE-SVC-<hash>`.
4. **KUBE-SVC selects a backend** using `--probability` logic (1/N per rule). Suppose `KUBE-SEP-ABC` wins.
5. **DNAT applied**: the iptables NAT hook rewrites the packet destination from `10.96.0.1:443` to `10.244.1.5:443`. Conntrack records the original tuple `(10.244.0.3:src_port → 10.96.0.1:443)` and reply tuple `(10.244.1.5:443 → 10.244.0.3:src_port)`; status gains `IPS_DST_NAT`.
6. **Conntrack entry confirmed**: on the first accepted packet, `nf_conntrack_hash_check_insert` inserts the entry into the hash table; status gains `IPS_CONFIRMED`.
7. **Packet routed to backend**: the kernel routes the packet (now dst=`10.244.1.5:443`) to the backend pod via the node bridge (same node) or an overlay/BGP route (cross-node).
8. **Backend sends SYN-ACK**: the reply packet has src=`10.244.1.5:443`, dst=`10.244.0.3:src_port`.
9. **Conntrack reverse-DNAT**: the reply matches the stored reply tuple; conntrack rewrites the source from `10.244.1.5:443` back to `10.96.0.1:443` before the packet is delivered.
10. **Client receives SYN-ACK** appearing to come from `10.96.0.1:443` — the ClusterIP — exactly as if a server were listening there. The DNAT is fully transparent to the application.

---

## 4. NetworkPolicy Implementation

NetworkPolicy is an API object that specifies allowed ingress and egress traffic for a set of pods. The Kubernetes API server stores the policy, but enforcement is entirely the CNI plugin's responsibility — kube-proxy does not participate.

Implementation approaches vary by CNI plugin:

**Calico** (iptables/nftables):
- Uses `ipset` to represent sets of pod IPs matching a selector
- Installs iptables rules in the `filter` table (INPUT/FORWARD chains) that match src/dst against IP sets
- Policy changes update ipsets atomically; no per-pod rule duplication

**Cilium** (eBPF/tc):
- Attaches BPF programs to the `tc ingress` hook on each pod-facing veth interface
- Policy rules compiled into BPF bytecode; pod IP sets stored in BPF hash maps
- BPF program checks packet against the map and returns `TC_ACT_OK` (allow) or `TC_ACT_SHOT` (drop)
- Advantage: policy enforcement in the kernel data path, no iptables traversal

**Flannel** (no policy enforcement):
- Flannel itself provides only routing/overlay; it does not implement NetworkPolicy
- Requires a separate policy controller (e.g., Calico as a network policy provider alongside Flannel VXLAN)

### tc BPF Hook Example (Cilium pattern)

```bash
# Show tc BPF programs attached to a pod veth on the host:
tc filter show dev <veth-name> ingress

# View BPF map contents (policy map for a pod endpoint):
bpftool map dump name cilium_policy_<id>
```

---

## 5. Pod DNS Resolution (CoreDNS)

Every pod's `/etc/resolv.conf` is injected by kubelet and points to the CoreDNS Service ClusterIP (typically `10.96.0.10`). DNS resolution for a Service name follows four steps:

1. **Application calls `getaddrinfo("nginx.default.svc.cluster.local")`**. glibc opens a UDP socket (`struct sock`) and sends a DNS query to `10.96.0.10:53`.
2. **DNAT intercepts the DNS packet**: kube-proxy has a DNAT rule for `10.96.0.10:53` → one of the CoreDNS pod IPs. conntrack records the translation.
3. **CoreDNS responds** with the ClusterIP of the `nginx` Service (e.g., `10.96.1.100`). The reply is reverse-DNATed back through conntrack so it appears to come from `10.96.0.10:53`.
4. **Application connects to the ClusterIP** (`10.96.1.100`). kube-proxy's DNAT rules then handle Service load balancing for the actual connection, as described in Section 3.

---

## 6. Common Network Failure Patterns

| Symptom | Likely Cause | Diagnosis Command |
|---------|-------------|-------------------|
| Pod can't reach Service | kube-proxy iptables rules missing or stale | `iptables-save \| grep KUBE-SVC-<hash>`; check kube-proxy pod logs |
| Pod IP not routable between nodes | CNI misconfiguration (routing or overlay) | `ip route show`; `ip link show`; check CNI plugin logs |
| DNS lookup fails | CoreDNS pod down or crashlooping | `kubectl -n kube-system get pods -l k8s-app=kube-dns` |
| conntrack table full | High connection rate exhausting conntrack slots | `sysctl net.nf_conntrack_count`; compare with `net.nf_conntrack_max` |
| Intermittent packet loss (SYN dropped) | conntrack hash insert race (`insert_failed`) | `conntrack -S \| grep insert_failed`; increase `net.nf_conntrack_buckets` |
| Service latency spikes at scale | iptables O(N) linear rule scan too slow | `iptables-save \| grep -c KUBE`; consider migrating kube-proxy to IPVS mode |

---

## 7. Key Kernel References

| Symbol | File | Source |
|--------|------|--------|
| `struct nf_conn` | `include/net/netfilter/nf_conntrack.h` | https://elixir.bootlin.com/linux/v6.9/source/include/net/netfilter/nf_conntrack.h |
| `struct net` | `include/net/net_namespace.h` | https://elixir.bootlin.com/linux/v6.9/source/include/net/net_namespace.h |
| `veth_xmit` | `drivers/net/veth.c` | https://elixir.bootlin.com/linux/v6.9/source/drivers/net/veth.c |
| `nf_conntrack_hash_check_insert` | `net/netfilter/nf_conntrack_core.c` | https://elixir.bootlin.com/linux/v6.9/source/net/netfilter/nf_conntrack_core.c |
| `ip_vs_schedule` | `net/netfilter/ipvs/ip_vs_core.c` | https://elixir.bootlin.com/linux/v6.9/source/net/netfilter/ipvs/ip_vs_core.c |
| `tcp_v4_rcv` | `net/ipv4/tcp_ipv4.c` | https://elixir.bootlin.com/linux/v6.9/source/net/ipv4/tcp_ipv4.c |
| `tcp_v4_connect` | `net/ipv4/tcp_ipv4.c` | https://elixir.bootlin.com/linux/v6.9/source/net/ipv4/tcp_ipv4.c |
