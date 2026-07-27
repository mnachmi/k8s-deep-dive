# 12-a — KVM Architecture: `struct kvm`, `struct kvm_vcpu`, VM Entry/Exit

## The Insight That Made KVM Possible

In 2006, Avi Kivity at Qumranet — a startup later acquired by Red Hat — submitted a hypervisor to the Linux kernel mailing list. It was 10,000 lines of code. What made it unusual was not its size but its architecture: instead of building a standalone hypervisor like Xen, which had its own scheduler, its own memory manager, and its own device model, KVM was a kernel module that turned the existing Linux kernel into a hypervisor.

The insight was that a modern OS kernel already does most of what a hypervisor does. It has a scheduler that time-shares the CPU. It has a virtual memory system that gives each process its own address space. It has an interrupt handling infrastructure, a device driver model, and decades of bug fixes for edge cases that took years to discover. Why rebuild all of that for a hypervisor when Linux already has it? Instead, add a thin layer that uses hardware virtualization extensions (Intel VT-x, AMD-V) to let the kernel run guest operating systems as special processes, and let the existing Linux scheduler, memory manager, and I/O infrastructure handle the rest.

KVM was merged into Linux 2.6.20 in February 2007, the fastest major feature addition in Linux kernel history at the time. It was not complete — it needed QEMU for device emulation, had limited SMP support, and had no live migration. But the architecture was right, and everything since has been refinement.

## Source Locations

| File | Key Symbols | URL |
|------|-------------|-----|
| `virt/kvm/kvm_main.c` | `kvm_dev_ioctl()`, `kvm_vm_ioctl()`, `kvm_vcpu_ioctl()`, `kvm_run_vcpu()` | https://elixir.bootlin.com/linux/v6.9/source/virt/kvm/kvm_main.c |
| `include/linux/kvm_host.h` | `struct kvm`, `struct kvm_vcpu`, `struct kvm_run` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/kvm_host.h |
| `arch/x86/kvm/vmx/vmx.c` | `vmx_vcpu_run()`, `vmx_handle_exit()`, `vmcs_write64()` | https://elixir.bootlin.com/linux/v6.9/source/arch/x86/kvm/vmx/vmx.c |
| `arch/x86/kvm/x86.c` | `kvm_arch_vcpu_ioctl_run()`, `kvm_emulate_instruction()` | https://elixir.bootlin.com/linux/v6.9/source/arch/x86/kvm/x86.c |
| `arch/x86/include/asm/vmx.h` | `EXIT_REASON_*` constants, VMCS field encodings | https://elixir.bootlin.com/linux/v6.9/source/arch/x86/include/asm/vmx.h |

## 1. The KVM Module Architecture

KVM is structured as two modules on x86:

```
kvm.ko          — generic KVM core (scheduler integration, ioctls, memory)
kvm-intel.ko    — Intel VT-x backend (VMCS management, VM entry/exit)
kvm-amd.ko      — AMD-V backend (VMCB management, SVM entry/exit)
```

`/dev/kvm` is the control interface. QEMU (or any VMM) opens it and drives the VM lifecycle through three levels of ioctls:

```
/dev/kvm                        → system-level: get KVM version, create VMs
/dev/kvm → ioctl(KVM_CREATE_VM) → /dev/kvm-vm-N  (per-VM fd)
/dev/kvm-vm-N → ioctl(KVM_CREATE_VCPU) → /dev/kvm-vcpu-N (per-vCPU fd)
```

Each vCPU fd is backed by a `struct kvm_vcpu` in kernel memory. QEMU runs each vCPU in a dedicated thread that calls `ioctl(vcpu_fd, KVM_RUN)` in a loop. The kernel runs the guest inside this thread until a VM exit occurs, then returns control to QEMU to handle the exit.

## 2. `struct kvm` — The Virtual Machine

