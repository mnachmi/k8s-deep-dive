# 06-b — struct socket, struct sock, struct tcp_sock: TCP Lifecycle

Every TCP connection in Linux is represented by three nested structs layered on top of one another. `struct socket` is the VFS object that userspace touches through a file descriptor. `struct sock` is the protocol-independent network layer that manages buffers, callbacks, and state. `struct tcp_sock` is the TCP-specific extension that carries sequence numbers, congestion control state, and RTT estimates. Understanding all three — and the exact call path from `socket(2)` to packet transmission — is a prerequisite for reading eBPF programs, diagnosing connection drops, and tuning kernel TCP behaviour in Kubernetes.

---

## 1. Source Locations

| File | Contents | Source |
|------|----------|--------|
| `include/linux/net.h` | `struct socket`, `socket_state` enum, `proto_ops` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/net.h |
| `include/net/sock.h` | `struct sock`, `struct sock_common`, `sk_buff_head`, callbacks | https://elixir.bootlin.com/linux/v6.9/source/include/net/sock.h |
| `include/linux/tcp.h` | `struct tcp_sock`, `struct tcp_options_received` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/tcp.h |
| `net/socket.c` | `__sys_socket()`, `sock_create()`, `sock_map_fd()` | https://elixir.bootlin.com/linux/v6.9/source/net/socket.c |
| `net/ipv4/tcp.c` | `tcp_sendmsg()`, `tcp_recvmsg()`, `tcp_write_xmit()` | https://elixir.bootlin.com/linux/v6.9/source/net/ipv4/tcp.c |

---

## 2. struct socket (VFS-facing)

```c
// include/linux/net.h
struct socket {
    socket_state         state;   // SS_FREE, SS_UNCONNECTED, SS_CONNECTING,
                                  // SS_CONNECTED, SS_DISCONNECTING
    short                type;    // SOCK_STREAM (TCP), SOCK_DGRAM (UDP), SOCK_RAW...
    unsigned long        flags;   // SOCK_NOSPACE, SOCK_PASSCRED, SOCK_PASSSEC...
    struct file         *file;    // the VFS file this socket is associated with
    struct sock         *sk;      // pointer to the protocol-specific sock
    const struct proto_ops *ops;  // inet_stream_ops, inet_dgram_ops...
                                  // (connect, accept, sendmsg, recvmsg, bind...)
    struct socket_wq     wq;      // wait queue for select/poll/epoll
};
```

Source: https://elixir.bootlin.com/linux/v6.9/source/include/linux/net.h

`struct socket` is the VFS-facing layer. Every `socket(2)` call allocates one and creates a corresponding `struct file`. The socket fd in userspace is simply an index into the process's file descriptor table pointing at that `struct file`. VFS operations (`read`, `write`, `poll`, `close`) are redirected to socket methods through the file's `f_op` (`socket_file_ops`).

`struct socket` knows nothing about TCP, UDP, or any other protocol. It delegates all protocol work to `struct sock` via the `sk` pointer. The `ops` field holds a vtable (`proto_ops`) with per-address-family, per-type implementations — for TCP/IPv4, `ops` points to `inet_stream_ops`, which dispatches `connect()` to `inet_stream_connect()`, `accept()` to `inet_accept()`, and so on.

The `state` field is a coarse liveness indicator for the socket object itself (`SS_CONNECTED`, `SS_DISCONNECTING`). It is distinct from the TCP FSM state tracked inside `struct sock` via `sk_state` (`TCP_ESTABLISHED`, `TCP_FIN_WAIT1`, etc.).

---

## 3. struct sock (protocol-independent)

