# 06-C: Network Namespaces and veth Pairs — Container Network Isolation

## The Problem of Shared Network State

In the original Unix model, the network stack was global. Every process on the machine shared the same interfaces, the same routing table, and the same port namespace. If process A bound to port 80, no other process could bind to port 80. If a process called `sysctl -w net.ipv4.tcp_rmem=...`, it changed the buffer size for every TCP connection on the machine. This made perfect sense in 1971 when a Unix system was a single, trusted, time-sharing machine. It made containers impossible.

The fundamental problem with containers is not resource isolation — cgroups solve that. The problem is that two containers running nginx on port 80 cannot coexist on the same network stack. They would collide in the port binding table. And beyond port collisions, a container should not be able to reach the host's routing table, inspect the host's conntrack entries, or change network-wide sysctl values that affect other tenants.

Network namespaces (added to Linux 2.6.24 in January 2008, the culmination of several years of incremental namespace work) gave each process its own private network stack. Clone with `CLONE_NEWNET` and the new process has its own routing table, its own conntrack table, its own set of network interfaces, its own iptables rules, and its own sysctl namespace for all `net.*` variables. It starts with nothing — not even a loopback interface — and must be explicitly wired up to the outside world.

veth pairs are that wiring. A veth pair is a virtual Ethernet cable: two network interfaces bound together in the kernel such that whatever is sent into one end comes out the other. When the container runtime creates a pod, it creates a veth pair, moves one end into the pod's network namespace (renaming it `eth0`), and leaves the other end on the host. The CNI plugin — Flannel, Calico, Cilium — then connects that host-side veth end to the cluster network, whether through a bridge, BGP routes, or eBPF redirect maps. The pod sees a clean network with just its eth0. The host sees a veth interface for each pod. The kernel routes between them at full software speed.

Network namespaces are the kernel mechanism that gives every pod its own private view of the network stack: its own interfaces, routing table, conntrack table, and sysctl settings. veth pairs are the wires that connect those isolated stacks back to the host.

---

## 1. Source Locations

| File | Purpose | URL |
|------|---------|-----|
| `include/net/net_namespace.h` | `struct net` definition (the network namespace) | https://elixir.bootlin.com/linux/v6.9/source/include/net/net_namespace.h |
| `net/core/net_namespace.c` | Namespace lifecycle: create, copy, destroy | https://elixir.bootlin.com/linux/v6.9/source/net/core/net_namespace.c |
| `drivers/net/veth.c` | veth driver: transmit, newlink, peer pointer | https://elixir.bootlin.com/linux/v6.9/source/drivers/net/veth.c |
| `net/core/rtnetlink.c` | Netlink dispatch for `ip link` operations | https://elixir.bootlin.com/linux/v6.9/source/net/core/rtnetlink.c |

---

## 2. `struct net` — The Network Namespace

Every network namespace is represented by a single `struct net` in the kernel. It is a completely self-contained networking stack — not a filter over a shared stack, but a wholly independent instance.

```c
// include/net/net_namespace.h (key fields)
// https://elixir.bootlin.com/linux/v6.9/source/include/net/net_namespace.h
struct net {
    refcount_t          passive;        // passive reference count (struct net freed when 0)
    spinlock_t          rules_mod_lock;
    atomic_t            dev_unreg_count;
    unsigned int        dev_base_seq;   // incremented on every interface add/remove
    int                 ifindex;        // next interface index to assign

    struct list_head    dev_base_head;  // list of all net_devices in this namespace
    struct hlist_head  *dev_name_head;  // hash by name (for dev_get_by_name)
    struct hlist_head  *dev_index_head; // hash by ifindex (for dev_get_by_index)

    struct ns_common    ns;             // common namespace header — inode for /proc/pid/ns/net
    struct user_namespace *user_ns;     // owning user namespace
    struct ucounts      *ucounts;

    struct proc_dir_entry *proc_net;    // /proc/net entry for this namespace
    struct net_device   *loopback_dev;  // loopback device for this namespace

    struct netns_ipv4   ipv4;           // IPv4: routing table (fib_main), ARP, sysctl knobs
    struct netns_ipv6   ipv6;           // IPv6-specific state
    struct netns_nftables nft;          // nftables tables for this namespace
    struct netns_ct     ct;             // conntrack table for this namespace
    struct list_head    rules_ops;      // routing policy rules
};
```

### Key field explanations

**`ipv4.fib_main` / `ipv4.fib_default`** — Each namespace carries its own routing table. Running `ip route` inside a container reads from this table, completely independent of the host's routing table.

