#define _GNU_SOURCE
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>
#include <fcntl.h>
#include <errno.h>
#include <sys/types.h>
#include <sys/wait.h>
#include <sys/stat.h>

#define CGROUP_ROOT  "/sys/fs/cgroup"
#define DEMO_NAME    "cgroup-demo-test"
#define DEMO_PATH    CGROUP_ROOT "/" DEMO_NAME
#define MEM_MAX      (32 * 1024 * 1024)   /* 32 MiB */
#define PID_MAX      10
#define ALLOC_STEP   (4 * 1024 * 1024)    /* 4 MiB per step */
#define ALLOC_STEPS  10

static int cg_write(const char *path, const char *val)
{
    int fd = open(path, O_WRONLY);
    if (fd < 0) {
        fprintf(stderr, "open(%s): %s\n", path, strerror(errno));
        return -1;
    }
    ssize_t n = write(fd, val, strlen(val));
    int saved = errno;
    close(fd);
    if (n < 0) {
        errno = saved;
        fprintf(stderr, "write(%s, %s): %s\n", path, val, strerror(errno));
        return -1;
    }
    return 0;
}

static void cg_read_print(const char *label, const char *path)
{
    char buf[512];
    int fd = open(path, O_RDONLY);
    if (fd < 0) { printf("  %s: (unavailable)\n", label); return; }
    ssize_t n = read(fd, buf, sizeof(buf) - 1);
    close(fd);
    if (n > 0) {
        buf[n] = '\0';
        printf("  %s: %s", label, buf);
        if (buf[n-1] != '\n') printf("\n");
    }
}

static void setup(void)
{
    char path[256], val[64];

    if (mkdir(DEMO_PATH, 0755) < 0 && errno != EEXIST) {
        perror("mkdir " DEMO_PATH); exit(1);
    }
    printf("[setup] %s\n", DEMO_PATH);

    /* Enable controllers at root level (ignore error if already enabled) */
    snprintf(path, sizeof(path), "%s/cgroup.subtree_control", CGROUP_ROOT);
    cg_write(path, "+memory +pids");

    snprintf(val, sizeof(val), "%d", MEM_MAX);
    snprintf(path, sizeof(path), "%s/memory.max", DEMO_PATH);
    if (cg_write(path, val) < 0) exit(1);
    printf("[setup] memory.max = %d MiB\n", MEM_MAX / (1024 * 1024));

    snprintf(val, sizeof(val), "%d", PID_MAX);
    snprintf(path, sizeof(path), "%s/pids.max", DEMO_PATH);
    if (cg_write(path, val) < 0) exit(1);
    printf("[setup] pids.max   = %d\n", PID_MAX);
}

static void show_stats(void)
{
    printf("\n[stats]\n");
    cg_read_print("memory.current", DEMO_PATH "/memory.current");
    cg_read_print("pids.current  ", DEMO_PATH "/pids.current");
    cg_read_print("memory.events ", DEMO_PATH "/memory.events");
}

static void cleanup(void)
{
    if (rmdir(DEMO_PATH) < 0)
        fprintf(stderr, "[cleanup] rmdir %s: %s\n", DEMO_PATH, strerror(errno));
    else
        printf("[cleanup] removed %s\n", DEMO_PATH);
}

static void child_run(void)
{
    char pidstr[32];
    snprintf(pidstr, sizeof(pidstr), "%d", (int)getpid());

    if (cg_write(DEMO_PATH "/cgroup.procs", pidstr) < 0) {
        fprintf(stderr, "[child] failed to join cgroup\n");
        exit(1);
    }
    printf("[child pid=%d] joined %s\n", (int)getpid(), DEMO_PATH);

    /* Show our cgroup assignment */
    char buf[256];
    int fd = open("/proc/self/cgroup", O_RDONLY);
    if (fd >= 0) {
        ssize_t n = read(fd, buf, sizeof(buf) - 1);
        close(fd);
        if (n > 0) { buf[n] = '\0'; printf("[child] /proc/self/cgroup: %s", buf); }
    }

    printf("[child] allocating in %d MiB steps (limit: %d MiB)\n",
           ALLOC_STEP / (1024*1024), MEM_MAX / (1024*1024));

    for (int i = 0; i < ALLOC_STEPS; i++) {
        void *p = malloc(ALLOC_STEP);
        if (!p) {
            printf("[child] malloc returned NULL at step %d — allocator refused\n", i+1);
            break;
        }
        memset(p, 0xAB, ALLOC_STEP);  /* touch pages so kernel accounts them */
        printf("[child] +%d MiB → total ~%d MiB\n",
               ALLOC_STEP / (1024*1024), (i+1) * ALLOC_STEP / (1024*1024));
    }
    printf("[child] done\n");
    exit(0);
}

int main(void)
{
    printf("=== cgroup v2 demo (requires root) ===\n\n");

    setup();

    printf("\n[parent] forking child\n");
    pid_t child = fork();
    if (child < 0) { perror("fork"); return 1; }
    if (child == 0) { child_run(); /* never returns */ }

    printf("[parent] waiting for pid %d\n", (int)child);
    int status;
    if (waitpid(child, &status, 0) < 0) { perror("waitpid"); return 1; }

    if (WIFEXITED(status)) {
        printf("[parent] child exited normally (status=%d)\n", WEXITSTATUS(status));
    } else if (WIFSIGNALED(status)) {
        int sig = WTERMSIG(status);
        printf("[parent] child killed by signal %d%s\n", sig,
               sig == 9 ? " (SIGKILL — OOM killed by cgroup memory controller)" : "");
    }

    show_stats();
    cleanup();
    return 0;
}
