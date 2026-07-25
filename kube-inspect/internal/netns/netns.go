package netns

// InterfaceStats holds per-interface counters from a network namespace.
// Implemented in checkpoint 06.
type InterfaceStats struct{}

func ReadStats(netnsPath string) ([]InterfaceStats, error) {
	return nil, nil
}