**`ct`** (`struct netns_ct`) — The conntrack table lives inside the namespace. Firewall rules and NAT state are per-namespace; a rule in the host netns has no effect inside a pod.

**`proc_net`** — This is why `/proc/net` shows different content inside a container. The kernel resolves `/proc/net` through the calling process's `struct net`, so each namespace has its own proc subtree populated from its own `struct net`.

**`loopback_dev`** — Every namespace always has a loopback device. It is created automatically but comes up in the `DOWN` state. The CNI plugin is responsible for bringing it up (`ip link set lo up`) during pod setup.

**`ns.inum`** — The inode number exposed at `/proc/<pid>/ns/net`. Two processes sharing the same inode number are in the same network namespace. This is how `ip netns identify` and container runtimes determine namespace membership.

---

## 3. veth Pair Architecture

A `veth` (virtual Ethernet) pair consists of two `net_device` instances bound together by a peer pointer. Traffic transmitted on one end is injected directly as received traffic on the other end — no copying, no kernel socket, just a pointer hand-off.

```
Pod network namespace              Host network namespace
┌─────────────────────┐           ┌──────────────────────────────────┐
│  eth0 (veth end A)  │──────────▶│  vethXXXXXX (veth end B)        │
│  IP: 10.244.1.5/24  │◀──────────│  (no IP — bridge member)        │
└─────────────────────┘           │                                  │
                                  │  cni0 bridge                     │
                                  │  IP: 10.244.1.1/24               │
                                  └──────────────────────────────────┘
```

Each end is a normal `net_device`. The critical difference is the `nd_net` pointer: `eth0` inside the pod has `nd_net` pointing to the pod's `struct net`; `vethXXXXXX` on the host has `nd_net` pointing to the host's `struct net`. They are in different namespaces but physically the same pair.

### Transmit path in the kernel

```c
// drivers/net/veth.c — simplified
// https://elixir.bootlin.com/linux/v6.9/source/drivers/net/veth.c
static netdev_tx_t veth_xmit(struct sk_buff *skb, struct net_device *dev)
{
    struct net_device *rcv = veth_peer_dev(dev);  // peer device in the other netns

    // Hand the skb directly to the peer's receive function — no copy:
    netif_receive_skb(skb);  // runs in the peer's net namespace context
}
```

`veth_peer_dev()` returns the peer `net_device` via `struct veth_priv`. `netif_receive_skb()` then processes the `sk_buff` as if it had just been received on the peer device — the kernel protocol stack (IP layer, conntrack, iptables) of the receiving namespace then handles it. The namespace switch is implicit because the peer's `nd_net` points to a different `struct net`.

---

## 4. Container Network Setup — CNI Flow

When a pod is created, the kubelet calls the CNI plugin via exec, passing the pod's network namespace path. The CNI plugin performs the following steps:

**Step 1 — Create the network namespace**

```bash
clone(CLONE_NEWNET)   # or: unshare(CLONE_NEWNET)
```

