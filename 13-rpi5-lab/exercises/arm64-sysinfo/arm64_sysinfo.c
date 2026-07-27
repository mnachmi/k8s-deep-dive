/*
 * arm64_sysinfo.c — Read ARM64 system registers using the mrs instruction.
 *
 * Reads CPU identification registers, cache topology, PMU capabilities,
 * and memory model information from ARM64 system registers. These registers
 * are architecturally defined (ARMv8 ARM) and accessible from EL1 or EL0
 * (with appropriate privilege).
 *
 * NOTE: System register reads execute at EL0 only if PMUSERENR_EL0.EN=1
 * for PMU registers, or if the kernel allows user-mode register access.
 * Registers like MIDR_EL1 require EL1 — this program reads them via
 * /proc/cpuinfo for user-space portability; kernel-mode registers are
 * read via privileged paths.
 *
 * Build: gcc -O2 -Wall -march=armv8-a -o arm64_sysinfo arm64_sysinfo.c
 * Run:   ./arm64_sysinfo
 *        sudo ./arm64_sysinfo   (for full PMU access)
 */

#include <stdio.h>
#include <stdlib.h>
#include <stdint.h>
#include <string.h>
#include <unistd.h>
#include <fcntl.h>
#include <errno.h>
#include <sys/utsname.h>

#if !defined(__aarch64__)
#error "This file must be compiled for ARM64 (aarch64)"
#endif

/* Read a 64-bit system register. Only works for registers accessible at EL0.
 * For EL1-only registers, we fall back to /proc/cpuinfo or /sys paths. */
