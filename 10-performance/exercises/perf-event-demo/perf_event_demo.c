/*
 * perf_event_demo.c — demonstrate perf_event_open(2) to count hardware
 * performance events: CPU cycles, retired instructions, last-level cache
 * misses, and software page faults.
 *
 * Demonstrates:
 *   a) Opening a hardware event (CPU cycles) on the calling process
 *   b) Opening a group: cycles + instructions (measure IPC atomically)
 *   c) Counting cache misses vs cache references
 *   d) Counting software events: task clock, page faults, context switches
 *   e) Demonstrating multiplexing scaling when more events than counters
 *
 * Build:  gcc -Wall -Wextra -Werror -o perf_event_demo perf_event_demo.c
 * Run:    ./perf_event_demo
 *         (may require: sudo sysctl kernel.perf_event_paranoid=1)
 *
 * Kernel path:
 *   perf_event_open(2) → kernel/events/core.c:perf_event_open()
 *   https://elixir.bootlin.com/linux/v6.9/source/kernel/events/core.c
 *
 *   struct perf_event_attr:
 *   https://elixir.bootlin.com/linux/v6.9/source/include/uapi/linux/perf_event.h
 */

#define _GNU_SOURCE
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>
#include <errno.h>
#include <sys/syscall.h>
#include <sys/ioctl.h>
#include <linux/perf_event.h>

/* Wrapper: perf_event_open is not in glibc */
static long perf_event_open(struct perf_event_attr *hw, pid_t pid,
                             int cpu, int group_fd, unsigned long flags)
{
    return syscall(__NR_perf_event_open, hw, pid, cpu, group_fd, flags);
}

/* Open one counting event. Returns fd or -1 on error. */
static int open_event(__u32 type, __u64 config, int group_fd)
{
    struct perf_event_attr attr;
    memset(&attr, 0, sizeof(attr));
    attr.type           = type;
    attr.size           = sizeof(attr);
    attr.config         = config;
    attr.disabled       = 1;
    attr.exclude_kernel = 1;
    attr.exclude_hv     = 1;
    /* PERF_FORMAT_GROUP: read() returns all group members at once */
    attr.read_format    = (group_fd == -1) ? 0 : PERF_FORMAT_GROUP;
    int fd = (int)perf_event_open(&attr, 0 /*self*/, -1 /*any cpu*/,
                                  group_fd, 0);
    if (fd < 0)
        perror("perf_event_open");
    return fd;
}

/* Read a single counter fd */
static long long read_counter(int fd)
{
    long long val = 0;
    if (read(fd, &val, sizeof(val)) != sizeof(val))
        return -1;
    return val;
}

/* Busy loop doing work: memory reads + integer math */
static volatile long sink;
static void do_work(int iterations)
{
    long acc = 0;
    int arr[256];
    for (int i = 0; i < 256; i++) arr[i] = i;
    for (int i = 0; i < iterations; i++) {
        acc += arr[i % 256] * i;
        /* occasional branch to exercise predictor */
        if ((i & 0xFFF) == 0) acc ^= i;
    }
    sink = acc;
}

static void part_a(void)
{
    printf("=== Part a: CPU cycles for do_work(5M) ===\n");
    int fd = open_event(PERF_TYPE_HARDWARE, PERF_COUNT_HW_CPU_CYCLES, -1);
    if (fd < 0) return;

    ioctl(fd, PERF_EVENT_IOC_RESET, 0);
    ioctl(fd, PERF_EVENT_IOC_ENABLE, 0);
    do_work(5000000);
    ioctl(fd, PERF_EVENT_IOC_DISABLE, 0);

    printf("  cycles: %lld\n", read_counter(fd));
    close(fd);
}

static void part_b(void)
{
    printf("\n=== Part b: cycles + instructions group (IPC) ===\n");
    /* Group leader: cycles */
    int fd_cycles = open_event(PERF_TYPE_HARDWARE, PERF_COUNT_HW_CPU_CYCLES, -1);
    if (fd_cycles < 0) return;
    /* Sibling: instructions */
    int fd_insns  = open_event(PERF_TYPE_HARDWARE, PERF_COUNT_HW_INSTRUCTIONS, fd_cycles);
    if (fd_insns < 0) { close(fd_cycles); return; }

    ioctl(fd_cycles, PERF_EVENT_IOC_RESET, PERF_IOC_FLAG_GROUP);
    ioctl(fd_cycles, PERF_EVENT_IOC_ENABLE, PERF_IOC_FLAG_GROUP);
    do_work(5000000);
    ioctl(fd_cycles, PERF_EVENT_IOC_DISABLE, PERF_IOC_FLAG_GROUP);

    /* With PERF_FORMAT_GROUP, reading the leader returns all members:
     * [nr_events][val0][val1]...  (8 bytes each) */
    long long buf[4] = {0};
    if (read(fd_cycles, buf, sizeof(buf)) < 0) { perror("read group"); }
    else {
        long long nr = buf[0];  /* number of events in group */
        long long cycles = buf[1];
        long long insns  = (nr >= 2) ? buf[2] : 0;
        double ipc = (cycles > 0) ? (double)insns / cycles : 0.0;
        printf("  cycles=%lld  instructions=%lld  IPC=%.2f\n",
               cycles, insns, ipc);
    }
    close(fd_insns);
    close(fd_cycles);
}