The kernel calls `copy_net_ns()` (https://elixir.bootlin.com/linux/v6.9/source/net/core/net_namespace.c) which allocates a fresh `struct net`, initialises all subsystems (`setup_net()`), and creates the loopback device. The namespace is down and empty.

**Step 2 — Create a veth pair**

```bash
ip link add veth0 type veth peer name veth1
```

Kernel path: `rtnetlink` → `rtnl_newlink()` (https://elixir.bootlin.com/linux/v6.9/source/net/core/rtnetlink.c) → `veth_newlink()` (https://elixir.bootlin.com/linux/v6.9/source/drivers/net/veth.c). This allocates two `net_device` structs, fills their `struct veth_priv` peer pointers, and registers both in the current namespace.

**Step 3 — Move one end into the pod namespace**

```bash
ip link set veth0 netns <fd>
```

Kernel path: `dev_change_net_namespace()` (https://elixir.bootlin.com/linux/v6.9/source/net/core/dev.c). This unregisters the device from the host namespace's `dev_base_head` list, updates `nd_net` to point to the pod's `struct net`, and re-registers it in the pod namespace. The peer pointer in `struct veth_priv` is unchanged.

**Step 4 — Configure IP in pod namespace**

```bash
ip addr add 10.244.1.5/24 dev veth0
```

This is a standard `rtnetlink` call (`RTM_NEWADDR`) processed in the context of the pod's network namespace, updating `ipv4.fib_main` with the local route.

**Step 5 — Attach the host end to a bridge (or set up routes)**

```bash
ip link set veth1 master cni0
```

Bridges the host-side veth into the CNI bridge device. Traffic destined for the pod's subnet is now forwarded through `cni0`.

**Step 6 — Set default route in pod namespace**

```bash
ip route add default via 10.244.1.1
```

Adds `0.0.0.0/0` to the pod namespace's `ipv4.fib_main` pointing at the bridge IP. All outbound traffic from the pod is now routed through this gateway.

---

## 5. `/proc/<pid>/net` Contents

Every process's `/proc/<pid>/net/` resolves through that process's network namespace. Two processes with the same `/proc/<pid>/ns/net` inode are in the same namespace and see identical content here.

| File | Content |
|------|---------|
| `dev` | Per-interface rx/tx packet and byte counters (equivalent to `ip -s link`) |
| `tcp` | All TCP sockets: state, local/remote address, port, socket inode, uid |
| `tcp6` | IPv6 TCP sockets |
| `udp` | UDP sockets |
| `fib_trie` | Full routing table in human-readable trie format |
| `arp` | ARP cache entries |
| `if_inet6` | IPv6 interface addresses |
| `netstat` | Aggregated socket statistics across all protocols |

Because `/proc/<pid>/net` resolves to the namespace's own `proc_net` directory (`struct net.proc_net`), running `cat /proc/1/net/dev` inside a container shows only the container's interfaces. Running the same command on the host shows only the host's interfaces.

---

## 6. Network Namespace Lifecycle

**Creation** — `clone(CLONE_NEWNET)` or `unshare(CLONE_NEWNET)` triggers `copy_net_ns()`:
- Allocates a new `struct net`
- Calls `setup_net()` which runs every registered `pernet_operations->init` callback
- Creates the loopback device (`lo`) in the `DOWN` state
- Initialises an empty routing table, empty conntrack table, and fresh sysctl tree (values inherited from the creator at clone time)

**Identification** — The namespace inode (`ns.inum`) is exposed at `/proc/<pid>/ns/net`. Bind-mounting this path (`mount --bind /proc/<pid>/ns/net /run/netns/mypod`) keeps the namespace alive even after all processes inside it exit.

**Reference counting** — `struct net` is kept alive by its `passive` refcount. References are held by:
- Each process with that namespace as its active netns
- Open file descriptors to `/proc/<pid>/ns/net`
- Bind mounts of the namespace file

**Destruction** — When `passive` drops to zero, `net_drop_ns()` is called. Every `pernet_operations->exit` and `->exit_batch` callback is invoked (destroying routing tables, conntrack, nftables, etc.), then `free_net()` releases the `struct net`.

---

## 7. Live Observation

```bash
# List all named network namespaces (those bind-mounted into /run/netns/):
ip netns list

# Enter a container's netns by PID and list its interfaces:
CPID=$(crictl inspect <container-id> | jq '.info.pid')
nsenter -t $CPID -n ip link

# Show TCP sockets visible inside the container's netns:
nsenter -t $CPID -n ss -tnp

# Show all veth devices on the host and their peer indexes:
ip link show type veth

# Show interfaces in a named netns:
ip -n <nsname> link
```

```bash
# bpftrace: trace veth transmit — packets crossing the namespace boundary
bpftrace -e '
kprobe:veth_xmit {
    $skb = (struct sk_buff *)arg0;
    printf("veth_xmit: pid=%d comm=%s len=%u\n", pid, comm, $skb->len);
}'

# bpftrace: trace network namespace creation
bpftrace -e '
kprobe:copy_net_ns {
    printf("new netns: pid=%d comm=%s\n", pid, comm);
}'
```

The `veth_xmit` probe fires every time a packet crosses a namespace boundary through a veth pair. `copy_net_ns` fires whenever a new network namespace is created (container start, `unshare -n`, `ip netns add`).

---

## 8. Key References

| Symbol | File | URL |
|--------|------|-----|
| `struct net` | `include/net/net_namespace.h` | https://elixir.bootlin.com/linux/v6.9/source/include/net/net_namespace.h |
| `copy_net_ns` | `net/core/net_namespace.c` | https://elixir.bootlin.com/linux/v6.9/source/net/core/net_namespace.c |
| `dev_change_net_namespace` | `net/core/dev.c` | https://elixir.bootlin.com/linux/v6.9/source/net/core/dev.c |
| `veth_xmit` | `drivers/net/veth.c` | https://elixir.bootlin.com/linux/v6.9/source/drivers/net/veth.c |
| `rtnl_newlink` | `net/core/rtnetlink.c` | https://elixir.bootlin.com/linux/v6.9/source/net/core/rtnetlink.c |
