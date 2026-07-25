/*
 * oom_score_demo.c — demonstrate /proc/<pid>/oom_score_adj and observe
 * how the kernel computes the OOM badness score.
 *
 * Demonstrates:
 *   a) Read current oom_score and oom_score_adj for this process
 *   b) Write oom_score_adj values matching kubelet's QoS assignments:
 *        Guaranteed = -998, BestEffort = 1000, Burstable = 500
 *   c) Fork three children with different oom_score_adj values and
 *      print each child's resulting oom_score
 *   d) Show the top-5 processes by oom_score (highest = first killed)
 *
 * Build:  gcc -Wall -Wextra -Werror -o oom_score_demo oom_score_demo.c
 * Run:    sudo ./oom_score_demo   (root needed to write oom_score_adj)
 *
 * Kernel path:
 *   /proc/<pid>/oom_score_adj → fs/proc/base.c:proc_oom_score_adj_write()
 *   /proc/<pid>/oom_score     → fs/proc/base.c:proc_oom_score_read()
 *   https://elixir.bootlin.com/linux/v6.9/source/fs/proc/base.c
 */

#define _GNU_SOURCE
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>
#include <errno.h>
#include <fcntl.h>
#include <sys/types.h>
#include <sys/wait.h>
#include <dirent.h>

static int read_int_file(const char *path)
{
    FILE *f = fopen(path, "r");
    if (!f) return -1;
    int v = -1;
    fscanf(f, "%d", &v);
    fclose(f);
    return v;
}

static int write_int_file(const char *path, int v)
{
    FILE *f = fopen(path, "w");
    if (!f) return -1;
    fprintf(f, "%d\n", v);
    fclose(f);
    return 0;
}

static void show_oom_info(pid_t pid, const char *label)
{
    char path[64];
    snprintf(path, sizeof(path), "/proc/%d/oom_score_adj", pid);
    int adj = read_int_file(path);
    snprintf(path, sizeof(path), "/proc/%d/oom_score", pid);
    int score = read_int_file(path);
    printf("  %-20s pid=%-6d oom_score_adj=%-6d oom_score=%d\n",
           label, pid, adj, score);
}

static void set_oom_adj(pid_t pid, int adj)
{
    char path[64];
    snprintf(path, sizeof(path), "/proc/%d/oom_score_adj", pid);
    if (write_int_file(path, adj) != 0)
        fprintf(stderr, "  warning: cannot write %s: %s\n", path, strerror(errno));
}

static void show_top_oom(int n)
{
    printf("\n=== Top %d processes by oom_score ===\n", n);
    /* Collect (score, pid, comm) for all /proc/<pid> entries */
    DIR *d = opendir("/proc");
    if (!d) { perror("opendir /proc"); return; }

    /* Simple insertion sort into a small fixed array */
    struct entry { int score; int pid; char comm[64]; } top[16] = {0};
    int count = 0;
    if (n > 16) n = 16;

    struct dirent *de;
    while ((de = readdir(d)) != NULL) {
        if (de->d_name[0] < '1' || de->d_name[0] > '9') continue;
        int pid = atoi(de->d_name);
        if (pid <= 0) continue;

        char path[64];
        snprintf(path, sizeof(path), "/proc/%d/oom_score", pid);
        int score = read_int_file(path);
        if (score <= 0) continue;

        snprintf(path, sizeof(path), "/proc/%d/comm", pid);
        char comm[64] = "(unknown)";
        FILE *f = fopen(path, "r");
        if (f) { fgets(comm, sizeof(comm), f); fclose(f); }
        comm[strcspn(comm, "\n")] = '\0';

        /* insert into top[] if score is higher than minimum */
        if (count < n || score > top[count-1].score) {
            int i = (count < n) ? count++ : n - 1;
            top[i].score = score;
            top[i].pid = pid;
            strncpy(top[i].comm, comm, sizeof(top[i].comm) - 1);
            /* bubble up */
            for (; i > 0 && top[i].score > top[i-1].score; i--) {
                struct entry tmp = top[i]; top[i] = top[i-1]; top[i-1] = tmp;
            }
        }
    }
    closedir(d);

    for (int i = 0; i < count; i++)
        printf("  #%-2d oom_score=%-6d pid=%-6d %s\n",
               i+1, top[i].score, top[i].pid, top[i].comm);
}

int main(void)
{
    printf("=== Part a: this process OOM info ===\n");
    show_oom_info(getpid(), "self");

    printf("\n=== Part b: simulate kubelet QoS assignments ===\n");
    printf("  (writing oom_score_adj — requires root)\n");

    /* Fork three children and assign QoS-like oom_score_adj values */
    struct { int adj; const char *label; } qos[] = {
        { -998, "Guaranteed" },
        {  500, "Burstable" },
        { 1000, "BestEffort" },
    };

    printf("\n=== Part c: children with QoS oom_score_adj ===\n");
    for (int i = 0; i < 3; i++) {
        pid_t child = fork();
        if (child == 0) {
            /* child: set adj, sleep briefly so parent can read our score */
            set_oom_adj(getpid(), qos[i].adj);
            usleep(200000);  /* 200 ms */
            _exit(0);
        }
        /* parent: wait a moment then read child score */
        usleep(50000);  /* 50 ms — let child write its adj */
        show_oom_info(child, qos[i].label);
        waitpid(child, NULL, 0);
    }

    show_top_oom(5);

    return 0;
}
