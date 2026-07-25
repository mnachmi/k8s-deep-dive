# 06-d — Netfilter, conntrack, and kube-proxy packet filtering

Netfilter is the Linux kernel framework that intercepts packets at well-defined points in the network stack. It is the foundation for iptables, nftables, connection tracking (conntrack), NAT, and IPVS. In Kubernetes, kube-proxy (iptables mode) programs Netfilter rules to implement Service load balancing; conntrack makes the NAT transparent on the return path.

---

## 1. Source Locations

| File | Contents | Source |
|------|----------|--------|
| `include/linux/netfilter.h` | `NF_HOOK`, hook priorities, `struct nf_hook_ops`, verdict codes | https://elixir.bootlin.com/linux/v6.9/source/include/linux/netfilter.h |
| `net/netfilter/nf_conntrack_core.c` | Connection tracking core: `nf_conntrack_hash_check_insert`, tuple lookup, GC | https://elixir.bootlin.com/linux/v6.9/source/net/netfilter/nf_conntrack_core.c |
| `include/net/netfilter/nf_conntrack.h` | `struct nf_conn`, `struct nf_conntrack_tuple`, `IPS_*` status flags | https://elixir.bootlin.com/linux/v6.9/source/include/net/netfilter/nf_conntrack.h |
| `net/ipv4/netfilter/iptable_nat.c` | IPv4 NAT table registration, DNAT/SNAT hook registration | https://elixir.bootlin.com/linux/v6.9/source/net/ipv4/netfilter/iptable_nat.c |
| `net/netfilter/ipvs/ip_vs_core.c` | IPVS core: `ip_vs_schedule`, Netfilter hook registration, backend selection | https://elixir.bootlin.com/linux/v6.9/source/net/netfilter/ipvs/ip_vs_core.c |

---

## 2. Netfilter Hook Framework

Netfilter defines five hook points in the IPv4/IPv6 packet path. Every packet that enters or leaves the kernel passes through these points in order, and registered hook functions are called at each one.

```
NF_INET_PRE_ROUTING   (0) — first hook after IP header validation; DNAT happens here
NF_INET_LOCAL_IN      (1) — packets destined for this host; iptables INPUT chain
NF_INET_FORWARD       (2) — packets being forwarded through the host; FORWARD chain
NF_INET_LOCAL_OUT     (3) — locally-generated packets leaving the host; OUTPUT chain
NF_INET_POST_ROUTING  (4) — last hook before NIC transmit; SNAT/MASQUERADE happens here
```

Each hook point holds an ordered list of `struct nf_hook_ops` registered by kernel modules (iptables, nftables, conntrack, eBPF). Modules register hooks at load time; the kernel calls them in priority order for every packet.

### struct nf_hook_ops

```c
// include/linux/netfilter.h
struct nf_hook_ops {
    nf_hookfn          *hook;      // the hook function pointer
    struct net_device  *dev;       // device filter (NULL = all devices)
    void               *priv;      // private data passed to hook function
    u8                  pf;        // protocol family: NFPROTO_IPV4, NFPROTO_IPV6
    enum nf_hook_ops_type type;    // NF_HOOK_OP_UNDEFINED, NF_HOOK_OP_NF_TABLES, ...
    unsigned int        hooknum;   // NF_INET_PRE_ROUTING, NF_INET_LOCAL_IN, etc.
    int                 priority;  // lower number = called first; determines order among hooks
};
```

Source: https://elixir.bootlin.com/linux/v6.9/source/include/linux/netfilter.h

### Hook Return Verdicts

Each hook function must return one of these verdict codes to tell Netfilter what to do with the packet:

| Verdict | Value | Meaning |
|---------|-------|---------|
| `NF_ACCEPT` | 1 | Continue processing the packet normally |
| `NF_DROP` | 0 | Silently discard the packet |
| `NF_STOLEN` | 2 | The hook function takes ownership of the `sk_buff`; no further processing |
| `NF_QUEUE` | 3 | Send the packet to userspace via NFQUEUE for decision |
| `NF_REPEAT` | 4 | Call this hook function again on the same packet |

### Priority Ordering: Why conntrack Before NAT

Hook priority determines invocation order among hooks registered at the same hook point. Key priorities:

- `NF_IP_PRI_CONNTRACK` = **-200** — connection tracking
- `NF_IP_PRI_NAT_DST` = **-100** — DNAT (destination NAT)