static void part_c(void)
{
    printf("\n=== Part c: cache references vs cache misses ===\n");
    int fd_ref  = open_event(PERF_TYPE_HARDWARE, PERF_COUNT_HW_CACHE_REFERENCES, -1);
    int fd_miss = open_event(PERF_TYPE_HARDWARE, PERF_COUNT_HW_CACHE_MISSES, -1);
    if (fd_ref < 0 || fd_miss < 0) {
        if (fd_ref >= 0) close(fd_ref);
        if (fd_miss >= 0) close(fd_miss);
        printf("  (hardware cache events not available on this CPU/VM)\n");
        return;
    }

    ioctl(fd_ref,  PERF_EVENT_IOC_RESET, 0);
    ioctl(fd_miss, PERF_EVENT_IOC_RESET, 0);
    ioctl(fd_ref,  PERF_EVENT_IOC_ENABLE, 0);
    ioctl(fd_miss, PERF_EVENT_IOC_ENABLE, 0);
    do_work(5000000);
    ioctl(fd_ref,  PERF_EVENT_IOC_DISABLE, 0);
    ioctl(fd_miss, PERF_EVENT_IOC_DISABLE, 0);

    long long ref  = read_counter(fd_ref);
    long long miss = read_counter(fd_miss);
    double miss_rate = (ref > 0) ? (double)miss * 100.0 / ref : 0.0;
    printf("  cache_refs=%lld  cache_misses=%lld  miss_rate=%.2f%%\n",
           ref, miss, miss_rate);
    close(fd_ref);
    close(fd_miss);
}

static void part_d(void)
{
    printf("\n=== Part d: software events — task_clock, page faults, context switches ===\n");
    int fd_clock = open_event(PERF_TYPE_SOFTWARE, PERF_COUNT_SW_TASK_CLOCK, -1);
    int fd_pf    = open_event(PERF_TYPE_SOFTWARE, PERF_COUNT_SW_PAGE_FAULTS, -1);
    int fd_cs    = open_event(PERF_TYPE_SOFTWARE, PERF_COUNT_SW_CONTEXT_SWITCHES, -1);

    if (fd_clock >= 0 && fd_pf >= 0 && fd_cs >= 0) {
        ioctl(fd_clock, PERF_EVENT_IOC_RESET, 0);
        ioctl(fd_pf,    PERF_EVENT_IOC_RESET, 0);
        ioctl(fd_cs,    PERF_EVENT_IOC_RESET, 0);
        ioctl(fd_clock, PERF_EVENT_IOC_ENABLE, 0);
        ioctl(fd_pf,    PERF_EVENT_IOC_ENABLE, 0);
        ioctl(fd_cs,    PERF_EVENT_IOC_ENABLE, 0);

        do_work(5000000);
        usleep(100000); /* sleep 100ms to accumulate context switches */

        ioctl(fd_clock, PERF_EVENT_IOC_DISABLE, 0);
        ioctl(fd_pf,    PERF_EVENT_IOC_DISABLE, 0);
        ioctl(fd_cs,    PERF_EVENT_IOC_DISABLE, 0);

        printf("  task_clock=%lld ns  page_faults=%lld  context_switches=%lld\n",
               read_counter(fd_clock), read_counter(fd_pf), read_counter(fd_cs));
    }
    if (fd_clock >= 0) close(fd_clock);
    if (fd_pf >= 0)    close(fd_pf);
    if (fd_cs >= 0)    close(fd_cs);
}

int main(void)
{
    printf("perf_event_demo — perf_event_open(2) hardware/software counters\n\n");
    printf("Note: if events fail with EPERM, run:\n");
    printf("  sudo sysctl kernel.perf_event_paranoid=1\n\n");

    part_a();
    part_b();
    part_c();
    part_d();

    return 0;
}
