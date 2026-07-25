# 10-b — ftrace, Tracepoints, `struct trace_event_call`, Ring Buffer

## 1. Source Locations

| File | Key Symbols | URL |
|------|-------------|-----|
| `include/linux/tracepoint.h` | `struct tracepoint`, `DEFINE_TRACE()`, `TRACE_EVENT()` macro | https://elixir.bootlin.com/linux/v6.9/source/include/linux/tracepoint.h |
| `include/linux/trace_events.h` | `struct trace_event_call`, `struct trace_event_class`, `struct trace_event` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/trace_events.h |
| `kernel/trace/trace.c` | `trace_array`, ftrace core, `/sys/kernel/tracing/` | https://elixir.bootlin.com/linux/v6.9/source/kernel/trace/trace.c |
| `kernel/trace/ring_buffer.c` | `ring_buffer_lock_reserve()`, `ring_buffer_unlock_commit()` | https://elixir.bootlin.com/linux/v6.9/source/kernel/trace/ring_buffer.c |
| `include/linux/ftrace.h` | `struct ftrace_ops`, `ftrace_func_t`, `register_ftrace_function()` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/ftrace.h |

## 2. Tracepoint Mechanism Overview

A tracepoint is a hook point placed inside kernel code with near-zero overhead when no probe is attached. It uses the **jump label** mechanism: an unconditional NOP in the hot path that is patched to a JMP when enabled — a single `LOCK CMPXCHG` rewrite of 5 bytes.

```
Kernel code:
    ... normal code ...
    TRACE_EVENT(sched_switch, TP_PROTO(...), TP_ARGS(...), ...)
    ... normal code ...

When disabled (default):
    NOP         // 5-byte nop — zero overhead

When enabled (bpftrace / perf attaches):
    CALL __tracepoint_sched_switch  // JMP to tracepoint handler
        → iterates probe list
        → calls each registered probe function
        → writes to per-CPU ring buffer
```

The `TRACE_EVENT()` macro expands in multiple passes via `#include` tricks (see `include/trace/define_trace.h`). Each expansion produces:
- A `struct tracepoint` variable (`__tracepoint_<name>`)
- A `struct trace_event_call` registered with the ftrace subsystem
- A `/sys/kernel/tracing/events/<subsystem>/<event>/` directory entry
- A generated `format` file describing the event fields in a machine-readable format

The probe list stored in `tracepoint.funcs` is RCU-protected: readers hold an RCU read lock while calling probes, writers serialize via a mutex in `tracepoint.c`. This means enabling a tracepoint never blocks the hot path — it only patches the jump label and appends to the probe list.

## 3. `struct tracepoint`

```c
// include/linux/tracepoint.h (Linux 6.9)
struct tracepoint {
    const char                  *name;          // e.g. "sched_switch"
    struct static_key_false      key;           // jump label — default false/disabled (NOP), enabled → JMP
    struct static_call_key      *static_call_key;  // faster static call dispatch (no indirect jump)
    void                        *static_call_tramp; // trampoline for the static call
    void                        *iterator;          // iterator function for multiple probes
    void                        *probestub;         // stub when no probes registered
    int                        (*regfunc)(void);    // called when first probe registers; returns 0 or -errno
    void                       (*unregfunc)(void);  // called when last probe unregisters
    struct tracepoint_func __rcu *funcs;            // RCU-protected list of probe functions
};
```

`static_key_false key`: when 0 (no probes), the jump label generates a NOP instruction inline. When `tracepoint_probe_register()` is called, `static_key_slow_inc()` patches all NOP sites to JMP instructions. The overhead of an unattached tracepoint is literally zero — the NOP is on the same cache line as the surrounding code.

`tracepoint_func` is defined as:

```c
struct tracepoint_func {
    void       *func;   // probe function pointer
    void       *data;   // opaque data passed to probe (e.g. bpf_prog *)
    int         prio;   // priority for ordering multiple probes
};
```

When more than one probe is registered (e.g., a bpftrace script and a `perf` event simultaneously), the tracepoint iterates the `funcs` array and calls each in priority order. Each call is a direct indirect call — no further dispatch overhead.

`regfunc` / `unregfunc` are optional callbacks for tracepoints that need per-tracepoint bookkeeping when the enable/disable state changes (e.g., enabling hardware performance counters that feed a tracepoint).

## 4. `struct trace_event_call`

