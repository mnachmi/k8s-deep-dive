# Exercise: cluster-join

Automate k3s agent node join and verify the new node reaches Ready status. Demonstrates the Kubernetes node registration lifecycle and labels nodes with detected hardware information.

## Build and Run

```bash
# Build on the control plane node (rpi5-01)
go build -o cluster-join .

# Dry run: see the install command without executing it
./cluster-join \
  --server https://192.168.1.101:6443 \
  --token $(sudo cat /var/lib/rancher/k3s/server/node-token) \
  --node-name rpi5-02 \
  --node-ip 192.168.1.102 \
  --dry-run

# Actual join (run on rpi5-02 with the token from rpi5-01):
# Copy the binary to rpi5-02 first:
scp cluster-join ubuntu@192.168.1.102:~/

# Then on rpi5-02:
sudo ./cluster-join \
  --server https://192.168.1.101:6443 \
  --token K10...::server:... \
  --node-name rpi5-02 \
  --node-ip 192.168.1.102
```

## Expected Output

```
=== k3s Cluster Node Join ===

Step 1: Checking control plane connectivity at https://192.168.1.101:6443...
  ✓ Control plane reachable

Step 2: k3s agent install command:
  curl -sfL https://get.k3s.io \
    | INSTALL_K3S_EXEC="agent \
    --server=https://192.168.1.101:6443 \
    --token=K10...::server:... \
    --node-ip=192.168.1.102 \
    --node-name=rpi5-02" sh -

Step 3: Installing k3s agent...
  [k3s install output]
  ✓ k3s agent installed

Step 4: Waiting for node "rpi5-02" to become Ready (timeout: 3m0s)...
  Polling.........
  ✓ Node "rpi5-02" is Ready

Step 5: Node information:
  Architecture     : arm64
  Kernel           : 6.8.0-1013-raspi
  Container runtime: containerd://1.7.x
  Kubelet version  : v1.29.x+k3s1

Step 6: Applying hardware labels:
  hardware/arch=arm64
  hardware/type=raspberry-pi
  hardware/cpu=cortex-a76
  hardware/model=rpi5
  ✓ Labels applied

=== Join Complete ===
Run: kubectl get nodes -o wide
```

## What Happens When a Node Joins

When k3s agent starts on rpi5-02:

1. Agent reads `/etc/rancher/k3s/agent/kubelet.kubeconfig` (generated at install)
2. kubelet sends `POST /api/v1/nodes` to the API server on rpi5-01
3. API server validates the bootstrap token, creates a `Node` object in etcd
4. kubelet begins posting `NodeReady` conditions
5. kube-controller-manager's node lifecycle controller watches for `Ready=True`
6. scheduler adds rpi5-02 to the eligible node list

The `Node` object transition from `NotReady` → `Ready` is what this tool waits for.

## Node Labels and Scheduling

After this exercise, you can use the applied labels in pod scheduling:

```yaml
# Schedule only on RPi5 nodes:
spec:
  nodeSelector:
    hardware/model: rpi5

# Prefer ARM64, tolerate amd64:
spec:
  affinity:
    nodeAffinity:
      preferredDuringSchedulingIgnoredDuringExecution:
        - weight: 100
          preference:
            matchExpressions:
              - key: hardware/arch
                operator: In
                values: ["arm64"]
```

## Connection to Chapter

- `setup/03-k3s-expand.md` — manual version of these steps
- `k8s/13-k8s-connection.md` — two-node topology and scheduling realities
- Chapter 09 (`09-k8s-connection.md`) — how kubelet registration works via etcd
