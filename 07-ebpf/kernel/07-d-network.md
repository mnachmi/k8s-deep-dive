# eBPF in the Network Data Path — XDP, TC, sockops

eBPF programs can be attached at multiple points in the Linux network stack, from the moment a packet arrives in a NIC driver all the way to the socket send path. This document covers the four major network hook families — XDP, TC cls_bpf, sockops, and sk_msg — explaining the context struct and return values available at each hook, how they compare in terms of position and overhead, and the kernel symbols that implement them.

## 1. Source Locations

| File | Key Symbols | URL |
|------|-------------|-----|
| `net/core/filter.c` | `sk_filter_trim_cap`, `bpf_prog_run_xdp`, `xdp_do_generic_redirect` | https://elixir.bootlin.com/linux/v6.9/source/net/core/filter.c |
| `net/core/dev.c` | `netif_receive_skb`, `do_xdp_generic` | https://elixir.bootlin.com/linux/v6.9/source/net/core/dev.c |
| `include/linux/bpf.h` | `BPF_PROG_TYPE_XDP`, `BPF_PROG_TYPE_SCHED_CLS`, `BPF_PROG_TYPE_SOCK_OPS` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/bpf.h |
| `net/sched/cls_bpf.c` | `cls_bpf_classify`, `cls_bpf_prog_run` | https://elixir.bootlin.com/linux/v6.9/source/net/sched/cls_bpf.c |
| `include/uapi/linux/if_xdp.h` | `xdp_desc`, `xdp_mmap_offsets`, AF_XDP socket layout | https://elixir.bootlin.com/linux/v6.9/source/include/uapi/linux/if_xdp.h |

## 2. XDP (eXpress Data Path)

XDP attaches a BPF program at the earliest possible point in the RX path — in the NIC driver itself, before `sk_buff` allocation. Because the kernel has not yet built an `sk_buff` descriptor, the BPF program operates directly on the raw DMA buffer, making this the lowest-latency intervention point available to a network program.

```
NIC DMA → driver interrupt → xdp_frame (raw packet buffer)
                               └─ BPF program runs here (XDP hook)
                                    → XDP_DROP      (free the frame, zero copies; intentional drop)
                                    → XDP_PASS      (continue to netif_receive_skb, allocate sk_buff)
                                    → XDP_TX        (transmit back out the same NIC)
                                    → XDP_REDIRECT  (redirect to another NIC, CPU, or AF_XDP socket)
                                    → XDP_ABORTED   (bug/error; drops and increments XDP_STATS_ABORTED error counter)
```

### Verdict Semantics

- **XDP_DROP** — intentionally discard the frame. The DMA buffer is freed immediately with no further copies. Used for DDoS mitigation and packet filtering.
- **XDP_PASS** — hand the packet to the normal network stack. The kernel allocates an `sk_buff` and calls `netif_receive_skb()`.
- **XDP_TX** — bounce the packet back out the same NIC. The driver re-queues the frame on the TX ring without copying.
- **XDP_REDIRECT** — forward the frame to another NIC, another CPU queue, or an AF_XDP socket via `bpf_redirect()` or `bpf_redirect_map()`.
- **XDP_ABORTED** — signals a BPF program bug (unhandled error path). The frame is dropped AND the driver increments the `XDP_STATS_ABORTED` counter, distinguishing it from intentional `XDP_DROP` drops. Observing this counter in production indicates a program logic error.

### `struct xdp_md` — BPF Context for XDP Programs

```c
struct xdp_md {
    __u32 data;             // offset of packet start (cast to void * in BPF)
    __u32 data_end;         // offset of packet end
    __u32 data_meta;        // metadata area before data (for inter-program communication)
    __u32 ingress_ifindex;  // interface index the packet arrived on
    __u32 rx_queue_index;   // RX queue number
    __u32 egress_ifindex;   // interface index for XDP_REDIRECT (read-only from program)
};
```

The fields `data` and `data_end` are not raw pointers — they are 32-bit offsets that the BPF program casts to `void *`. The verifier tracks these as pointers into the packet buffer and requires a bounds check before any dereference.

### Verifier Bounds-Check Pattern

The BPF verifier requires that every pointer derived from `ctx->data` is validated against `ctx->data_end` before it is dereferenced. The idiomatic pattern is:

```c
SEC("xdp")
int xdp_prog(struct xdp_md *ctx)
{
    void *data     = (void *)(long)ctx->data;
    void *data_end = (void *)(long)ctx->data_end;
    struct ethhdr *eth = data;

    if ((void *)(eth + 1) > data_end)   // bounds check — required by verifier
        return XDP_DROP;

    // eth->h_proto is now safe to read
    return XDP_PASS;
}
```

If the bounds check is absent, the verifier rejects the program at load time with an "invalid access to packet" error.

### Operating Modes

