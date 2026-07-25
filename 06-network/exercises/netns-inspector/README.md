# netns-inspector

A Go tool that reads `/proc/<pid>/net/dev` to display per-interface receive and transmit statistics for any Linux process — and therefore for any container. With `--watch`, it refreshes every two seconds showing per-second delta rates, making it easy to observe live traffic flowing through a container's network namespace.

## What It Demonstrates

- **Parsing `/proc/<pid>/net/dev`** line by line using the 16-field kernel counter format
- **`NetInterface` struct fields**: Name, RxBytes, RxPackets, RxErrors, RxDropped, TxBytes, TxPackets, TxErrors, TxDropped
- **Network namespaces**: every process (and every container) has its own `/proc/<pid>/net/dev` view, showing only the interfaces visible in that process's network namespace
- **Delta rate calculation**: subtracting two successive counter snapshots and dividing by elapsed time gives bytes/s and packets/s without requiring kernel-level monitoring tools
- **Container network observability**: the same counters exposed here are what cAdvisor reads to produce the `container_network_receive_bytes_total` and `container_network_transmit_bytes_total` Prometheus metrics scraped by Kubernetes

## Build and Run

```bash
# Build the binary
make build

# Show interface stats for the current process
make run

# Show interface stats for a specific PID
make run-pid PID=1

# Show interface stats for PID 1234
./netns-inspector --pid 1234

# Watch delta rates every 2 seconds for a container process
./netns-inspector --pid 1234 --watch

# Show help
./netns-inspector --help
```

## /proc/pid/net/dev Format

Each data line of `/proc/<pid>/net/dev` contains the interface name followed by 16 unsigned 64-bit counter values — 8 for the receive direction and 8 for the transmit direction:

```
<iface>: rx_bytes rx_packets rx_errs rx_drop rx_fifo rx_frame rx_comp rx_multi tx_bytes tx_packets tx_errs tx_drop tx_fifo tx_colls tx_carr tx_comp
```

| Field | Direction | Meaning |
|---|---|---|
| `rx_bytes` | RX | Total bytes received on the interface |
| `rx_packets` | RX | Total packets received |
| `rx_errs` | RX | Receive errors (bad FCS, truncated, etc.) |
| `rx_drop` | RX | Packets dropped in the receive path (ring buffer overflow, etc.) |
| `rx_fifo` | RX | FIFO buffer errors |
| `rx_frame` | RX | Frame alignment errors |
| `rx_comp` | RX | Compressed packets received (SLIP/PPP) |
| `rx_multi` | RX | Multicast packets received |
| `tx_bytes` | TX | Total bytes transmitted |
| `tx_packets` | TX | Total packets transmitted |
| `tx_errs` | TX | Transmit errors |
| `tx_drop` | TX | Packets dropped in the transmit path |
| `tx_fifo` | TX | FIFO buffer errors on transmit |
| `tx_colls` | TX | Collision count (Ethernet only) |
| `tx_carr` | TX | Carrier sense errors |
| `tx_comp` | TX | Compressed packets transmitted (SLIP/PPP) |

The file always begins with two header lines that are skipped during parsing:

```
Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
```

The kernel emits these counters from `dev_seq_printf_stats()` defined in:
https://elixir.bootlin.com/linux/v6.9/source/net/core/net-procfs.c

The underlying per-cpu counters are accumulated in `struct net_device_stats` and `struct rtnl_link_stats64` defined in:
https://elixir.bootlin.com/linux/v6.9/source/include/uapi/linux/if_link.h

## Expected Output

For a container process with `eth0` (the veth end inside the pod) and `lo`:

```
Network interfaces for PID 4821:
  IFACE          RX-BYTES    RX-PKTS   RX-ERR  RX-DROP      TX-BYTES    TX-PKTS   TX-ERR  TX-DROP
  lo                    0          0        0        0             0          0        0        0
  eth0            1048576       1024        0        0        524288        512        0        0
```

## --watch Output Example

With `--watch`, the tool first prints the absolute snapshot and then prints per-second delta rates every two seconds:

