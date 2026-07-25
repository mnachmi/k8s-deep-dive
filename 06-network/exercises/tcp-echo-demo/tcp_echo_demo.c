/*
 * tcp_echo_demo.c — TCP socket lifecycle: socket/bind/listen/accept/connect
 *
 * Chapter 06: Networking — Linux kernel to Kubernetes.
 *
 * The program:
 *   1. Creates a listening socket on loopback, port chosen by the kernel.
 *   2. Calls getsockname() to retrieve the assigned port.
 *   3. Forks: child becomes the server; parent becomes the client.
 *   4. Server: accept() → read() → write() (echo) → close().
 *   5. Client: connect() → send 5 messages → receive echoes → close().
 *
 * Compile:
 *   gcc -Wall -Wextra -Werror -o tcp_echo_demo tcp_echo_demo.c
 */

#include <arpa/inet.h>
#include <errno.h>
#include <netinet/in.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/socket.h>
#include <sys/types.h>
#include <sys/wait.h>
#include <unistd.h>

#define BACKLOG    5
#define NUM_MSGS   5
#define BUF_SIZE   256

static void die(const char *msg)
{
    perror(msg);
    exit(EXIT_FAILURE);
}

/* ------------------------------------------------------------------ server */

static void run_server(int listen_fd)
{
    struct sockaddr_in peer_addr;
    socklen_t peer_len = sizeof(peer_addr);
    char buf[BUF_SIZE];
    ssize_t n;
    int conn_fd;

    printf("[server] listen_fd=%d  waiting for accept()\n", listen_fd);
    fflush(stdout);

    conn_fd = accept(listen_fd, (struct sockaddr *)&peer_addr, &peer_len);
    if (conn_fd < 0)
        die("accept");

    printf("[server] accept() returned conn_fd=%d  peer=%s:%d\n",
           conn_fd,
           inet_ntoa(peer_addr.sin_addr),
           ntohs(peer_addr.sin_port));
    fflush(stdout);

    /* Echo loop: read until client closes the connection */
    while ((n = read(conn_fd, buf, sizeof(buf) - 1)) > 0) {
        buf[n] = '\0';
        printf("[server] read %zd bytes: \"%s\"\n", n, buf);
        fflush(stdout);

        if (write(conn_fd, buf, (size_t)n) != n)
            die("write");

        printf("[server] echoed %zd bytes back to client\n", n);
        fflush(stdout);
    }

    if (n < 0)
        die("read");

    printf("[server] client closed connection — closing conn_fd=%d\n", conn_fd);
    fflush(stdout);

    close(conn_fd);
    close(listen_fd);

    printf("[server] done\n");
    fflush(stdout);
}

/* ------------------------------------------------------------------ client */

static void run_client(int server_port)
{
    struct sockaddr_in srv_addr;
    struct sockaddr_in local_addr;
    socklen_t local_len = sizeof(local_addr);
    char buf[BUF_SIZE];
    char msg[BUF_SIZE];
    int sock_fd;
    ssize_t n;
    int i;

    sock_fd = socket(AF_INET, SOCK_STREAM, 0);
    if (sock_fd < 0)
        die("socket (client)");

    printf("[client] socket_fd=%d\n", sock_fd);
    fflush(stdout);

    memset(&srv_addr, 0, sizeof(srv_addr));
    srv_addr.sin_family      = AF_INET;
    srv_addr.sin_port        = htons((uint16_t)server_port);
    srv_addr.sin_addr.s_addr = htonl(INADDR_LOOPBACK);

    printf("[client] connect() → 127.0.0.1:%d\n", server_port);
    fflush(stdout);

    if (connect(sock_fd, (struct sockaddr *)&srv_addr, sizeof(srv_addr)) < 0)
        die("connect");

    /* Show the kernel-assigned local port for the client side */
    if (getsockname(sock_fd, (struct sockaddr *)&local_addr, &local_len) == 0) {
        printf("[client] local address after connect: 127.0.0.1:%d\n",
               ntohs(local_addr.sin_port));
        fflush(stdout);
    }

    for (i = 1; i <= NUM_MSGS; i++) {
        int msg_len = snprintf(msg, sizeof(msg), "Hello %d", i);
        if (msg_len < 0)
            die("snprintf");

        printf("[client] send: \"%s\"\n", msg);
        fflush(stdout);

        if (write(sock_fd, msg, (size_t)msg_len) != msg_len)
            die("write (client)");

        n = read(sock_fd, buf, sizeof(buf) - 1);
        if (n < 0)
            die("read (client)");
        buf[n] = '\0';

        printf("[client] echo received: \"%s\"\n", buf);
        fflush(stdout);
    }

    printf("[client] closing socket_fd=%d\n", sock_fd);
    fflush(stdout);

    close(sock_fd);

    printf("[client] done\n");
    fflush(stdout);
}

/* -------------------------------------------------------------------  main */

int main(void)
{
    struct sockaddr_in srv_addr;
    struct sockaddr_in bound_addr;
    socklen_t bound_len = sizeof(bound_addr);
    int listen_fd;
    int opt = 1;
    int server_port;
    pid_t child_pid;
    int status;

    /* ---- 1. Create the listening socket ---- */
    listen_fd = socket(AF_INET, SOCK_STREAM, 0);
    if (listen_fd < 0)
        die("socket");

    printf("[main]   socket() → listen_fd=%d  (AF_INET, SOCK_STREAM)\n",
           listen_fd);
    fflush(stdout);

    /* SO_REUSEADDR: lets the server restart immediately without TIME_WAIT */
    if (setsockopt(listen_fd, SOL_SOCKET, SO_REUSEADDR, &opt, sizeof(opt)) < 0)
        die("setsockopt SO_REUSEADDR");

    printf("[main]   setsockopt(SO_REUSEADDR) set\n");
    fflush(stdout);

    /* ---- 2. bind() to loopback, port=0 (kernel picks) ---- */
    memset(&srv_addr, 0, sizeof(srv_addr));
    srv_addr.sin_family      = AF_INET;
    srv_addr.sin_port        = 0;               /* let kernel assign */
    srv_addr.sin_addr.s_addr = htonl(INADDR_LOOPBACK);

    if (bind(listen_fd, (struct sockaddr *)&srv_addr, sizeof(srv_addr)) < 0)
        die("bind");

    /* Retrieve the kernel-assigned port */
    if (getsockname(listen_fd, (struct sockaddr *)&bound_addr, &bound_len) < 0)
        die("getsockname");

    server_port = ntohs(bound_addr.sin_port);
    printf("[main]   bind() → 127.0.0.1:%d  (port assigned by kernel)\n",
           server_port);
    fflush(stdout);

    /* ---- 3. listen() ---- */
    if (listen(listen_fd, BACKLOG) < 0)
        die("listen");

    printf("[main]   listen(backlog=%d) — socket is now LISTEN state\n",
           BACKLOG);
    fflush(stdout);

    /* ---- 4. fork: child=server, parent=client ---- */
    child_pid = fork();
    if (child_pid < 0)
        die("fork");

    if (child_pid == 0) {
        /* --- child: server --- */
        run_server(listen_fd);
        exit(EXIT_SUCCESS);
    }

    /* --- parent: client --- */
    /* Brief yield so the server reaches accept() first */
    usleep(10000); /* 10 ms */

    run_client(server_port);

    /* Wait for server child to exit */
    if (waitpid(child_pid, &status, 0) < 0)
        die("waitpid");

    if (WIFEXITED(status))
        printf("[main]   server child exited with status %d\n",
               WEXITSTATUS(status));

    return 0;
}