```c
// include/net/sock.h (key fields)
struct sock {
    struct sock_common  __sk_common;      // embedded: family, state, hash, refcount, net ptr
    /* Aliases from __sk_common — accessed via #define shortcuts: */
    // #define sk_family    __sk_common.skc_family   (AF_INET, AF_INET6, AF_UNIX...)
    // #define sk_state     __sk_common.skc_state    (TCP_ESTABLISHED, TCP_LISTEN, TCP_CLOSE...)
    // skc_net is of type possible_net_t — use sock_net(sk) to get struct net *

    socket_lock_t        sk_lock;         // protects sock from concurrent syscalls
    atomic_t             sk_drops;        // packets dropped due to buffer overflow
    int                  sk_rcvbuf;       // receive buffer size (bytes) — rmem_default
    int                  sk_sndbuf;       // send buffer size (bytes) — wmem_default
    struct sk_buff_head  sk_receive_queue;// received-but-not-read packets
    struct sk_buff_head  sk_write_queue;  // data waiting to be sent / retransmitted
    refcount_t           sk_wmem_alloc;   // bytes charged against sndbuf
    atomic_t             sk_rmem_alloc;   // bytes charged against rcvbuf
    struct socket       *sk_socket;       // back-pointer to struct socket
    void                *sk_user_data;    // user-set pointer (used by some protocols/hooks)
    struct sk_filter    *sk_filter;       // BPF socket filter (SO_ATTACH_FILTER/SO_ATTACH_BPF)
    struct socket_wq    *sk_wq;           // wait queue for select/poll/epoll
    void     (*sk_data_ready)(struct sock *);   // callback when data arrives
    void     (*sk_write_space)(struct sock *);  // callback when sndbuf frees up
    void     (*sk_state_change)(struct sock *); // callback on state change
    struct proto        *sk_prot;         // TCP/UDP/RAW protocol operations table
    __u32                sk_priority;     // SO_PRIORITY → IP TOS
    __u32                sk_mark;         // SO_MARK → fwmark (routing policy / iptables)
    kuid_t               sk_uid;          // socket owner UID (netfilter owner matching)
    u8                   sk_shutdown;     // RCV_SHUTDOWN | SEND_SHUTDOWN
};
```

Source: https://elixir.bootlin.com/linux/v6.9/source/include/net/sock.h

**Network namespace access — critical accuracy note.** The network namespace pointer is NOT a plain `struct net *sk_net` field on `struct sock`. It lives in `__sk_common.skc_net`, which is of type `possible_net_t` — a wrapper that may or may not hold a refcount depending on `CONFIG_NET_NS`. Always access it through the `sock_net(sk)` inline helper, which resolves to the correct `struct net *`. Do not dereference `skc_net` directly.

**`sk_receive_queue`** holds `sk_buff` structs that the TCP stack has received, validated, acknowledged, and placed in order — but that userspace has not yet read via `recv()`/`read()`. Each `sk_buff` here represents reassembled, in-order data. When userspace calls `recv()`, `tcp_recvmsg()` drains this queue, copying bytes into the user buffer and freeing the skbs.

**`sk_write_queue`** holds `sk_buff` structs that have been queued for transmission but are still in flight or awaiting acknowledgement. TCP does not free an skb from this queue until the peer ACKs the data it carries — the queue is the retransmission buffer. `tcp_write_xmit()` walks this queue to decide which segments to send based on the congestion window.

**`sk_rcvbuf` and `sk_sndbuf`** are per-socket memory limits. The kernel charges each received byte against `sk_rmem_alloc`; if `sk_rmem_alloc` would exceed `sk_rcvbuf`, the packet is dropped and `sk_drops` is incremented. Similarly, `tcp_sendmsg()` blocks the writer if `sk_wmem_alloc` approaches `sk_sndbuf`. Tuning these limits (via `SO_RCVBUF`/`SO_SNDBUF` or `net.core.rmem_max`) is often the first fix for throughput problems on high-latency links.

**`sk_filter`** is how `SO_ATTACH_FILTER` and `SO_ATTACH_BPF` work. When a filter is attached, every incoming packet is evaluated by the BPF program before being placed on `sk_receive_queue`. If the program returns zero, the packet is dropped; otherwise it returns the number of bytes to keep. Classic socket filters (`tcpdump` capture filters) use this path. `SO_ATTACH_BPF` is the modern `BPF_PROG_TYPE_SOCKET_FILTER` equivalent.

**Callbacks (`sk_data_ready`, `sk_write_space`, `sk_state_change`)** decouple the protocol stack from higher layers. When TCP receives new data, it calls `sk->sk_data_ready(sk)`, which wakes any process blocked in `epoll_wait()`, `select()`, or `read()` on that socket. `sk_write_space` wakes writers when send buffer space is freed (after ACKs arrive). `sk_state_change` is called on TCP state transitions and is used by `accept()` to detect when a connection moves to ESTABLISHED.

---

## 4. struct tcp_sock (TCP-specific extension)

