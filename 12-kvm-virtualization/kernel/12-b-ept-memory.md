# 12-b — Extended Page Tables: Two-Level Address Translation, IOMMU, and SR-IOV

## The Memory Virtualization Problem

Chapter 04 described how the Linux kernel gives each process its own virtual address space through page tables: the process sees virtual addresses, the MMU translates them to physical addresses via CR3, and the OS manages the mapping. This works because there is one OS and one set of physical addresses.

Virtualization breaks the model. A guest OS wants to manage its own page tables and its own physical address space. The hypervisor has its own page tables and its own physical address space. The result is two independent address translation layers that must be composed:

```
Guest virtual address
       ↓  (guest OS page tables — guest virtual → guest physical)
Guest physical address
       ↓  (hypervisor EPT — guest physical → host physical)
Host physical address
```

Before hardware support for two-level translation existed, hypervisors used **shadow page tables**: the hypervisor maintained a hidden set of page tables that directly mapped guest virtual addresses to host physical addresses, intercepted every guest page table modification via write-protect page faults, and kept the shadow tables synchronized. This worked but was enormously expensive — every guest page table update required a VM exit and hypervisor intervention.

Intel introduced Extended Page Tables (EPT) in the Nehalem microarchitecture in 2008. AMD introduced Nested Page Tables (NPT) in the Barcelona microarchitecture in 2007. Both provide hardware support for the second translation layer, eliminating shadow page table overhead entirely.

## Source Locations

| File | Key Symbols | URL |
|------|-------------|-----|
| `arch/x86/kvm/mmu/mmu.c` | `kvm_mmu_page_fault()`, EPT violation handler, `kvm_tdp_page_fault()` | https://elixir.bootlin.com/linux/v6.9/source/arch/x86/kvm/mmu/mmu.c |
| `arch/x86/kvm/mmu/tdp_mmu.c` | Two-dimensional paging (TDP) MMU: EPT page table walks | https://elixir.bootlin.com/linux/v6.9/source/arch/x86/kvm/mmu/tdp_mmu.c |
| `arch/x86/include/asm/vmx.h` | `EXIT_REASON_EPT_VIOLATION`, `EXIT_REASON_EPT_MISCONFIG` | https://elixir.bootlin.com/linux/v6.9/source/arch/x86/include/asm/vmx.h |
| `drivers/iommu/intel/iommu.c` | Intel VT-d IOMMU driver, DMA remapping | https://elixir.bootlin.com/linux/v6.9/source/drivers/iommu/intel/iommu.c |

## 1. The EPT Page Walk

With EPT enabled, the hardware MMU performs a two-dimensional page walk on every TLB miss:

```
Guest virtual address (GVA)
  → walk guest CR3 page tables (4 levels: PGD→P4D→PUD→PMD→PTE)
  → each guest page table entry points to a guest physical address (GPA)
  → for each GPA, walk the EPT (4 levels: PML4→PDPT→PD→PT)
  → produce host physical address (HPA)
```

In the worst case — a 4-level guest page table on top of a 4-level EPT — a single TLB miss requires:

- 4 guest page table entries × 1 EPT walk each = 4 × 5 memory accesses
- Plus the final EPT walk for the data page itself = 5 more accesses
- **Total: 24 memory accesses** for one TLB miss

Compare to bare metal: 4 memory accesses for a 4-level page table walk, then the data access.

This is the hidden tax of running Kubernetes in a VM. Every cache miss that requires a page table walk costs 6× more memory accesses inside a VM than on bare metal. Workloads with large working sets and high TLB miss rates — in-memory databases, large ML inference models — pay this cost on every cache miss.

### EPT Structure

```
VMCS: EPT_POINTER → host physical address of EPT PML4 table
                             ↓
EPT PML4 [512 entries] → EPT PDPT page
                             ↓
EPT PDPT [512 entries] → EPT PD page (or 1GB huge page)
                             ↓
EPT PD [512 entries]   → EPT PT page (or 2MB huge page)
                             ↓
EPT PT [512 entries]   → host physical page frame number
```

EPT entries carry R/W/X permission bits. An EPT violation (guest accesses a GPA not mapped in EPT, or with wrong permissions) causes `EXIT_REASON_EPT_VIOLATION` and KVM must handle it: either map a new host page or terminate the guest if the access is invalid.

## 2. VPID: TLB Tagging Across VM Entries

Without tagging, every VM entry/exit requires a full TLB flush — all cached guest translations are invalid from the host's perspective and vice versa. On a system with thousands of VM entries per second, this TLB thrash is catastrophic.

Intel's VPID (Virtual Processor ID) tags TLB entries with a 16-bit identifier. The host kernel uses VPID=0 for host translations. Each vCPU gets a unique VPID for its guest translations. On VM entry and exit, the CPU switches the active VPID; it does not flush — host and guest TLB entries coexist, tagged separately.

