# 06-a — struct sk_buff and struct net_device

`struct sk_buff` is the fundamental unit of network data in the Linux kernel. Every packet — incoming or outgoing — is represented as an `sk_buff` from the moment it is received off the wire (or allocated for transmission) until it is consumed by a socket or sent out a device. `struct net_device` is the kernel's abstraction for a network interface, whether physical (e0, ens3), virtual (veth, lo, tun), or bridged.

---

## 1. Source Locations

| File | Contents | Source |
|------|----------|--------|
| `include/linux/skbuff.h` | `struct sk_buff` definition, all inline helpers (`skb_push`, `skb_pull`, `skb_put`, `skb_clone`) | https://elixir.bootlin.com/linux/v6.9/source/include/linux/skbuff.h |
| `include/linux/netdevice.h` | `struct net_device`, `struct net_device_ops`, `struct napi_struct` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/netdevice.h |
| `net/core/skbuff.c` | `__alloc_skb()`, `kfree_skb()`, `skb_clone()`, `pskb_expand_head()` | https://elixir.bootlin.com/linux/v6.9/source/net/core/skbuff.c |
| `net/core/dev.c` | `netif_receive_skb()`, `dev_queue_xmit()`, `napi_poll()`, `net_rx_action()` | https://elixir.bootlin.com/linux/v6.9/source/net/core/dev.c |

---

## 2. struct sk_buff

### Full Struct (key fields)

```c
// include/linux/skbuff.h (key fields)
struct sk_buff {
    /* Data references */
    struct sk_buff      *next;          // linked list (tx queue, socket receive queue)
    struct sk_buff      *prev;
    struct sock         *sk;            // owning socket (NULL for forwarded packets)
    struct net_device   *dev;           // device this skb arrived on / will leave via
    unsigned int         len;           // total packet length (data + fragments)
    unsigned int         data_len;      // length of paged (fragment) data
    __u16                mac_len;       // length of MAC (Ethernet) header
    __u16                hdr_len;       // writable header length (for clones)
    unsigned int         truesize;      // skb + data memory charged to socket sndbuf/rcvbuf

    /* Packet pointers (all within the skb's linear buffer) */
    sk_buff_data_t       tail;          // end of actual data
    sk_buff_data_t       end;           // end of allocated buffer
    unsigned char       *head;          // start of allocated buffer
    unsigned char       *data;          // start of actual data (moves as headers are added/removed)

    /* Checksums and offload */
    __wsum               csum;          // checksum (partial or full, depending on ip_summed)
    __u8                 ip_summed;     // CHECKSUM_NONE / CHECKSUM_UNNECESSARY / CHECKSUM_PARTIAL / CHECKSUM_COMPLETE
    __u8                 pkt_type;      // PACKET_HOST, PACKET_BROADCAST, PACKET_MULTICAST, PACKET_OTHERHOST
    __be16               protocol;      // ETH_P_IP, ETH_P_IPV6, ETH_P_ARP...

    /* Routing and NAT */
    struct dst_entry    *_skb_refdst;   // routing destination (dst cache entry)
    struct nf_conntrack *nfct;          // netfilter conntrack entry (NULL if not tracked)
    __u32                mark;          // packet mark (used by iptables -m mark, routing policy)

    /* Fragment list for non-linear packets */
    skb_frag_t           frags[MAX_SKB_FRAGS]; // page fragments for large I/O (scatter-gather)
    struct sk_buff      *frag_list;     // list of sk_buffs for oversized packets

    /* Timestamps */
    ktime_t              tstamp;        // receive timestamp (set by net_timestamp_check)

    /* Transport/network header offsets (from skb->head) */
    sk_buff_data_t       transport_header; // points to TCP/UDP header
    sk_buff_data_t       network_header;   // points to IP header
    sk_buff_data_t       mac_header;       // points to Ethernet header
};
```

Source: https://elixir.bootlin.com/linux/v6.9/source/include/linux/skbuff.h

### Field-by-Field Explanation

**`next` / `prev`**
`sk_buff` structs are embedded in doubly-linked lists throughout the network stack — the socket's receive queue (`sk->sk_receive_queue`), the transmit queue inside a qdisc, and the NAPI backlog queue. These two pointers are the list links. The list head is always a `struct sk_buff_head` which carries a spinlock and a queue length counter.

