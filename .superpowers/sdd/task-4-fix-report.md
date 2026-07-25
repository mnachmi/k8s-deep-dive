# Fix Report — 10-k8s-connection.md (task-4)

## File Modified
`10-performance/k8s/10-k8s-connection.md`

## Fixes Applied

### Fix 1 — `perf_event_paranoid` threshold (Section 8 notes + Section 5 table)
- **Section 8 Notes:** Changed `perf_event_paranoid ≤ 1` to
  `perf_event_paranoid ≤ 2 (for user-space only profiling) or ≤ 1 (for kernel profiling)`.
  The old text was incorrect: level ≤ 2 is the threshold for user-space per-process profiling;
  level ≤ 1 is required for kernel profiling. Both thresholds are now documented.
- **Section 5 Best Practices table:** Changed `perf_event_paranoid=1 allows per-process perf`
  to `perf_event_paranoid≤2 allows per-process perf` for consistency.

### Fix 2 — Throttle ratio awk script hardcodes period (Section 7)
- Added comment before the awk throttle-ratio loop:
  `# NOTE: assumes default CFS period of 100ms (100000 µs); check cpu.max if period was changed`
- This makes it explicit that `p*100000` in the awk END block is only valid when the CFS period
  has not been changed from the 100ms default.

### Fix 3 — GKE seccomp attribution (Section 3 + Section 6)
- **Section 3 (item 3):** Changed `GKE's default seccomp profile (RuntimeDefault) blocks perf_event_open`
  to `RuntimeDefault seccomp profile (the default on GKE, EKS, AKS, and most managed clusters) blocks perf_event_open`.
- **Section 6 (EPERM diagnosis paragraph):** Changed `On GKE, perf_event_open is blocked by the default seccomp profile`
  to `On clusters using the RuntimeDefault seccomp profile (the default on GKE, EKS, AKS, and most managed clusters), perf_event_open is blocked`.

### Fix 4 — `cpuCFSBurst` feature gate (Section 5)
- Replaced the stale `cpuCFSBurst feature gate (alpha in 1.27+)` claim with:
  `CFS burst support (kernel cpu.max.burst, Linux 5.14; check your Kubernetes version's feature gates for status)`.
  The original specific alpha-in-1.27 claim is replaced with a version-agnostic hedge.

## Accuracy Notes
- The Section 3 paranoid table (levels 3/2/1/≤0) was already correct and was not changed.
- No other `≤ 1` threshold claims for per-process profiling were found elsewhere in the document.