VPID is to VM entry/exit what PCID (from Chapter 00) is to context switches: it eliminates TLB flushes at the boundary by tagging entries with their address space identity.

## 3. IOMMU: Direct Device Assignment

The EPT solves CPU memory access. Devices that perform DMA — network cards, NVMe drives — have a separate problem. DMA bypasses the CPU's MMU; the device reads and writes host physical addresses directly. A guest that controls a DMA-capable device could DMA into host kernel memory, escaping the VM.

The IOMMU (Input-Output Memory Management Unit) sits between DMA-capable devices and the memory bus, remapping device DMA addresses the same way the CPU's MMU remaps virtual addresses. Intel's implementation is VT-d (Virtualization Technology for Directed I/O); AMD's is AMD-Vi.

With IOMMU, a device assigned to a VM can only DMA into guest memory (its EPT-mapped pages). The IOMMU enforces the boundary. Without IOMMU, device assignment to a VM is a security hole.

## 4. SR-IOV: Near-Native NIC Performance for Pods

SR-IOV (Single Root I/O Virtualization) is a PCIe specification that allows a single physical NIC to present itself as multiple virtual PCIe devices, each independently assignable to a VM or container.

```
Physical NIC (Physical Function — PF)
  ├── Virtual Function 0 (VF0) → assigned to VM/pod 0
  ├── Virtual Function 1 (VF1) → assigned to VM/pod 1
  ├── Virtual Function 2 (VF2) → assigned to VM/pod 2
  └── ...up to 256 VFs on some hardware
```

Each VF has its own PCIe BAR, its own interrupt lines, and its own DMA queues. The IOMMU assigns each VF's DMA space to its guest/pod. From the guest's perspective, the VF appears to be a dedicated NIC. Network traffic bypasses the host kernel entirely — packets go directly from the NIC hardware into guest memory via DMA.

The Kubernetes SR-IOV device plugin (`sriov-network-device-plugin`) exposes VFs as allocatable resources:
```yaml
resources:
  limits:
    intel.com/sriov_netdevice: "1"  # allocate one SR-IOV VF
```

At pod scheduling time, the plugin assigns a specific VF to the pod, configures the IOMMU mapping, and passes the VF's PCI address to the container runtime. The pod gets a VF as its network interface — throughput and latency approach bare-metal performance, with none of the virtio overhead.

## 5. Huge Pages in KVM: The 1GB Optimization

EPT supports 1GB huge pages (PML4 → PDPT → 1GB host physical region). When guest memory is backed by 1GB huge pages on the host, the EPT page walk for any address in that region terminates at the PDPT level — 1 EPT walk instead of 4. This reduces the maximum TLB miss cost from 24 memory accesses to roughly 12.

This is why cloud providers offer "memory-optimized" and "compute-optimized" instances backed by 1GB huge pages, and why enabling huge pages in Kubernetes (`hugepages-1Gi` resource type) has a measurable latency reduction for in-memory workloads — the EPT walk is shorter for every TLB miss.

## 6. Live Observation

```bash
# Check if EPT is enabled (from host)
grep -E "ept|npt" /sys/module/kvm_intel/parameters/
cat /sys/module/kvm_intel/parameters/ept
# → Y

# Check if IOMMU is active (from host)
dmesg | grep -i iommu | head -5
cat /sys/class/iommu/*/name 2>/dev/null
# → DMAR0, DMAR1 (Intel VT-d)

# List SR-IOV VFs on a NIC (from host, requires SR-IOV capable NIC)
cat /sys/class/net/eth0/device/sriov_numvfs
cat /sys/class/net/eth0/device/sriov_totalvfs

# EPT violations per vCPU (KVM stats)
cat /sys/kernel/debug/kvm/mmu_shadow_zapped   # shadow page invalidations
cat /sys/kernel/debug/kvm/mmu_pte_write        # guest PTE write traps
cat /sys/kernel/debug/kvm/remote_tlb_flush     # TLB invalidations

# TLB miss rate inside a guest (using perf)
perf stat -e dTLB-load-misses,iTLB-load-misses -- sleep 1
# High numbers inside a VM indicate EPT walk overhead
```

## 7. Key Kernel References

| Symbol | File | URL |
|--------|------|-----|
| `kvm_mmu_page_fault()` | `arch/x86/kvm/mmu/mmu.c` | https://elixir.bootlin.com/linux/v6.9/source/arch/x86/kvm/mmu/mmu.c |
| EPT violation handler | `arch/x86/kvm/mmu/tdp_mmu.c` | https://elixir.bootlin.com/linux/v6.9/source/arch/x86/kvm/mmu/tdp_mmu.c |
| Intel IOMMU driver | `drivers/iommu/intel/iommu.c` | https://elixir.bootlin.com/linux/v6.9/source/drivers/iommu/intel/iommu.c |
| VPID handling | `arch/x86/kvm/vmx/vmx.c` | https://elixir.bootlin.com/linux/v6.9/source/arch/x86/kvm/vmx/vmx.c |