```c
// include/linux/trace_events.h (Linux 6.9, key fields)
struct trace_event_call {
    struct list_head        list;               // entry in event list
    struct trace_event_class *class;            // shared class (format, fields)
    union {
        char                *name;              // event name
        struct tracepoint   *tp;                // underlying tracepoint
    };
    struct trace_event      event;              // embedded trace_event (type, funcs)
    char                   *print_fmt;          // printf format string for trace output
    struct event_filter        *filter;          // per-event filter
    union {
        void        *module;    // owning module pointer (for module-defined events)
        atomic_t     refcnt;    // refcount (for dynamic events)
    };
    void                   *data;
    int                     flags;              // TRACE_EVENT_FL_*
    int                     perf_refcount;      // number of perf users
    struct hlist_head __percpu *perf_events;    // per-CPU perf event list
    // ...
};
```

Each `TRACE_EVENT()` macro in the kernel generates a `struct trace_event_call` and a corresponding `/sys/kernel/tracing/events/<subsystem>/<event>/` directory.

`struct trace_event_class` groups events that share the same recording format and field definitions. Multiple `trace_event_call` instances can share one `trace_event_class` (e.g., all events in a subsystem that log the same fields). The class holds the `define_fields()` function pointer used to populate the `format` file.

`struct trace_event` (embedded in `trace_event_call.event`) carries the numeric event type ID and a `trace_event_functions` vtable with `trace`, `raw`, `hex`, `binary` output callbacks. The type ID is used as the discriminator in the ring buffer so that readers know how to decode each record.

`print_fmt` is the human-readable format string exposed in `/sys/kernel/tracing/events/<subsystem>/<event>/format`. Tools like `bpftrace` and `perf` parse this field to discover argument names and types at runtime — it is the contract between the kernel event definition and userspace tooling.

`filter` holds a compiled filter expression (e.g., `prev_prio < 100`) applied before the event is written to the ring buffer. Filtering happens in-kernel, reducing ring buffer pressure for high-frequency events.

`perf_refcount` / `perf_events`: when a `perf_event_open()` call uses `PERF_TYPE_TRACEPOINT`, the kernel increments `perf_refcount` and registers a per-CPU `perf_event` in `perf_events`. The tracepoint probe then calls into `perf_trace_<event>()` which delivers the sample to the perf ABI alongside the ftrace ring buffer path.

## 5. Ring Buffer

The ftrace ring buffer is a per-CPU lock-free circular buffer. Each CPU writes independently (no cross-CPU locking). Readers can consume the buffer from `/sys/kernel/tracing/trace_pipe` or via `perf_event_open` with `PERF_TYPE_TRACEPOINT`.

```
struct trace_array (per trace instance)
    │
    └── struct array_buffer buffer
            │
            └── struct trace_buffer
                    │
                    └── struct ring_buffer_per_cpu __percpu *buffers
                            │
                            ├── head_page, tail_page (struct buffer_page)
                            ├── reader_page (for lockless reads)
                            └── entries (atomic_t — count of available entries)
```

Write path:
1. `ring_buffer_lock_reserve(buffer, len)` — atomically advances `tail_page->write` pointer
2. Caller writes event data into the reserved slot
3. `ring_buffer_unlock_commit(buffer)` — marks slot as committed, visible to readers

The buffer never blocks writers — old entries are overwritten when the buffer is full (overwrite mode) or new writes fail silently (discard mode).

Each `buffer_page` is a single kernel page (4 KB). Pages are linked in a circular list per CPU. The `reader_page` is swapped atomically with the current tail page to give readers a stable snapshot without stopping writers. This is the key to the lockless design: readers and writers never operate on the same page simultaneously.

Event records inside each page use a compact encoding. The first word of each record is a `ring_buffer_event` header:

```c
struct ring_buffer_event {
    u32 type_len  :  5;  // record type / inline length
    u32 time_delta: 27;  // nanosecond delta from previous event
    u32 array[];         // variable-length data
};
```

`time_delta` stores the nanosecond offset from the previous event on the same CPU, keeping timestamps compact. An absolute timestamp is only emitted when the delta overflows 27 bits or when crossing a page boundary.

The global instance of the ftrace ring buffer is `global_trace` (`struct trace_array`), defined in `kernel/trace/trace.c`. Tracefs mounts expose per-instance directories under `/sys/kernel/tracing/instances/`, each backed by its own `trace_array` and independent ring buffers.

## 6. ftrace Function Tracer

ftrace hooks all kernel functions via a `call` instruction inserted by the compiler (`-mfentry` on x86). `register_ftrace_function()` installs a probe:

