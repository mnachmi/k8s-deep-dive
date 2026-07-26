# 11-c: kexec_load, kdump, struct kimage, /proc/vmcore

## 1. Source Locations

| File | Key Symbols | URL |
|------|-------------|-----|
| `include/uapi/linux/kexec.h` | `kexec_load(2)` flags: `KEXEC_ON_CRASH`, `KEXEC_SEGMENT_*`, `struct kexec_segment` | https://elixir.bootlin.com/linux/v6.9/source/include/uapi/linux/kexec.h |
| `kernel/kexec_core.c` | `machine_kexec()`, `kexec_load_purgatory()`, `kimage` struct | https://elixir.bootlin.com/linux/v6.9/source/kernel/kexec_core.c |
| `include/linux/kexec.h` | `struct kimage`, `struct kexec_segment`, `kexec_crash_loaded()` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/kexec.h |
| `arch/x86/kernel/machine_kexec_64.c` | x86_64 `machine_kexec()` — disables paging, jumps to new kernel | https://elixir.bootlin.com/linux/v6.9/source/arch/x86/kernel/machine_kexec_64.c |
| `fs/proc/vmcore.c` | `/proc/vmcore` — ELF core dump from crashed kernel | https://elixir.bootlin.com/linux/v6.9/source/fs/proc/vmcore.c |

## 2. `kexec_load(2)` Syscall

```c
// include/uapi/linux/kexec.h
long kexec_load(
    unsigned long entry,   // entry point address in new kernel
    unsigned long nr_segments,
    struct kexec_segment *segments,  // array of memory segments
    unsigned long flags    // KEXEC_ON_CRASH for kdump; 0 for regular kexec
);

struct kexec_segment {
    const void __user *buf;    // source data in userspace
    size_t             bufsz;  // size of source data
    const void __user *mem;    // target physical address
    size_t             memsz;  // size of target region
};
```

`KEXEC_ON_CRASH`: loads the new kernel into the memory region reserved by the `crashkernel=` boot parameter. This memory is kept separate from the running kernel — it cannot be allocated by the normal allocator.

## 3. `struct kimage`

```c
// include/linux/kexec.h (selected fields, Linux 6.9)
struct kimage {
    kimage_entry_t  *entry;         // start of kimage entry list
    kimage_entry_t  *last_free;     // last free entry in list
    kimage_entry_t  *next_entry;    // current pointer
    struct list_head control_pages; // pages used for control code
    struct list_head dest_pages;    // pages to copy to their final location
    struct list_head unusable_pages;// pages that cannot be used

    unsigned long   start;          // entry point (physical address)
    struct page    *control_code_page; // page containing assembly trampoline
    unsigned long   nr_segments;    // number of loaded segments
    struct kexec_segment segment[KEXEC_SEGMENT_MAX]; // segments (up to 16)

    unsigned int    type;           // KEXEC_TYPE_DEFAULT or KEXEC_TYPE_CRASH
    // ...
};
```

## 4. kdump Boot Sequence

```
Panic in production kernel
    │
    ▼
crash_kexec(regs)         // kernel/kexec_core.c
    │  machine_kexec()    // arch/x86/kernel/machine_kexec_64.c
    │    - disables paging, IRQs, APIC
    │    - copies control code to identity-mapped page
    │    - jumps to crash kernel entry point
    ▼
Crash kernel boots (smaller, no KASLR)
    │  uses crashkernel= reserved memory only
    │  runs makedumpfile OR copies /proc/vmcore
    ▼
/proc/vmcore
    │  ELF core file: PT_LOAD segments map production kernel's physical memory
    │  PT_NOTE: contains ELF notes with register state, kernel version
    ▼
`crash` tool:  crash vmlinux /proc/vmcore
    │  bt    → backtrace at time of crash
    │  log   → dmesg from crashed kernel
    │  ps    → task list
```

## 5. `/proc/vmcore`

`/proc/vmcore` is an ELF core file implemented in `fs/proc/vmcore.c`. It presents the crashed kernel's memory as an ELF PT_LOAD map. The crash kernel reads the previous kernel's memory using physical addresses saved in the ELF notes.

Key ELF notes in `/proc/vmcore`:
- `VMCOREINFO` note: kernel version, page size, symbol offsets, struct sizes — everything `makedumpfile` and `crash` need to parse the kernel state
- `NT_PRSTATUS` per-CPU: register state of each CPU at crash time

```bash
# On crash kernel (after kdump):
makedumpfile -c -d 31 /proc/vmcore /var/crash/vmcore.flat
crash /boot/vmlinux-$(uname -r) /proc/vmcore
```

## 6. Live Observation

```bash
# Check if crash kernel is loaded
cat /sys/kernel/kexec_crash_loaded   # 1 = crash kernel loaded (kdump ready)
cat /sys/kernel/kexec_loaded         # 1 = regular kexec kernel loaded
cat /sys/kernel/kexec_crash_size     # size reserved for crash kernel

# Check crashkernel parameter
cat /proc/cmdline | grep -o 'crashkernel=[^ ]*'

# Check reserved memory region
cat /proc/iomem | grep "Crash kernel"

# Configure panic-to-kdump
sysctl kernel.panic_on_oops=1
sysctl kernel.panic=10            # reboot after 10s (kdump must complete first)

# bpftrace: trace kexec_load syscall
bpftrace -e 'tracepoint:syscalls:sys_enter_kexec_load {
    printf("kexec_load: pid=%d flags=%lx\n", pid, args->flags);
}'
```

## 7. Key Kernel References

| Symbol | File | URL |
|--------|------|-----|
| `struct kimage` | `include/linux/kexec.h` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/kexec.h |
| `struct kexec_segment` | `include/uapi/linux/kexec.h` | https://elixir.bootlin.com/linux/v6.9/source/include/uapi/linux/kexec.h |
| `machine_kexec()` | `arch/x86/kernel/machine_kexec_64.c` | https://elixir.bootlin.com/linux/v6.9/source/arch/x86/kernel/machine_kexec_64.c |
| `crash_kexec()` | `kernel/kexec_core.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/kexec_core.c |
| `/proc/vmcore` handler | `fs/proc/vmcore.c` | https://elixir.bootlin.com/linux/v6.9/source/fs/proc/vmcore.c |
| `kexec_crash_loaded()` — tests `kexec_crash_image != NULL`; sysfs: `/sys/kernel/kexec_crash_loaded` | `include/linux/kexec.h` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/kexec.h |
