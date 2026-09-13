# Chapter 06 — Networking Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Write Chapter 06 (Linux Networking) in full: kernel deep dives on sk_buff, socket/sock, network namespaces + veth, and Netfilter/conntrack; K8s connection covering pod networking, CNI, kube-proxy, Services, and NetworkPolicy; C exercise demonstrating TCP sockets; Go exercise inspecting per-pod network interface stats; kube-inspect checkpoint 06 adding network namespace inspection.

**Architecture:** Three-track structure — `kernel/` (four docs), `k8s/` (K8s connection), `exercises/` (C + Go). kube-inspect extends `internal/netns` with network namespace interface reading.

**Tech Stack:** Markdown, C (gcc -Wall -Wextra -Werror), Go 1.22+, Linux 6.9 kernel source, bpftrace, /proc/net, /proc/pid/net, ss, ip

## Global Constraints

- Every kernel struct/function reference MUST include elixir.bootlin.com URL for Linux 6.9
- bpftrace: use `kernel_clone` (not `do_fork`); `copy_process` arg3 = `struct kernel_clone_args *`
- C: `gcc -Wall -Wextra -Werror`, binaries excluded via `.gitignore`
- Go: `go vet ./...` clean, binaries excluded
- Exercise folders: `README.md`, `Makefile` (build/run/clean), `.gitignore`
- References tables: use bare URLs (raw https:// — NOT [link](url) markdown shorthand)
- Data structure deep dive mandatory: full struct, every field, lifecycle, locking, object graph, live observation
- No placeholder text anywhere
- Kernel version: Linux 6.9

---

## Task 1: Chapter 06 README + sk_buff deep dive

**Files:**
- Modify: `06-network/README.md` (replace stub)
- Create: `06-network/kernel/06-a-skbuff.md`

- [ ] **Step 1: Write `06-network/README.md`**

Include:
- Intro paragraph: Every packet entering or leaving a Kubernetes pod passes through the Linux kernel networking stack. A single HTTP request from a pod crosses multiple layers: the container's veth pair, the host bridge or routing table, iptables NAT rules set by kube-proxy, and the physical NIC. This chapter teaches the kernel networking stack from the core data structures (sk_buff, struct sock, struct net) through Netfilter to how Kubernetes assembles pod networks using CNI plugins, iptables DNAT, and conntrack. The same mechanisms power NetworkPolicy enforcement, pod-to-pod routing, and the ServiceIP abstraction.
- Learning objectives (5):
  1. Trace a packet from NIC receive through the TCP/IP stack to a user process
  2. Explain struct sk_buff: the packet buffer data structure, headroom/tailroom, cloning
  3. Explain how network namespaces isolate pod network stacks and how veth pairs connect them to the host
  4. Trace how kube-proxy programs iptables DNAT rules for a ClusterIP Service
  5. Interpret /proc/pid/net/dev and ss output to diagnose pod network issues
- Prerequisites: Chapter 02 (network namespaces, clone flags), Chapter 03 (cgroup-based traffic control)
- Reading order table: 06-a-skbuff.md, 06-b-socket.md, 06-c-netns.md, 06-d-netfilter.md, k8s/06-k8s-connection.md, exercises/tcp-echo-demo/, exercises/netns-inspector/
- ASCII diagram of the Linux network stack with container context:
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

- [ ] **Step 2: Write `06-network/kernel/06-a-skbuff.md`**

Full sk_buff and net_device deep dive. Must include ALL of these sections:

**Section 1 — Source locations**
- `include/linux/skbuff.h` — https://elixir.bootlin.com/linux/v6.9/source/include/linux/skbuff.h (struct sk_buff)
- `include/linux/netdevice.h` — https://elixir.bootlin.com/linux/v6.9/source/include/linux/netdevice.h (struct net_device)
- `net/core/skbuff.c` — https://elixir.bootlin.com/linux/v6.9/source/net/core/skbuff.c (alloc/free/clone)
- `net/core/dev.c` — https://elixir.bootlin.com/linux/v6.9/source/net/core/dev.c (receive path: netif_receive_skb)

**Section 2 — struct sk_buff**
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

Explain each field. Key concepts:
- **Linear buffer layout**: `head → [headroom] → data → [payload] → tail → [tailroom] → end`. The headroom allows prepending protocol headers (TCP → IP → Ethernet) without copying. `skb_push()` moves `data` backwards into headroom. `skb_pull()` moves `data` forward (strips headers). `skb_put()` extends `tail` (appends data).
- **Cloning**: `skb_clone()` creates a second sk_buff pointing to the same data pages — only the sk_buff struct itself is copied. Used when the same packet needs to go to multiple consumers (e.g., packet socket + normal delivery). Modifying a clone's headers requires `pskb_expand_head()` to copy-on-write the header area.
- **truesize**: The total memory charged to the owning socket's send/receive buffer quota. Critical for socket buffer accounting — if a socket receives more data than its `sk_rcvbuf` limit, the kernel drops packets.
- **nfct**: Points to the conntrack entry. Set by the conntrack module in the PREROUTING hook. kube-proxy's DNAT rules rely on conntrack to translate reply packets back transparently.

**Section 3 — struct net_device (key fields)**
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

Explain: `nd_net` binds the device to a network namespace. A veth pair has two ends in different namespaces — each end is a separate `net_device` with `nd_net` pointing to its respective `struct net`. The `netdev_ops->ndo_start_xmit` is the driver transmit function — for veth, it calls the peer's `netif_receive_skb()` directly.

**Section 4 — Receive path: NIC → socket**
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

**Section 5 — Transmit path: socket → NIC**
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

**Section 6 — NAPI and multi-queue**
NAPI (New API) is the Linux mechanism for high-performance interrupt mitigation. Instead of firing an interrupt per packet (which would overwhelm the CPU at 10Gbps+), the NIC fires one interrupt to say "I have packets". The kernel then disables the interrupt and polls the NIC's ring buffer in a loop (via `napi_poll()`) until either the budget (typically 64 packets) is exhausted or the ring is empty. Only then are interrupts re-enabled. Multi-queue NICs have one ring buffer (and thus one NAPI instance) per CPU core — this allows true parallel receive processing without locking.

**Section 7 — Live Observation**
```bash
# Interface stats:
ip -s link show eth0
cat /proc/net/dev

# Per-container interface stats (in container's netns):
ip netns exec <ns> ip -s link
cat /proc/$CPID/net/dev

# bpftrace: trace every packet received by the kernel
bpftrace -e 'kprobe:netif_receive_skb {
    $skb = (struct sk_buff *)arg0;
    printf("rx: dev=%s len=%u\n",
           str($skb->dev->name), $skb->len);
}'

# bpftrace: trace TCP connections being established
bpftrace -e 'kprobe:tcp_v4_connect {
    printf("connect: pid=%d comm=%s\n", pid, comm);
}'

# bpftrace: count packets by protocol
bpftrace -e 'kprobe:ip_rcv { @[comm] = count(); }'
```

**Section 8 — Key References table**
struct sk_buff, struct net_device, netif_receive_skb, dev_queue_xmit, tcp_v4_rcv, ip_rcv — each with file path and bare elixir v6.9 URL.

- [ ] **Step 3: git add and commit**
```bash
git add 06-network/
git commit -m "feat(ch06): README and sk_buff/net_device deep dive"
```

---

## Task 2: Socket + TCP deep dive

**Files:**
- Create: `06-network/kernel/06-b-socket.md`

- [ ] **Step 1: Write `06-network/kernel/06-b-socket.md`**

**Section 1 — Source locations**
- `include/linux/net.h` — https://elixir.bootlin.com/linux/v6.9/source/include/linux/net.h (struct socket)
- `include/net/sock.h` — https://elixir.bootlin.com/linux/v6.9/source/include/net/sock.h (struct sock)
- `include/linux/tcp.h` — https://elixir.bootlin.com/linux/v6.9/source/include/linux/tcp.h (struct tcp_sock)
- `net/socket.c` — https://elixir.bootlin.com/linux/v6.9/source/net/socket.c (socket syscalls)
- `net/ipv4/tcp.c` — https://elixir.bootlin.com/linux/v6.9/source/net/ipv4/tcp.c (TCP send/receive)

**Section 2 — struct socket (VFS-facing)**
```c
// include/linux/net.h
struct socket {
    socket_state        state;      // SS_FREE, SS_UNCONNECTED, SS_CONNECTING, SS_CONNECTED, SS_DISCONNECTING
    short               type;       // SOCK_STREAM (TCP), SOCK_DGRAM (UDP), SOCK_RAW...
    unsigned long       flags;      // SOCK_NOSPACE, SOCK_PASSCRED, SOCK_PASSSEC...
    struct file        *file;       // the VFS file this socket is associated with
    struct sock        *sk;         // pointer to the protocol-specific sock
    const struct proto_ops *ops;    // inet_stream_ops, inet_dgram_ops... (connect, accept, sendmsg...)
    struct socket_wq    wq;         // wait queue for select/poll/epoll
};
```
Source: https://elixir.bootlin.com/linux/v6.9/source/include/linux/net.h

The `struct socket` is the VFS-facing layer. Every `socket(2)` call allocates one and creates a corresponding `struct file`. The socket fd in userspace refers to this file. `struct socket` delegates all protocol work to `struct sock` via the `sk` pointer.

**Section 3 — struct sock (protocol-independent)**
```c
// include/net/sock.h (key fields)
struct sock {
    struct sock_common  __sk_common;    // embedded: family, state, hash, refcount, net ptr
    /* === shorthand aliases from __sk_common === */
    // sk_family       (AF_INET, AF_INET6, AF_UNIX...)
    // sk_state        (TCP_ESTABLISHED, TCP_LISTEN, TCP_CLOSE...)
    // sk_net          (pointer to struct net — the network namespace)

    socket_lock_t       sk_lock;        // protects sock from concurrent syscalls
    atomic_t            sk_drops;       // packets dropped due to buffer overflow
    int                 sk_rcvbuf;      // receive buffer size (bytes) — defaults to rmem_default
    int                 sk_sndbuf;      // send buffer size (bytes) — defaults to wmem_default
    struct sk_buff_head sk_receive_queue; // received-but-not-read packets
    struct sk_buff_head sk_write_queue;   // data waiting to be sent
    union {
        int             sk_wmem_alloc;  // bytes charged against sndbuf
        refcount_t      sk_wmem_alloc;
    };
    atomic_t            sk_rmem_alloc;  // bytes charged against rcvbuf
    struct socket      *sk_socket;      // back-pointer to struct socket
    void               *sk_user_data;   // user-set pointer (used by some protocols/hooks)
    struct sk_filter   *sk_filter;      // BPF socket filter (SO_ATTACH_FILTER)
    struct socket_wq   *sk_wq;          // wait queue for select/poll/epoll
    void              (*sk_data_ready)(struct sock *); // callback when data arrives
    void              (*sk_write_space)(struct sock *); // callback when sndbuf frees up
    void              (*sk_state_change)(struct sock *); // callback on state change
    struct proto       *sk_prot;        // TCP/UDP/RAW protocol operations
    struct net         *sk_net_refcnt;  // (see __sk_common.skc_net)
    __u32               sk_priority;    // SO_PRIORITY → IP TOS
    __u32               sk_mark;        // SO_MARK → fwmark (routing policy / iptables)
    kuid_t              sk_uid;         // socket owner UID (for netfilter owner matching)
    u8                  sk_txrehash;    // SO_TXREHASH — recompute hash on retransmit
    u8                  sk_shutdown;    // RCV_SHUTDOWN, SEND_SHUTDOWN
};
```
Source: https://elixir.bootlin.com/linux/v6.9/source/include/net/sock.h

Explain: `sk_receive_queue` holds sk_buffs that have been received and TCP-acknowledged but not yet read by userspace. `sk_write_queue` holds sk_buffs waiting to be sent (or retransmitted). The `sk_rcvbuf`/`sk_sndbuf` limits prevent any single socket from consuming unlimited memory. `sk_filter` is how `SO_ATTACH_FILTER` / `SO_ATTACH_BPF` works — the filter is evaluated on every incoming packet.

**Section 4 — struct tcp_sock (TCP-specific extension)**
```c
// include/linux/tcp.h (key fields)
struct tcp_sock {
    struct inet_connection_sock inet_conn; // embeds struct inet_sock → struct sock (MUST be first)

    u16     tcp_header_len;     // bytes of TCP header (including options)
    u32     segs_in;            // segments received (monotonic counter)
    u32     segs_out;           // segments sent (monotonic counter)
    u32     snd_una;            // oldest unacknowledged sequence number
    u32     snd_nxt;            // next sequence number to send
    u32     rcv_nxt;            // next expected sequence number from peer
    u32     snd_wnd;            // peer's receive window (how much we can send)
    u32     rcv_wnd;            // our receive window (how much peer can send)
    u32     snd_cwnd;           // congestion window (Reno, CUBIC, BBR...)
    u32     snd_ssthresh;       // slow-start threshold
    u32     srtt_us;            // smoothed RTT in microseconds
    u32     mdev_us;            // mean deviation of RTT (for RTO calculation)
    u32     rto;                // retransmission timeout (jiffies)
    ktime_t tcp_clock_cache;    // cached TCP clock
    struct  tcp_options_received rx_opt; // parsed TCP options (timestamps, SACK, window scale)
};
```
Source: https://elixir.bootlin.com/linux/v6.9/source/include/linux/tcp.h

The embedding chain: `struct tcp_sock` → `struct inet_connection_sock` → `struct inet_sock` → `struct sock`. `container_of()` navigates the chain. This is the kernel's single-inheritance pattern: a TCP socket IS a sock, IS an inet_sock, IS an inet_connection_sock, and finally IS a tcp_sock with all TCP state.

**Section 5 — TCP state machine**
The `sk_state` field in `struct sock` tracks the TCP FSM state. Key transitions:
```
LISTEN ──accept()──> SYN_RECV ──> ESTABLISHED ──close()──> FIN_WAIT1 ──> FIN_WAIT2 ──> TIME_WAIT
                                  ESTABLISHED <──accept()─ NEW_SYN (request_sock in SYN queue)
CLOSE_WAIT (passive close: peer sent FIN, we haven't called close() yet)
```

Server socket lifecycle:
1. `socket(AF_INET, SOCK_STREAM, 0)` — allocates struct socket + struct sock, state=CLOSE
2. `bind(2)` → `inet_bind()` — sets local address:port in sock
3. `listen(2)` → `inet_listen()` — state→LISTEN, allocates SYN queue (`icsk_accept_queue`)
4. `accept(2)` → `inet_accept()` — dequeues a completed connection from the accept queue
5. Connection arrives: SYN → `tcp_v4_rcv()` → creates `request_sock` in SYN queue → sends SYN-ACK → ACK arrives → promotes to full `sock` in accept queue

Client socket lifecycle:
1. `connect(2)` → `tcp_v4_connect()` → sends SYN, state→SYN_SENT
2. SYN-ACK arrives → `tcp_rcv_synsent_state_process()` → sends ACK → state→ESTABLISHED

**Section 6 — socket(2) kernel path**
```
socket(AF_INET, SOCK_STREAM, IPPROTO_TCP)
  └─ sys_socket() → __sys_socket()                     # net/socket.c
       └─ sock_create()
            └─ inet_create()                            # net/ipv4/af_inet.c
                 ├─ alloc struct socket
                 ├─ alloc struct sock (via sk_alloc → tcp_prot.slab)
                 └─ tcp_v4_init_sock()                  # net/ipv4/tcp_ipv4.c
                      └─ tcp_init_sock() — initialize congestion control, timers
  └─ sock_map_fd() → alloc struct file, install fd
```
Source: https://elixir.bootlin.com/linux/v6.9/source/net/socket.c

**Section 7 — SO_REUSEPORT and SO_REUSEADDR**
- `SO_REUSEADDR`: allows binding to a port in TIME_WAIT state; also allows multiple sockets to bind the same address:port if all set it (but only one receives for a given connection).
- `SO_REUSEPORT`: allows N independent sockets to bind the exact same address:port. The kernel distributes incoming connections/datagrams across all N sockets using a hash. Used by multi-process/multi-threaded servers (nginx) and increasingly by Kubernetes ingress controllers to fan traffic across worker goroutines.

**Section 8 — Live Observation**
```bash
# All TCP connections with state:
ss -tnp

# Listening sockets with their socket/recv/send buffer sizes:
ss -tlnpm

# Per-connection TCP stats (RTT, cwnd, retransmits):
ss -tni dst <ip>

# Socket receive/send buffer stats for a process:
cat /proc/$PID/net/sockstat

# bpftrace: trace new TCP connections (connect and accept)
bpftrace -e 'kprobe:tcp_v4_connect {
    printf("connect: pid=%-6d comm=%-20s\n", pid, comm);
}'

bpftrace -e 'kprobe:inet_csk_accept {
    $sk = (struct sock *)retval;
    printf("accept: pid=%-6d comm=%-20s\n", pid, comm);
}'

# bpftrace: trace TCP state changes
bpftrace -e 'kprobe:tcp_set_state {
    $sk = (struct sock *)arg0;
    $state = arg1;
    printf("tcp_state: pid=%d comm=%s state=%d\n", pid, comm, $state);
}'
```

**Section 9 — Key References table**
struct socket, struct sock, struct tcp_sock, tcp_v4_connect, inet_csk_accept, tcp_sendmsg, tcp_v4_rcv — each with file path and bare elixir v6.9 URL.

- [ ] **Step 2: git add and commit**
```bash
git add 06-network/kernel/06-b-socket.md
git commit -m "feat(ch06): socket/sock/tcp_sock deep dive — TCP lifecycle and state machine"
```

---

## Task 3: Network namespaces + veth pairs deep dive

**Files:**
- Create: `06-network/kernel/06-c-netns.md`

- [ ] **Step 1: Write `06-network/kernel/06-c-netns.md`**

**Section 1 — Source locations**
- `include/net/net_namespace.h` — https://elixir.bootlin.com/linux/v6.9/source/include/net/net_namespace.h (struct net)
- `net/core/net_namespace.c` — https://elixir.bootlin.com/linux/v6.9/source/net/core/net_namespace.c (namespace lifecycle)
- `drivers/net/veth.c` — https://elixir.bootlin.com/linux/v6.9/source/drivers/net/veth.c (veth driver)
- `net/core/rtnetlink.c` — https://elixir.bootlin.com/linux/v6.9/source/net/core/rtnetlink.c (netlink for ip link operations)

**Section 2 — struct net (network namespace)**
```c
// include/net/net_namespace.h (key fields)
struct net {
    refcount_t          passive;        // passive reference count (struct net can be freed when 0)
    spinlock_t          rules_mod_lock;
    atomic_t            dev_unreg_count;
    unsigned int        dev_base_seq;   // incremented on every interface add/remove
    int                 ifindex;        // next interface index to assign

    struct list_head    dev_base_head;  // list of all net_devices in this namespace
    struct hlist_head  *dev_name_head;  // hash by name (for dev_get_by_name)
    struct hlist_head  *dev_index_head; // hash by ifindex (for dev_get_by_index)

    struct ns_common    ns;             // common namespace header (inode for /proc/pid/ns/net)
    struct user_namespace *user_ns;     // user namespace
    struct ucounts      *ucounts;

    struct proc_dir_entry *proc_net;    // /proc/net symlink target for this namespace
    struct net_device   *loopback_dev;  // the loopback device for this namespace

    /* Routing tables */
    struct netns_ipv4   ipv4;          // IPv4-specific: routing table, arp table, sysctl knobs
    struct netns_ipv6   ipv6;          // IPv6-specific
    struct netns_nftables nft;         // nftables tables for this namespace
    struct netns_ct     ct;            // conntrack tables for this namespace
    struct list_head    rules_ops;     // routing policy rules
};
```
Source: https://elixir.bootlin.com/linux/v6.9/source/include/net/net_namespace.h

Explain: Each `struct net` is a completely independent networking stack. It has its own routing table (`ipv4.fib_main`, `ipv4.fib_default`), its own conntrack table (`ct`), its own sysctl namespace (`/proc/sys/net/`), and its own set of interfaces. The loopback device (`loopback_dev`) is always present. The `proc_net` pointer is why `/proc/net` shows different content inside a container — it resolves to the container's `struct net`.

**Section 3 — veth pair architecture**

A `veth` (virtual Ethernet) pair is the standard way to connect a container's network namespace to the host. It consists of two `net_device` instances bound together: traffic transmitted on one end appears as received traffic on the other end, without any copying.

```
Pod network namespace              Host network namespace
┌─────────────────────┐           ┌──────────────────────────────────┐
│  eth0 (veth end A)  │──────────▶│  vethXXXXXX (veth end B)        │
│  IP: 10.244.1.5/24  │◀──────────│  (no IP — or bridge member)     │
└─────────────────────┘           │  cni0 bridge (IP: 10.244.1.1/24)│
                                  └──────────────────────────────────┘
```

Veth transmit path in the kernel:
```c
// drivers/net/veth.c
static netdev_tx_t veth_xmit(struct sk_buff *skb, struct net_device *dev)
{
    struct veth_priv *rcv_priv = netdev_priv(rcv);  // peer device
    // Hand the skb directly to the peer's receive function:
    netif_receive_skb(skb);  // runs in peer's net namespace context
}
```

There is no actual copying — the sk_buff is passed by pointer from one end to the other. The `nd_net` on each end points to a different `struct net`.

**Section 4 — Container network setup (CNI flow)**

When a pod is created, the kubelet calls the CNI plugin via exec. The CNI plugin:
1. Creates a new network namespace: `clone(CLONE_NEWNET)` or `unshare(CLONE_NEWNET)`
2. Creates a veth pair: `ip link add veth0 type veth peer name veth1`
3. Moves one end into the pod namespace: `ip link set veth0 netns <fd>`
4. Configures IP in pod namespace: `ip addr add 10.244.1.5/24 dev veth0`
5. Attaches the host end to a bridge or sets up routes: `ip link set veth1 master cni0`
6. Sets up default routes in pod namespace: `ip route add default via 10.244.1.1`

At the kernel level, step 2 calls `rtnetlink` → `rtnl_newlink()` → `veth_newlink()` which allocates two `net_device` structs and links them via the `struct veth_priv` peer pointer. Step 3 calls `dev_change_net_namespace()`.

**Section 5 — /proc/pid/net contents**

Every process's `/proc/<pid>/net/` is a symlink to its network namespace's proc directory. Key files:
- `dev` — per-interface rx/tx stats (same as `ip -s link`)
- `tcp` — all TCP sockets in this namespace (state, addresses, ports, socket inode)
- `tcp6` — IPv6 TCP sockets
- `udp` — UDP sockets
- `fib_trie` — routing table (human-readable version)
- `arp` — ARP cache
- `if_inet6` — IPv6 interface addresses
- `netstat` — aggregated socket statistics

**Section 6 — Network namespace lifecycle**

Network namespaces are created with `CLONE_NEWNET` passed to `clone(2)` or `unshare(2)`. A new namespace gets:
- A fresh loopback device (down by default — the CNI plugin must bring it up)
- An empty routing table
- An empty conntrack table
- Fresh sysctl settings (inherited from the creator's namespace at clone time)

Namespaces are identified by the inode of `/proc/<pid>/ns/net`. Two processes with the same inode are in the same network namespace. The namespace is freed when all references (bind mounts, open fds to `/proc/<pid>/ns/net`) are released.

**Section 7 — Live Observation**
```bash
# List all network namespaces (requires root):
ip netns list

# Run command in container's netns:
nsenter -t $CPID -n ip link
nsenter -t $CPID -n ss -tnp

# Show veth peer relationships:
ip link show type veth

# Show which netns a veth belongs to:
ip -n <nsname> link

# bpftrace: trace network namespace creation
bpftrace -e 'kprobe:net_ns_get_by_pid {
    printf("netns: pid=%d comm=%s\n", pid, comm);
}'

# bpftrace: trace veth transmit (packets crossing namespace boundary)
bpftrace -e 'kprobe:veth_xmit {
    $skb = (struct sk_buff *)arg0;
    printf("veth_xmit: pid=%d comm=%s len=%u\n", pid, comm, $skb->len);
}'
```

**Section 8 — Key References table**
struct net, dev_change_net_namespace, veth_xmit, rtnl_newlink, net_ns_get_by_pid — each with file path and bare elixir v6.9 URL.

- [ ] **Step 2: git add and commit**
```bash
git add 06-network/kernel/06-c-netns.md
git commit -m "feat(ch06): network namespace and veth pair deep dive — container network isolation"
```

---

## Task 4: Netfilter + conntrack + K8s connection

**Files:**
- Create: `06-network/kernel/06-d-netfilter.md`
- Create: `06-network/k8s/06-k8s-connection.md`

- [ ] **Step 1: Write `06-network/kernel/06-d-netfilter.md`**

**Section 1 — Source locations**
- `include/linux/netfilter.h` — https://elixir.bootlin.com/linux/v6.9/source/include/linux/netfilter.h (NF_HOOK, hook priorities)
- `net/netfilter/nf_conntrack_core.c` — https://elixir.bootlin.com/linux/v6.9/source/net/netfilter/nf_conntrack_core.c (conntrack)
- `include/net/netfilter/nf_conntrack.h` — https://elixir.bootlin.com/linux/v6.9/source/include/net/netfilter/nf_conntrack.h (struct nf_conn)
- `net/ipv4/netfilter/iptable_nat.c` — https://elixir.bootlin.com/linux/v6.9/source/net/ipv4/netfilter/iptable_nat.c (NAT)
- `net/netfilter/ipvs/ip_vs_core.c` — https://elixir.bootlin.com/linux/v6.9/source/net/netfilter/ipvs/ip_vs_core.c (IPVS)

**Section 2 — Netfilter hook framework**
Netfilter defines 5 hook points in the packet path for each protocol family:
```
NF_INET_PRE_ROUTING   (1) — first hook after IP header validation; DNAT happens here
NF_INET_LOCAL_IN      (2) — packets destined for this host; firewall INPUT chain
NF_INET_FORWARD       (3) — packets being forwarded; firewall FORWARD chain
NF_INET_LOCAL_OUT     (4) — locally-generated packets; firewall OUTPUT chain
NF_INET_POST_ROUTING  (5) — last hook before NIC transmit; SNAT/MASQUERADE here
```

Each hook is a linked list of `struct nf_hook_ops` registered by modules (iptables, conntrack, nftables, eBPF). Each hook function returns one of:
- `NF_ACCEPT` — continue processing
- `NF_DROP` — drop the packet silently
- `NF_STOLEN` — module takes ownership of skb (no further processing)
- `NF_QUEUE` — send to userspace via NFQUEUE
- `NF_REPEAT` — call this hook again

```c
// include/linux/netfilter.h
struct nf_hook_ops {
    nf_hookfn          *hook;          // the hook function
    struct net_device  *dev;           // device filter (NULL = all devices)
    void               *priv;          // private data for hook
    u8                  pf;            // protocol family: NFPROTO_IPV4, NFPROTO_IPV6...
    enum nf_hook_ops_type type;        // NF_HOOK_OP_UNDEFINED, NF_HOOK_OP_NF_TABLES...
    unsigned int        hooknum;       // NF_INET_PRE_ROUTING, NF_INET_LOCAL_IN, etc.
    int                 priority;      // NF_IP_PRI_CONNTRACK, NF_IP_PRI_NAT_DST, etc.
};
```
Source: https://elixir.bootlin.com/linux/v6.9/source/include/linux/netfilter.h

Hook priority determines call order at the same hook point. Conntrack runs at `NF_IP_PRI_CONNTRACK` (-200) which is before NAT (`NF_IP_PRI_NAT_DST` = -100). This ordering is critical: conntrack must see the original packet BEFORE NAT translates the address, so it can store the original tuple for reply translation.

**Section 3 — struct nf_conn (conntrack entry)**
```c
// include/net/netfilter/nf_conntrack.h (key fields)
struct nf_conn {
    struct nf_conntrack ct_general;     // refcount
    spinlock_t          lock;
    u32                 timeout;        // when this entry expires (jiffies)
    struct nf_conntrack_tuple_hash tuplehash[IP_CT_DIR_MAX]; // original + reply tuples
    unsigned long       status;         // IPS_CONFIRMED, IPS_SRC_NAT, IPS_DST_NAT, IPS_DYING
    possible_net_t      ct_net;         // network namespace
    struct nf_ct_ext   *ext;            // extensions: NAT, seqadj, acct, labels, timestamp
    union nf_conntrack_proto proto;     // protocol-specific: tcp state, udp reply count...
};
```

Each conntrack entry tracks a bidirectional flow by storing two tuples:
- `tuplehash[IP_CT_DIR_ORIGINAL]` — the original packet direction (src IP:port → dst IP:port)
- `tuplehash[IP_CT_DIR_REPLY]` — the expected reply direction (after NAT translation)

For a kube-proxy DNAT rule (ClusterIP:port → PodIP:port):
- PREROUTING: DNAT translates dst from ClusterIP:port to PodIP:port; conntrack stores both tuples
- Reply packet: conntrack sees the reply tuple, applies reverse DNAT (src PodIP:port → ClusterIP:port)
- Userspace process sees the reply coming from ClusterIP:port — the NAT is transparent

**Section 4 — iptables chains and kube-proxy rules**

kube-proxy (iptables mode) adds rules to `KUBE-SERVICES` chain (jumped to from PREROUTING and OUTPUT):

```
PREROUTING
  └─ KUBE-SERVICES
       ├─ KUBE-SVC-<hash>  (for ClusterIP:port)
       │    ├─ KUBE-SEP-<hash1>  (probability 0.333) → DNAT to pod1:port
       │    ├─ KUBE-SEP-<hash2>  (probability 0.5)   → DNAT to pod2:port
       │    └─ KUBE-SEP-<hash3>                       → DNAT to pod3:port
       └─ KUBE-NODEPORTS
            └─ KUBE-SVC-<hash>  (for NodePort)
```

Each `KUBE-SEP-*` rule uses a statistic module for random load balancing:
```
-A KUBE-SVC-XYZ -m statistic --mode random --probability 0.33333 -j KUBE-SEP-ABC
-A KUBE-SEP-ABC -p tcp -j DNAT --to-destination 10.244.1.5:8080
```

The `--probability` is computed as `1/remaining_endpoints`. The first endpoint fires with probability 1/N, the second (if first misses) fires with 1/(N-1), and so on, giving equal distribution across all N endpoints.

**Section 5 — IPVS mode (alternative to iptables)**

kube-proxy also supports IPVS mode, which uses the kernel's IP Virtual Server (IPVS) load balancer instead of iptables chains. IPVS scales much better for large clusters (10,000+ services) because:
- iptables uses a linear rule scan; O(N) per packet for N rules
- IPVS uses hash tables; O(1) per packet regardless of service count

IPVS is implemented as a Netfilter hook in LOCAL_IN (for established connections) and uses `ip_vs_schedule()` to select the backend. It also uses conntrack for connection state.

**Section 6 — Live Observation**
```bash
# View all iptables rules including kube-proxy rules:
iptables-save | grep -E "KUBE|DNAT"

# Count kube-proxy iptables rules (scales with service count):
iptables-save | grep -c KUBE

# Show conntrack table (all active connections):
conntrack -L

# Show conntrack stats per CPU:
conntrack -S

# IPVS service table (if kube-proxy in ipvs mode):
ipvsadm -Ln

# bpftrace: trace new conntrack entries
bpftrace -e 'kprobe:nf_conntrack_hash_check_insert {
    printf("conntrack new: pid=%d comm=%s\n", pid, comm);
}'

# bpftrace: trace iptables rule matches
bpftrace -e 'kprobe:ipt_do_table {
    printf("iptables: pid=%d comm=%s\n", pid, comm);
}'
```

**Section 7 — Key References table**
struct nf_hook_ops, struct nf_conn, nf_conntrack_hash_check_insert, ipt_do_table, ip_vs_schedule — each with file path and bare elixir v6.9 URL.

- [ ] **Step 2: Write `06-network/k8s/06-k8s-connection.md`**

**Section 1 — Pod Networking Model**
Table of K8s network mechanisms:
| Mechanism | Kernel Component | Purpose |
|-----------|-----------------|---------|
| Pod IP assignment | struct net + veth pair | Each pod gets its own network namespace |
| Pod-to-pod (same node) | Bridge + routing | Via cni0 bridge or host routing |
| Pod-to-pod (cross node) | Overlay (VXLAN/IPIP) or BGP | CNI plugin routes packets between nodes |
| Service ClusterIP | iptables DNAT + conntrack | kube-proxy programs DNAT rules per Service |
| Service NodePort | iptables DNAT + MASQUERADE | Hairpin NAT for external traffic |
| NetworkPolicy | iptables/nftables/eBPF | CNI plugin enforces L3/L4 policy |
| DNS (CoreDNS) | UDP socket, struct sock | In-cluster DNS for service discovery |

**Section 2 — CNI Plugin Lifecycle**
Walk through what happens when a pod is scheduled:
1. kubelet calls CRI (containerd) → containerd creates pod sandbox (network namespace)
2. kubelet executes CNI plugin binary with `ADD` command and pod netns fd
3. CNI plugin creates veth pair, moves one end into pod netns, assigns IP, sets routes
4. CNI plugin returns IP in JSON; kubelet stores it in pod status
5. On pod deletion: kubelet calls CNI with `DEL` command → CNI removes veth pair and routes

Show how to verify:
```bash
# Pod network namespace (same as /proc/<pid>/ns/net inode):
ls -la /proc/$CPID/ns/net
ip netns identify $CPID

# Veth pair for a pod:
nsenter -t $CPID -n ip link
# Note the veth index, then on host:
ip link show | grep -A1 "^<veth-index>"
```

**Section 3 — Service ClusterIP packet walk**
Walk through a complete packet from pod → Service ClusterIP → backend pod:
1. Pod sends SYN to 10.96.0.1:443 (ClusterIP)
2. PREROUTING hook fires; conntrack creates new entry
3. kube-proxy iptables KUBE-SERVICES chain matches 10.96.0.1:443
4. KUBE-SVC rule selects a KUBE-SEP with --probability logic
5. DNAT: dst changed to 10.244.1.5:443 (backend pod IP)
6. Conntrack stores original tuple (→ ClusterIP) and reply tuple (← pod IP)
7. Packet routed to backend pod (via bridge or overlay)
8. Reply: backend sends SYN-ACK from 10.244.1.5:443 to src pod
9. conntrack reverse-DNAT: src changed back to 10.96.0.1:443
10. src pod sees the reply as coming from ClusterIP — transparent

**Section 4 — NetworkPolicy implementation**
NetworkPolicy is enforced at L3/L4 by the CNI plugin (not kube-proxy). Implementation varies:
- **Calico**: uses iptables/nftables rules with IP sets
- **Cilium**: uses eBPF programs attached to tc hooks on pod veth interfaces
- **flannel (no policy)**: no enforcement — requires a separate policy controller

The kernel provides the primitives; the CNI plugin programs them. With eBPF (Cilium):
- BPF program attached to `tc ingress` on the pod-facing veth end
- Program checks packet against policy (IP sets stored in BPF maps)
- Returns `TC_ACT_OK` (allow) or `TC_ACT_SHOT` (drop)

**Section 5 — Pod DNS resolution (CoreDNS)**
Every pod gets `/etc/resolv.conf` injected by kubelet pointing to the CoreDNS ClusterIP. DNS resolution:
1. Container glibc `getaddrinfo("nginx.default.svc.cluster.local")`
2. UDP socket to CoreDNS IP:53 (itself a Service with DNAT)
3. CoreDNS responds with ClusterIP of the nginx Service
4. Container connects to ClusterIP; kube-proxy DNAT routes to a pod

**Section 6 — Common Network Failure Patterns**
Table: Symptom | Cause | Diagnosis
- Pod can't reach Service | iptables rules missing | `iptables-save | grep KUBE-SVC-<hash>`; check kube-proxy logs
- Pod IP not routable | CNI misconfiguration | Check veth pair; `ip route show`; check bridge
- DNS lookup fails | CoreDNS pod down | `kubectl -n kube-system get pods -l k8s-app=kube-dns`
- conntrack table full | High connection rate | `sysctl net.nf_conntrack_count`; `sysctl net.nf_conntrack_max`
- Intermittent packet loss | conntrack race (SYN packet dropped) | Check `conntrack -S | grep insert_failed`
- Service latency spikes | iptables rule scan O(N) | Check rule count; consider migrating to IPVS

**Section 7 — Key Kernel References table**
struct nf_conn, struct net, veth_xmit, nf_conntrack_hash_check_insert, ip_vs_schedule, tcp_v4_rcv, tcp_v4_connect — each with file path and bare elixir v6.9 URL.

- [ ] **Step 3: git add and commit**
```bash
git add 06-network/kernel/06-d-netfilter.md 06-network/k8s/06-k8s-connection.md
git commit -m "feat(ch06): Netfilter/conntrack deep dive and k8s networking connection"
```

---

## Task 5: C exercise — `tcp-echo-demo`

**Files:**
- Create: `06-network/exercises/tcp-echo-demo/tcp_echo_demo.c`
- Create: `06-network/exercises/tcp-echo-demo/Makefile`
- Create: `06-network/exercises/tcp-echo-demo/README.md`
- Create: `06-network/exercises/tcp-echo-demo/.gitignore`

**What it demonstrates:** A minimal TCP echo server and client in a single C file. The program forks: the child becomes the server (socket/bind/listen/accept/echo loop); the parent is the client (connects, sends messages, receives echoes, disconnects). Uses `SO_REUSEADDR` to allow immediate restart. Demonstrates the socket → bind → listen → accept/connect lifecycle at the system call level.

The server:
1. `socket(AF_INET, SOCK_STREAM, 0)` + `setsockopt(SO_REUSEADDR)`
2. `bind(INADDR_LOOPBACK, 0)` — let kernel pick port; retrieve with `getsockname()`
3. `listen(backlog=5)`
4. `accept()` in a loop — read until EOF, write back verbatim, close

The client:
1. `socket(AF_INET, SOCK_STREAM, 0)`
2. `connect(127.0.0.1, server_port)`
3. Send 5 messages ("Hello 1" through "Hello 5"), receive each echo, print
4. Close socket, wait for server to exit

Both sides print their fd numbers, addresses, and each operation so the reader can follow the kernel path in real time.

Compile: `gcc -Wall -Wextra -Werror -o tcp_echo_demo tcp_echo_demo.c`

README sections:
1. What It Demonstrates
2. Build and run: `make run`
3. Expected output (both server and client prints interleaved)
4. Kernel path: `socket(2)` → `inet_create()` at https://elixir.bootlin.com/linux/v6.9/source/net/ipv4/af_inet.c; `listen(2)` → `inet_listen()` at https://elixir.bootlin.com/linux/v6.9/source/net/ipv4/af_inet.c; `accept(2)` → `inet_accept()` → `inet_csk_accept()` at https://elixir.bootlin.com/linux/v6.9/source/net/ipv4/inet_connection_sock.c — all bare URLs
5. K8s connection: describe how the kubelet uses TCP sockets to talk to containerd (CRI), how readiness/liveness probes use `connect(2)` to check pod health, and how kube-apiserver traffic follows the same TCP lifecycle
6. Exercises: (a) Use `ss -tnp` to observe the connection in ESTABLISHED state while the server is sleeping. (b) Add `SO_KEEPALIVE` + `TCP_KEEPIDLE` to the server socket and explain why Kubernetes uses keepalives for its long-lived connections to the API server. (c) Try connecting without the server running and observe the ECONNREFUSED error.

- [ ] **Step 5: Test compilation and commit**
```bash
cd 06-network/exercises/tcp-echo-demo
gcc -Wall -Wextra -Werror -o tcp_echo_demo tcp_echo_demo.c
rm -f tcp_echo_demo
cd ../../..
git add 06-network/exercises/tcp-echo-demo/
git commit -m "feat(ch06): tcp-echo-demo C exercise — TCP socket lifecycle"
```

---

## Task 6: Go exercise — `netns-inspector`

**Files:**
- Create: `06-network/exercises/netns-inspector/go.mod`
- Create: `06-network/exercises/netns-inspector/main.go`
- Create: `06-network/exercises/netns-inspector/Makefile`
- Create: `06-network/exercises/netns-inspector/README.md`
- Create: `06-network/exercises/netns-inspector/.gitignore`

**Module:** `github.com/linux-to-k8s/netns-inspector`, go 1.22

**What it demonstrates:** Parse `/proc/<pid>/net/dev` to read per-interface receive/transmit statistics for any process (and thus any container). Show all interfaces in the process's network namespace, their byte/packet counts, and error rates.

Key type:
```go
type NetInterface struct {
    Name      string
    RxBytes   uint64
    RxPackets uint64
    RxErrors  uint64
    RxDropped uint64
    TxBytes   uint64
    TxPackets uint64
    TxErrors  uint64
    TxDropped uint64
}
```

Parse `/proc/<pid>/net/dev`: skip the two header lines, then each line is:
`<iface>: rx_bytes rx_packets rx_errs rx_drop rx_fifo rx_frame rx_comp rx_multi tx_bytes tx_packets tx_errs tx_drop tx_fifo tx_colls tx_carr tx_comp`
Trim the trailing `:` from the interface name.

Modes:
- `netns-inspector` — current process
- `netns-inspector --pid <pid>` — target process
- `netns-inspector --pid <pid> --watch` — refresh every 2 seconds showing per-second delta rates

Makefile targets: build, run, run-pid (PID=...), clean

README sections:
1. What It Demonstrates
2. Build and run examples
3. /proc/pid/net/dev format explanation with all 16 fields labeled
4. Expected output for a container process (showing eth0 and lo)
5. --watch output example (showing per-second byte rates)
6. Exercises: (a) Compare rx/tx bytes between pod and its host veth peer. (b) Watch bytes climb during a `curl` inside a container. (c) Add --json flag.
7. K8s connection: the kubelet reads interface stats from cAdvisor (which uses /proc/pid/net/dev) to populate `container_network_receive_bytes_total` and `container_network_transmit_bytes_total` Prometheus metrics

Verify: `go build -o netns-inspector .`, `go vet ./...`

- [ ] **Step 6: Commit**
```bash
git add 06-network/exercises/netns-inspector/
git commit -m "feat(ch06): netns-inspector Go exercise — parse /proc/pid/net/dev"
```

---

## Task 7: kube-inspect checkpoint 06 — network namespace interface stats

**Files:**
- Create: `kube-inspect/internal/netns/netns.go` (new package)
- Modify: `kube-inspect/cmd/kube-inspect/main.go` (add `--netns` flag)
- Modify: `kube-inspect/CHECKPOINT.md` (mark checkpoint 06 done)

**What it adds:**
- New package `internal/netns`
- `netns.NetInterface` struct: Name string, RxBytes/RxPackets/RxErrors/RxDropped/TxBytes/TxPackets/TxErrors/TxDropped uint64
- `netns.ListPodInterfaces(podUID string) ([]NetInterface, error)` — finds the pod's PID via `proc.ListPodProcesses`, reads `/proc/<pid>/net/dev`, parses all interfaces
- `--netns` flag in main.go: shows interface table for the pod

```go
// internal/netns/netns.go
package netns

import (
    "bufio"
    "fmt"
    "os"
    "strconv"
    "strings"

    "github.com/linux-to-k8s/kube-inspect/internal/proc"
)

type NetInterface struct {
    Name      string
    RxBytes   uint64
    RxPackets uint64
    RxErrors  uint64
    RxDropped uint64
    TxBytes   uint64
    TxPackets uint64
    TxErrors  uint64
    TxDropped uint64
}

func ListPodInterfaces(podUID string) ([]NetInterface, error) {
    procs, err := proc.ListPodProcesses(podUID)
    if err != nil || len(procs) == 0 {
        return nil, fmt.Errorf("no processes for pod %s: %w", podUID, err)
    }
    return readNetDev(procs[0].PID)
}

func readNetDev(pid int) ([]NetInterface, error) {
    // parse /proc/<pid>/net/dev
    // skip first 2 header lines
    // format: <iface>: rx_bytes rx_packets rx_errs rx_drop ... tx_bytes tx_packets tx_errs tx_drop ...
}
```

`--netns` output:
```
Network interfaces for pod <uid>:
  INTERFACE   RX-BYTES    RX-PKTS  RX-ERR  RX-DROP   TX-BYTES    TX-PKTS  TX-ERR  TX-DROP
  lo          1024        12       0       0         1024        12       0       0
  eth0        204800      512      0       0         98304       384      0       0
```

- [ ] **Step 7: Verify and commit**
```bash
cd kube-inspect
go build ./...
go vet ./...
cd ..
git add kube-inspect/
git commit -m "feat(kube-inspect): checkpoint 06 — network namespace interface stats"
```

Update CHECKPOINT.md: change checkpoint 06 status from "pending" to "done".

---

## Self-Review

**Spec coverage:**
- ✅ `06-network/README.md` — objectives, prerequisites, reading order, VFS diagram
- ✅ `kernel/06-a-skbuff.md` — struct sk_buff, struct net_device, rx/tx paths, NAPI
- ✅ `kernel/06-b-socket.md` — struct socket/sock/tcp_sock, TCP FSM, socket syscall path
- ✅ `kernel/06-c-netns.md` — struct net, veth pair, CNI flow, /proc/pid/net
- ✅ `kernel/06-d-netfilter.md` — struct nf_hook_ops, struct nf_conn, iptables chains, IPVS
- ✅ `k8s/06-k8s-connection.md` — pod networking, CNI, Service DNAT walk, NetworkPolicy, DNS
- ✅ `exercises/tcp-echo-demo/` — C exercise compiles with -Wall -Wextra -Werror
- ✅ `exercises/netns-inspector/` — Go exercise, go vet clean
- ✅ `kube-inspect/` — NetInterface, ListPodInterfaces, --netns flag, checkpoint 06 done

**Placeholder scan:** All sections require complete content — no TBD/TODO allowed.

**Type consistency:** `netns.NetInterface` defined in Task 7; `netns.ListPodInterfaces()` uses `proc.ListPodProcesses` (already exists in proc.go). `main.go` uses `ifaces[i].Name`, `ifaces[i].RxBytes` etc. — must match struct fields exactly.
