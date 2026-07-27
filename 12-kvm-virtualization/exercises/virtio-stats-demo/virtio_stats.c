/*
 * virtio_stats.c — Read virtio device statistics from sysfs.
 *
 * Demonstrates how to enumerate virtio devices from a KVM guest and read
 * their statistics via the sysfs virtio bus interface. Each virtio device
 * (net, blk, balloon, rng) appears under /sys/bus/virtio/devices/virtioN/.
 *
 * Build: gcc -O2 -Wall -o virtio_stats virtio_stats.c
 * Run:   ./virtio_stats
 */

#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <dirent.h>
#include <fcntl.h>
#include <unistd.h>
#include <errno.h>
#include <sys/stat.h>

#define VIRTIO_BUS_PATH     "/sys/bus/virtio/devices"
#define NET_STATS_PATH      "/sys/class/net"
#define MAX_DEVICES         32
#define MAX_PATH_LEN        512

/* virtio device IDs from the virtio spec */
#define VIRTIO_ID_NET       1
#define VIRTIO_ID_BLOCK     2
#define VIRTIO_ID_BALLOON   5
#define VIRTIO_ID_CONSOLE   3
#define VIRTIO_ID_RNG       4
#define VIRTIO_ID_9P        9

struct virtio_dev {
    char name[64];          /* e.g. "virtio0" */
    int  device_id;         /* from sysfs device file */
    char driver[64];        /* e.g. "virtio_net", "virtio_blk" */
    char iommu_group[16];
};

static int read_sysfs_int(const char *path)
{
    FILE *f = fopen(path, "r");
    if (!f)
        return -1;
    int val = -1;
    fscanf(f, "%i", &val);
    fclose(f);
    return val;
}

static int read_sysfs_str(const char *path, char *buf, size_t len)
{
    FILE *f = fopen(path, "r");
    if (!f)
        return -1;
    if (!fgets(buf, (int)len, f)) {
        fclose(f);
        return -1;
    }
    fclose(f);
    /* strip newline */
    size_t n = strlen(buf);
    if (n > 0 && buf[n-1] == '\n')
        buf[n-1] = '\0';
    return 0;
}

static const char *virtio_id_name(int id)
{
    switch (id) {
    case VIRTIO_ID_NET:     return "virtio-net";
    case VIRTIO_ID_BLOCK:   return "virtio-blk";
    case VIRTIO_ID_BALLOON: return "virtio-balloon";
    case VIRTIO_ID_CONSOLE: return "virtio-console";
    case VIRTIO_ID_RNG:     return "virtio-rng";
    case VIRTIO_ID_9P:      return "virtio-9p";
    default:                return "virtio-unknown";
    }
}

static void print_separator(void)
{
    printf("──────────────────────────────────────────────────────────\n");
}

/* For virtio-net: read NIC statistics from /sys/class/net/<ifname>/statistics/ */
static void print_net_stats(const char *devname)
{
    /* Find which network interface uses this virtio device.
     * /sys/class/net/<ifname>/device symlink points to the PCI/virtio device. */
    DIR *netdir = opendir(NET_STATS_PATH);
    if (!netdir)
        return;

    struct dirent *entry;
    while ((entry = readdir(netdir)) != NULL) {
        if (entry->d_name[0] == '.')
            continue;

        char devlink[MAX_PATH_LEN];
        snprintf(devlink, sizeof(devlink), "%s/%s/device", NET_STATS_PATH, entry->d_name);

        char target[MAX_PATH_LEN];
        ssize_t n = readlink(devlink, target, sizeof(target) - 1);
        if (n < 0)
            continue;
        target[n] = '\0';

        /* Check if this NIC's device symlink contains our virtio device name */
        if (!strstr(target, devname))
            continue;

        printf("  Network Interface: %s\n", entry->d_name);

        struct {
            const char *stat;
            const char *label;
        } stats[] = {
            {"rx_packets",      "RX packets"},
            {"tx_packets",      "TX packets"},
            {"rx_bytes",        "RX bytes"},
            {"tx_bytes",        "TX bytes"},
            {"rx_dropped",      "RX dropped"},
            {"tx_dropped",      "TX dropped"},
            {"rx_errors",       "RX errors"},
            {"tx_errors",       "TX errors"},
        };

        char statpath[MAX_PATH_LEN];
        char val[64];
        for (size_t i = 0; i < sizeof(stats)/sizeof(stats[0]); i++) {
            snprintf(statpath, sizeof(statpath), "%s/%s/statistics/%s",
                     NET_STATS_PATH, entry->d_name, stats[i].stat);
            if (read_sysfs_str(statpath, val, sizeof(val)) == 0)
                printf("    %-20s: %s\n", stats[i].label, val);
        }
        break;
    }
    closedir(netdir);
}

