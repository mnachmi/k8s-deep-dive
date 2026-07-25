#define _GNU_SOURCE
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>
#include <errno.h>
#include <stdint.h>

#include <sys/syscall.h>
#include <sys/socket.h>
#include <sys/types.h>
#include <netinet/in.h>
#include <arpa/inet.h>

#include <linux/bpf.h>
#include <linux/bpf_common.h>

/*
 * bpf_syscall_demo.c  —  Raw bpf(2) syscall: map + prog + socket filter
 *
 * Demonstrates the bpf(2) syscall directly using only linux/bpf.h and gcc —
 * no libbpf, no clang/LLVM.  The sequence is:
 *
 *   1. BPF_MAP_CREATE  — create a BPF_MAP_TYPE_ARRAY (key=u32, value=u64)
 *   2. BPF_PROG_LOAD   — load a minimal socket-filter program that:
 *                          a) looks up map[0]
 *                          b) increments the counter by 1
 *                          c) returns 1 (SK_PASS — accept the packet)
 *   3. SO_ATTACH_BPF   — attach the program to a UDP receive socket
 *   4. send/recv       — trigger the filter with one loopback UDP packet
 *   5. BPF_MAP_LOOKUP_ELEM — read map[0] back from userspace and print it
 *
 * Kernel path (Linux v6.9):
 *   syscall(SYS_bpf) -> __sys_bpf() -> bpf_prog_load() -> bpf_check() -> JIT
 *   https://elixir.bootlin.com/linux/v6.9/source/kernel/bpf/syscall.c
 *   https://elixir.bootlin.com/linux/v6.9/source/kernel/bpf/verifier.c
 *
 * Requires: root or CAP_BPF + CAP_NET_ADMIN
 */

/* -------------------------------------------------------------------------
 * eBPF instruction-building macros
 *
 * linux/bpf.h defines struct bpf_insn and the raw opcode constants but does
 * NOT ship the convenience macros (those live in tools/include/linux/filter.h
 * in the kernel tree).  We re-define the subset we need here.
 * ------------------------------------------------------------------------- */

/* Opcode bytes not in bpf_common.h for eBPF */
#define BPF_DW      0x18   /* 64-bit load/store */
#define BPF_ALU64   0x07   /* 64-bit ALU class  */
#define BPF_JMP     0x05   /* jump class        */
#define BPF_JMP32   0x06   /* 32-bit jump class */
#define BPF_MOV     0xb0   /* move opcode       */
#define BPF_ADD     0x00
#define BPF_JEQ     0x10
#define BPF_CALL    0x80
#define BPF_EXIT    0x90
#define BPF_K       0x00
#define BPF_X       0x08
#define BPF_MEM     0x60
#define BPF_IMM     0x00

/* Build a raw bpf_insn */
#define BPF_INSN(CODE, DST, SRC, OFF, IMM) \
    ((struct bpf_insn){ .code = (CODE), .dst_reg = (DST), \
                        .src_reg = (SRC), .off = (OFF), .imm = (IMM) })

/* 64-bit ALU: dst = imm */
#define BPF_MOV64_IMM(DST, IMM) \
    BPF_INSN(BPF_ALU64 | BPF_MOV | BPF_K, (DST), 0, 0, (IMM))

/* 64-bit ALU: dst = src */
#define BPF_MOV64_REG(DST, SRC) \
    BPF_INSN(BPF_ALU64 | BPF_MOV | BPF_X, (DST), (SRC), 0, 0)

/* 64-bit ALU: dst += imm */
#define BPF_ALU64_IMM(OP, DST, IMM) \
    BPF_INSN(BPF_ALU64 | (OP) | BPF_K, (DST), 0, 0, (IMM))

/* Store immediate word to stack: *(u32 *)(dst + off) = imm */
#define BPF_ST_MEM(SIZE, DST, OFF, IMM) \
    BPF_INSN(BPF_ST | BPF_MEM | (SIZE), (DST), 0, (OFF), (IMM))