```c
// include/linux/tcp.h (key fields)
struct tcp_sock {
    struct inet_connection_sock inet_conn; // embeds inet_sock → sock (MUST be first field)

    u16     tcp_header_len;     // bytes of TCP header including options (min 20)
    u32     segs_in;            // total segments received (monotonic counter)
    u32     segs_out;           // total segments sent (monotonic counter)

    /* Sequence number state */
    u32     snd_una;            // oldest unacknowledged byte (left edge of send window)
    u32     snd_nxt;            // next sequence number to assign to new data
    u32     rcv_nxt;            // next expected sequence number from peer

    /* Window sizes */
    u32     snd_wnd;            // peer's receive window (limits how much we can send)
    u32     rcv_wnd;            // our receive window (limits how much peer can send us)

    /* Congestion control */
    u32     snd_cwnd;           // congestion window (bytes; Reno, CUBIC, BBR manage this)
    u32     snd_ssthresh;       // slow-start threshold

    /* RTT estimation */
    u32     srtt_us;            // smoothed RTT in microseconds (EWMA of measured RTTs)
    u32     mdev_us;            // mean deviation of RTT (used to compute RTO)
    u32     rto;                // retransmission timeout (in jiffies)

    ktime_t tcp_clock_cache;    // cached TCP clock value (reduces clock_gettime cost)

    /* Parsed TCP options from peer */
    struct tcp_options_received rx_opt; // timestamps, SACK blocks, window scale factor
};
```

Source: https://elixir.bootlin.com/linux/v6.9/source/include/linux/tcp.h

**Embedding chain.** The kernel uses C struct embedding as a single-inheritance mechanism. The chain from most specific to most general is:

```
struct tcp_sock
  └─ struct inet_connection_sock  inet_conn      (first field)
       └─ struct inet_sock        icsk_inet      (first field)
            └─ struct sock        sk             (first field)
                 └─ struct sock_common  __sk_common  (first field)
```

Because each embedded struct is always the first field, `container_of()` with zero offset casts freely between them. A `struct sock *sk` received by `tcp_v4_rcv()` can be safely cast to `struct tcp_sock *` with `tcp_sk(sk)` — the macro expands to `container_of(sk, struct tcp_sock, inet_conn.icsk_inet.sk)`. This is not pointer arithmetic magic; it works because the C standard guarantees a struct and its first member share the same address.

**Congestion control.** `snd_cwnd` (congestion window) is the sender-side limit on how much unacknowledged data can be in flight at once, expressed in bytes (older code used segments). It starts at the initial window (`TCP_INIT_CWND`, typically 10 segments), grows exponentially during slow start (doubling on each RTT until it exceeds `snd_ssthresh`), then grows linearly in congestion avoidance (one MSS per RTT). On packet loss (triple duplicate ACKs or RTO expiry), `snd_ssthresh` is set to half the current `snd_cwnd`, and `snd_cwnd` is reduced. BBR ignores loss as a primary signal and instead models bandwidth and RTT directly, keeping `snd_cwnd` proportional to the estimated bandwidth-delay product. The congestion control algorithm is pluggable; the active algorithm for a socket is stored in `inet_conn.icsk_ca_ops`.

**RTT estimation.** `srtt_us` is an exponentially-weighted moving average (EWMA) of measured round-trip times. `mdev_us` is the mean deviation (a proxy for jitter). The retransmission timeout is computed as `RTO = srtt_us + 4 * mdev_us`, following RFC 6298. A connection with high jitter therefore gets a larger RTO, reducing spurious retransmissions at the cost of slower loss recovery.

---

## 5. TCP State Machine

The `sk_state` field (aliased from `__sk_common.skc_state`) tracks where a connection is in the TCP finite state machine. The kernel defines states as `TCP_ESTABLISHED = 1`, `TCP_SYN_SENT`, `TCP_SYN_RECV`, `TCP_FIN_WAIT1`, `TCP_FIN_WAIT2`, `TCP_TIME_WAIT`, `TCP_CLOSE`, `TCP_CLOSE_WAIT`, `TCP_LAST_ACK`, `TCP_LISTEN`, `TCP_CLOSING`.

Key transitions:

```
LISTEN ──(SYN arrives)──> SYN_RECV ──(ACK arrives)──> ESTABLISHED
                                                          │
                                       active close()    │   passive close (peer FIN)
                                              │           │           │
                                         FIN_WAIT1   CLOSE_WAIT ─close()─> LAST_ACK
                                              │
                                         FIN_WAIT2
                                              │
                                         TIME_WAIT (2*MSL, ~60–120 s)
```