/* For virtio-balloon: read balloon page count and MemAvailable */
static void print_balloon_stats(void)
{
    FILE *f = fopen("/proc/meminfo", "r");
    if (!f)
        return;

    char line[256];
    unsigned long balloon_kb = 0, memavail_kb = 0, memtotal_kb = 0;

    while (fgets(line, sizeof(line), f)) {
        if (strncmp(line, "MemTotal:", 9) == 0)
            sscanf(line + 9, "%lu", &memtotal_kb);
        else if (strncmp(line, "MemAvailable:", 13) == 0)
            sscanf(line + 13, "%lu", &memavail_kb);
        else if (strncmp(line, "Balloon:", 8) == 0)
            sscanf(line + 8, "%lu", &balloon_kb);
    }
    fclose(f);

    printf("  Balloon pages held : %lu kB (%lu MB)\n",
           balloon_kb, balloon_kb / 1024);
    printf("  MemAvailable       : %lu kB (%lu MB)\n",
           memavail_kb, memavail_kb / 1024);
    if (memtotal_kb > 0) {
        double pct = (double)balloon_kb / (double)memtotal_kb * 100.0;
        printf("  Balloon/Total RAM  : %.1f%%", pct);
        if (pct > 25.0)
            printf("  🔴 CRITICAL: host reclaiming >25%% of guest RAM");
        else if (pct > 10.0)
            printf("  ⚠  WARNING");
        else
            printf("  ✓  OK");
        printf("\n");
    }
}

/* For virtio-blk: read request statistics */
static void print_blk_stats(const char *devname)
{
    /* virtio-blk exposes block device under /sys/block/vdX */
    DIR *blkdir = opendir("/sys/block");
    if (!blkdir)
        return;

    struct dirent *entry;
    while ((entry = readdir(blkdir)) != NULL) {
        if (entry->d_name[0] == '.')
            continue;
        if (strncmp(entry->d_name, "vd", 2) != 0)
            continue;

        char devlink[MAX_PATH_LEN];
        snprintf(devlink, sizeof(devlink), "/sys/block/%s/device", entry->d_name);
        char target[MAX_PATH_LEN];
        ssize_t n = readlink(devlink, target, sizeof(target) - 1);
        if (n < 0)
            continue;
        target[n] = '\0';

        if (!strstr(target, devname))
            continue;

        printf("  Block Device : /dev/%s\n", entry->d_name);

        char statpath[MAX_PATH_LEN];
        snprintf(statpath, sizeof(statpath), "/sys/block/%s/stat", entry->d_name);
        FILE *f = fopen(statpath, "r");
        if (f) {
            unsigned long rd_ios, rd_merges, rd_sectors, rd_ticks;
            unsigned long wr_ios, wr_merges, wr_sectors, wr_ticks;
            fscanf(f, "%lu %lu %lu %lu %lu %lu %lu %lu",
                   &rd_ios, &rd_merges, &rd_sectors, &rd_ticks,
                   &wr_ios, &wr_merges, &wr_sectors, &wr_ticks);
            fclose(f);
            printf("    Read  I/Os  : %lu (%lu sectors, %lu ms)\n",
                   rd_ios, rd_sectors, rd_ticks);
            printf("    Write I/Os  : %lu (%lu sectors, %lu ms)\n",
                   wr_ios, wr_sectors, wr_ticks);
        }
        break;
    }
    closedir(blkdir);
}

