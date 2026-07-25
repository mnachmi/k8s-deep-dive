package cgroup

// Stats holds cgroup v2 resource usage for a pod.
// Implemented in checkpoint 03.
type Stats struct{}

func ReadStats(cgroupPath string) (Stats, error) {
	return Stats{}, nil
}
