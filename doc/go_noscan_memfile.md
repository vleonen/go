# File-backed memory region for large noscan objects

This document describes the *noscan file region*: an optional runtime feature
that serves large, pointer-free heap allocations from a dedicated,
file-backed (or block-device-backed) memory region.

Status: implemented for `linux/amd64` and `linux/arm64`. Disabled everywhere
else, and when not configured.

## 1. Motivation

The Go heap distinguishes *scan* spans (which may contain pointers and must be
traced by the garbage collector) from *noscan* spans (which contain only
non-pointer data and are never scanned). Large noscan allocations — the backing
arrays of big `[]byte`, `[]float64`, `string`, etc. — are pure data with no GC
trace work attached to them.

Because their contents are opaque to the runtime, these allocations are a
natural fit for backing stores other than anonymous RAM:

* **zram** — a compressed RAM block device. Backing the region with
  `/dev/zramN` transparently compresses large data blobs, trading a little CPU
  for a large reduction in effective memory footprint.
* **A file on disk** — useful for workloads that want large, transient data
  buffers to spill to a file (e.g. scratch space, large intermediate results)
  without changing application code.
* **A memory-mapped file that is shared with another process**, since
  `MAP_SHARED` makes writes visible to other mappers of the same file.

The feature lets a program opt into this by setting two environment variables.
No source changes are required: ordinary `make([]byte, …)` calls for large
sizes are transparently served from the region when it is enabled.

## 2. What is routed to the region

Only allocations that are **both**:

* **large** — larger than the small-allocation threshold
  (`maxSmallSize - mallocHeaderSize`, currently `32768 - 8 = 32760` bytes), i.e.
  they take the large-object path (`mallocgcLarge` → `mcache.allocLarge` →
  `mheap.alloc` with size class 0); and
* **noscan** — the allocated type contains no pointers (`typ == nil ||
  !typ.Pointers()`).

In span-class terms, that is exactly `makeSpanClass(0, /*noscan=*/true)`.

Everything else — small noscan allocations, all scan allocations, stacks, GC
metadata, user arenas — is unaffected and continues to come from the regular
heap. Small noscan objects deliberately stay on the normal heap because they
flow through the per-size-class `mcentral`/`mcache` hot path, and moving them
would add overhead to the most common allocations for little benefit.

## 3. Enabling the feature

The feature is controlled entirely by environment variables, read once at
process startup.

| Variable | Meaning |
|---|---|
| `GONOSCANFILE` | Path to the backing file or block device (e.g. `/var/lib/myapp/noscan.bin`, `/dev/zram0`). Created with mode 0600 if it does not exist. |
| `GONOSCANFILESIZE` | Size of the region in bytes, with an optional binary suffix: `K`/`KiB`/`KB` (1024), `M`/`MiB`/`MB`, `G`/`GiB`/`GB` (case-insensitive). Examples: `256MiB`, `1GB`, `536870912`. |

Both must be set and the size must parse to a positive value. Otherwise the
feature is disabled and the program behaves exactly as before.

The size is rounded **up** to a multiple of the page-allocator chunk size
(`pallocChunkBytes`: 256 KiB on amd64, 4 MiB on arm64 with 64 KiB pages), and
the region base is aligned to a heap arena (`heapArenaBytes`, 64 MiB on 64-bit).

### Example: backing with a regular file

```sh
GONOSCANFILE=/var/lib/myapp/noscan.bin \
GONOSCANFILESIZE=1GB \
./myapp
```

### Example: backing with zram

Create a zram device of the desired size, then point `GONOSCANFILE` at it:

```sh
# Create a 2 GiB zram0 device formatted as a swapless raw block device.
sudo modprobe zram num_devices=1
echo 2G | sudo tee /sys/block/zram0/disksize
sudo chmod 0666 /dev/zram0

GONOSCANFILE=/dev/zram0 GONOSCANFILESIZE=2GB ./myapp
```

> Note: a regular file is `ftruncate`-d to the configured size at startup. A
> block device cannot be truncated; current behavior requires a regular file
> for `ftruncate` to succeed (see §8 Limitations). On a device, make sure the
> configured size does not exceed the device size.

## 4. How it works

### 4.1 Region setup

At startup, after the environment is parsed and after `gcinit`, while the world
is still stopped (`runtime.schedinit`), `noscanFileRegionInit`:

1. Reserves a contiguous, heap-arena-aligned chunk of virtual address space with
   `sysReserveAligned` (a `PROT_NONE` anonymous reservation). The address is
   chosen by the kernel and is disjoint from the regular heap's arena hints.
2. Creates or opens the backing file (`open(O_RDWR|O_CREAT, 0600)`) and sizes
   it with `ftruncate`.
3. Overlays the reservation with a single `MAP_SHARED|MAP_FIXED` mapping of the
   file, replacing the `PROT_NONE` placeholder at exactly the same address. The
   region is now read/write and file-backed.