Conntrack runs at -200, which is numerically less than -100, so it is called **first**. This ordering is not arbitrary: conntrack must see the original, untranslated packet — with the original source and destination addresses — before NAT modifies them. Only by recording the original tuple can conntrack later apply the correct reverse translation to reply packets. If NAT ran first, conntrack would record the post-NAT address as the "original," and the reply translation would be wrong.

---

## 3. struct nf_conn — The conntrack Entry

Every new connection causes the kernel to allocate a `struct nf_conn` entry in a hash table. Subsequent packets belonging to the same connection are looked up in this table rather than re-evaluated against all rules.

```c
// include/net/netfilter/nf_conntrack.h (key fields)
struct nf_conn {
    struct nf_conntrack             ct_general;                  // reference count
    spinlock_t                      lock;
    u32                             timeout;                     // expiry in jiffies
    struct nf_conntrack_tuple_hash  tuplehash[IP_CT_DIR_MAX];   // original + reply tuples
    unsigned long                   status;                      // IPS_CONFIRMED, IPS_SRC_NAT, IPS_DST_NAT, IPS_DYING
    possible_net_t                  ct_net;                      // network namespace
    struct nf_ct_ext               *ext;                         // extensions: NAT, seqadj, acct, labels, timestamp
    union nf_conntrack_proto        proto;                       // protocol-specific: tcp state, udp reply count
};
```

Source: https://elixir.bootlin.com/linux/v6.9/source/include/net/netfilter/nf_conntrack.h

### Two Tuples: Original and Reply

`tuplehash[IP_CT_DIR_MAX]` holds exactly two entries:

- `tuplehash[IP_CT_DIR_ORIGINAL]` — the original packet direction: source IP:port → destination IP:port as seen before any NAT
- `tuplehash[IP_CT_DIR_REPLY]` — the expected reply direction: after NAT translation, what the return packet will look like

### DNAT Example: ClusterIP → PodIP

For a kube-proxy DNAT rule that maps `10.96.0.1:443` (ClusterIP) to `10.244.1.5:443` (PodIP):

1. SYN packet arrives with dst=`10.96.0.1:443`. Conntrack allocates `nf_conn`; original tuple = `(src, 10.96.0.1:443)`.
2. NAT hook fires (priority -100, after conntrack): dst rewritten to `10.244.1.5:443`. Reply tuple = `(10.244.1.5:443, src)`.
3. Reply (SYN-ACK) arrives with src=`10.244.1.5:443`. Conntrack matches reply tuple; reverse-DNAT rewrites src back to `10.96.0.1:443`.
4. Originating pod receives the SYN-ACK appearing to come from `10.96.0.1:443` — the DNAT is fully transparent.

### IPS_* Status Flags

The `status` field is a bitmask of `IPS_*` flags that track the state of the conntrack entry:

| Flag | Meaning |
|------|---------|
| `IPS_CONFIRMED` | Entry has been inserted into the hash table (first packet committed) |
| `IPS_SRC_NAT` | Source NAT (SNAT/MASQUERADE) is applied to this connection |
| `IPS_DST_NAT` | Destination NAT (DNAT) is applied to this connection |
| `IPS_DYING` | Entry is being removed; no new packets accepted |

---

## 4. iptables Chains and kube-proxy Rules

kube-proxy (iptables mode) installs rules in two main iptables tables: `nat` (for PREROUTING and OUTPUT) and `filter`. The PREROUTING chain is the critical path for Service load balancing.

### Chain Hierarchy

```
PREROUTING
  └─ KUBE-SERVICES          (jump from PREROUTING and OUTPUT)
       ├─ KUBE-SVC-<hash>   (matches ClusterIP:port for each Service)
       │    ├─ KUBE-SEP-<hash1>  (probability 0.33333) → DNAT → pod1:port
       │    ├─ KUBE-SEP-<hash2>  (probability 0.5)     → DNAT → pod2:port
       │    └─ KUBE-SEP-<hash3>  (no probability check) → DNAT → pod3:port
       └─ KUBE-NODEPORTS    (matches NodePort for external traffic)
            └─ KUBE-SVC-<hash>
```

### Actual iptables Rule Format

