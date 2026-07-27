# Setup 03 — Adding RPi5 #2: Two-Node Cluster

## The Point of Two Nodes

A single-node Kubernetes cluster is a developer sandbox. Pod scheduling, node affinity, taints and tolerations — all the placement machinery runs but has only one place to put things. `kubectl drain` works, but there is nowhere for the drained pods to go. Node failure means the entire cluster is down.

A two-node cluster crosses a threshold: pod scheduling becomes real. When you drain rpi5-01, the pods actually move to rpi5-02. When rpi5-02 is unreachable, the scheduler cannot place pods and they stay Pending. Inter-node network traffic flows over real Ethernet — Flannel encapsulates packets in VXLAN, they traverse a Gigabit switch, the other node decapsulates them. The conntrack table on each node grows with inter-node connections. The eBPF programs running on each node see only local traffic.

This is the environment where the concepts from every chapter become simultaneously real and verifiable with hardware you own and can reset.

## Prerequisites

- RPi5 #1 running Ubuntu 24.04 + k3s server (from setup/02-k3s-single.md)
- RPi5 #2 flashed with Ubuntu 24.04, setup from setup/01-os-and-kernel.md complete
- Both RPi5s on the same network (same switch/router), with static IPs

## 1. Prepare RPi5 #2

Follow `setup/01-os-and-kernel.md` on the second RPi5 with these differences:

```bash
# On RPi5 #2:
# Set hostname to rpi5-02 (in /etc/hostname and cloud-init user-data)
sudo hostnamectl set-hostname rpi5-02

# Set static IP different from RPi5 #1
# /etc/netplan/01-static.yaml — use 192.168.1.102 (or whatever your scheme uses)
export RPi5_02_IP=192.168.1.102

# Verify connectivity to RPi5 #1
ping -c 3 192.168.1.101
```

## 2. Join RPi5 #2 as a k3s Agent

```bash
# On RPi5 #2: join the cluster as an agent node
# Replace with your RPi5 #1 IP and token from setup/02-k3s-single.md
export K3S_URL="https://192.168.1.101:6443"
export K3S_TOKEN="K10...::server:..."   # from: sudo cat /var/lib/rancher/k3s/server/node-token on rpi5-01

export NODE_IP=192.168.1.102

curl -sfL https://get.k3s.io | INSTALL_K3S_EXEC="agent \
  --server=${K3S_URL} \
  --token=${K3S_TOKEN} \
  --node-ip=${NODE_IP}" sh -
```

## 3. Verify Two-Node Cluster

```bash
# On RPi5 #1 (control plane):
kubectl get nodes
# NAME       STATUS   ROLES                  AGE    VERSION
# rpi5-01    Ready    control-plane,master   12m    v1.29.x+k3s1
# rpi5-02    Ready    <none>                 45s    v1.29.x+k3s1

# Both nodes should be Ready within 1-2 minutes
# If rpi5-02 stays NotReady, check:
kubectl describe node rpi5-02
# Look for: "kubelet is not ready" or cgroup-related messages

# Verify architecture on both nodes
kubectl get nodes -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.status.nodeInfo.architecture}{"\n"}{end}'
# rpi5-01    arm64
# rpi5-02    arm64
```

## 4. Test Inter-Node Pod Scheduling

```bash
# Deploy nginx with 2 replicas — should spread across both nodes
kubectl create deployment nginx --image=nginx:alpine --replicas=2
kubectl get pods -o wide
# NAME          READY  STATUS   NODE
# nginx-xxx-1   1/1    Running  rpi5-01
# nginx-xxx-2   1/1    Running  rpi5-02

# If both land on the same node, the scheduler had a reason
# Check for taints or affinity rules:
kubectl describe pod nginx-xxx-1 | grep -E "Node:|Tolerations:"

# Force spread with pod anti-affinity:
kubectl patch deployment nginx -p '{
  "spec": {
    "template": {
      "spec": {
        "affinity": {
          "podAntiAffinity": {
            "preferredDuringSchedulingIgnoredDuringExecution": [{
              "weight": 100,
              "podAffinityTerm": {
                "labelSelector": {"matchLabels": {"app": "nginx"}},
                "topologyKey": "kubernetes.io/hostname"
              }
            }]
          }
        }
      }
    }
  }
}'
```

## 5. Verify Inter-Node Networking (Flannel VXLAN)