```c
// include/linux/kvm_host.h (selected fields, Linux 6.9)
struct kvm {
    struct mutex lock;                    // protects vcpus, memslots
    struct mm_struct *mm;                 // QEMU process's mm — guest RAM is mapped here
    struct kvm_memslots __rcu *memslots[KVM_ADDRESS_SPACE_NUM];
                                          // guest physical address → host virtual address map
    struct kvm_vcpu *vcpus[KVM_MAX_VCPUS]; // per-vCPU state
    atomic_t online_vcpus;               // number of runnable vCPUs
    int max_vcpus;

    struct list_head vm_list;            // all VMs on this host
    struct kvm_coalesced_mmio_ring *coalesced_mmio_ring;

    /* architecture-specific */
    struct kvm_arch arch;                // x86: EPT root, APIC state, etc.
    /* ... */
};
```

`kvm->mm` is the QEMU process's `mm_struct`. Guest RAM is `mmap()`-ed into QEMU's address space; the kernel uses QEMU's page tables (plus EPT) to manage it. This is why killing QEMU frees all guest memory — the guest's RAM is just anonymous memory in the VMM's address space.

## 3. `struct kvm_vcpu` — A vCPU Is a `task_struct`

This is the key connection to Chapter 01. A vCPU is not a hardware thread or a special kernel object. It is a `task_struct` — a Linux process (actually a thread in the QEMU process) that the CFS scheduler manages like any other task. When the guest is running, the vCPU thread is in `TASK_RUNNING` state, executing guest code in hardware-assisted mode. When the guest is blocked on I/O, the vCPU thread sleeps in `TASK_INTERRUPTIBLE`.

```c
// include/linux/kvm_host.h (selected fields, Linux 6.9)
struct kvm_vcpu {
    struct kvm *kvm;                    // back-pointer to VM
    int cpu;                            // physical CPU this vCPU is running on
    int vcpu_id;                        // vCPU number (0, 1, 2, ...)
    int vcpu_idx;                       // index in kvm->vcpus[]

    struct mutex mutex;
    struct kvm_run *run;                // shared page with QEMU: exit reason, I/O data

    /* scheduling */
    struct task_struct *pid;            // the QEMU thread that runs this vCPU
    unsigned long last_used_slot;

    /* statistics */
    u64 stat.exits;                     // total VM exits
    u64 stat.io_exits;                  // exits due to I/O port access
    u64 stat.mmio_exits;               // exits due to MMIO
    u64 stat.halt_exits;               // exits due to HLT instruction
    u64 stat.irq_exits;                // exits to deliver interrupt
    u64 stat.request_irq_exits;

    /* architecture-specific */
    struct kvm_vcpu_arch arch;          // x86: VMCS pointer, guest registers, etc.
};
```

**The CPU steal consequence:** Because a vCPU is a `task_struct`, it is subject to all the same CFS scheduling dynamics as any other process. When the host is overcommitted — more vCPUs than physical CPUs — some vCPUs will wait in the CFS runqueue. While a vCPU thread is waiting, the guest cannot make progress. The guest kernel has no visibility into this wait; from its perspective, time simply stopped advancing. The accumulation of these waits is CPU steal time.

## 4. Intel VT-x: VMCS and VM Entry/Exit

Intel's hardware virtualization extension, VT-x (introduced in 2005 with Pentium 4 Prescott), adds two new CPU instructions and a data structure:

- `VMLAUNCH` — enter a new guest (first time after VMCS setup)
- `VMRESUME` — resume a guest after a VM exit
- `VMCS` — Virtual Machine Control Structure, a 4KB hardware-managed data structure

The VMCS contains three regions:
1. **Guest-state area** — registers saved on VM exit, restored on VM entry: RIP, RSP, CR0, CR3, RFLAGS, segment registers, MSRs
2. **Host-state area** — the host kernel context to restore after a VM exit: host RSP, host RIP (the exit handler), host CR3 (host page tables)
3. **Control fields** — what events cause VM exits: I/O port access bitmaps, MSR read/write bitmaps, exception bitmap, NMI/interrupt behavior

On VM entry, the CPU:
1. Loads guest registers from the VMCS guest-state area
2. Switches CR3 to the EPT root pointer (guest physical → host physical translation)
3. Sets CPL to whatever the guest was at (Ring 0 or Ring 3)
4. Jumps to the guest RIP

On VM exit (triggered by any event in the exit control bitmap):
1. Saves guest registers to the VMCS guest-state area
2. Restores host registers from the VMCS host-state area
3. Writes the exit reason to `VMCS_EXIT_REASON`
4. Jumps to the host exit handler at the address stored in the VMCS host-state