/* Load from memory: dst = *(size *)(src + off) */
#define BPF_LDX_MEM(SIZE, DST, SRC, OFF) \
    BPF_INSN(BPF_LDX | BPF_MEM | (SIZE), (DST), (SRC), (OFF), 0)

/* Store register to memory: *(size *)(dst + off) = src */
#define BPF_STX_MEM(SIZE, DST, SRC, OFF) \
    BPF_INSN(BPF_STX | BPF_MEM | (SIZE), (DST), (SRC), (OFF), 0)

/* Jump: if dst == imm, skip off instructions */
#define BPF_JMP_IMM(OP, DST, IMM, OFF) \
    BPF_INSN(BPF_JMP | (OP) | BPF_K, (DST), 0, (OFF), (IMM))

/* Call helper: BPF_FUNC_xxx */
#define BPF_EMIT_CALL(FUNC) \
    BPF_INSN(BPF_JMP | BPF_CALL, 0, 0, 0, (FUNC))

/* Exit instruction */
#define BPF_EXIT_INSN() \
    BPF_INSN(BPF_JMP | BPF_EXIT, 0, 0, 0, 0)

/*
 * BPF_LD_MAP_FD — load a map file descriptor into a register.
 *
 * This is a "wide" (128-bit / two-instruction) load using BPF_LD | BPF_DW.
 * The first insn carries the lower 32 bits of the fd in .imm; the second
 * insn (a zero-imm filler) carries the upper 32 bits.  For an fd value that
 * fits in 32 bits the upper word is zero.
 *
 * src_reg = BPF_PSEUDO_MAP_FD tells the verifier this immediate is a map fd.
 */
#define BPF_LD_MAP_FD(DST, MAP_FD)                                      \
    BPF_INSN(BPF_LD | BPF_DW | BPF_IMM, (DST), BPF_PSEUDO_MAP_FD, 0,  \
             (int)(MAP_FD)),                                             \
    BPF_INSN(0, 0, 0, 0, 0)   /* upper 32 bits = 0 */

/* -------------------------------------------------------------------------
 * Thin wrapper around the bpf(2) syscall
 * ------------------------------------------------------------------------- */
static long bpf_syscall(enum bpf_cmd cmd, union bpf_attr *attr, unsigned int size)
{
    return syscall(SYS_bpf, cmd, attr, size);
}

/* -------------------------------------------------------------------------
 * step 1 — BPF_MAP_CREATE
 *
 * Create a BPF_MAP_TYPE_ARRAY with:
 *   key_size    = 4  (u32 index)
 *   value_size  = 8  (u64 counter)
 *   max_entries = 8
 * ------------------------------------------------------------------------- */
static int create_map(void)
{
    union bpf_attr attr;

    memset(&attr, 0, sizeof(attr));
    attr.map_type    = BPF_MAP_TYPE_ARRAY;
    attr.key_size    = sizeof(__u32);
    attr.value_size  = sizeof(__u64);
    attr.max_entries = 8;
    strncpy(attr.map_name, "pkt_counter", sizeof(attr.map_name) - 1);

    int fd = (int)bpf_syscall(BPF_MAP_CREATE, &attr, sizeof(attr));
    if (fd < 0) {
        fprintf(stderr, "[!] BPF_MAP_CREATE failed: %s\n", strerror(errno));
        if (errno == EPERM)
            fprintf(stderr, "    Hint: run as root or with CAP_BPF + CAP_NET_ADMIN\n");
        exit(1);
    }
    printf("[+] BPF_MAP_CREATE  => map_fd=%d  (type=ARRAY, key=u32, value=u64, entries=8)\n", fd);
    return fd;
}