**Server socket lifecycle (4 steps):**

1. `socket(AF_INET, SOCK_STREAM, 0)` — allocates `struct socket` and `struct sock`, `sk_state = TCP_CLOSE`.
2. `bind(2)` → `inet_bind()` — sets local address and port in `inet_sock.inet_saddr` / `inet_sport`.
3. `listen(2)` → `inet_listen()` — sets `sk_state = TCP_LISTEN`, allocates the SYN queue (`icsk_accept_queue`) that will hold half-open connections.
4. `accept(2)` → `inet_accept()` — blocks until `inet_csk_accept()` dequeues a fully-established connection from the accept queue and returns a new `struct sock`.

When a SYN arrives, `tcp_v4_rcv()` creates a lightweight `request_sock` in the SYN queue, sends SYN-ACK, and waits. When the client's ACK arrives, the `request_sock` is promoted to a full `struct sock` (via `tcp_v4_syn_recv_sock()`), which moves to the accept queue. The original listening socket never changes state; only the newly created per-connection sock transitions through `SYN_RECV → ESTABLISHED`.

**Client socket lifecycle (3 steps):**

1. `connect(2)` → `tcp_v4_connect()` — performs a route lookup, sets the source IP and port, sends SYN, sets `sk_state = TCP_SYN_SENT`.
2. SYN-ACK arrives → `tcp_rcv_synsent_state_process()` — validates the SYN-ACK, records `irs` (initial receive sequence), sends ACK.
3. ACK sent → `sk_state = TCP_ESTABLISHED`. `connect(2)` returns 0 to userspace.

---

## 6. socket(2) Kernel Call Path

```
socket(AF_INET, SOCK_STREAM, IPPROTO_TCP)
  └─ sys_socket() → __sys_socket()                     # net/socket.c
       └─ sock_create()
            └─ __sock_create()
                 └─ net_families[AF_INET]->create()
                      └─ inet_create()                  # net/ipv4/af_inet.c
                           ├─ sock_alloc() — alloc struct socket + inode
                           ├─ sk_alloc() — alloc struct sock from tcp_prot.slab
                           └─ tcp_v4_init_sock()        # net/ipv4/tcp_ipv4.c
                                └─ tcp_init_sock()
                                     ├─ tcp_init_xmit_timers()
                                     ├─ skb_queue_head_init(&tp->out_of_order_queue)
                                     └─ icsk->icsk_ca_ops = &tcp_init_congestion_ops
  └─ sock_map_fd() → alloc struct file, install into fd table, return fd
```

Source: https://elixir.bootlin.com/linux/v6.9/source/net/socket.c

The path has two distinct allocation steps. `sock_alloc()` allocates the `struct socket` (and its associated inode, since sockets are VFS objects). `sk_alloc()` allocates the protocol-specific `struct sock` — for TCP this is actually a `struct tcp_sock` allocated from `tcp_prot.slab`, a dedicated `kmem_cache` sized to `sizeof(struct tcp_sock)`. The slab allocator gives near-zero allocation cost for the common case of many short-lived connections.

`tcp_init_sock()` initialises congestion control to the system default (via `tcp_init_congestion_ops`), sets up retransmit timers, and initialises the out-of-order queue. At this point the socket is fully usable but unbound — `sk_state = TCP_CLOSE`, no port assigned.

`sock_map_fd()` allocates a `struct file` (with `socket_file_ops` as its `f_op`), installs it into the process's file descriptor table, and returns the fd number. From userspace's perspective, the socket is now just a file descriptor.

---

## 7. SO_REUSEPORT and SO_REUSEADDR

**`SO_REUSEADDR`** has two effects. First, it allows a socket to bind to a port that is still in `TIME_WAIT` state from a previous connection — useful for servers that restart quickly without waiting 60–120 seconds for all TIME_WAIT sockets to expire. Second, multiple sockets can bind the same local address:port if all of them set `SO_REUSEADDR`, but kernel routing still delivers each connection to only one of them (typically the most recently bound).