| Mode | Flag | Kernel path | Notes |
|------|------|-------------|-------|
| Native | `XDP_FLAGS_DRV_MODE` | Runs inside the NIC driver, before `sk_buff` allocation | Requires driver support (mlx5, i40e, ixgbe, virtio_net, veth, …) |
| Offloaded | `XDP_FLAGS_HW_MODE` | Runs on the NIC SmartNIC ASIC itself | Requires firmware support (Netronome); lowest latency, highest complexity |
| Generic | `XDP_FLAGS_SKB_MODE` | Runs after `sk_buff` allocation inside `do_xdp_generic()` in `net/core/dev.c` | Works on any NIC; higher overhead; used for testing and driver-less environments |

## 3. TC (Traffic Control) eBPF

TC BPF programs attach to the Traffic Control subsystem as `cls_bpf` classifiers. Unlike XDP, TC programs run after the kernel has allocated an `sk_buff`, giving them access to a richer packet view including L3/L4 metadata already parsed into the socket buffer. TC hooks exist on both ingress and egress, making them the natural location for NAT, packet marking, and per-socket filtering.

```
ingress:  NIC → netif_receive_skb → sch_ingress → cls_bpf (BPF program) → socket
egress:   socket → __dev_queue_xmit → sch_direct_xmit → cls_bpf (BPF program) → NIC
```

### Attaching TC BPF Programs

```bash
# Create the clsact qdisc (provides both ingress and egress hooks):
tc qdisc add dev eth0 clsact

# Attach a BPF program to ingress:
tc filter add dev eth0 ingress bpf obj filter.bpf.o sec tc direct-action

# Attach a BPF program to egress:
tc filter add dev eth0 egress bpf obj filter.bpf.o sec tc direct-action

# Show attached filters:
tc filter show dev eth0 ingress
```

The `direct-action` flag tells TC to use the BPF return value directly as a TC action code, bypassing the separate action layer. This is the standard approach for TC BPF programs.

### `struct __sk_buff` — BPF View of `sk_buff`

`struct __sk_buff` is the BPF-visible alias for `sk_buff`. It is not the same struct — it is a stable ABI exposed to BPF programs, with the kernel translating field accesses to the real `sk_buff` layout at load time. Selected key fields:

```c
struct __sk_buff {
    __u32 len;              // total packet length
    __u32 pkt_type;         // PACKET_HOST, PACKET_BROADCAST, PACKET_MULTICAST, ...
    __u32 mark;             // skb->mark; used for policy routing and fwmark
    __u32 queue_mapping;
    __u32 protocol;         // L3 protocol (htons(ETH_P_IP), htons(ETH_P_IPV6), ...)
    __u32 vlan_present;
    __u32 vlan_tci;
    __u32 vlan_proto;
    __u32 priority;
    __u32 ingress_ifindex;  // interface index the packet arrived on
    __u32 ifindex;          // current interface index
    __u32 tc_index;
    __u32 cb[5];            // 20 bytes of scratch space for BPF program communication
    __u32 hash;
    __u32 tc_classid;
    __u32 data;             // pointer to packet data
    __u32 data_end;
    __u32 napi_id;
    // ... additional fields
};
```

`cb[5]` (control buffer) provides 20 bytes of per-skb scratch space that BPF programs can use to pass state between TC programs chained on the same packet path.

### TC Return Values

| Return value | Meaning |
|-------------|---------|
| `TC_ACT_OK` | Continue processing through the TC pipeline |
| `TC_ACT_SHOT` | Drop the packet immediately |
| `TC_ACT_REDIRECT` | Redirect the packet (paired with `bpf_redirect()`) |
| `TC_ACT_PIPE` | Pass to the next action in the action chain |
| `TC_ACT_UNSPEC` | Use the default action (typically determined by the qdisc) |

## 4. sockops and sk_msg

Where XDP and TC operate on the packet data path, sockops and sk_msg operate on the socket layer — after the TCP stack has reassembled data. This makes them the right tool for per-connection policy and socket-to-socket redirection.

### `BPF_PROG_TYPE_SOCK_OPS`

sockops programs are invoked on socket lifecycle events. The `op` field of `struct bpf_sock_ops` identifies which event triggered the call:

- `BPF_SOCK_OPS_TCP_CONNECT_CB` — active connect (client side)
- `BPF_SOCK_OPS_ACTIVE_ESTABLISHED_CB` — connection established on client
- `BPF_SOCK_OPS_PASSIVE_ESTABLISHED_CB` — new connection accepted on server
- `BPF_SOCK_OPS_TCP_LISTEN_CB` — socket enters LISTEN state
- `BPF_SOCK_OPS_RTO_CB`, `BPF_SOCK_OPS_RETRANS_CB` — TCP retransmit events

sockops programs can call `bpf_sock_ops_cb_flags_set()` to subscribe to additional events and can set TCP socket options (e.g., `bpf_setsockopt()`) to influence connection behavior — for example, forcing ECN or adjusting initial congestion window based on destination IP.

### `BPF_PROG_TYPE_SK_MSG`