/* -------------------------------------------------------------------------
 * step 2 — BPF_PROG_LOAD
 *
 * Assemble a BPF socket-filter program in raw bytecode.  The program:
 *
 *   insn  0     : r6 = r1             (save ctx pointer)
 *   insn  1–2   : r1 = map_fd         (BPF_LD_MAP_FD — two insns, 16 bytes)
 *   insn  3     : r2 = r10            (r10 = frame pointer)
 *   insn  4     : r2 += -4            (r2 = &key on stack)
 *   insn  5     : *(u32 *)(r10-4) = 0 (key = 0)
 *   insn  6     : call map_lookup_elem(r1=map, r2=&key)
 *   insn  7     : if r0 == 0, skip 5  (NULL check — jump to accept)
 *   insn  8     : r1 = r0             (r1 = value pointer)
 *   insn  9     : r2 = *(u64 *)(r1+0) (load current counter)
 *   insn 10     : r2 += 1             (increment)
 *   insn 11     : *(u64 *)(r1+0) = r2 (write back)
 *   insn 12     : r0 = 1              (SK_PASS — accept)
 *   insn 13     : exit
 * ------------------------------------------------------------------------- */
static int load_prog(int map_fd)
{
    /* BPF_LD_MAP_FD expands to TWO instructions — counted below */
    struct bpf_insn prog[] = {
        /* insn 0: save ctx */
        BPF_MOV64_REG(BPF_REG_6, BPF_REG_1),

        /* insn 1–2: load map fd (wide / two-insn form) */
        BPF_LD_MAP_FD(BPF_REG_1, map_fd),

        /* insn 3: r2 = frame pointer */
        BPF_MOV64_REG(BPF_REG_2, BPF_REG_10),

        /* insn 4: r2 = &key (fp - 4) */
        BPF_ALU64_IMM(BPF_ADD, BPF_REG_2, -4),

        /* insn 5: *(u32 *)(fp-4) = 0  (key=0) */
        BPF_ST_MEM(BPF_W, BPF_REG_10, -4, 0),

        /* insn 6: r0 = map_lookup_elem(map, &key) */
        BPF_EMIT_CALL(BPF_FUNC_map_lookup_elem),

        /* insn 7: if r0 == NULL skip 5 insns -> insn 13 (r0=1 + exit) */
        BPF_JMP_IMM(BPF_JEQ, BPF_REG_0, 0, 5),

        /* insn 8: r1 = value pointer */
        BPF_MOV64_REG(BPF_REG_1, BPF_REG_0),

        /* insn 9: r2 = *(u64 *)(r1+0)  (current counter) */
        BPF_LDX_MEM(BPF_DW, BPF_REG_2, BPF_REG_1, 0),

        /* insn 10: r2 += 1 */
        BPF_ALU64_IMM(BPF_ADD, BPF_REG_2, 1),

        /* insn 11: *(u64 *)(r1+0) = r2  (write back) */
        BPF_STX_MEM(BPF_DW, BPF_REG_1, BPF_REG_2, 0),

        /* insn 12: r0 = 1  (SK_PASS) */
        BPF_MOV64_IMM(BPF_REG_0, 1),

        /* insn 13: exit */
        BPF_EXIT_INSN(),
    };

    char log_buf[4096] = {0};

    union bpf_attr attr;
    memset(&attr, 0, sizeof(attr));
    attr.prog_type  = BPF_PROG_TYPE_SOCKET_FILTER;
    attr.insn_cnt   = (__u32)(sizeof(prog) / sizeof(prog[0]));
    attr.insns      = (__aligned_u64)(uintptr_t)prog;
    attr.license    = (__aligned_u64)(uintptr_t)"GPL";
    attr.log_level  = 1;
    attr.log_size   = sizeof(log_buf);
    attr.log_buf    = (__aligned_u64)(uintptr_t)log_buf;
    strncpy(attr.prog_name, "pkt_count_sk", sizeof(attr.prog_name) - 1);

    int fd = (int)bpf_syscall(BPF_PROG_LOAD, &attr, sizeof(attr));
    if (fd < 0) {
        fprintf(stderr, "[!] BPF_PROG_LOAD failed: %s\n", strerror(errno));
        if (log_buf[0])
            fprintf(stderr, "--- verifier log ---\n%s\n--------------------\n", log_buf);
        if (errno == EPERM)
            fprintf(stderr, "    Hint: run as root or with CAP_BPF + CAP_NET_ADMIN\n");
        exit(1);
    }
    printf("[+] BPF_PROG_LOAD   => prog_fd=%d  (%zu insns, type=SOCKET_FILTER, name=pkt_count_sk)\n",
           fd, sizeof(prog) / sizeof(prog[0]));
    return fd;
}

