#define _GNU_SOURCE
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>
#include <sys/mman.h>

#define MB  (1024UL * 1024UL)
#define ALLOC_SIZE (64 * MB)

static long get_rss_kb(void)
{
    FILE *f = fopen("/proc/self/statm", "r");
    if (!f) return -1;
    long dummy = 0, pages = 0;
    /* field 2: resident set size in pages */
    (void)fscanf(f, "%ld %ld", &dummy, &pages);
    fclose(f);
    return pages * (sysconf(_SC_PAGESIZE) / 1024);
}

static long get_minor_faults(void)
{
    FILE *f = fopen("/proc/self/stat", "r");
    if (!f) return -1;
    long minflt = 0;
    /* field 10 in /proc/self/stat is minflt */
    (void)fscanf(f, "%*d %*s %*c %*d %*d %*d %*d %*d %*u %ld", &minflt);
    fclose(f);
    return minflt;
}

static void print_fault_stats(const char *label, long faults_before, long rss_before)
{
    long faults_after = get_minor_faults();
    long rss_after    = get_rss_kb();
    printf("  %-38s  faults=%-6ld  RSS delta=%ld KiB\n",
           label,
           faults_after - faults_before,
           rss_after - rss_before);
}

int main(void)
{
    printf("=== Page Fault Demo ===\n");
    printf("Allocation size: %lu MiB\n\n", ALLOC_SIZE / MB);

    /* Demo 1: malloc without touching */
    printf("[1] malloc %lu MiB (no touch)\n", ALLOC_SIZE / MB);
    long f0 = get_minor_faults(), r0 = get_rss_kb();
    void *p1 = malloc(ALLOC_SIZE);
    if (!p1) { perror("malloc"); return 1; }
    print_fault_stats("after malloc (no touch)", f0, r0);

    /* Demo 2: touch all pages */
    printf("[2] touch all pages (memset)\n");
    f0 = get_minor_faults(); r0 = get_rss_kb();
    memset(p1, 0xAB, ALLOC_SIZE);
    print_fault_stats("after memset (all pages touched)", f0, r0);
    free(p1);

    /* Demo 3: mmap anonymous + touch */
    printf("[3] mmap(MAP_ANONYMOUS) %lu MiB + touch\n", ALLOC_SIZE / MB);
    f0 = get_minor_faults(); r0 = get_rss_kb();
    void *p2 = mmap(NULL, ALLOC_SIZE, PROT_READ | PROT_WRITE,
                    MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
    if (p2 == MAP_FAILED) { perror("mmap anon"); return 1; }
    char *cp = (char *)p2;
    for (size_t i = 0; i < ALLOC_SIZE; i += 4096)
        cp[i] = (char)(i & 0xFF);
    print_fault_stats("after mmap anon + touch", f0, r0);
    munmap(p2, ALLOC_SIZE);

    /* Demo 4: mmap + MADV_HUGEPAGE hint + touch */
    printf("[4] mmap + MADV_HUGEPAGE (THP hint) + touch\n");
    f0 = get_minor_faults(); r0 = get_rss_kb();
    void *p3 = mmap(NULL, ALLOC_SIZE, PROT_READ | PROT_WRITE,
                    MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
    if (p3 == MAP_FAILED) { perror("mmap huge"); return 1; }
    if (madvise(p3, ALLOC_SIZE, MADV_HUGEPAGE) < 0)
        fprintf(stderr, "  (MADV_HUGEPAGE not available)\n");
    memset(p3, 0xCD, ALLOC_SIZE);
    print_fault_stats("after mmap + MADV_HUGEPAGE + touch", f0, r0);
    munmap(p3, ALLOC_SIZE);

    /* Demo 5: mmap + touch + MADV_DONTNEED + re-touch */
    printf("[5] mmap + touch + MADV_DONTNEED (release pages) + re-touch\n");
    void *p4 = mmap(NULL, ALLOC_SIZE, PROT_READ | PROT_WRITE,
                    MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
    if (p4 == MAP_FAILED) { perror("mmap dontneed"); return 1; }
    memset(p4, 0xEF, ALLOC_SIZE);
    long rss_after_touch = get_rss_kb();
    madvise(p4, ALLOC_SIZE, MADV_DONTNEED);
    long rss_after_dontneed = get_rss_kb();
    printf("  RSS after touch:         %ld KiB\n", rss_after_touch);
    printf("  RSS after MADV_DONTNEED: %ld KiB\n", rss_after_dontneed);
    printf("  Pages released: ~%ld KiB\n", rss_after_touch - rss_after_dontneed);
    f0 = get_minor_faults(); r0 = get_rss_kb();
    memset(p4, 0xEF, ALLOC_SIZE);
    print_fault_stats("re-touch after MADV_DONTNEED", f0, r0);
    munmap(p4, ALLOC_SIZE);

    printf("\nNote: minor faults = demand paging from RAM (zero-fill, CoW)\n");
    printf("      major faults = disk reads (swap-in, file read)\n");
    printf("      check /proc/self/stat fields 10 (minflt) and 12 (majflt)\n");
    return 0;
}