sk_msg programs run on the `sendmsg(2)` path for sockets that have been added to a `BPF_MAP_TYPE_SOCKMAP`. They receive `struct sk_msg_md` as context and can inspect or modify the message before it is sent. Return values `SK_PASS` and `SK_DROP` control whether the message continues.

### `BPF_MAP_TYPE_SOCKMAP` — Socket-to-Socket Redirect

A sockmap is a BPF array or hash map whose values are references to open sockets. An sk_msg program can call `bpf_msg_redirect_map()` to forward a message directly from one socket to another, bypassing the full TCP send/receive cycle:

```
sender socket → sendmsg → sk_msg BPF prog → bpf_msg_redirect_map() → receiver socket
                                                                       (TCP stack bypassed)
```

This lets two processes exchange data through a sockmap without the data ever leaving the kernel or going through the loopback NIC, eliminating copying and protocol processing overhead for local communication.

### Cilium: sockops for Local Pod-to-Pod Traffic

Cilium uses `BPF_PROG_TYPE_SOCK_OPS` together with `BPF_MAP_TYPE_SOCKMAP` to accelerate local pod-to-pod traffic on the same node. When a new TCP connection is established between two pods on the same host, the sockops program identifies both endpoints, inserts their sockets into a sockmap, and sk_msg redirect then short-circuits all subsequent `send()` calls directly between the two socket receive queues. This path bypasses Netfilter, the veth pairs, and the virtual switch entirely, reducing per-message latency significantly for node-local service communication.

## 5. Hook Point Comparison

| Hook | Prog Type | Context struct | Path position | Overhead | Use case |
|------|-----------|----------------|---------------|----------|----------|
| XDP (native) | `BPF_PROG_TYPE_XDP` | `struct xdp_md` | Before `sk_buff` alloc, inside NIC driver | ~50 ns | DDoS drop, LB, AF_XDP |
| XDP (generic) | `BPF_PROG_TYPE_XDP` | `struct xdp_md` | After `sk_buff` alloc, in `do_xdp_generic()` | ~150 ns | Testing, any NIC, no driver support needed |
| TC ingress/egress | `BPF_PROG_TYPE_SCHED_CLS` | `struct __sk_buff` | After `sk_buff` alloc, in TC classifier | ~100–200 ns | Filtering, NAT, packet marking, redirect |
| sockops | `BPF_PROG_TYPE_SOCK_OPS` | `struct bpf_sock_ops` | Socket lifecycle event (connect/accept/established) | Per event | TCP option injection, socket redirect setup |
| sk_msg | `BPF_PROG_TYPE_SK_MSG` | `struct sk_msg_md` | `sendmsg(2)` path, after TCP layer | Per syscall | Socket-level redirect, message filtering |

## 6. Live Observation

```bash
# Show XDP program attached to an interface:
ip link show dev eth0   # look for "xdp" annotation in output

# List all XDP, TC, and flow-dissector programs attached to interfaces:
bpftool net list

# Show TC BPF filters on ingress:
tc filter show dev eth0 ingress

# Trace XDP exceptions (XDP_ABORTED and driver-level errors):
bpftrace -e 'tracepoint:xdp:xdp_exception { printf("ifindex=%d action=%d prog_id=%d\n", args->ifindex, args->act, args->prog_id); }'

# Count TX timeouts per interface (proxy for egress TC drops under load):
bpftrace -e 'tracepoint:net:net_dev_xmit_timeout { @[str(args->name)] = count(); }'

# List all loaded BPF programs related to socket operations:
bpftool prog list | grep sock
```

The `tracepoint:xdp:xdp_exception` probe fires whenever a program returns `XDP_ABORTED` or the driver encounters a verdict it cannot handle. Monitoring `args->act` lets you distinguish `XDP_ABORTED` (value 5) from other error conditions and correlate them with the program ID (`args->prog_id`) visible in `bpftool prog list`.

## 7. Key Kernel References

| Symbol | File | URL |
|--------|------|-----|
| `struct xdp_md` | `include/uapi/linux/bpf.h` | https://elixir.bootlin.com/linux/v6.9/source/include/uapi/linux/bpf.h |
| `bpf_prog_run_xdp` | `net/core/filter.c` | https://elixir.bootlin.com/linux/v6.9/source/net/core/filter.c |
| `do_xdp_generic` | `net/core/dev.c` | https://elixir.bootlin.com/linux/v6.9/source/net/core/dev.c |
| `cls_bpf_classify` | `net/sched/cls_bpf.c` | https://elixir.bootlin.com/linux/v6.9/source/net/sched/cls_bpf.c |
| `struct __sk_buff` | `include/uapi/linux/bpf.h` | https://elixir.bootlin.com/linux/v6.9/source/include/uapi/linux/bpf.h |
| `xdp_do_generic_redirect` | `net/core/filter.c` | https://elixir.bootlin.com/linux/v6.9/source/net/core/filter.c |
| `sk_filter_trim_cap` | `net/core/filter.c` | https://elixir.bootlin.com/linux/v6.9/source/net/core/filter.c |