```c
// arch/x86/include/asm/vmx.h — common exit reasons
#define EXIT_REASON_EXCEPTION_NMI        0   // guest exception or NMI
#define EXIT_REASON_EXTERNAL_INTERRUPT   1   // host interrupt arrived
#define EXIT_REASON_IO_INSTRUCTION       30  // guest IN/OUT instruction
#define EXIT_REASON_MSR_READ             31  // guest RDMSR
#define EXIT_REASON_MSR_WRITE            32  // guest WRMSR
#define EXIT_REASON_HLT                  12  // guest executed HLT
#define EXIT_REASON_CPUID                10  // guest executed CPUID
#define EXIT_REASON_EPT_VIOLATION        48  // guest page fault in EPT
#define EXIT_REASON_EPT_MISCONFIG        49  // EPT misconfiguration
#define EXIT_REASON_PREEMPTION_TIMER     52  // VMX preemption timer expired
```

## 5. VM Exit Cost

Every VM exit is expensive. The CPU must:
1. Complete all in-flight memory operations (fence)
2. Save ~150 bytes of guest state to the VMCS
3. Load ~64 bytes of host state from the VMCS
4. Switch CR3 (TLB flush or VPID invalidation)
5. Jump to the host exit handler

On modern hardware, a VM exit costs 1,000–5,000 CPU cycles. An HLT exit (guest is idle) is relatively harmless. An MMIO exit that requires QEMU emulation costs tens of thousands of cycles because it must context-switch to QEMU userspace, execute the emulation, and re-enter the guest.

This cost is why paravirtualized devices (Chapter 12-c) exist: they replace high-cost MMIO exits with shared-memory communication using the virtio protocol.

## 6. `/sys/kernel/debug/kvm/` — Live Observation

```bash
# Check if KVM is loaded and hardware virtualization is available
lsmod | grep kvm
# → kvm_intel  303104  3
# → kvm        786432  1 kvm_intel

# KVM aggregate statistics across all VMs on this host
ls /sys/kernel/debug/kvm/
cat /sys/kernel/debug/kvm/exits           # total VM exits since boot
cat /sys/kernel/debug/kvm/io_exits        # I/O port exits
cat /sys/kernel/debug/kvm/mmio_exits      # MMIO exits
cat /sys/kernel/debug/kvm/halt_exits      # HLT exits (guest idle)
cat /sys/kernel/debug/kvm/irq_exits       # interrupt injection exits

# View per-VM vCPU threads (run from host)
ps -eo pid,comm,psr | grep -i qemu
# Each "CPU N/KVM" thread is one vCPU's task_struct

# Check if running inside a VM (from guest)
systemd-detect-virt
# → kvm

# Read CPUID hypervisor leaf (from guest, requires cpuid tool)
cpuid -l 0x40000000
# → hypervisor vendor: "KVMKVMKVM"

# From inside a KVM guest: check steal time exposure
grep steal /proc/stat
# cpu  ... 0 0 [steal_ticks] 0 0
```

## 7. Key Kernel References

| Symbol | File | URL |
|--------|------|-----|
| `struct kvm` | `include/linux/kvm_host.h` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/kvm_host.h |
| `struct kvm_vcpu` | `include/linux/kvm_host.h` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/kvm_host.h |
| `kvm_vcpu_run()` | `virt/kvm/kvm_main.c` | https://elixir.bootlin.com/linux/v6.9/source/virt/kvm/kvm_main.c |
| `vmx_vcpu_run()` | `arch/x86/kvm/vmx/vmx.c` | https://elixir.bootlin.com/linux/v6.9/source/arch/x86/kvm/vmx/vmx.c |
| `EXIT_REASON_*` | `arch/x86/include/asm/vmx.h` | https://elixir.bootlin.com/linux/v6.9/source/arch/x86/include/asm/vmx.h |
| `/dev/kvm` ioctl interface | `Documentation/virt/kvm/api.rst` | https://elixir.bootlin.com/linux/v6.9/source/Documentation/virt/kvm/api.rst |
