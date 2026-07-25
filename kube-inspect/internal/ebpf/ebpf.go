package ebpf

// Loader manages BPF program lifecycle.
// Implemented in checkpoint 07.
type Loader struct{}

func New() *Loader { return &Loader{} }
