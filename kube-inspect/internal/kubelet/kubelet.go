package kubelet

// NodeInfo holds kubelet-reported node resource state.
// Implemented in checkpoint 09.
type NodeInfo struct{}

func FetchNodeInfo() (NodeInfo, error) {
	return NodeInfo{}, nil
}