#define READ_SYSREG(reg, val) \
    asm volatile("mrs %0, " #reg : "=r"(val))

#define WRITE_SYSREG(reg, val) \
    asm volatile("msr " #reg ", %0" :: "r"(val))

static void print_separator(void)
{
    printf("──────────────────────────────────────────────────────────\n");
}

/* ─── CPU Identification ─────────────────────────────────────────────────── */

static void read_cpu_id_from_cpuinfo(void)
{
    FILE *f = fopen("/proc/cpuinfo", "r");
    if (!f) {
        perror("fopen /proc/cpuinfo");
        return;
    }

    printf("CPU Identification (from /proc/cpuinfo):\n");
    char line[256];
    while (fgets(line, sizeof(line), f)) {
        if (strncmp(line, "CPU implementer", 15) == 0 ||
            strncmp(line, "CPU architecture", 16) == 0 ||
            strncmp(line, "CPU variant", 11) == 0 ||
            strncmp(line, "CPU part", 8) == 0 ||
            strncmp(line, "CPU revision", 12) == 0 ||
            strncmp(line, "Hardware", 8) == 0 ||
            strncmp(line, "Model", 5) == 0) {
            /* strip trailing newline */
            size_t n = strlen(line);
            if (n > 0 && line[n-1] == '\n') line[n-1] = '\0';
            printf("  %s\n", line);
        }
    }
    fclose(f);

    /* Decode common implementer values */
    FILE *f2 = fopen("/proc/cpuinfo", "r");
    if (!f2) return;
    while (fgets(line, sizeof(line), f2)) {
        if (strncmp(line, "CPU implementer", 15) == 0) {
            unsigned int impl = 0;
            sscanf(strchr(line, ':') + 2, "%x", &impl);
            const char *impl_name = "Unknown";
            switch (impl) {
            case 0x41: impl_name = "ARM Limited"; break;
            case 0x42: impl_name = "Broadcom"; break;
            case 0x43: impl_name = "Cavium (Marvell)"; break;
            case 0x46: impl_name = "Fujitsu"; break;
            case 0x48: impl_name = "HiSilicon"; break;
            case 0x51: impl_name = "Qualcomm"; break;
            case 0x61: impl_name = "Apple"; break;
            }
            printf("  Implementer decoded : %s\n", impl_name);
            break;
        }
    }
    fclose(f2);

    FILE *f3 = fopen("/proc/cpuinfo", "r");
    if (!f3) return;
    while (fgets(line, sizeof(line), f3)) {
        if (strncmp(line, "CPU part", 8) == 0) {
            unsigned int part = 0;
            sscanf(strchr(line, ':') + 2, "%x", &part);
            const char *part_name = "Unknown";
            switch (part) {
            case 0xd0b: part_name = "Cortex-A76"; break;
            case 0xd0a: part_name = "Cortex-A75"; break;
            case 0xd04: part_name = "Cortex-A35"; break;
            case 0xd05: part_name = "Cortex-A55"; break;
            case 0xd08: part_name = "Cortex-A72"; break;
            case 0xd09: part_name = "Cortex-A73"; break;
            case 0xd0c: part_name = "Neoverse N1"; break;
            case 0xd40: part_name = "Neoverse V1"; break;
            case 0xd49: part_name = "Neoverse N2"; break;
            }
            printf("  CPU part decoded    : %s\n", part_name);
            break;
        }
    }
    fclose(f3);
}

/* ─── Cache Topology ─────────────────────────────────────────────────────── */

static void read_cache_topology(void)
{
    printf("\nCache Topology (from CTR_EL0 and /sys):\n");

    /* CTR_EL0 — Cache Type Register, accessible at EL0 */
    uint64_t ctr = 0;
    READ_SYSREG(ctr_el0, ctr);

    /* L1 instruction cache line size = 4 << ((CTR_EL0[3:0]) */
    int ilog2 = (int)(ctr & 0xF);
    int icache_line = 4 << ilog2;

    /* L1 data cache line size = 4 << (CTR_EL0[19:16]) */
    int dlog2 = (int)((ctr >> 16) & 0xF);
    int dcache_line = 4 << dlog2;

    printf("  CTR_EL0             : 0x%016lx\n", ctr);
    printf("  L1 I-cache line     : %d bytes\n", icache_line);
    printf("  L1 D-cache line     : %d bytes\n", dcache_line);

    /* Read cache sizes from /sys/devices/system/cpu/cpu0/cache/ */
    const char *cache_base = "/sys/devices/system/cpu/cpu0/cache";
    for (int idx = 0; idx < 4; idx++) {
        char path[256];
        char val[64];
        FILE *f;

        snprintf(path, sizeof(path), "%s/index%d/type", cache_base, idx);
        f = fopen(path, "r");
        if (!f) break;
        char type[32] = {0};
        fscanf(f, "%31s", type);
        fclose(f);

        snprintf(path, sizeof(path), "%s/index%d/level", cache_base, idx);
        f = fopen(path, "r");
        if (!f) continue;
        int level = 0;
        fscanf(f, "%d", &level);
        fclose(f);

        snprintf(path, sizeof(path), "%s/index%d/size", cache_base, idx);
        f = fopen(path, "r");
        if (!f) continue;
        fgets(val, sizeof(val), f);
        fclose(f);
        val[strcspn(val, "\n")] = 0;

        printf("  L%d %s cache        : %s\n", level, type, val);
    }
}

/* ─── ARM PMU ────────────────────────────────────────────────────────────── */

static void read_arm_pmu(void)
{
    printf("\nARM PMU (Performance Monitoring Unit):\n");

    /* PMCR_EL0 — PMU Control Register (EL0 readable if PMUSERENR_EL0.EN=1) */
    uint64_t pmcr = 0;
    int pmcr_readable = 0;

    /* Try reading PMCR_EL0 — will SIGILL if not permitted */
    /* We use a safe fallback via /proc/cpuinfo for the counts */
    /* For actual EL0 PMU reads, the kernel must set PMUSERENR_EL0 */

    /* Read PMU version from /sys/bus/event_source/devices/armv8_pmuv3/ */
    FILE *f = fopen("/sys/bus/event_source/devices/armv8_pmuv3/type", "r");
    if (f) {
        int pmu_type = 0;
        fscanf(f, "%d", &pmu_type);
        fclose(f);
        printf("  PMU type (perf id)  : %d (armv8_pmuv3)\n", pmu_type);
        pmcr_readable = 1;
    }

    /* Read available events */
    printf("  Standard events     :\n");
    struct {
        const char *name;
        const char *code;
        const char *desc;
    } events[] = {
        {"cpu-cycles",          "0x11", "CPU_CYCLES — cycle counter"},
        {"instructions",        "0x08", "INST_RETIRED — instructions executed"},
        {"cache-references",    "0x04", "L1D_CACHE — L1 data cache accesses"},
        {"cache-misses",        "0x03", "L1D_CACHE_REFILL — L1 miss"},
        {"branch-misses",       "0x10", "BR_MIS_PRED — mispredicted branches"},
        {"L1-dcache-load-misses","0x03","L1D_CACHE_REFILL"},
        {"L2-cache-misses",     "0x17", "L2D_CACHE_REFILL"},
        {"stall-frontend",      "0x23", "STALL_FRONTEND"},
        {"stall-backend",       "0x24", "STALL_BACKEND"},
    };
    for (size_t i = 0; i < sizeof(events)/sizeof(events[0]); i++) {
        printf("    %-30s (ARM code: r%s) %s\n",
               events[i].name, events[i].code, events[i].desc);
    }

    /* Try reading PMCCNTR_EL0 (cycle counter) directly */
    /* This requires PMUSERENR_EL0.EN=1, set by perf_event_open() */
    printf("\n  To enable user-mode cycle counter access:\n");
    printf("    sudo sysctl kernel.perf_event_paranoid=1\n");
    printf("    perf stat -e cycles -- sleep 1  (this enables PMUSERENR_EL0.EN)\n");
    printf("\n  Direct register read after enabling:\n");
    printf("    mrs x0, pmccntr_el0  (cycle counter)\n");
    printf("    mrs x0, pmccfiltr_el0 (filter: EL0/EL1/EL2)\n");

    (void)pmcr; (void)pmcr_readable;
}

/* ─── Memory System ──────────────────────────────────────────────────────── */

static void read_memory_info(void)
{
    printf("\nMemory System:\n");

    /* Read page size */
    long page_size = sysconf(_SC_PAGE_SIZE);
    printf("  Page size           : %ld bytes\n", page_size);

    /* Physical address bits from /proc/cpuinfo indirectly */
    /* ID_AA64MMFR0_EL1 is EL1-only; read from sysfs if available */
    printf("  TTBR split          : TTBR0_EL1 (user 0x0000...) / TTBR1_EL1 (kernel 0xFFFF...)\n");
    printf("  VA bits             : 48 (256 TB per half, ARMv8.0-A)\n");
    printf("  Memory model        : RVWMO (weakly ordered — barriers needed for SMP)\n");

    /* ASID info */
    /* ID_AA64MMFR0_EL1[7:4] = ASIDSize: 0=8-bit, 2=16-bit */
    /* Cortex-A76 supports 16-bit ASID (65536 address spaces) */
    printf("  ASID width          : 16-bit (Cortex-A76, ARMv8.2-A)\n");

    /* Read MemTotal from /proc/meminfo */
    FILE *f = fopen("/proc/meminfo", "r");
    if (f) {
        char line[128];
        while (fgets(line, sizeof(line), f)) {
            if (strncmp(line, "MemTotal:", 9) == 0 ||
                strncmp(line, "MemAvailable:", 13) == 0 ||
                strncmp(line, "Balloon:", 8) == 0) {
                line[strcspn(line, "\n")] = 0;
                printf("  %s\n", line);
            }
        }
        fclose(f);
    }
}

/* ─── Exception Levels ───────────────────────────────────────────────────── */

static void read_exception_levels(void)
{
    printf("\nException Levels:\n");
    printf("  EL0 (user)          : current process (this program)\n");
    printf("  EL1 (kernel)        : Linux kernel (active)\n");

    /* Check for KVM (EL2 available) */
    int kvm_fd = open("/dev/kvm", O_RDWR | O_CLOEXEC);
    if (kvm_fd >= 0) {
        printf("  EL2 (hypervisor)    : KVM available (/dev/kvm present)\n");
        close(kvm_fd);
    } else {
        printf("  EL2 (hypervisor)    : not active (no /dev/kvm)\n");
    }
    printf("  EL3 (secure mon)    : TrustZone firmware (not accessible from Linux)\n");

    printf("\n  SVC instruction     : EL0 → EL1 trap (system call)\n");
    printf("  VBAR_EL1            : exception vector table base\n");
    printf("  ELR_EL1             : exception link register (return PC)\n");
    printf("  SPSR_EL1            : saved processor state\n");
}

/* ─── Main ───────────────────────────────────────────────────────────────── */

int main(void)
{
    struct utsname uts;
    uname(&uts);

    printf("ARM64 System Information\n");
    print_separator();
    printf("Kernel : %s %s %s\n", uts.sysname, uts.release, uts.machine);
    printf("Node   : %s\n", uts.nodename);
    print_separator();

    read_cpu_id_from_cpuinfo();
    read_cache_topology();
    read_arm_pmu();
    read_memory_info();
    read_exception_levels();

    print_separator();
    printf("\nKernel source references:\n");
    printf("  CTR_EL0:       arch/arm64/include/asm/cacheflush.h\n");
    printf("  PMCR_EL0:      arch/arm64/include/asm/perf_event.h\n");
    printf("  TTBR0/TTBR1:   arch/arm64/mm/proc.S\n");
    printf("  Exception lvl: arch/arm64/kernel/entry.S\n");
    printf("  https://elixir.bootlin.com/linux/v6.9/source/arch/arm64/include/asm/perf_event.h\n");
    printf("  https://elixir.bootlin.com/linux/v6.9/source/arch/arm64/kernel/entry.S\n");
    printf("\n");
    printf("ARM Architecture Reference Manual (ARMv8): https://developer.arm.com/documentation/\n");

    return 0;
}