```
Network interfaces for PID 4821:
  IFACE          RX-BYTES    RX-PKTS   RX-ERR  RX-DROP      TX-BYTES    TX-PKTS   TX-ERR  TX-DROP
  lo                    0          0        0        0             0          0        0        0
  eth0            1048576       1024        0        0        524288        512        0        0

Network interface rates for PID 4821 (per second, interval 2.0s):
  IFACE            RX-BYTES/s    RX-PKTS/s     TX-BYTES/s    TX-PKTS/s
  lo                      0.0          0.0            0.0          0.0
  eth0                 8192.0          8.0         4096.0          4.0

Network interface rates for PID 4821 (per second, interval 2.0s):
  IFACE            RX-BYTES/s    RX-PKTS/s     TX-BYTES/s    TX-PKTS/s
  lo                      0.0          0.0            0.0          0.0
  eth0                16384.0         16.0         8192.0          8.0
```

The RX-BYTES/s and TX-BYTES/s columns show the actual byte throughput visible inside the container's network namespace.

## Exercises

**(a) Compare rx/tx bytes between a pod and its host veth peer:**

```bash
# Find the container's PID
PID=$(crictl inspect <container-id> | jq .info.pid)

# Show the container's interface counters (eth0 inside the pod)
sudo ./netns-inspector --pid $PID

# Find the matching veth peer on the host side
ip link show | grep veth

# Show the host network namespace interface counters for the matching vethXXX
./netns-inspector --pid 1
```

The rx_bytes on `eth0` inside the pod should equal tx_bytes on the matching `vethXXX` on the host, and vice versa, because they are two ends of the same virtual Ethernet cable.

**(b) Watch bytes climb during a `curl` inside a container:**

```bash
# Terminal 1: watch the container's network stats
PID=$(crictl inspect <container-id> | jq .info.pid)
sudo ./netns-inspector --pid $PID --watch

# Terminal 2: run curl inside the container and transfer a large response
kubectl exec -it <pod-name> -- curl -o /dev/null https://releases.ubuntu.com/22.04/ubuntu-22.04-desktop-amd64.iso
```

You should see RX-BYTES/s spike on `eth0` in Terminal 1 while the download is in progress.

**(c) Add --json flag:**

Add a `--json` flag that marshals the `[]NetInterface` slice to JSON so the output is pipeable to `jq`:

```bash
./netns-inspector --pid 1 --json | jq '[.[] | select(.Name == "eth0") | {name: .Name, rx_mb: (.RxBytes / 1048576 | floor)}]'
```

## Kernel References

| Symbol | URL |
|---|---|
| `dev_seq_printf_stats()` (emits /proc/pid/net/dev lines) | https://elixir.bootlin.com/linux/v6.9/source/net/core/net-procfs.c |
| `struct rtnl_link_stats64` (64-bit interface counters) | https://elixir.bootlin.com/linux/v6.9/source/include/uapi/linux/if_link.h |
| `struct net_device` (kernel network device object) | https://elixir.bootlin.com/linux/v6.9/source/include/linux/netdevice.h |
| `struct net` (network namespace object) | https://elixir.bootlin.com/linux/v6.9/source/include/net/net_namespace.h |
| `net/core/dev.c` (device registration and packet Rx/Tx paths) | https://elixir.bootlin.com/linux/v6.9/source/net/core/dev.c |
| `drivers/net/veth.c` (virtual Ethernet pair driver) | https://elixir.bootlin.com/linux/v6.9/source/drivers/net/veth.c |

## Kubernetes Connection

The kubelet reads per-container network statistics from cAdvisor, which uses `/proc/<pid>/net/dev` to obtain interface counters for the container's init process PID. These counters are exposed as Prometheus metrics:

- `container_network_receive_bytes_total` — maps directly to `rx_bytes` in `/proc/<pid>/net/dev`
- `container_network_transmit_bytes_total` — maps directly to `tx_bytes` in `/proc/<pid>/net/dev`
- `container_network_receive_packets_total` — maps to `rx_packets`
- `container_network_transmit_packets_total` — maps to `tx_packets`
- `container_network_receive_errors_total` — maps to `rx_errs`
- `container_network_transmit_errors_total` — maps to `tx_errs`

These metrics are scraped by the Kubernetes metrics pipeline and are the basis for network-related HPA (Horizontal Pod Autoscaler) external metrics, network policy enforcement dashboards, and bandwidth billing in multi-tenant clusters.

cAdvisor source that reads /proc/pid/net/dev:
https://github.com/google/cadvisor/blob/master/container/libcontainer/handler.go
