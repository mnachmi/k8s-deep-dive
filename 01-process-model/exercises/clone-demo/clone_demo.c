#define _GNU_SOURCE
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>
#include <sched.h>
#include <sys/types.h>
#include <sys/wait.h>
#include <sys/utsname.h>
#include <errno.h>

#define STACK_SIZE (1024 * 1024)  /* 1 MiB child stack */

/* The function that runs in the new namespaces */
static int child_fn(void *arg) {
    (void)arg;

    printf("[child] PID inside new PID namespace: %d\n", getpid());
    printf("[child] PPID inside new PID namespace: %d\n", getppid());

    /* Set a new hostname in our UTS namespace */
    if (sethostname("container-demo", 14) < 0) {
        perror("sethostname");
        return 1;
    }

    struct utsname uts;
    uname(&uts);
    printf("[child] hostname in new UTS namespace: %s\n", uts.nodename);

    /* Show our /proc/self/ns links */
    char buf[256];
    ssize_t n;

    n = readlink("/proc/self/ns/pid", buf, sizeof(buf) - 1);
    if (n > 0) { buf[n] = '\0'; printf("[child] /proc/self/ns/pid -> %s\n", buf); }

    n = readlink("/proc/self/ns/uts", buf, sizeof(buf) - 1);
    if (n > 0) { buf[n] = '\0'; printf("[child] /proc/self/ns/uts -> %s\n", buf); }

    printf("[child] sleeping 2s so parent can observe us...\n");
    sleep(2);

    return 0;
}

int main(void) {
    char *stack = malloc(STACK_SIZE);
    if (!stack) { perror("malloc"); return 1; }

    char *stack_top = stack + STACK_SIZE;  /* stack grows downward */

    printf("[parent] my PID: %d\n", getpid());

    struct utsname uts;
    uname(&uts);
    printf("[parent] my hostname: %s\n", uts.nodename);

    /* clone() with new PID and UTS namespaces */
    pid_t child_pid = clone(child_fn, stack_top,
                            CLONE_NEWPID | CLONE_NEWUTS | SIGCHLD,
                            NULL);
    if (child_pid < 0) {
        perror("clone");
        free(stack);
        return 1;
    }

    printf("[parent] child host-PID (as seen from parent namespace): %d\n", child_pid);
    printf("[parent] notice: child thinks it is PID 1, parent sees it as PID %d\n", child_pid);

    /* While child sleeps, show its namespace links from the parent */
    sleep(1);
    char path[256], ns_link[256];
    snprintf(path, sizeof(path), "/proc/%d/ns/pid", child_pid);
    ssize_t n = readlink(path, ns_link, sizeof(ns_link) - 1);
    if (n > 0) {
        ns_link[n] = '\0';
        printf("[parent] child /proc/%d/ns/pid -> %s\n", child_pid, ns_link);
    }

    int status;
    waitpid(child_pid, &status, 0);
    printf("[parent] child exited with %d\n", WEXITSTATUS(status));

    free(stack);
    return 0;
}
