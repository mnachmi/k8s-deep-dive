# clone-demo: PID and UTS Namespaces

## What It Demonstrates

This exercise calls `clone()` with `CLONE_NEWPID | CLONE_NEWUTS` to create a child process that:

- Sees itself as **PID 1** inside its own PID namespace
- Has its own **UTS namespace** (hostname isolation)
- Sets a custom hostname (`container-demo`) that does not affect the host

The student observes the fundamental gap between the **child's view** (PID 1, isolated hostname) and the **host's view** (a real PID in the parent namespace, original hostname unchanged). This is the same mechanism that container runtimes like `runc` use to isolate container processes.

## Build and Run

```bash
make build
make run
```

### Expected Output

```
[parent] my PID: 12345
[parent] my hostname: mynode
[child] PID inside new PID namespace: 1
[child] PPID inside new PID namespace: 0
[child] hostname in new UTS namespace: container-demo
[child] /proc/self/ns/pid -> pid:[4026532xxx]
[child] /proc/self/ns/uts -> uts:[4026532yyy]
[child] sleeping 2s so parent can observe us...
[parent] child host-PID (as seen from parent namespace): 12346
[parent] notice: child thinks it is PID 1, parent sees it as PID 12346
[parent] child /proc/12346/ns/pid -> pid:[4026532xxx]
[parent] child exited with 0
```

Note: Output ordering between parent and child lines may vary due to scheduling. The key observation is that `[child] PID inside new PID namespace: 1` while the parent reports a higher real PID.

> If you get `clone: Operation not permitted`, run with `sudo ./clone_demo` or check that your kernel has `CONFIG_PID_NS=y` and `CONFIG_UTS_NS=y`.

## Kernel References

- `kernel/fork.c: copy_process()` — the core of `clone()`, allocates and initializes the new task struct
- `kernel/pid_namespace.c: create_pid_namespace()` — allocates a new PID namespace; the first process in it gets pid 1
- `kernel/utsname.c: copy_utsname()` — copies the UTS namespace struct when `CLONE_NEWUTS` is set, giving the child an independent hostname

## Exercises

**(a) Add `CLONE_NEWNET`**
Pass `CLONE_NEWPID | CLONE_NEWUTS | CLONE_NEWNET | SIGCHLD` to `clone()`. Inside `child_fn`, run `system("ip link")` — observe the child has only a loopback interface (`lo`), not the host's network interfaces.

**(b) Add `CLONE_NEWUSER` with UID mapping**
Add `CLONE_NEWUSER` to the flags and write a UID/GID mapping to `/proc/<child_pid>/uid_map` and `/proc/<child_pid>/gid_map` from the parent before the child calls `sethostname`. This allows running the demo as a non-root user.

**(c) Use `unshare()` + mount a new `/proc`**
Instead of `clone()`, use `unshare(CLONE_NEWNS | CLONE_NEWPID)` in a child process, then call `mount("proc", "/proc", "proc", 0, NULL)`. This makes `/proc` reflect the new namespace correctly, so tools like `ps` show only namespace-local PIDs.

## Kubernetes Connection

This demo is essentially the first step of what `runc` does when it starts a container:

1. `clone()` with namespace flags — isolates PID, UTS, network, mount, IPC
2. Sets hostname, UID mapping, cgroup membership
3. `pivot_root()` + overlayfs mount — switches the filesystem
4. `exec()` — replaces the init process with the container entrypoint

See the reference implementation in the OCI runtime spec:
`opencontainers/runc/libcontainer/container_linux.go: newContainer()`

Kubernetes itself never calls `clone()` directly — it delegates to `containerd` → `runc` via the CRI (Container Runtime Interface). But every pod's PID 1 exists because `runc` called `clone()` exactly as this demo does.