/* -------------------------------------------------------------------------
 * step 3 — create UDP sockets and attach BPF program
 *
 * We use two UDP sockets on loopback:
 *   recv_sock — bound to 127.0.0.1:PORT; the BPF filter is attached here
 *   send_sock — unbound sender
 * ------------------------------------------------------------------------- */
#define UDP_PORT 59876

static void setup_sockets(int prog_fd, int *send_out, int *recv_out)
{
    int recv_sock = socket(AF_INET, SOCK_DGRAM, 0);
    int send_sock = socket(AF_INET, SOCK_DGRAM, 0);
    if (recv_sock < 0 || send_sock < 0) {
        fprintf(stderr, "[!] socket(): %s\n", strerror(errno));
        exit(1);
    }

    struct sockaddr_in addr;
    memset(&addr, 0, sizeof(addr));
    addr.sin_family      = AF_INET;
    addr.sin_addr.s_addr = htonl(INADDR_LOOPBACK);
    addr.sin_port        = htons(UDP_PORT);

    if (bind(recv_sock, (struct sockaddr *)&addr, sizeof(addr)) < 0) {
        fprintf(stderr, "[!] bind(): %s\n", strerror(errno));
        exit(1);
    }

    /* Attach BPF program to the receiving socket */
    if (setsockopt(recv_sock, SOL_SOCKET, SO_ATTACH_BPF,
                   &prog_fd, sizeof(prog_fd)) < 0) {
        fprintf(stderr, "[!] SO_ATTACH_BPF: %s\n", strerror(errno));
        if (errno == EPERM)
            fprintf(stderr, "    Hint: run as root or with CAP_BPF + CAP_NET_ADMIN\n");
        exit(1);
    }

    printf("[+] SO_ATTACH_BPF   => recv_sock=%d  send_sock=%d  (loopback UDP port %d)\n",
           recv_sock, send_sock, UDP_PORT);

    *recv_out = recv_sock;
    *send_out = send_sock;
}

/* -------------------------------------------------------------------------
 * step 4 — trigger the filter
 *
 * Send one UDP datagram from send_sock to recv_sock.  The kernel runs the
 * BPF program on the received packet, incrementing map[0].
 * ------------------------------------------------------------------------- */
static void trigger_filter(int send_sock, int recv_sock)
{
    struct sockaddr_in dst;
    memset(&dst, 0, sizeof(dst));
    dst.sin_family      = AF_INET;
    dst.sin_addr.s_addr = htonl(INADDR_LOOPBACK);
    dst.sin_port        = htons(UDP_PORT);

    const char payload[] = "bpf-demo";
    ssize_t n = sendto(send_sock, payload, sizeof(payload) - 1, 0,
                       (struct sockaddr *)&dst, sizeof(dst));
    if (n < 0) {
        fprintf(stderr, "[!] sendto(): %s\n", strerror(errno));
        exit(1);
    }

    char buf[64];
    ssize_t r = recv(recv_sock, buf, sizeof(buf) - 1, 0);
    if (r < 0) {
        fprintf(stderr, "[!] recv(): %s\n", strerror(errno));
        exit(1);
    }
    buf[r] = '\0';
    printf("[+] UDP trigger      => sent %zd bytes, received %zd bytes (\"%s\")\n",
           n, r, buf);
    printf("    BPF filter ran on the received packet — map[0] should now be 1\n");
}

