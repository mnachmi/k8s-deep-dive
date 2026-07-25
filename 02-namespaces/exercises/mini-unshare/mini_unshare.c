#define _GNU_SOURCE
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>
#include <sched.h>
#include <sys/types.h>
#include <sys/wait.h>
#include <sys/mount.h>
#include <errno.h>

static const struct {
    const char *name;
    int         flag;
    const char *desc;
} ns_flags[] = {
    { "mount",  CLONE_NEWNS,     "mount namespace"   },
    { "uts",    CLONE_NEWUTS,    "UTS namespace"     },
    { "ipc",    CLONE_NEWIPC,    "IPC namespace"     },
    { "net",    CLONE_NEWNET,    "network namespace" },
    { "pid",    CLONE_NEWPID,    "PID namespace"     },
    { "user",   CLONE_NEWUSER,   "user namespace"    },
    { "cgroup", CLONE_NEWCGROUP, "cgroup namespace"  },
};
#define NS_FLAGS_LEN ((int)(sizeof(ns_flags) / sizeof(ns_flags[0])))

static void usage(const char *prog)
{
    fprintf(stderr, "usage: %s [--<ns>...] -- <command> [args...]\n\n", prog);
    fprintf(stderr, "Namespace flags:\n");
    for (int i = 0; i < NS_FLAGS_LEN; i++)
        fprintf(stderr, "  --%-10s  create new %s\n",
                ns_flags[i].name, ns_flags[i].desc);
    fprintf(stderr, "\nExamples:\n");
    fprintf(stderr, "  sudo %s --uts -- hostname\n", prog);
    fprintf(stderr, "  sudo %s --net -- ip addr\n", prog);
    fprintf(stderr, "  %s --user --pid --mount -- bash\n", prog);
}

static void show_ns(const char *label, const char *types[], int n)
{
    char path[64];
    char target[128];
    ssize_t len;

    printf("%s:\n", label);
    for (int i = 0; i < n; i++) {
        snprintf(path, sizeof(path), "/proc/self/ns/%s", types[i]);
        len = readlink(path, target, sizeof(target) - 1);
        if (len < 0) {
            printf("  %-22s  (unreadable)\n", types[i]);
        } else {
            target[len] = '\0';
            printf("  %-22s  %s\n", types[i], target);
        }
    }
}

int main(int argc, char *argv[])
{
    int  flags   = 0;
    int  cmd_idx = -1;

    for (int i = 1; i < argc; i++) {
        if (strcmp(argv[i], "--") == 0) {
            cmd_idx = i + 1;
            break;
        }
        if (strncmp(argv[i], "--", 2) != 0) {
            fprintf(stderr, "error: expected --<namespace> or --, got '%s'\n",
                    argv[i]);
            usage(argv[0]);
            return 1;
        }
        const char *name  = argv[i] + 2;
        int         found = 0;
        for (int j = 0; j < NS_FLAGS_LEN; j++) {
            if (strcmp(name, ns_flags[j].name) == 0) {
                flags |= ns_flags[j].flag;
                found  = 1;
                break;
            }
        }
        if (!found) {
            fprintf(stderr, "error: unknown namespace '%s'\n", name);
            usage(argv[0]);
            return 1;
        }
    }

    if (flags == 0 || cmd_idx < 0 || cmd_idx >= argc) {
        usage(argv[0]);
        return 1;
    }

    const char *show_types[] = { "net", "uts", "mnt", "ipc", "pid" };
    int         show_n       = (int)(sizeof(show_types) / sizeof(show_types[0]));

    show_ns("[before unshare]", show_types, show_n);

    if (unshare(flags) < 0) {
        perror("unshare");
        if (errno == EPERM)
            fprintf(stderr,
                    "hint: run with sudo, or add --user for unprivileged mode\n");
        return 1;
    }

    show_ns("[after  unshare]", show_types, show_n);

    /*
     * CLONE_NEWPID only affects children, not the calling process.
     * Fork so the exec'd child is born into the new PID namespace
     * and sees itself as PID 1.
     */
    if (flags & CLONE_NEWPID) {
        pid_t child = fork();
        if (child < 0) {
            perror("fork");
            return 1;
        }
        if (child > 0) {
            int status;
            if (waitpid(child, &status, 0) < 0) {
                perror("waitpid");
                return 1;
            }
            return WIFEXITED(status) ? WEXITSTATUS(status) : 1;
        }
        /* child falls through to mount + exec */
    }

    /*
     * Remount /proc so tools like ps(1) work correctly inside the
     * new PID + mount namespace combination.
     */
    if ((flags & CLONE_NEWPID) && (flags & CLONE_NEWNS)) {
        if (mount("proc", "/proc", "proc", 0, NULL) < 0)
            fprintf(stderr,
                    "warning: mount /proc failed (%s) — ps may show stale data\n",
                    strerror(errno));
    }

    execvp(argv[cmd_idx], argv + cmd_idx);
    perror("execvp");
    return 1;
}