**`SO_REUSEPORT`** allows N completely independent sockets — even in different processes — to bind the exact same address:port simultaneously. The kernel distributes incoming connections and datagrams across all N sockets using a consistent hash of the 4-tuple (source IP, source port, destination IP, destination port). Each socket gets its own accept queue; no socket holds a lock that blocks the others.

`SO_REUSEPORT` eliminates the single-listener bottleneck. Traditional multi-process servers (nginx, HAProxy) used one listening socket shared across worker processes with `SO_REUSEADDR`, which meant only one worker could call `accept()` at a time (serialised by a kernel lock). With `SO_REUSEPORT`, each worker creates its own socket and binds the same port, allowing fully parallel `accept()` calls with no contention.

In Kubernetes, `SO_REUSEPORT` is used by ingress controllers and service proxies to distribute connections across worker goroutines or processes. For example, Envoy and Nginx ingress both support `SO_REUSEPORT` listener mode, where each worker thread owns a socket. Incoming connections from kube-proxy's DNAT rewrite land directly on the socket owned by the least-busy worker, without any user-space fan-out.

The hash used by `SO_REUSEPORT` is seeded per-socket-group to prevent cross-tenant port inference attacks. When upgrading a server binary (e.g., nginx reload), the new process creates its `SO_REUSEPORT` sockets before the old process closes its sockets, allowing zero-downtime handoff.

---

## 8. Live Observation

```bash
# All TCP connections with state and process:
ss -tnp

# Listening sockets with socket, recv, and send buffer sizes:
ss -tlnpm

# Per-connection TCP stats (RTT, cwnd, retransmits) to a specific host:
ss -tni dst <ip>

# Socket receive/send buffer stats for a process:
cat /proc/$PID/net/sockstat

# bpftrace: trace outbound TCP connection attempts
bpftrace -e 'kprobe:tcp_v4_connect {
    printf("connect: pid=%-6d comm=%-20s\n", pid, comm);
}'

# bpftrace: trace accepted connections (fires after three-way handshake completes)
bpftrace -e 'kretprobe:inet_csk_accept {
    $sk = (struct sock *)retval;
    if ($sk != 0) {
        printf("accept: pid=%-6d comm=%-20s\n", pid, comm);
    }
}'

# bpftrace: trace TCP state transitions
bpftrace -e 'kprobe:tcp_set_state {
    $sk = (struct sock *)arg0;
    $new_state = arg1;
    printf("tcp_state: pid=%d comm=%s newstate=%d\n", pid, comm, $new_state);
}'
```

`ss -tni` reads `/proc/net/tcp` and calls `INET_DIAG` netlink to pull per-socket TCP stats directly from `struct tcp_sock` fields: `srtt_us` becomes the RTT field, `snd_cwnd` becomes `cwnd`, and retransmit counters come from `tcp_sock.total_retrans`. This is the fastest way to inspect live TCP internals without writing any BPF code.

The `tcp_set_state` kprobe fires on every TCP FSM transition. `arg1` maps to the `TCP_*` enum: `1=ESTABLISHED`, `4=FIN_WAIT1`, `6=TIME_WAIT`, `8=CLOSE_WAIT`, `10=LISTEN`. Tracing this probe reveals connection establishment latency, TIME_WAIT accumulation, and unusual state patterns (e.g., connections stuck in SYN_RECV indicating SYN flood or slow backend).

---

## 9. Key References

| Symbol | File | Source |
|--------|------|--------|
| `struct socket` | `include/linux/net.h` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/net.h |
| `struct sock` | `include/net/sock.h` | https://elixir.bootlin.com/linux/v6.9/source/include/net/sock.h |
| `struct tcp_sock` | `include/linux/tcp.h` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/tcp.h |
| `tcp_v4_connect()` | `net/ipv4/tcp_ipv4.c` | https://elixir.bootlin.com/linux/v6.9/source/net/ipv4/tcp_ipv4.c |
| `inet_csk_accept()` | `net/ipv4/inet_connection_sock.c` | https://elixir.bootlin.com/linux/v6.9/source/net/ipv4/inet_connection_sock.c |
| `tcp_sendmsg()` | `net/ipv4/tcp.c` | https://elixir.bootlin.com/linux/v6.9/source/net/ipv4/tcp.c |
| `tcp_v4_rcv()` | `net/ipv4/tcp_ipv4.c` | https://elixir.bootlin.com/linux/v6.9/source/net/ipv4/tcp_ipv4.c |
