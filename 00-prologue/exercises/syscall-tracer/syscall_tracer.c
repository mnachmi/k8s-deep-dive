#define _GNU_SOURCE
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>
#include <sys/ptrace.h>
#include <sys/types.h>
#include <sys/wait.h>
#include <sys/user.h>
#include <sys/syscall.h>
#include <errno.h>

/* syscall name table — x86_64, partial (covers container-relevant calls) */
static const char *syscall_names[] = {
    [SYS_read]      = "read",
    [SYS_write]     = "write",
    [SYS_open]      = "open",
    [SYS_close]     = "close",
    [SYS_openat]    = "openat",
    [SYS_clone]     = "clone",
    [SYS_fork]      = "fork",
    [SYS_execve]    = "execve",
    [SYS_exit]      = "exit",
    [SYS_exit_group]= "exit_group",
    [SYS_wait4]     = "wait4",
    [SYS_mmap]      = "mmap",
    [SYS_munmap]    = "munmap",
    [SYS_brk]       = "brk",
    [SYS_getpid]    = "getpid",
    [SYS_mount]     = "mount",
    [SYS_unshare]   = "unshare",
    [SYS_setns]     = "setns",
    [SYS_pivot_root]= "pivot_root",
};
#define SYSCALL_NAMES_LEN (sizeof(syscall_names) / sizeof(syscall_names[0]))

static const char *syscall_name(long nr) {
    if (nr >= 0 && (size_t)nr < SYSCALL_NAMES_LEN && syscall_names[nr])
        return syscall_names[nr];
    return "unknown";
}

int main(int argc, char *argv[]) {
    if (argc < 2) {
        fprintf(stderr, "usage: %s <program> [args...]\n", argv[0]);
        return 1;
    }

    pid_t child = fork();
    if (child < 0) {
        perror("fork");
        return 1;
    }

    if (child == 0) {
        /* child: stop itself, then exec the target */
        ptrace(PTRACE_TRACEME, 0, NULL, NULL);
        raise(SIGSTOP);
        execvp(argv[1], argv + 1);
        perror("execvp");
        _exit(1);
    }

    /* parent: wait for child's initial stop, then trace syscalls */
    int status;
    waitpid(child, &status, 0);
    ptrace(PTRACE_SETOPTIONS, child, 0, PTRACE_O_TRACESYSGOOD);

    long syscall_count = 0;
    int in_syscall = 0;

    while (1) {
        ptrace(PTRACE_SYSCALL, child, NULL, NULL);
        waitpid(child, &status, 0);

        if (WIFEXITED(status)) {
            printf("\n--- child exited with %d ---\n", WEXITSTATUS(status));
            printf("total syscalls traced: %ld\n", syscall_count);
            break;
        }

        if (WIFSTOPPED(status) && (WSTOPSIG(status) & 0x80)) {
            struct user_regs_struct regs;
            ptrace(PTRACE_GETREGS, child, NULL, &regs);

            if (!in_syscall) {
                /* syscall entry: rax = syscall number */
                printf("[%5ld] syscall %-16s (nr=%lld)\n",
                       syscall_count++,
                       syscall_name((long)regs.orig_rax),
                       (long long)regs.orig_rax);
                in_syscall = 1;
            } else {
                /* syscall exit: rax = return value */
                in_syscall = 0;
            }
        }
    }

    return 0;
}