**`sk`**
The owning socket. Set when a socket allocates an skb for transmission (via `sock_wmalloc()`) or when the TCP receive path associates an incoming segment with a socket. For packets being forwarded (routing, bridging) this is NULL — the packet does not belong to any local socket.

**`dev`**
The `net_device` on which this skb arrived (on the receive path) or will be sent out (on the transmit path). Updated as the packet traverses the stack — for example, after a DNAT rewrite, the routing code may change `skb->dev` to point to the correct output device.

**`len` and `data_len`**
`len` is the total logical packet length: linear data plus all paged fragment data. `data_len` is the length that lives in page fragments (non-linear). The linear portion length is therefore `len - data_len`. When `data_len == 0` the skb is fully linear and `skb->data` through `skb->tail` contains the entire packet.

**`mac_len`**
The length of the MAC (Ethernet) header. Used by the network layer to locate the start of the IP header: `skb_network_header(skb) == skb->data + skb->mac_len` before the Ethernet header is pulled.

**`hdr_len`**
The length of the cloned header area that is writable by the clone's owner. When `skb_clone()` is called, both the original and the clone share the same data pages. `hdr_len` tracks how much of the beginning of the data area each sk_buff considers "its own" for the purposes of copy-on-write via `pskb_expand_head()`.

**`truesize`**
The total memory (in bytes) that this skb charges to the socket's send or receive buffer. It includes `sizeof(struct sk_buff)` plus the size of the linear data buffer (`end - head`). When an skb is added to a socket receive queue via `skb_set_owner_r()`, `truesize` is added to `sk->sk_rmem_alloc`. The kernel uses this to enforce `sk_rcvbuf` limits — if the socket's receive buffer is already full, the incoming skb is dropped. For transmit, `truesize` is charged to `sk_wmem_alloc` and released when the NIC confirms the packet was sent.

### Linear Buffer Layout

The linear buffer is a contiguous allocation. The four pointer/offset fields carve it into regions:

```
    head                data            tail          end
     │                   │               │             │
     ▼                   ▼               ▼             ▼
     ┌───────────────────┬───────────────┬─────────────┐
     │    headroom       │   payload     │  tailroom   │
     └───────────────────┴───────────────┴─────────────┘
```

- **headroom** (`data - head`): free space before the payload. Used to prepend protocol headers without copying. As a packet moves down the stack (TCP → IP → Ethernet), each layer calls `skb_push()` to claim headroom and write its header.
- **payload** (`tail - data`): the live packet bytes.
- **tailroom** (`end - tail`): free space after the payload. Used by `skb_put()` to append data (e.g., when building a packet from scratch in the transmit path).

**`skb_push(skb, len)`** — moves `skb->data` backward by `len` bytes (into headroom) and increases `skb->len`. Used to prepend a header. Panics if there is insufficient headroom.
https://elixir.bootlin.com/linux/v6.9/source/include/linux/skbuff.h

**`skb_pull(skb, len)`** — moves `skb->data` forward by `len` bytes (past a consumed header) and decreases `skb->len`. Used to strip a header as the packet moves up the stack (Ethernet header stripped before IP processing).
https://elixir.bootlin.com/linux/v6.9/source/include/linux/skbuff.h

**`skb_put(skb, len)`** — extends `skb->tail` by `len` bytes and increases `skb->len`. Used to append data into the tailroom. Panics if there is insufficient tailroom.
https://elixir.bootlin.com/linux/v6.9/source/include/linux/skbuff.h

### Header Offset Fields

`mac_header`, `network_header`, and `transport_header` store offsets (from `skb->head`) to the start of each protocol layer's header. The inline accessor functions `skb_mac_header()`, `skb_network_header()`, and `skb_transport_header()` compute the actual pointer as `skb->head + skb->xxx_header`. These offsets survive across `skb_pull()` operations because they are absolute from `head` rather than relative to `data`.

**`protocol`**
The EtherType of the packet as seen at layer 2: `ETH_P_IP` (0x0800), `ETH_P_IPV6` (0x86DD), `ETH_P_ARP` (0x0806), etc. Set by the Ethernet driver when the frame is received. The `ptype_base` hash table in `net/core/dev.c` uses this value to dispatch to the correct layer-3 handler (`ip_rcv`, `ipv6_rcv`, `arp_rcv`).