int main(void)
{
    printf("virtio Device Statistics — KVM Guest\n");
    print_separator();

    /* Check if we're in a VM by looking for virtio bus */
    struct stat st;
    if (stat(VIRTIO_BUS_PATH, &st) != 0 || !S_ISDIR(st.st_mode)) {
        fprintf(stderr, "Error: %s not found.\n", VIRTIO_BUS_PATH);
        fprintf(stderr, "This tool must run inside a KVM/virtio guest.\n");
        fprintf(stderr, "Not detected: either running on bare metal or non-virtio hypervisor.\n");
        return 1;
    }

    DIR *dir = opendir(VIRTIO_BUS_PATH);
    if (!dir) {
        perror("opendir " VIRTIO_BUS_PATH);
        return 1;
    }

    struct dirent *entry;
    int found = 0;

    while ((entry = readdir(dir)) != NULL) {
        if (entry->d_name[0] == '.')
            continue;
        if (strncmp(entry->d_name, "virtio", 6) != 0)
            continue;

        struct virtio_dev dev = {0};
        strncpy(dev.name, entry->d_name, sizeof(dev.name) - 1);

        char path[MAX_PATH_LEN];

        /* Read device ID (hex) */
        snprintf(path, sizeof(path), "%s/%s/device", VIRTIO_BUS_PATH, entry->d_name);
        char devid_str[16] = {0};
        read_sysfs_str(path, devid_str, sizeof(devid_str));
        dev.device_id = (int)strtol(devid_str, NULL, 16);

        /* Read driver name via symlink */
        snprintf(path, sizeof(path), "%s/%s/driver", VIRTIO_BUS_PATH, entry->d_name);
        char drv_target[MAX_PATH_LEN];
        ssize_t n = readlink(path, drv_target, sizeof(drv_target) - 1);
        if (n > 0) {
            drv_target[n] = '\0';
            strncpy(dev.driver, strrchr(drv_target, '/') + 1, sizeof(dev.driver) - 1);
        }

        printf("\nDevice: %s\n", dev.name);
        printf("  Type   : %s (ID 0x%04x)\n", virtio_id_name(dev.device_id), dev.device_id);
        printf("  Driver : %s\n", dev.driver[0] ? dev.driver : "(not bound)");

        /* Per-device type stats */
        switch (dev.device_id) {
        case VIRTIO_ID_NET:
            print_net_stats(dev.name);
            break;
        case VIRTIO_ID_BALLOON:
            print_balloon_stats();
            break;
        case VIRTIO_ID_BLOCK:
            print_blk_stats(dev.name);
            break;
        default:
            printf("  (no additional stats available for this device type)\n");
            break;
        }

        found++;
    }
    closedir(dir);

    if (found == 0) {
        printf("No virtio devices found under %s\n", VIRTIO_BUS_PATH);
        printf("Ensure virtio drivers are loaded: lsmod | grep virtio\n");
    }

    print_separator();
    printf("\nKernel source references:\n");
    printf("  struct virtio_device: include/linux/virtio.h\n");
    printf("  struct virtqueue:     include/linux/virtio.h\n");
    printf("  virtio_net driver:    drivers/net/virtio_net.c\n");
    printf("  virtio_blk driver:    drivers/block/virtio_blk.c\n");
    printf("  virtio_balloon:       drivers/virtio/virtio_balloon.c\n");
    printf("  https://elixir.bootlin.com/linux/v6.9/source/include/linux/virtio.h\n");

    return 0;
}
