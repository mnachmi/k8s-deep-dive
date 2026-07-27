# Setup 02 — k3s Single-Node Cluster on RPi5

## Why k3s, Not kubeadm

kubeadm installs the full Kubernetes control plane: etcd (separate binary), kube-apiserver, kube-controller-manager, kube-scheduler, kubelet — each a separate process. On a Raspberry Pi 5 with 8GB RAM, the control plane itself consumes ~2GB before any workloads run. The Kubernetes minimum node requirement is 2GB RAM; the RPi5 with 4GB is borderline for kubeadm.

k3s (Rancher, 2019) packages all control plane components into a single binary (~70MB). It replaces etcd with SQLite by default (switching to etcd only when adding nodes requires HA). Memory overhead for the full control plane on k3s: ~512MB. This leaves ~7GB on an 8GB RPi5 for workloads and the kernel's slab/page cache.

The trade-off: k3s makes opinionated choices (Flannel CNI, Traefik ingress, local-path storage by default) that differ from production cluster defaults. These choices can be overridden. For learning kernel-Kubernetes interactions, k3s is identical to full Kubernetes — it uses the same kubelet, the same CRI (containerd), the same cgroup v2 integration, the same eBPF hooks.

## 1. Install k3s

```bash
# Single-line installer (fetches from GitHub releases, current stable)
# IMPORTANT: Specify your RPi5's static IP so the TLS cert covers it
export NODE_IP=192.168.1.101  # replace with your RPi5's static IP

curl -sfL https://get.k3s.io | INSTALL_K3S_EXEC="server \
  --node-ip=${NODE_IP} \
  --advertise-address=${NODE_IP} \
  --write-kubeconfig-mode=644 \
  --disable=traefik" sh -

# --disable=traefik: we don't need the ingress controller for these exercises
# --write-kubeconfig-mode=644: lets non-root users run kubectl without sudo
```

## 2. Verify Installation

```bash
# k3s installs kubectl as k3s kubectl — set up an alias or symlink
sudo ln -sf /usr/local/bin/k3s /usr/local/bin/kubectl
# Or export KUBECONFIG:
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml

# Wait for the node to become Ready (30-60 seconds)
kubectl get nodes --watch
# NAME       STATUS   ROLES                  AGE   VERSION
# rpi5-01    Ready    control-plane,master   47s   v1.29.x+k3s1

# Check all system pods are Running
kubectl get pods -n kube-system
# NAME                                    READY   STATUS    RESTARTS   AGE
# coredns-xxx                             1/1     Running   0          2m
# local-path-provisioner-xxx              1/1     Running   0          2m
# metrics-server-xxx                      1/1     Running   0          2m

# Verify Kubernetes version and architecture
kubectl version
# Server Version: v1.29.x+k3s1
# Platform: linux/arm64  ← ARM64 confirmed

# Check node info
kubectl describe node rpi5-01 | grep -E "Arch|OS|Runtime|Capacity|Allocatable"
# Architecture:          arm64
# Operating System:      linux
# Container Runtime Version:  containerd://1.7.x
```

## 3. Verify kube-inspect Works on ARM64

```bash
# Clone the repo if not already present
git clone https://github.com/mnachmi/k8s-deep-dive.git
cd k8s-deep-dive/kube-inspect

# Build for ARM64 (should work natively on the RPi5)
go build -o kube-inspect ./cmd/kube-inspect/

# Verify binary is ARM64
file kube-inspect
# → ELF 64-bit LSB executable, ARM aarch64

# Run all flags from earlier chapters
sudo ./kube-inspect --namespaces     # ch02: namespace inspection
sudo ./kube-inspect --cgroups        # ch03: cgroup hierarchy
sudo ./kube-inspect --memory         # ch04: memory stats
sudo ./kube-inspect --vfs            # ch05: VFS and mounts
sudo ./kube-inspect --network        # ch06: network namespaces
sudo ./kube-inspect --ebpf           # ch07: eBPF programs
sudo ./kube-inspect --scheduler      # ch08: CFS and runqueue
sudo ./kube-inspect --k8s            # ch09: Kubernetes internals
sudo ./kube-inspect --perf           # ch10: performance metrics
sudo ./kube-inspect --health         # ch11: node health
sudo ./kube-inspect --arch           # ch13: ARM64-specific info (NEW)
```