**`pkt_type`**
Describes the delivery type: `PACKET_HOST` (addressed to this host), `PACKET_BROADCAST`, `PACKET_MULTICAST`, or `PACKET_OTHERHOST` (seen in promiscuous mode but not addressed to us). Set by the Ethernet driver based on the destination MAC address. Packets with `PACKET_OTHERHOST` are dropped by `ip_rcv()` unless the interface is in promiscuous mode and something (e.g., a packet socket) has registered interest.

**`ip_summed`**
Indicates the checksum state. `CHECKSUM_NONE` means the kernel must verify the checksum in software. `CHECKSUM_UNNECESSARY` means hardware already verified it (NIC checksum offload). `CHECKSUM_PARTIAL` means the kernel has computed a partial checksum and the NIC should complete it (transmit offload). `CHECKSUM_COMPLETE` means hardware provided a full checksum value in `skb->csum`.

**`_skb_refdst`**
The routing destination cache entry (`struct dst_entry`). Set after a route lookup (`ip_route_input()` on receive, `ip_route_output()` on transmit). The dst entry carries the next-hop gateway, the output device, and function pointers for the output path (`dst->output`). Must be released with `dst_release()` when the skb is freed; the `skb_dst_drop()` helper handles this.

### Cloning via `skb_clone()`

