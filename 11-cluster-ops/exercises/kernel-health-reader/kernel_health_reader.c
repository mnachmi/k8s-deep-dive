/*
 * kernel_health_reader.c — read kernel health state from procfs and sysctl.
 *
 * Demonstrates access to:
 *   a) Kernel version from /proc/version
 *   b) Taint flags from /proc/sys/kernel/tainted (decode each bit)
 *   c) Watchdog state from /proc/sys/kernel/nmi_watchdog and watchdog_thresh
 *   d) Panic configuration from /proc/sys/kernel/panic and panic_on_oops
 *   e) kdump readiness from /sys/kernel/kexec_crash_loaded and /proc/cmdline
 *
 * Build:  gcc -Wall -Wextra -Werror -o kernel_health_reader kernel_health_reader.c
 * Run:    ./kernel_health_reader
 *
 * Kernel paths:
 *   /proc/sys/kernel/tainted  → include/linux/panic.h TAINT_* flags
 *   /sys/kernel/kexec_crash_loaded  → kernel/kexec_core.c kexec_crash_loaded()
 */

#define _GNU_SOURCE
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

/* taint flag names — index matches bit position (TAINT_* in include/linux/panic.h) */
static const char *taint_names[] = {
    "P: proprietary module",     /*  0 */
    "F: forced module load",     /*  1 */
    "S: CPU out of spec (overclocking, thermals, hw errata)",     /*  2 */
    "R: forced module rmmod",    /*  3 */
    "M: machine check error",    /*  4 */
    "B: bad page accessed",      /*  5 */
    "U: userspace set taint",    /*  6 */
    "D: kernel oops/BUG",        /*  7 */
    "A: ACPI table overridden",  /*  8 */
    "W: WARN_ON fired",          /*  9 */
    "C: staging driver",         /* 10 */
    "I: firmware workaround",    /* 11 */
    "O: out-of-tree module",     /* 12 */
    "E: unsigned module",        /* 13 */
    "L: soft lockup",            /* 14 */
    "K: livepatch applied",      /* 15 */
    "X: auxiliary taint",        /* 16 */
    "T: randstruct",             /* 17 */
    "N: test module",            /* 18 */
};
#define NTAINT_FLAGS ((int)(sizeof(taint_names) / sizeof(taint_names[0])))

static void read_file(const char *path, char *buf, size_t len)
{
    FILE *f = fopen(path, "r");
    if (!f) {
        snprintf(buf, len, "(unreadable)");
        return;
    }
    if (fgets(buf, (int)len, f) == NULL)
        snprintf(buf, len, "(empty)");
    /* strip trailing newline */
    char *nl = strchr(buf, '\n');
    if (nl) *nl = '\0';
    fclose(f);
}

static void part_a(void)
{
    char ver[256];
    printf("=== Part a: Kernel version (/proc/version) ===\n");
    read_file("/proc/version", ver, sizeof(ver));
    printf("  %s\n", ver);
}

static void part_b(void)
{
    char tbuf[32];
    printf("\n=== Part b: Taint flags (/proc/sys/kernel/tainted) ===\n");
    read_file("/proc/sys/kernel/tainted", tbuf, sizeof(tbuf));
    long taint = strtol(tbuf, NULL, 10);
    printf("  raw value: %ld\n", taint);
    if (taint == 0) {
        printf("  kernel is CLEAN (no taints)\n");
    } else {
        printf("  active taints:\n");
        for (int i = 0; i < NTAINT_FLAGS; i++) {
            if (taint & (1L << i))
                printf("    bit %2d — %s\n", i, taint_names[i]);
        }
    }
}

static void part_c(void)
{
    char nmi[16], thresh[16];
    printf("\n=== Part c: Watchdog state ===\n");
    read_file("/proc/sys/kernel/nmi_watchdog", nmi, sizeof(nmi));
    read_file("/proc/sys/kernel/watchdog_thresh", thresh, sizeof(thresh));
    printf("  nmi_watchdog: %s  (0=disabled, 1=enabled)\n", nmi);
    printf("  watchdog_thresh: %ss  (softlockup at %s×2s, hardlockup at ~%ss)\n",
           thresh, thresh, thresh);
}

static void part_d(void)
{
    char panic_timeout[16], panic_on_oops[16], softlockup_panic[16];
    printf("\n=== Part d: Panic configuration ===\n");
    read_file("/proc/sys/kernel/panic", panic_timeout, sizeof(panic_timeout));
    read_file("/proc/sys/kernel/panic_on_oops", panic_on_oops, sizeof(panic_on_oops));
    read_file("/proc/sys/kernel/softlockup_panic", softlockup_panic, sizeof(softlockup_panic));
    printf("  kernel.panic:           %s  (0=halt, >0=reboot after N secs, <0=immediate)\n",
           panic_timeout);
    printf("  kernel.panic_on_oops:   %s  (0=kill process, 1=panic)\n", panic_on_oops);
    printf("  kernel.softlockup_panic:%s  (0=print only, 1=panic)\n", softlockup_panic);
}

static void part_e(void)
{
    char loaded[16], cmdline[512];
    printf("\n=== Part e: kdump readiness ===\n");
    read_file("/sys/kernel/kexec_crash_loaded", loaded, sizeof(loaded));
    read_file("/proc/cmdline", cmdline, sizeof(cmdline));
    printf("  kexec_crash_loaded: %s  (1=crash kernel ready, 0=no kdump)\n", loaded);
    /* find crashkernel= parameter */
    char *ck = strstr(cmdline, "crashkernel=");
    if (ck) {
        char *end = strchr(ck, ' ');
        if (end) *end = '\0';
        printf("  crashkernel param: %s\n", ck);
    } else {
        printf("  crashkernel param: (not present — kdump not configured)\n");
    }
}

int main(void)
{
    printf("kernel-health-reader — Linux node health state\n\n");
    part_a();
    part_b();
    part_c();
    part_d();
    part_e();
    return 0;
}