## 4. Deploy a Test Workload

```bash
# Create a simple nginx deployment to verify scheduling and networking
kubectl create deployment nginx --image=nginx:alpine --replicas=1
kubectl expose deployment nginx --port=80 --type=NodePort

# Wait for pod to be Running
kubectl get pods --watch

# Get the NodePort
NODE_PORT=$(kubectl get svc nginx -o jsonpath='{.spec.ports[0].nodePort}')
curl http://192.168.1.101:${NODE_PORT}
# Should return nginx welcome page

# Verify cgroup v2 integration for this pod
POD_ID=$(kubectl get pod -l app=nginx -o jsonpath='{.items[0].metadata.uid}')
ls /sys/fs/cgroup/kubepods.slice/
# Should show the pod's cgroup slice

# Verify namespace isolation
POD_PID=$(kubectl get pod -l app=nginx -o jsonpath='{.items[0].status.containerStatuses[0].containerID}' | \
  sed 's|containerd://||')
# Find the actual process
ps aux | grep nginx | grep -v grep
```

## 5. Verify eBPF Works on RPi5

```bash
# Basic bpftrace verification (must work for ch07 exercises)
sudo bpftrace -e 'tracepoint:syscalls:sys_enter_read { @[comm] = count(); } interval:s:5 { print(@); exit(); }'

# Verify kprobe on ARM64
sudo bpftrace -e 'kprobe:vfs_read { @[comm] = count(); } interval:s:5 { print(@); exit(); }'

# Verify uprobe (user-space probe) — ARM64 uprobe uses hardware breakpoints
sudo bpftrace -e 'uprobe:/bin/bash:readline { printf("bash readline called\n"); }' &
bash -c 'echo test'
kill %1

# Verify BPF maps work
sudo bpftrace -e 'BEGIN { @[1] = 1; @[2] = 2; print(@); exit(); }'
# Should show a map with two entries
```

## 6. Check cgroup v2 Integration

```bash
# k3s uses cgroup v2 — verify the kubelet is using cgroup v2 drivers
cat /var/lib/rancher/k3s/agent/etc/containerd/config.toml | grep -i cgroup
# → SystemdCgroup = true

# Verify CPU throttle metrics are accessible (ch08 content)
# Find the nginx pod's cgroup
CGROUP_PATH=$(find /sys/fs/cgroup -name "cpu.stat" | grep kubepods | head -1)
cat ${CGROUP_PATH}
# usage_usec X
# user_usec X
# system_usec X
# nr_periods X
# nr_throttled X
# throttled_usec X

# Apply CPU limit to nginx pod and verify throttling
kubectl patch deployment nginx -p '{"spec":{"template":{"spec":{"containers":[{"name":"nginx","resources":{"limits":{"cpu":"50m"}}}]}}}}'
kubectl get pod -l app=nginx --watch  # wait for rollout

# Apply load and check throttle
kubectl exec -l app=nginx -- sh -c 'while true; do :; done' &
sleep 5
cat ${CGROUP_PATH} | grep throttled
# nr_throttled should be > 0

kubectl exec -l app=nginx -- kill 1  # stop the busy loop
```

## 7. Save the k3s Token (for Node Join)

```bash
# The k3s token is needed to join additional RPi5 nodes
sudo cat /var/lib/rancher/k3s/server/node-token
# → K10...::server:...

# Save it somewhere safe — you'll need it in setup/03-k3s-expand.md
# Also save your control plane IP:
echo "K3S_URL=https://192.168.1.101:6443"
echo "K3S_TOKEN=$(sudo cat /var/lib/rancher/k3s/server/node-token)"
```

When all verification steps pass, your single-node k3s cluster is functional. The ARM64 architecture runs the same Linux kernel abstractions covered in chapters 01-12. Continue to `setup/03-k3s-expand.md` to add a second RPi5.