```
-A KUBE-SVC-XYZ -m statistic --mode random --probability 0.33333 -j KUBE-SEP-ABC
-A KUBE-SEP-ABC -p tcp -j DNAT --to-destination 10.244.1.5:8080

-A KUBE-SVC-XYZ -m statistic --mode random --probability 0.50000 -j KUBE-SEP-DEF
-A KUBE-SEP-DEF -p tcp -j DNAT --to-destination 10.244.2.7:8080

-A KUBE-SVC-XYZ -j KUBE-SEP-GHI
-A KUBE-SEP-GHI -p tcp -j DNAT --to-destination 10.244.3.9:8080
```

### Probability Calculation

For N equally-weighted endpoints, kube-proxy computes `--probability` as `1 / remaining_endpoints` at each rule in sequence:

- Rule 1: `1/3 ≈ 0.33333` — fires with probability 1/3; if it fires, pod 1 is selected
- Rule 2: `1/2 = 0.50000` — evaluated only if rule 1 did not fire (probability 2/3 of reaching here); `(2/3) × (1/2) = 1/3` — pod 2 selected with probability 1/3
- Rule 3: no check — evaluated only if rules 1 and 2 both did not fire (probability 1/3); pod 3 selected with probability 1/3

All three endpoints receive equal traffic: each with probability exactly 1/N.

---

## 5. IPVS Mode

kube-proxy supports an alternative IPVS mode that replaces iptables chains with the kernel's IP Virtual Server (IPVS) load balancer. IPVS was designed specifically for high-throughput load balancing and uses hash tables internally.

### Why IPVS Scales Better

| Mode | Lookup Complexity | 10,000 services |
|------|-------------------|-----------------|
| iptables | O(N) linear rule scan per packet | ~10,000 rules evaluated per packet |
| IPVS | O(1) hash table lookup | Constant time regardless of service count |

As the number of Services grows, iptables performance degrades linearly because every packet must traverse every rule until a match is found. IPVS performs a single hash lookup against a virtual server table, making it the correct choice for clusters with 10,000 or more Services.

### IPVS Kernel Integration

IPVS registers its Netfilter hook at `NF_INET_LOCAL_IN` rather than `NF_INET_PRE_ROUTING`. For each incoming packet destined for a virtual server IP, `ip_vs_schedule()` selects a real server (backend pod) according to the configured scheduling algorithm (round-robin, least-connection, etc.). IPVS then uses conntrack for connection state tracking, ensuring reply packets are correctly handled without re-scheduling.

Source: https://elixir.bootlin.com/linux/v6.9/source/net/netfilter/ipvs/ip_vs_core.c

IPVS mode is recommended when the cluster has more than approximately 1,000 Services, or when network latency is a concern and the overhead of O(N) rule traversal is measurable.

---

## 6. Live Observation

```bash
# View all iptables rules including kube-proxy DNAT rules:
iptables-save | grep -E "KUBE|DNAT"

# Count kube-proxy iptables rules (scales with number of Services × endpoints):
iptables-save | grep -c KUBE

# Show all active conntrack entries (one line per connection):
conntrack -L

# Show conntrack statistics per CPU (insert_failed indicates hash collisions / SYN drops):
conntrack -S

# IPVS virtual server and real server table (kube-proxy must be in ipvs mode):
ipvsadm -Ln

# bpftrace: trace new conntrack entries being inserted into the hash table
bpftrace -e 'kprobe:nf_conntrack_hash_check_insert {
    printf("conntrack insert: pid=%d comm=%s\n", pid, comm);
}'

# bpftrace: trace every iptables table evaluation (high frequency — use with care)
bpftrace -e 'kprobe:ipt_do_table {
    printf("iptables eval: pid=%d comm=%s\n", pid, comm);
}'
```

---

## 7. Key References

| Symbol | File | Source |
|--------|------|--------|
| `struct nf_hook_ops` | `include/linux/netfilter.h` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/netfilter.h |
| `struct nf_conn` | `include/net/netfilter/nf_conntrack.h` | https://elixir.bootlin.com/linux/v6.9/source/include/net/netfilter/nf_conntrack.h |
| `nf_conntrack_hash_check_insert` | `net/netfilter/nf_conntrack_core.c` | https://elixir.bootlin.com/linux/v6.9/source/net/netfilter/nf_conntrack_core.c |
| `ipt_do_table` | `net/ipv4/netfilter/ip_tables.c` | https://elixir.bootlin.com/linux/v6.9/source/net/ipv4/netfilter/ip_tables.c |
| `ip_vs_schedule` | `net/netfilter/ipvs/ip_vs_core.c` | https://elixir.bootlin.com/linux/v6.9/source/net/netfilter/ipvs/ip_vs_core.c |