Flannel uses VXLAN to encapsulate pod-to-pod traffic across nodes. Each pod gets a routable IP from a per-node CIDR. The VXLAN tunnel encapsulates pod traffic in UDP packets sent between node IPs.

```bash
# Check Flannel VXLAN device on each node
ip link show flannel.1   # VXLAN interface
ip -d link show flannel.1 | grep -i vxlan
# → vxlan id 1 remote 0.0.0.0 local 192.168.1.101 dev eth0 srcport 0 0 dstport 8472

# Pod CIDRs (each node gets a /24 or /16 slice):
kubectl get nodes -o jsonpath='{range .items[*]}{.metadata.name}{": "}{.spec.podCIDR}{"\n"}{end}'
# rpi5-01: 10.42.0.0/24
# rpi5-02: 10.42.1.0/24

# Test pod-to-pod communication across nodes:
# Get a pod IP on rpi5-02
POD2_IP=$(kubectl get pod -l app=nginx -o jsonpath='{.items[?(@.spec.nodeName=="rpi5-02")].status.podIP}')

# Exec into the pod on rpi5-01 and ping the pod on rpi5-02
POD1=$(kubectl get pod -l app=nginx -o jsonpath='{.items[?(@.spec.nodeName=="rpi5-01")].metadata.name}')
kubectl exec ${POD1} -- ping -c 3 ${POD2_IP}
# PING 10.42.1.xxx: 3 packets transmitted, 3 received

# Capture the VXLAN encapsulation on the host:
sudo tcpdump -i eth0 -n 'udp port 8472' &
kubectl exec ${POD1} -- ping -c 1 ${POD2_IP}
# You should see: 192.168.1.101 → 192.168.1.102: VXLAN, VNI 1
kill %1
```

## 6. Test Node Drain — Real Workload Migration

```bash
# Create a stateless workload with 2 replicas spread across nodes
kubectl create deployment test-drain --image=nginx:alpine --replicas=2

# Verify distribution
kubectl get pods -o wide -l app=test-drain

# Drain rpi5-02 (evict all pods, mark as unschedulable)
kubectl drain rpi5-02 --ignore-daemonsets --delete-emptydir-data

# Watch pod migration to rpi5-01
kubectl get pods -o wide -l app=test-drain --watch
# Both replicas should move to rpi5-01 within 30-60 seconds

# Verify rpi5-02 is cordoned (unschedulable)
kubectl get nodes
# NAME       STATUS                     ROLES    AGE
# rpi5-01    Ready                      master   30m
# rpi5-02    Ready,SchedulingDisabled   <none>   15m

# Bring rpi5-02 back online
kubectl uncordon rpi5-02

# New pods will start scheduling on rpi5-02 again:
kubectl scale deployment test-drain --replicas=4
kubectl get pods -o wide -l app=test-drain
```

## 7. Verify kube-inspect on Both Nodes

```bash
# Build kube-inspect ARM64 binary on rpi5-01
cd /path/to/k8s-deep-dive/kube-inspect
go build -o kube-inspect ./cmd/kube-inspect/

# Copy to rpi5-02
scp kube-inspect ubuntu@192.168.1.102:~/

# Run --network on both nodes and compare
sudo ./kube-inspect --network
# On rpi5-01: shows flannel.1, cni0 (pod bridge), eth0
# On rpi5-02: same — each node has its own full network stack

# Run --arch on both (should show identical ARM64 info)
sudo ./kube-inspect --arch

# Run --scheduler on each to see per-node CFS state
sudo ./kube-inspect --scheduler
```

## 8. Observe Conntrack Across Nodes

```bash
# On rpi5-01: watch conntrack entries grow as cross-node traffic flows
watch -n 1 'sudo conntrack -C'
# Count should increase as inter-node pod traffic flows

# List established inter-node connections
sudo conntrack -L -p tcp --state ESTABLISHED 2>/dev/null | grep "10\.42\."
# Shows TCP connections between pod IPs on different nodes

# On rpi5-02: the VXLAN tunnel shows up as regular UDP in conntrack
sudo conntrack -L -p udp 2>/dev/null | grep 8472
# Shows VXLAN tunnel flows: src=192.168.1.101 dst=192.168.1.102 sport=... dport=8472
```

Your two-node cluster is operational. For hardware shopping and NVMe configuration, see `setup/04-hardware.md`. For ARM64 kernel concepts specific to this hardware, see the `kernel/` directory.
