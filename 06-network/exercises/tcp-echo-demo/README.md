# tcp-echo-demo

TCP socket lifecycle: socket/bind/listen/accept/connect — Chapter 06: Networking.

---

## What It Demonstrates

- **`socket(AF_INET, SOCK_STREAM, 0)`**: allocates a socket file descriptor backed by a kernel `struct socket` and `struct sock`. The protocol family (`AF_INET`) and type (`SOCK_STREAM`) select TCP/IP.

- **`setsockopt(SO_REUSEADDR)`**: allows the server to rebind the same address immediately after restart, bypassing the `TIME_WAIT` delay that would otherwise block the port for ~60 seconds.

- **`bind(INADDR_LOOPBACK, 0)`**: attaches the socket to the loopback interface. Port `0` tells the kernel to pick a free ephemeral port from the local port range (`/proc/sys/net/ipv4/ip_local_port_range`).

- **`getsockname()`**: retrieves the address actually assigned by the kernel after `bind()`, revealing the chosen port number.

- **`listen(backlog=5)`**: moves the socket into the `TCP_LISTEN` state and creates two queues: the SYN queue (incomplete connections) and the accept queue (fully established connections ready for `accept()`).

- **`accept()`**: dequeues the next completed connection from the accept queue and returns a new file descriptor for that connection. The original listening fd remains open for further connections.

- **`connect()`**: on the client side, triggers the TCP three-way handshake — SYN, SYN-ACK, ACK. The kernel assigns an ephemeral local port automatically.

The program forks after `listen()`. The child runs the server (`accept` → echo loop); the parent runs the client (five sends + receives). Both sides print their fd numbers, addresses, and each operation so you can follow the kernel path step by step.

---

## Build and Run

```
make run
```

Build only:

```
make
```

Remove the binary:

```
make clean
```

---

## Expected Output

The server and client lines interleave. Exact port numbers will differ each run.

```
[main]   socket() → listen_fd=3  (AF_INET, SOCK_STREAM)
[main]   setsockopt(SO_REUSEADDR) set
[main]   bind() → 127.0.0.1:54321  (port assigned by kernel)
[main]   listen(backlog=5) — socket is now LISTEN state
[server] listen_fd=3  waiting for accept()
[client] socket_fd=4
[client] connect() → 127.0.0.1:54321
[client] local address after connect: 127.0.0.1:49812
[server] accept() returned conn_fd=4  peer=127.0.0.1:49812
[client] send: "Hello 1"
[server] read 7 bytes: "Hello 1"
[server] echoed 7 bytes back to client
[client] echo received: "Hello 1"
[client] send: "Hello 2"
[server] read 7 bytes: "Hello 2"
[server] echoed 7 bytes back to client
[client] echo received: "Hello 2"
[client] send: "Hello 3"
[server] read 7 bytes: "Hello 3"
[server] echoed 7 bytes back to client
[client] echo received: "Hello 3"
[client] send: "Hello 4"
[server] read 7 bytes: "Hello 4"
[server] echoed 7 bytes back to client
[client] echo received: "Hello 4"
[client] send: "Hello 5"
[server] read 7 bytes: "Hello 5"
[server] echoed 7 bytes back to client
[client] echo received: "Hello 5"
[client] closing socket_fd=4
[client] done
[server] client closed connection — closing conn_fd=4
[server] done
[main]   server child exited with status 0
```

---

## Kernel Path

**`socket(2)` → `inet_create()`**

`socket(2)` enters the kernel via `sys_socket()`, which calls `sock_create()` and ultimately `inet_create()` for the `AF_INET` family. `inet_create()` allocates a `struct sock`, sets `sk->sk_protocol = IPPROTO_TCP`, and attaches the TCP operations vector.

https://elixir.bootlin.com/linux/v6.9/source/net/ipv4/af_inet.c

**`listen(2)` → `inet_listen()`**

`listen(2)` reaches `inet_listen()`, which calls `inet_csk_listen_start()` to allocate the accept queue and transition the socket to `TCP_LISTEN` state. The backlog parameter bounds the accept queue depth.

https://elixir.bootlin.com/linux/v6.9/source/net/ipv4/af_inet.c

**`accept(2)` → `inet_accept()` → `inet_csk_accept()`**

`accept(2)` calls `inet_accept()`, which calls `inet_csk_accept()` in `inet_connection_sock.c`. If no connection is ready, the calling process sleeps on the socket's wait queue. When the TCP stack completes a three-way handshake it wakes the waiter and `inet_csk_accept()` dequeues the new `struct sock` from the accept queue.

https://elixir.bootlin.com/linux/v6.9/source/net/ipv4/inet_connection_sock.c

---

## Kubernetes Connection

**kubelet ↔ containerd (CRI)**

The kubelet manages containers by talking to a CRI runtime (typically containerd) over a Unix-domain socket, but for remote runtimes and cluster-internal health checks the same `socket()` → `connect()` path applies. Every gRPC call from the kubelet to containerd is a TCP stream established with the exact sequence demonstrated here.

**Readiness and liveness probes**

When a Pod spec defines a TCP socket probe (`tcpSocket: { port: 8080 }`), the kubelet executes a `connect(2)` to the target port on every probe interval. A successful three-way handshake means the probe passes; `ECONNREFUSED` or a timeout causes the probe to fail and the Pod to be marked unready or restarted. This is `connect(2)` used purely as a reachability test — no data is sent.

**kube-apiserver traffic**

Every kubectl command, controller-manager reconcile loop, and scheduler decision is carried over a persistent TLS TCP connection to the kube-apiserver. That connection begins with `socket()` + `connect()` on the client side and `accept()` on the server side, exactly as in this demo. Kubernetes relies on `SO_KEEPALIVE` (and `TCP_KEEPIDLE` / `TCP_KEEPINTVL`) on those long-lived connections so that stale connections are detected and the client can reconnect before a reconcile loop stalls.

---

## Exercises

**(a)** Add a `sleep(30)` inside the server's echo loop (after `accept()` but before reading). Run the demo in one terminal and in another run:

```
ss -tnp
```

Observe the connection in `ESTABLISHED` state with the socket fd listed under the server process. This is what `ss` shows for every live kubelet-to-apiserver connection in a real cluster.

**(b)** Add `SO_KEEPALIVE` and `TCP_KEEPIDLE` to the server socket after `setsockopt(SO_REUSEADDR)`:

```c
int keep = 1;
int idle = 5; /* seconds before first keepalive probe */
setsockopt(listen_fd, SOL_SOCKET,  SO_KEEPALIVE, &keep, sizeof(keep));
setsockopt(listen_fd, IPPROTO_TCP, TCP_KEEPIDLE, &idle, sizeof(idle));
```

Kubernetes sets `TCP_KEEPIDLE` on connections to the API server so that a silent network partition (no RST, no FIN) is detected within a bounded time rather than hanging forever.

**(c)** Stop the server (or simply do not start it) and try to `connect()` to its port. Observe the `ECONNREFUSED` error — the kernel sends back a TCP RST immediately because nothing is listening on that port. Add error handling that prints `strerror(errno)` after a failed `connect()` to see this in user space.
