/*
 * sched_affinity_demo.c — demonstrate sched_setaffinity(2) and observe
 * the kernel scheduler obeying cpus_mask.
 *
 * Demonstrates:
 *   a) Pinning the main process to CPU 0 with sched_setaffinity(2)
 *   b) Spawning two threads: one pinned to CPU 0, one pinned to CPU 1
 *   c) Reading /proc/<tid>/status to verify Cpus_allowed_list (uses SYS_gettid, not /proc/self)
 *   d) Printing nice-to-weight table for nice values -5 to +5
 *   e) Printing the current task's scheduling policy and priority
 *
 * Build:  gcc -Wall -Wextra -Werror -o sched_affinity_demo sched_affinity_demo.c -lpthread
 * Run:    ./sched_affinity_demo
 *
 * Kernel path:
 *   sched_setaffinity(2) -> kernel/sched/core.c:sched_setaffinity()
 *   https://elixir.bootlin.com/linux/v6.9/source/kernel/sched/core.c
 */

#define _GNU_SOURCE
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>
#include <pthread.h>
#include <sched.h>
#include <errno.h>
#include <sys/resource.h>
#include <sys/syscall.h>
#include <sys/types.h>

/* Kernel nice-to-weight table (sched_prio_to_weight[], kernel/sched/core.c)
 * Index 0 = nice -20, index 39 = nice +19.
 * Only show nice -5..+5 (indices 15..25). */
static const int nice_to_weight[] = {
    /* nice -5 */ 3121,
    /* nice -4 */ 2501,
    /* nice -3 */ 1991,
    /* nice -2 */ 1586,
    /* nice -1 */ 1277,
    /* nice  0 */ 1024,
    /* nice +1 */  820,
    /* nice +2 */  655,
    /* nice +3 */  526,
    /* nice +4 */  423,
    /* nice +5 */  335,
};

static void print_cpus_allowed(const char *label)
{
    char buf[256];
    /* Use /proc/<tid>/status, not /proc/self/status — /proc/self is the
     * process (tgid), which always shows the main thread's affinity. */
    pid_t tid = (pid_t)syscall(SYS_gettid);
    snprintf(buf, sizeof(buf), "/proc/%d/status", tid);
    FILE *f = fopen(buf, "r");
    if (!f) { perror("fopen /proc/<tid>/status"); return; }
    while (fgets(buf, sizeof(buf), f)) {
        if (strncmp(buf, "Cpus_allowed_list:", 18) == 0) {
            printf("  [%s] Cpus_allowed_list: %s", label, buf + 18);
            break;
        }
    }
    fclose(f);
}

static void pin_to_cpu(int cpu)
{
    cpu_set_t set;
    CPU_ZERO(&set);
    CPU_SET(cpu, &set);
    if (sched_setaffinity(0, sizeof(set), &set) != 0) {
        perror("sched_setaffinity");
        exit(1);
    }
}

struct thread_arg { int cpu; int id; };

static void *thread_fn(void *arg)
{
    struct thread_arg *a = arg;
    pin_to_cpu(a->cpu);
    /* sched_getcpu() uses the VDSO getcpu fast path on x86 */
    int actual = sched_getcpu();
    printf("  Thread %d: pinned to CPU %d, actually running on CPU %d\n",
           a->id, a->cpu, actual);
    print_cpus_allowed("thread");
    return NULL;
}

int main(void)
{
    int nprocs = (int)sysconf(_SC_NPROCESSORS_ONLN);
    printf("Online CPUs: %d\n\n", nprocs);

    /* --- Part 1: pin main to CPU 0 --- */
    printf("=== Part 1: pin main to CPU 0 ===\n");
    pin_to_cpu(0);
    printf("  Running on CPU %d\n", sched_getcpu());
    print_cpus_allowed("main");

    /* --- Part 2: two threads on separate CPUs --- */
    printf("\n=== Part 2: two threads, CPU 0 and CPU %d ===\n",
           nprocs > 1 ? 1 : 0);
    pthread_t t1, t2;
    struct thread_arg a1 = {0, 1};
    struct thread_arg a2 = {nprocs > 1 ? 1 : 0, 2};
    pthread_create(&t1, NULL, thread_fn, &a1);
    pthread_create(&t2, NULL, thread_fn, &a2);
    pthread_join(t1, NULL);
    pthread_join(t2, NULL);

    /* --- Part 3: nice-to-weight table --- */
    printf("\n=== Part 3: nice → CFS weight (kernel/sched/core.c sched_prio_to_weight) ===\n");
    for (int i = 0; i <= 10; i++) {
        printf("  nice %+3d → weight %5d\n", i - 5, nice_to_weight[i]);
    }

    /* --- Part 4: current scheduling policy --- */
    printf("\n=== Part 4: current scheduling policy ===\n");
    int policy = sched_getscheduler(0);
    struct sched_param param;
    sched_getparam(0, &param);
    const char *policy_name =
        policy == SCHED_OTHER   ? "SCHED_OTHER (CFS)" :
        policy == SCHED_FIFO    ? "SCHED_FIFO (RT)" :
        policy == SCHED_RR      ? "SCHED_RR (RT)" :
        policy == SCHED_BATCH   ? "SCHED_BATCH" :
        policy == SCHED_IDLE    ? "SCHED_IDLE" :
        policy == SCHED_DEADLINE ? "SCHED_DEADLINE" : "UNKNOWN";
    printf("  policy=%s sched_priority=%d\n", policy_name, param.sched_priority);
    printf("  nice=%d\n", getpriority(PRIO_PROCESS, 0));

    return 0;
}