/* -------------------------------------------------------------------------
 * step 5a — BPF_MAP_UPDATE_ELEM (userspace write)
 *
 * The BPF program already incremented map[0] from inside the kernel.  Here
 * we also demonstrate calling BPF_MAP_UPDATE_ELEM from userspace to write
 * map[1] = 42.
 * ------------------------------------------------------------------------- */
static void map_update(int map_fd, __u32 key, __u64 value)
{
    union bpf_attr attr;
    memset(&attr, 0, sizeof(attr));
    attr.map_fd = (__u32)map_fd;
    attr.key    = (__aligned_u64)(uintptr_t)&key;
    attr.value  = (__aligned_u64)(uintptr_t)&value;
    attr.flags  = BPF_ANY;

    if (bpf_syscall(BPF_MAP_UPDATE_ELEM, &attr, sizeof(attr)) < 0) {
        fprintf(stderr, "[!] BPF_MAP_UPDATE_ELEM key=%u: %s\n", key, strerror(errno));
        exit(1);
    }
    printf("[+] BPF_MAP_UPDATE_ELEM  map[%u] = %llu  (userspace write)\n",
           key, (unsigned long long)value);
}

/* -------------------------------------------------------------------------
 * step 5b — BPF_MAP_LOOKUP_ELEM
 *
 * Read a value from the map and return it.
 * ------------------------------------------------------------------------- */
static __u64 map_lookup(int map_fd, __u32 key)
{
    __u64 value = 0;
    union bpf_attr attr;
    memset(&attr, 0, sizeof(attr));
    attr.map_fd = (__u32)map_fd;
    attr.key    = (__aligned_u64)(uintptr_t)&key;
    attr.value  = (__aligned_u64)(uintptr_t)&value;

    if (bpf_syscall(BPF_MAP_LOOKUP_ELEM, &attr, sizeof(attr)) < 0) {
        fprintf(stderr, "[!] BPF_MAP_LOOKUP_ELEM key=%u: %s\n", key, strerror(errno));
        exit(1);
    }
    return value;
}

/* -------------------------------------------------------------------------
 * main
 * ------------------------------------------------------------------------- */
int main(void)
{
    printf("=== bpf_syscall_demo: raw bpf(2) — no libbpf ===\n\n");

    /* 1. Create map */
    int map_fd = create_map();

    /* 2. Load BPF socket-filter program (references map_fd in bytecode) */
    int prog_fd = load_prog(map_fd);

    /* 3. Create UDP sockets and attach BPF */
    int send_sock, recv_sock;
    setup_sockets(prog_fd, &send_sock, &recv_sock);

    /* 4. Send one UDP packet to trigger the BPF filter */
    trigger_filter(send_sock, recv_sock);

    /* 5a. Userspace write: map[1] = 42 */
    printf("\n--- Userspace map operations ---\n");
    map_update(map_fd, 1, 42);

    /* 5b. Read map[0] (incremented by BPF program in kernel) */
    __u64 counter = map_lookup(map_fd, 0);
    printf("[+] BPF_MAP_LOOKUP_ELEM  map[0] = %llu  (BPF kernel increment)\n",
           (unsigned long long)counter);

    /* 5c. Read map[1] (written by userspace above) */
    __u64 val1 = map_lookup(map_fd, 1);
    printf("[+] BPF_MAP_LOOKUP_ELEM  map[1] = %llu  (userspace write)\n",
           (unsigned long long)val1);

    printf("\n=== Summary ===\n");
    printf("  map_fd=%d  prog_fd=%d  recv_sock=%d  send_sock=%d\n",
           map_fd, prog_fd, recv_sock, send_sock);
    printf("  map[0] counter after 1 packet : %llu\n", (unsigned long long)counter);
    printf("  map[1] set from userspace      : %llu\n", (unsigned long long)val1);
    printf("\nTip: inspect the loaded prog with:\n");
    printf("  bpftool prog show\n");
    printf("  cat /proc/self/fdinfo/%d   (prog tag)\n", prog_fd);

    close(recv_sock);
    close(send_sock);
    close(prog_fd);
    close(map_fd);

    return 0;
}