4. Creates a **dedicated `pageAlloc`** for the region (`pages`) and grows it over
   `[base, base+size)`.
5. Registers a `heapArena` for every arena overlapping the region in the global
   `mheap_.arenas` map, so that `spanOf` / `pageIndexOf` resolve region
   addresses.

On failure (out of address space, file cannot be opened, `mmap` fails, …) it
prints a `runtime:` diagnostic and leaves the feature disabled; allocations
then fall back to the regular heap.

### 4.2 Allocation routing

`mheap.alloc` is the single chokepoint for large objects (size class 0 has no
`mcentral`, so it is only reached from `mcache.allocLarge`). When the region is
enabled and the requested class is `makeSpanClass(0, true)`, it calls
`noscanFileRegionAlloc`:

* It draws `npages` from the region's own page allocator (`pages.alloc`).
* It reuses the standard `mheap.initSpan`, which sets the span class, sweep
  generation, state (`mSpanInUse`), `pageInUse` bit, `pagesInUse` counter,
  `needzero`, and publishes the span into `heapArena.spans`.
* It then updates the `spanAllocHeap` stats (`heapInUse`, the consistent `inHeap`
  counter) that the sweep path will later reverse.

If the region is exhausted, `noscanFileRegionAlloc` returns `nil` and `mheap.alloc`
falls through to the normal heap path (`allocSpan`), so exhaustion is
transparent. The object's `needzero` is handled correctly by the existing
`allocNeedsZero` mechanism (region arenas' `zeroedBase` advances monotonically),
so reused region pages are zeroed before reuse exactly like ordinary heap pages.

`mcache.allocLarge` does the rest of the large-object work unchanged: it bumps
`heapLive` (`gcController.update`), pushes the span onto the size class's swept
list so the background sweeper can find it, sets `freeindex`/`allocCount`, and
`mallocgcLarge` zero-fills and publishes the object.

### 4.3 Free routing

When a region span is freed, `mheap.freeSpanLocked` notices that its base lies in
a file region (`isFileRegionAddr`) and calls `noscanFileRegionFreeLocked`, which:

* mirrors the `spanAllocHeap` accounting that allocation performed
  (`pagesInUse--`, `heapInUse--`, `inHeap--`, clears the `pageInUse` bit);
* returns the pages to the **region's** page allocator (`pages.free`) instead of
  the heap's; and
* intentionally skips the heap's scavenge stats (`heapFree`/`heapReleased`) and
  `sysUsed`, since region pages are file-backed and never scavenged.

Region spans are freed through the same sweeper as ordinary large spans (they
were pushed into the size class's swept list at allocation time), so sweeping
needs no special handling.

### 4.4 Garbage collection

The region is a first-class part of the heap from the GC's point of view, so
marking and sweeping require no changes:

* **Marking.** When the GC follows a pointer into a region object,
  `greyobject` looks up the span via `spanOf` (which works because the region's
  arenas are registered) and fast-paths the noscan object to black, setting the
  `pageMarks` bit. The object's contents are never scanned, exactly as for any
  noscan object.
* **Sweeping.** Region spans are swept like any other large span.
* **Finalizers.** A finalizer attached to a region object is handled by the
  standard `markrootSpans` path; the finalizer function is scanned and the
  object's contents are skipped (as for all noscan spans with finalizers).
* **Scavenging.** The region is **excluded** from the scavenger by design: it
  uses a separate `pageAlloc` instance, so `mheap.pages.scavenge` cannot reach
  it, and its arenas are deliberately not added to `allArenas`, so any
  arena-iteration-based reclaim skips them. The region is intended to stay fully
  resident (no `MADV_FREE`/`MADV_DONTNEED`), which is the desired behavior for a
  file/zram-backed region.

### 4.5 Accounting summary

| Quantity | Region span | Regular span |
|---|---|---|
| `heapLive` | updated by `mcache.allocLarge` | same |
| `heapInUse` | `+nbytes` on alloc, `-nbytes` on free | same |
| consistent `inHeap` | `±nbytes` | same |
| `pagesInUse` | `±npages` (via `initSpan`) | same |
| `heapFree` / `heapReleased` / scavenge | **not touched** | yes |

So `runtime.ReadMemStats` reports region objects in `HeapAlloc`/`HeapInuse`/
`HeapSys`-related counters as expected. The total size of the underlying file
mapping is not added to `gcController.mappedReady` (see §8).

## 5. Concurrency and locking

The region's `pageAlloc` is guarded by its own `mutex` (`noscanFileRegion.lock`),
passed to `pageAlloc.init` as its `mheapLock`. Both `pageAlloc.alloc` and
`pageAlloc.free` are `//go:systemstack`, so the region wrappers dispatch to the
system stack. Lock order is always `mheap_.lock` → region `lock`, used
consistently by both allocation and freeing, so there is no deadlock.

The process keeps an append-only list of every region ever activated
(`allFileRegions`), so a span can always be freed back to the region that owns
it even if the active region is later replaced. (In production exactly one
region is created; the list mainly supports tests that create several.)

## 6. Configuration reference

* `GONOSCANFILE` — path (required to enable).
* `GONOSCANFILESIZE` — size with optional suffix (required to enable).

Size suffixes recognized (case-insensitive): none / `B`, `K`/`KB`/`KiB`,
`M`/`MB`/`MiB`, `G`/`GB`/`GiB`. Multipliers are power-of-two (1 KiB = 1024).

A zero or unparseable size disables the feature. The feature is a no-op on
non-Linux or non-amd64/arm64 builds (a build-tagged stub returns "disabled").

## 7. File map

| File | Role |
|---|---|
| `src/runtime/memfile.go` | Region manager, config parsing, alloc/free routing, startup init (build tag `linux && (amd64 \|\| arm64)`). |
| `src/runtime/memfile_stub.go` | Disabled-feature stubs for all other platforms. |
| `src/runtime/mheap.go` | Routing in `mheap.alloc` and `mheap.freeSpanLocked`. |
| `src/runtime/proc.go` | Calls `noscanFileRegionInit()` from `schedinit`. |
| `src/runtime/defs_linux_{amd64,arm64}.go` | `_MAP_SHARED`, `_O_RDWR`. |
| `src/runtime/sys_linux_{amd64,arm64}.s` | `ftruncate` system-call stub. |
| `src/runtime/memfile_test.go` | Tests (see §8). |
| `src/runtime/export_memfile_test.go` | Exports internals to the external test package. |

Key runtime functions: `noscanFileRegionInit`, `noscanFileRegionAlloc`,
`noscanFileRegionFreeLocked`, `noscanFileRegion.setup`, `registerArenas`,
`parseNoscanFileConfig`.

## 8. Testing

`src/runtime/memfile_test.go` covers each layer:

* `TestMapSharedFilePrimitive` — `MAP_SHARED` + `ftruncate` primitives.
* `TestParseMemSize`, `TestNoscanFileConfigFromEnv` — config parsing.
* `TestNoscanFileRegionSetup` — region maps a file and writes persist.
* `TestNoscanFileRegionArenaRegistered` — arena metadata resolves region addresses.
* `TestNoscanFileRegionPageAllocRoundTrip` — the dedicated page allocator.
* `TestLargeNoscanAllocFromRegion` / `TestLargeNoscanSurvivesGC` — routing and GC survival.
* `TestLargeNoscanReuseAfterFree` — end-to-end free→realloc.
* `TestLargeNoscanExhaustionFallback` — transparent fallback to the heap.
* `TestLargeNoscanNotScavenged` — the scavenger leaves the region resident.
* `TestLargeNoscanFinalizer` — finalizers on region objects.
* `TestLargeNoscanStress` — many sizes under GC pressure.
* `TestNoscanFileEnvIntegration` — runs a subprocess with the env vars set and
  verifies the allocation is file-backed end-to-end.

Run with:

```sh
bin/go test ./src/runtime -run 'TestLargeNoscan|TestNoscanFile|TestMapSharedFilePrimitive|TestParseMemSize'
```

## 9. Limitations and future work

* **Architectures.** Only `linux/amd64` and `linux/arm64`. Other platforms use
  the stub (feature disabled).
* **Large objects only.** Small noscan allocations are not routed.
* **Regular files only (today).** `ftruncate` is required to succeed, so a raw
  block device currently fails setup. Backing `/dev/zramN` requires adding a
  `BLKGETSIZE64`-based size path and skipping `ftruncate` for devices.
* **Fully resident.** Region pages are never scavenged. For zram this is usually
  fine (zram compresses transparently); for disk files, an optional mode that
  `MADV_DONTNEED`s cold region pages could be added.
* **Mapping stats.** The total file-mapping size is not added to
  `gcController.mappedReady`; in-use span bytes are accounted normally.
* **Single region.** One region per process, sized by `GONOSCANFILESIZE`; it
  does not grow. Exhaustion spills to the heap.
* **Lifecycle.** The backing file and mapping live for the whole process. The
  runtime does not unlink or shrink the file when objects are freed.

## 10. Quick start

```sh
# 1) Build the toolchain (once).
cd src && GOROOT_BOOTSTRAP=/path/to/go1.24.6+ ./make.bash

# 2) Run any program with the feature enabled.
GONOSCANFILE=/tmp/noscan.bin GONOSCANFILESIZE=512MiB ../bin/go run ./yourprogram
```

Large `[]byte`/`string` allocations in `yourprogram` now reside in
`/tmp/noscan.bin` (or `/dev/zramN`) instead of anonymous RAM, with no code
changes and full garbage-collection safety.