`skb_clone()` (https://elixir.bootlin.com/linux/v6.9/source/net/core/skbuff.c) creates a second `sk_buff` that shares the same underlying data pages as the original. Only the `sk_buff` struct itself is copied; the data buffer is reference-counted. This is used when the same packet must be delivered to multiple consumers — for example, a packet socket (`AF_PACKET`) registered for `tcpdump` and the normal IP stack both need to process the same frame without an expensive data copy.

Because data pages are shared, a clone must not modify the data that falls within another sk_buff's claimed range. If a clone needs to rewrite headers (e.g., for NAT), it must first call `pskb_expand_head()` (https://elixir.bootlin.com/linux/v6.9/source/net/core/skbuff.c) to allocate a new private header area — a copy-on-write operation. Only the header region is copied; page fragments remain shared.

The reference count on the data buffer is stored in `skb_shinfo(skb)->dataref`. When the last sk_buff referencing a data buffer is freed, the buffer itself is released.

### `nfct` — Conntrack Pointer

`nfct` (https://elixir.bootlin.com/linux/v6.9/source/include/linux/skbuff.h) points to the `nf_conntrack` entry for this packet. The conntrack module (`net/netfilter/nf_conntrack_core.c`) sets this pointer in the `NF_INET_PRE_ROUTING` hook when it creates or looks up a connection tracking entry. The entry records the original and reply tuples (src/dst IP, src/dst port, protocol) so that NAT translations applied to the forward direction can be reversed automatically on reply packets.

kube-proxy relies entirely on conntrack for its DNAT implementation. When a new connection arrives for a ClusterIP (e.g., `10.96.0.1:443`), kube-proxy's iptables rules DNAT the first packet to a backend pod IP. Conntrack records this translation. All subsequent packets in the connection match the existing conntrack entry and are translated automatically — no iptables rule traversal is needed for established connections.

### Object Graph

```
struct sk_buff
  ├─ sk     → struct sock (tcp_sock) → sk_receive_queue (sk_buff list)
  ├─ dev    → struct net_device      → nd_net → struct net (netns)
  ├─ _skb_refdst → struct dst_entry  → dev (output device)
  ├─ nfct   → struct nf_conntrack    → original tuple, reply tuple, NAT info
  └─ skb_shinfo(skb)
       ├─ dataref   (shared data refcount for clones)
       ├─ frags[]   (page fragments for non-linear data)
       └─ frag_list (chain of sk_buffs for very large packets)
```

### Lifecycle

1. **Allocation**: `alloc_skb(size, GFP_ATOMIC)` or `dev_alloc_skb()` (adds NET_SKB_PAD headroom for DMA alignment). The skb starts with `data == tail == head + NET_SKB_PAD` and all header offsets unset.
2. **Receive**: DMA fills the linear buffer. The driver calls `skb_put(skb, len)` to mark the received bytes, sets `skb->protocol`, and calls `netif_receive_skb()`.
3. **Stack traversal (up)**: Each layer calls `skb_pull()` to advance past its header and sets its header offset (`skb_reset_network_header()`, etc.).
4. **Delivery**: TCP copies data from skb page fragments into the user buffer via `skb_copy_datagram_iter()` and then calls `kfree_skb()`.
5. **Transmit (down)**: Each layer calls `skb_push()` to prepend its header. The qdisc holds the skb until the NIC is ready.
6. **Free**: `kfree_skb()` decrements the data refcount. If zero, frees the data buffer. Releases `nfct` via `nf_conntrack_put()`, `_skb_refdst` via `dst_release()`, and the sk_buff struct itself back to the skb cache.

---

## 3. struct net_device

### Key Fields

```c
// include/linux/netdevice.h (key fields)
struct net_device {
    char            name[IFNAMSIZ];      // interface name: "eth0", "veth0", "lo"
    unsigned long   state;              // IFF_UP, IFF_RUNNING, IFF_PROMISC...
    struct net_device_stats stats;      // rx/tx packets, bytes, errors (legacy)
    unsigned int    flags;              // IFF_BROADCAST, IFF_MULTICAST, IFF_LOOPBACK...
    unsigned int    priv_flags;         // IFF_802_1Q_VLAN, IFF_EBRIDGE, IFF_BONDING...
    int             ifindex;            // interface index (shown in `ip link`)
    unsigned int    mtu;                // maximum transmission unit
    unsigned char   dev_addr[MAX_ADDR_LEN]; // MAC address
    const struct net_device_ops *netdev_ops; // ndo_open, ndo_stop, ndo_start_xmit...
    struct net      *nd_net;            // network namespace this device belongs to
    struct Qdisc    *qdisc;             // traffic control qdisc (pfifo_fast, fq_codel...)
    struct netdev_rx_queue *_rx;        // per-CPU receive queues (for multiqueue NICs)
    struct netdev_queue    *_tx;        // per-CPU transmit queues
    unsigned long   tx_queue_len;       // maximum frames in tx queue before drop
    struct list_head dev_list;          // global net_device list
};
```

Source: https://elixir.bootlin.com/linux/v6.9/source/include/linux/netdevice.h

### Field Explanations

**`name`**
The human-readable interface name. In Kubernetes, the veth end inside a pod is typically `eth0`; the host end is named by the CNI plugin (e.g., `veth3f2a1b` for Flannel/Calico). The name is the key used by `ip link`, `ifconfig`, and `/proc/net/dev`.

**`ifindex`**
The numeric interface index, unique within a network namespace. Used by routing tables, socket options (`SO_BINDTODEVICE`), and Netlink messages. When tracing veth peers across namespaces, the peer's ifindex is readable via `ethtool -S` or the `IFLA_LINK` Netlink attribute.

**`nd_net`**
A pointer to the `struct net` (network namespace) that owns this device. This is the binding that isolates pod network stacks. A veth pair consists of two `net_device` structs — one in the pod's network namespace with `nd_net` pointing to the pod's `struct net`, and one in the host namespace with `nd_net` pointing to the host's `struct net`. Routing, ARP, and socket operations all operate within a single `struct net` — packets cannot cross namespace boundaries except through veth (or similar virtual devices) explicitly wired to do so.

**`netdev_ops` and `ndo_start_xmit`**
The `net_device_ops` vtable (https://elixir.bootlin.com/linux/v6.9/source/include/linux/netdevice.h) contains function pointers for all driver operations. The most critical for the transmit path is `ndo_start_xmit`, called by `dev_hard_start_xmit()` in `net/core/dev.c` once the qdisc has dequeued an skb. For a physical NIC, this programs the DMA descriptor ring. For veth, `veth_xmit()` (https://elixir.bootlin.com/linux/v6.9/source/drivers/net/veth.c) calls `netif_receive_skb()` on the peer device — the transmit on one end becomes a receive on the other, with no actual I/O. This is how pod-to-host packet transfer works at zero copy.

**`qdisc`**
The root traffic-control queueing discipline attached to this device. Default is `pfifo_fast` (a three-band priority FIFO). `dev_queue_xmit()` enqueues the skb into the qdisc; `sch_direct_xmit()` dequeues and calls `ndo_start_xmit`. More advanced qdiscs (`fq_codel`, `tbf`, `htb`) enable rate limiting and fair queuing — these are what `tc` commands manipulate and what the CNI bandwidth plugin uses for per-pod rate limiting.

**`_rx` and `_tx`**
Arrays of per-CPU/per-queue structures for multiqueue NICs. Each `netdev_rx_queue` and `netdev_queue` has its own lock and stats, enabling parallel processing across CPU cores. The number of queues is set by the driver at probe time and visible via `ethtool -l`.

---

## 4. Receive Path: NIC → Socket

```
NIC DMA interrupt
  └─ NAPI poll: napi_schedule() → net_rx_action()       # net/core/dev.c
       └─ netif_receive_skb()                           # net/core/dev.c
            └─ __netif_receive_skb_core()
                 ├─ packet_type handlers (ETH_P_IP → ip_rcv)
                 └─ ip_rcv()                            # net/ipv4/ip_input.c
                      └─ NF_HOOK(NFPROTO_IPV4, NF_INET_PRE_ROUTING)   # PREROUTING
                           └─ ip_rcv_finish()
                                └─ ip_local_deliver()   # for packets to this host
                                     └─ NF_HOOK(NF_INET_LOCAL_IN)     # INPUT
                                          └─ tcp_v4_rcv()             # net/ipv4/tcp_ipv4.c
                                               └─ sk->sk_data_ready()  # wake up reader
```

Key points:

- `netif_receive_skb()` (https://elixir.bootlin.com/linux/v6.9/source/net/core/dev.c) is the universal entry point into the protocol stack for received packets. Drivers call this directly (non-NAPI) or indirectly via NAPI poll.
- `__netif_receive_skb_core()` iterates over all registered packet type handlers for `skb->protocol`. `ETH_P_IP` maps to `ip_rcv`. Packet sockets (`AF_PACKET`) registered with `ETH_P_ALL` also receive a clone here — this is how `tcpdump` sees traffic.
- The `NF_HOOK(NFPROTO_IPV4, NF_INET_PRE_ROUTING)` call invokes all Netfilter hooks registered at the PREROUTING point. The conntrack module registers here to create/update connection tracking entries. kube-proxy's DNAT rules (set up via iptables) also execute here for Service traffic.
- `tcp_v4_rcv()` (https://elixir.bootlin.com/linux/v6.9/source/net/ipv4/tcp_ipv4.c) looks up the socket by the 4-tuple (`ip_hdr->saddr`, `th->source`, `ip_hdr->daddr`, `th->dest`), validates the TCP header and sequence numbers, and queues the skb onto `sk->sk_receive_queue`. It then calls `sk->sk_data_ready()` (which is `tcp_data_ready()` for TCP sockets), which wakes any process blocked in `read()` or `recv()`.

---

## 5. Transmit Path: Socket → NIC

```
tcp_sendmsg()                                           # net/ipv4/tcp.c
  └─ tcp_write_xmit()
       └─ tcp_transmit_skb() → ip_queue_xmit()         # net/ipv4/ip_output.c
            └─ NF_HOOK(NF_INET_LOCAL_OUT)              # OUTPUT
                 └─ ip_output() → ip_finish_output()
                      └─ NF_HOOK(NF_INET_POST_ROUTING) # POSTROUTING
                           └─ dev_queue_xmit()          # net/core/dev.c
                                └─ sch_direct_xmit()
                                     └─ dev->netdev_ops->ndo_start_xmit()
```

Key points:

- `tcp_sendmsg()` (https://elixir.bootlin.com/linux/v6.9/source/net/ipv4/tcp.c) copies user data into skb page fragments (zero-copy where possible via `get_user_pages()`), respects the congestion window and send buffer limits, and hands skbs to `tcp_write_xmit()`.
- `ip_queue_xmit()` (https://elixir.bootlin.com/linux/v6.9/source/net/ipv4/ip_output.c) performs the route lookup (or uses the cached route in `sk->sk_dst_cache`), sets `skb->dev`, and calls `skb_push()` to prepend the IP header.
- The `NF_HOOK(NF_INET_LOCAL_OUT)` call triggers the OUTPUT chain. kube-proxy installs DNAT rules here for outbound Service traffic (connecting from a pod to a ClusterIP traverses OUTPUT).
- `NF_HOOK(NF_INET_POST_ROUTING)` triggers the POSTROUTING chain. This is where MASQUERADE rules run for pod-to-external-internet traffic on nodes that do not have a dedicated external IP per pod.
- `dev_queue_xmit()` (https://elixir.bootlin.com/linux/v6.9/source/net/core/dev.c) hands the skb to the qdisc. If the qdisc is empty and the device is not busy, `sch_direct_xmit()` bypasses the queue and calls `ndo_start_xmit()` directly for minimum latency.

---

## 6. NAPI and Multi-Queue

NAPI (New API) is the Linux mechanism for high-performance interrupt mitigation. Without NAPI, each arriving packet would fire a hardware interrupt — at 10 Gbps with 1500-byte frames that is over 800,000 interrupts per second per queue, enough to saturate a CPU core. With NAPI:

1. The NIC fires **one interrupt** when the first packet arrives on an empty ring buffer.
2. The interrupt handler calls `napi_schedule()`, which queues the `napi_struct` onto the per-CPU `softnet_data.poll_list` and schedules `NET_RX_SOFTIRQ`.
3. The softirq handler `net_rx_action()` calls `napi->poll()` (the driver's poll function) with a `budget` parameter (default 64 packets). The driver drains its ring buffer, calling `netif_receive_skb()` for each packet, until the budget is exhausted or the ring is empty.
4. If the ring empties before the budget is used, **interrupts are re-enabled** (via `napi_complete()`). If the budget is exhausted with packets still pending, the NAPI instance is rescheduled without re-enabling interrupts — the interrupt stays masked until the ring drains.

This design amortizes interrupt overhead across batches of packets. At high rates the NIC never re-enables interrupts; the kernel polls continuously at softirq priority. At low rates the system returns to interrupt-driven mode and the CPU can sleep.

**Multi-queue NICs** extend this by having one independent ring buffer (RX queue) per CPU core, each with its own `napi_struct`. Receive-Side Scaling (RSS) distributes incoming flows across queues by hashing the 4-tuple, so connections are sticky to a queue and a CPU. This eliminates the spinlock contention that would arise from multiple CPUs racing to drain a single ring buffer. The kernel creates one NAPI instance per RX queue; each can be pinned to a specific CPU via `/proc/irq/<n>/smp_affinity`.

In Kubernetes, veth pairs do not use NAPI (they call `netif_receive_skb()` directly in the transmit context of the sender). High-throughput pod-to-pod traffic therefore runs entirely in process context on the sending CPU — a consideration when diagnosing CPU hotspots with `perf top`.

---

## 7. Live Observation

```bash
# Interface stats — packets, bytes, errors, drops per interface:
ip -s link show eth0
cat /proc/net/dev

# Per-container interface stats (in container's netns):
ip netns exec <ns> ip -s link
cat /proc/$CPID/net/dev

# bpftrace: trace every packet received by the kernel, print device name and length
bpftrace -e 'kprobe:netif_receive_skb {
    $skb = (struct sk_buff *)arg0;
    printf("rx: dev=%s len=%u\n",
           str($skb->dev->name), $skb->len);
}'

# bpftrace: trace TCP connections being established (outbound connect syscall path)
bpftrace -e 'kprobe:tcp_v4_connect {
    printf("connect: pid=%d comm=%s\n", pid, comm);
}'

# bpftrace: count ip_rcv calls (incoming IP packets) by process name
bpftrace -e 'kprobe:ip_rcv { @[comm] = count(); }'
```

The `netif_receive_skb` probe fires in softirq context — `comm` will often be `swapper/N` (idle) or the process that happened to be running when the softirq fired, not the process that sent the packet. Use `tcp_v4_rcv` or socket-level probes to attribute traffic to specific processes.

For production tracing with lower overhead, prefer `tracepoint:net:netif_receive_skb` (a static tracepoint that avoids the kprobe symbol stability caveats):

```bash
bpftrace -e 'tracepoint:net:netif_receive_skb {
    printf("dev=%s len=%u\n", args->name, args->len);
}'
```

---

## 8. Key References

| Symbol | File | Source |
|--------|------|--------|
| `struct sk_buff` | `include/linux/skbuff.h` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/skbuff.h |
| `struct net_device` | `include/linux/netdevice.h` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/netdevice.h |
| `netif_receive_skb()` | `net/core/dev.c` | https://elixir.bootlin.com/linux/v6.9/source/net/core/dev.c |
| `dev_queue_xmit()` | `net/core/dev.c` | https://elixir.bootlin.com/linux/v6.9/source/net/core/dev.c |
| `tcp_v4_rcv()` | `net/ipv4/tcp_ipv4.c` | https://elixir.bootlin.com/linux/v6.9/source/net/ipv4/tcp_ipv4.c |
| `ip_rcv()` | `net/ipv4/ip_input.c` | https://elixir.bootlin.com/linux/v6.9/source/net/ipv4/ip_input.c |
