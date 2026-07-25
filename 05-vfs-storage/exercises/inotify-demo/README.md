# inotify-demo

Linux `inotify` filesystem event watching — Chapter 05: VFS & Storage.

---

## What It Demonstrates

- **inotify file descriptor**: obtained with `inotify_init()`, represents a kernel notification queue. Each `inotify_add_watch()` call attaches that queue to a specific path. Kernel reference: https://elixir.bootlin.com/linux/v6.9/source/include/uapi/linux/inotify.h

- **`IN_*` event masks**: individual bit flags (`IN_CREATE`, `IN_MODIFY`, `IN_DELETE`, `IN_CLOSE_WRITE`, `IN_MOVED_FROM`, `IN_MOVED_TO`) that control which VFS operations trigger notifications on the watched path.

- **Event loop with `poll()`**: the parent blocks on `poll()` with a 200 ms timeout. When `POLLIN` fires, `read()` returns one or more packed `struct inotify_event` records; the loop walks them with pointer arithmetic.

- **`cookie` for rename pairs**: `IN_MOVED_FROM` and `IN_MOVED_TO` events share an identical non-zero `cookie` value, letting user-space match the source and destination of a single `rename(2)` call atomically.

- **Atomic rename pattern**: the program writes sensitive data to `tmp_secret.txt`, then calls `rename()` to replace `secret.txt` in one atomic VFS operation. This is the same technique Kubernetes uses so containers never observe a partially-written secret.

---

## Build and Run

```
make run
```

Build only:

```
make
```

Remove the binary:

```
make clean
```

---

## Expected Output

```
Watching directory: /tmp/inotify-demo-XXXXXX

mask       events       cookie     name
---------- ------------ ---------- ----
inotify: mask=0x00000100 (CREATE) cookie=0 name=file1.txt
inotify: mask=0x00000002 (MODIFY) cookie=0 name=file1.txt
inotify: mask=0x00000008 (CLOSE_WRITE) cookie=0 name=file1.txt
inotify: mask=0x00000002 (MODIFY) cookie=0 name=file1.txt
inotify: mask=0x00000008 (CLOSE_WRITE) cookie=0 name=file1.txt
inotify: mask=0x00000040 (MOVED_FROM) cookie=12345 name=file1.txt
inotify: mask=0x00000080 (MOVED_TO) cookie=12345 name=file2.txt
inotify: mask=0x00000100 (CREATE) cookie=0 name=tmp_secret.txt
inotify: mask=0x00000002 (MODIFY) cookie=0 name=tmp_secret.txt
inotify: mask=0x00000008 (CLOSE_WRITE) cookie=0 name=tmp_secret.txt
inotify: mask=0x00000040 (MOVED_FROM) cookie=12346 name=tmp_secret.txt
inotify: mask=0x00000080 (MOVED_TO) cookie=12346 name=secret.txt
inotify: mask=0x00000200 (DELETE) cookie=0 name=file2.txt
inotify: mask=0x00000200 (DELETE) cookie=0 name=secret.txt

Done. Temp directory removed.
```

Event explanations:

| Event | Trigger |
|---|---|
| `CREATE` | `open(..., O_CREAT, ...)` creates a new directory entry |
| `MODIFY` | `write()` to an open file descriptor |
| `CLOSE_WRITE` | `close()` on a file opened for writing |
| `MOVED_FROM` / `MOVED_TO` | `rename()` — both events carry the same `cookie` |
| `DELETE` | `unlink()` removes the directory entry |

The `cookie` field on `MOVED_FROM`/`MOVED_TO` pairs is non-zero and identical, enabling user-space to correlate the source and destination of a rename even when other events interleave.

---

## Kernel Path

**Watch registration:**

`inotify_add_watch(2)` system call enters the kernel as `sys_inotify_add_watch()`, which calls `inotify_update_watch()` to attach an `fsnotify_mark` to the target inode.

https://elixir.bootlin.com/linux/v6.9/source/fs/notify/inotify/inotify_user.c

**Event generation:**

Every VFS operation (create, write, rename, unlink, close) calls `fsnotify_parent()` and `fsnotify()` in `fs/notify/fsnotify.c`, which walks the inode's mark list and delivers events to all registered inotify file descriptors.

https://elixir.bootlin.com/linux/v6.9/source/fs/notify/fsnotify.c

**`struct inotify_event` layout** (the struct read from the fd):

https://elixir.bootlin.com/linux/v6.9/source/include/uapi/linux/inotify.h

---

## Kubernetes Connection

The **kubelet** watches `/sys/fs/cgroup/` via inotify (through Go's `fsnotify` library) to detect cgroup hierarchy changes — such as container creation and teardown — without polling.

The **secret volume controller** writes secret data to a temporary file and then calls `rename()` to atomically replace the target path. This guarantees that a container reading `/var/run/secrets/...` never sees a partial write. The `tmp_secret.txt` → `secret.txt` sequence in this demo is exactly that pattern: the `MOVED_FROM`/`MOVED_TO` event pair (with a shared `cookie`) appears instead of a `CREATE`+`MODIFY` sequence, confirming the atomic nature of the operation.

---

## Exercises

**(a)** Watch `/sys/fs/cgroup/` instead of a temp directory and observe the inotify events generated when a container starts or stops. Run `docker run --rm alpine echo hi` in a second terminal while the watcher is active.

**(b)** Add `IN_ACCESS` to the watch mask in `inotify_add_watch()` to capture read events. Notice how noisy the output becomes — every `open()` + `read()` triggers an event. This explains why production tools watch only write-side events.

**(c)** Call `inotify_add_watch()` on multiple paths simultaneously and print the `wd` (watch descriptor) field to identify which path each event came from. Multiple watches share the same inotify file descriptor.