```c
// include/linux/ftrace.h
struct ftrace_ops {
    ftrace_func_t func;             // callback: void fn(unsigned long ip, unsigned long parent_ip, ...)
    struct ftrace_ops_hash local_hash;
    struct ftrace_ops_hash *func_hash;  // function filter (which functions to trace)
    unsigned long flags;            // FTRACE_OPS_FL_*
    // ...
};
```

`ip` = instruction pointer of the traced function. `parent_ip` = return address (caller). This is the foundation of the function call graph tracers and latency analyzers.

`-mfentry` causes GCC/Clang to insert a `CALL __fentry__` at the very start of every non-inline kernel function, before the function prologue. At boot, these calls are NOPed out by `ftrace_init()` — the same jump-label patching technique as tracepoints. When `register_ftrace_function()` is called with a non-empty `func_hash`, only the matching functions are re-patched to call the ftrace trampoline.

`ftrace_ops_hash` stores a hash set of function addresses (or name patterns). The kernel provides `ftrace_set_filter()` and `ftrace_set_notrace()` helpers to populate it. Dynamic ftrace (`CONFIG_DYNAMIC_FTRACE`) is almost universally enabled in production kernels — without it, every function pays the `CALL` overhead unconditionally.

`flags` relevant values:
- `FTRACE_OPS_FL_SAVE_REGS` — save full `pt_regs` on entry (expensive, needed for kprobes)
- `FTRACE_OPS_FL_RECURSION` — protect against recursive invocation of the callback
- `FTRACE_OPS_FL_IPMODIFY` — allow the callback to modify `ip` (used by kretprobes to redirect return)

The function graph tracer (`CONFIG_FUNCTION_GRAPH_TRACER`) extends this by also hooking function returns via `ftrace_graph_return`. It maintains a per-task shadow stack of return addresses to measure function latency.

## 7. Live Observation

```bash
# List all available tracepoints
ls /sys/kernel/tracing/events/sched/

# Enable sched_switch tracepoint and read the ring buffer
echo 1 > /sys/kernel/tracing/events/sched/sched_switch/enable
cat /sys/kernel/tracing/trace_pipe

# bpftrace uses tracepoints
bpftrace -e 'tracepoint:sched:sched_switch { printf("%s -> %s\n", args->prev_comm, args->next_comm); }'

# perf with tracepoint
perf record -e sched:sched_switch -a sleep 5
perf report

# Count tracepoint fires via perf stat
perf stat -e sched:sched_switch -p <pid> sleep 5
```

Additional observations:

```bash
# Show all registered ftrace tracers
cat /sys/kernel/tracing/available_tracers

# Enable the function tracer for a specific function
echo function > /sys/kernel/tracing/current_tracer
echo do_sys_open > /sys/kernel/tracing/set_ftrace_filter
echo 1 > /sys/kernel/tracing/tracing_on
cat /sys/kernel/tracing/trace_pipe

# Use function_graph tracer to see call graph with latency
echo function_graph > /sys/kernel/tracing/current_tracer
echo schedule > /sys/kernel/tracing/set_graph_function
cat /sys/kernel/tracing/trace_pipe

# Inspect the format of a tracepoint (field names and types)
cat /sys/kernel/tracing/events/sched/sched_switch/format

# Check the jump label state (1 = enabled, 0 = disabled)
# Tracepoints appear in /sys/kernel/tracing/events/<sys>/<event>/enable
cat /sys/kernel/tracing/events/sched/sched_switch/enable

# Create an isolated trace instance to avoid polluting the global buffer
mkdir /sys/kernel/tracing/instances/mytest
echo 1 > /sys/kernel/tracing/instances/mytest/events/net/net_dev_xmit/enable
cat /sys/kernel/tracing/instances/mytest/trace_pipe
```

## 8. Key Kernel References

| Symbol | File | URL |
|--------|------|-----|
| `struct tracepoint` | `include/linux/tracepoint.h` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/tracepoint.h |
| `struct trace_event_call` | `include/linux/trace_events.h` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/trace_events.h |
| `ring_buffer_lock_reserve()` | `kernel/trace/ring_buffer.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/trace/ring_buffer.c |
| `ring_buffer_unlock_commit()` | `kernel/trace/ring_buffer.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/trace/ring_buffer.c |
| `struct ftrace_ops` | `include/linux/ftrace.h` | https://elixir.bootlin.com/linux/v6.9/source/include/linux/ftrace.h |
| `register_ftrace_function()` | `kernel/trace/ftrace.c` | https://elixir.bootlin.com/linux/v6.9/source/kernel/trace/ftrace.c |
