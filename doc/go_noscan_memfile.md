# File-backed memory region for noscan objects

This document describes the *noscan file region*: an optional runtime feature
that serves pointer-free heap allocations — large, small, and tiny — from a
dedicated, file-backed (or block-device-backed) memory region.

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
No source changes are required: ordinary `make([]byte, …)` and similar
allocations are transparently served from the region when it is enabled.

## 2. What is routed to the region

Allocations that are **noscan** — the allocated type contains no pointers
(`typ == nil || !typ.Pointers()`). This covers **all** noscan objects regardless
of size:

* **large** noscan objects (size class 0; backing arrays of big `[]byte`,
  `[]float64`, `string`, etc.), and
* **small** and **tiny** noscan objects (the per-size-class `mcentral`/`mcache`
  path, including the 16-byte tiny allocator).

In span-class terms, that is any span with the noscan bit set — all odd
`spanClass` values (large, small, and `tinySpanClass`).

Everything that is **not** noscan — scan allocations, stacks, GC metadata, user
arenas — is unaffected and continues to come from the regular heap.

Routing happens at the single span chokepoint `mheap.alloc`, which is reached
only when a span is grown (once per span, amortized over 128–1024 object
allocations), not on every allocation. When the region is enabled,
`noscanFileRegionAccepts` checks that the span class is noscan **and** that the
per-object size exceeds the configured minimum (`GONOSCANFILEMIN`, default 0).
Region spans are then preferred for noscan span growth; when the region is
exhausted, allocation falls back to the regular heap transparently.

> **Caveat (writeback churn).** Small and tiny noscan objects churn frequently.
> Because the region is file-backed, every span zero/refill touches the backing
> file's page cache, generating writeback. This is typically fine for zram
> (transparent compression) but can be a cost for a disk file under heavy
> small-object churn — prefer zram, a generously sized region, or (future) the
> selective annotation mode for disk-backed deployments.

## 3. Enabling the feature

The feature is controlled entirely by environment variables, read once at
process startup.

| Variable | Meaning |
|---|---|
| `GONOSCANFILE` | Path to the backing file or block device (e.g. `/var/lib/myapp/noscan.bin`, `/dev/zram0`). Created with mode 0600 if it does not exist. |
| `GONOSCANFILESIZE` | Size of the region in bytes, with an optional binary suffix: `K`/`KiB`/`KB` (1024), `M`/`MiB`/`MB`, `G`/`GiB`/`GB` (case-insensitive). Examples: `256MiB`, `1GB`, `536870912`. |
| `GONOSCANFILEMIN` | Minimum per-object size (bytes, same suffix syntax) for routing to the region. Objects whose size class is at or below this threshold stay on the regular heap. Default is the compile-time constant `defaultNoscanFileMinSize` (0 = accept all). Example: `256` filters out objects ≤256 B. |

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

> Note: a regular file is `ftruncate`-d to the configured size at startup,
> which guarantees zero pages. A block device cannot be truncated; when
> `ftruncate` fails the runtime queries the device size with
> `ioctl(BLKGETSIZE64)` and uses the device directly. Because a block device
> may retain data from previous use, the region is explicitly zeroed after
> mapping so the heap's zero-on-first-use assumption holds. Make sure the
> configured size does not exceed the device size.

### Compile-time minimum size

The env var `GONOSCANFILEMIN` defaults to the compile-time constant
`defaultNoscanFileMinSize` in `src/runtime/memfile.go` (set to 0 by default,
meaning all noscan objects are routed). To change the default without an env
var, edit the constant and rebuild:

```go
const defaultNoscanFileMinSize uintptr = 256 // filter <=256 B objects at build time
```

When `GONOSCANFILEMIN` is set in the environment, it overrides the
compile-time constant. An invalid value (e.g. `lots`) is silently ignored and
the compile-time default is used.

## 4. How it works

### 4.1 Region setup

At startup, after the environment is parsed and after `gcinit`, while the world
is still stopped (`runtime.schedinit`), `noscanFileRegionInit`:

1. Reserves a contiguous, heap-arena-aligned chunk of virtual address space with
   `sysReserveAligned` (a `PROT_NONE` anonymous reservation). The address is
   chosen by the kernel and is disjoint from the regular heap's arena hints.
2. Creates or opens the backing file (`open(O_RDWR|O_CREAT, 0600)`) and sizes
   it. For a regular file, `ftruncate` extends it to the requested size (and
   guarantees zero pages). If `ftruncate` fails (block device), the runtime
   queries the device size with `ioctl(BLKGETSIZE64)` and uses the device
   directly provided it is at least as large as the region; the region is then
   explicitly zeroed with `memclrNoHeapPointers` so that the heap's
   `allocNeedsZero` check (which assumes freshly-mapped pages are zero) remains
   valid.
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

`mheap.alloc` is the single chokepoint for **all** spans, small and large (small
spans arrive via `mcentral.grow`, large spans via `mcache.allocLarge`). When the
region is enabled and the requested class has the noscan bit set, it calls
`noscanFileRegionAlloc`:

* It draws `npages` from the region's own page allocator (`pages.alloc`).
* It reuses the standard `mheap.initSpan`, which sets the span class, sweep
  generation, state (`mSpanInUse`), `pageInUse` bit, `pagesInUse` counter,
  `needzero`, and publishes the span into `heapArena.spans`. `initSpan` handles
  every size class, so small and tiny spans are initialized identically to large
  ones (for noscan, no per-object pointer bitmap is reserved).
* It then updates the `spanAllocHeap` stats (`heapInUse`, the consistent `inHeap`
  counter) that the sweep path will later reverse.

If the region is exhausted, `noscanFileRegionAlloc` returns `nil` and `mheap.alloc`
falls through to the normal heap path (`allocSpan`), so exhaustion is
transparent. The object's `needzero` is handled correctly by the existing
`allocNeedsZero` mechanism (region arenas' `zeroedBase` advances monotonically),
so reused region pages are zeroed before reuse exactly like ordinary heap pages.

For small and tiny objects the surrounding allocation path is unchanged:
`mallocgcSmallNoscan`/`mallocgcTiny` use the per-P `mcache`'s cached span, and
only when that span is exhausted does `mcache.refill` → `mcentral.cacheSpan` →
`mcentral.grow` → `mheap.alloc` draw a fresh region span. Because the chokepoint
is per-span, the routing check executes at most once per 128–1024 object
allocations, and even less often in steady state (when `cacheSpan` is served from
swept partial spans). `mcache.allocLarge` does the rest of the large-object work
unchanged: it bumps `heapLive` (`gcController.update`), pushes the span onto the
size class's swept list so the background sweeper can find it, sets
`freeindex`/`allocCount`, and `mallocgcLarge` zero-fills and publishes the object.

### 4.3 Free routing

When a region span is freed, `mheap.freeSpanLocked` notices that its base lies in
a file region (`isFileRegionAddr`) and calls `noscanFileRegionFreeLocked`, which:

* mirrors the `spanAllocHeap` accounting that allocation performed
  (`pagesInUse--`, `heapInUse--`, `inHeap--`, clears the `pageInUse` bit);
* if pageout is enabled, **zeroes and evicts** the freed pages
  (`memclrNoHeapPointers` + `madvise(MADV_PAGEOUT)`) so that zram compresses
  them to near-nothing (zero pages compress to ~40 bytes per 4 KiB with
  zstd). On re-allocation the pages are re-faulted from zram and arrive as
  clean zeros;
* returns the pages to the **region's** page allocator (`pages.free`) instead of
  the heap's; and
* intentionally skips the heap's scavenge stats (`heapFree`/`heapReleased`) and
  `sysUsed`, since region pages are file-backed and never scavenged.

Region spans are freed through the same sweeper as ordinary spans of their size
class: large region spans are pushed onto the size-class-0 swept list at
allocation time (`mcache.allocLarge`), and small/tiny region spans cycle through
their `mcentral` partial/full lists just like heap spans. Either way, sweeping
needs no special handling.

### 4.4 Garbage collection

The region is a first-class part of the heap from the GC's point of view, so
marking and sweeping require no changes:

* **Marking.** When the GC follows a pointer into a region object,
  `greyobject` looks up the span via `spanOf` (which works because the region's
  arenas are registered) and fast-paths the noscan object to black, setting the
  `pageMarks` bit. The object's contents are never scanned, exactly as for any
  noscan object.
* **Sweeping.** Region spans are swept like any other span of the same class.
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
* `GONOSCANFILEMIN` — minimum per-object size for routing (optional; defaults
  to compile-time `defaultNoscanFileMinSize`, which is 0 = accept all).

Size suffixes recognized (case-insensitive): none / `B`, `K`/`KB`/`KiB`,
`M`/`MB`/`MiB`, `G`/`GB`/`GiB`. Multipliers are power-of-two (1 KiB = 1024).

A zero or unparseable size disables the feature. The feature is a no-op on
non-Linux or non-amd64/arm64 builds (a build-tagged stub returns "disabled").

## 7. File map

| File | Role |
|---|---|
| `src/runtime/memfile.go` | Region manager, config parsing, alloc/free routing, startup init, `ioctl`/`blkGetSize64` for block-device sizing (build tag `linux && (amd64 \|\| arm64)`). |
| `src/runtime/memfile_stub.go` | Disabled-feature stubs for all other platforms. |
| `src/runtime/mheap.go` | Routing in `mheap.alloc` and `mheap.freeSpanLocked`. |
| `src/runtime/proc.go` | Calls `noscanFileRegionInit()` from `schedinit`. |
| `src/runtime/defs_linux_{amd64,arm64}.go` | `_MAP_SHARED`, `_O_RDWR`. |
| `src/internal/runtime/syscall/defs_linux_{amd64,arm64}.go` | `SYS_IOCTL` constant. |
| `src/runtime/sys_linux_{amd64,arm64}.s` | `ftruncate` system-call stub. |
| `src/runtime/memfile_test.go` | Tests (see §8). |
| `src/runtime/export_memfile_test.go` | Exports internals to the external test package. |

Key runtime functions: `noscanFileRegionInit`, `noscanFileRegionAccepts`,
`noscanFileRegionAlloc`, `noscanFileRegionFreeLocked`,
`noscanFileRegion.setup`, `registerArenas`, `parseNoscanFileConfig`.

## 8. Testing

`src/runtime/memfile_test.go` covers each layer:

* `TestMapSharedFilePrimitive` — `MAP_SHARED` + `ftruncate` primitives.
* `TestParseMemSize`, `TestNoscanFileConfigFromEnv` — config parsing.
* `TestNoscanFileRegionSetup` — region maps a file and writes persist.
* `TestNoscanFileRegionArenaRegistered` — arena metadata resolves region addresses.
* `TestNoscanFileRegionPageAllocRoundTrip` — the dedicated page allocator.
* `TestLargeNoscanAllocFromRegion` / `TestLargeNoscanSurvivesGC` — large routing and GC survival.
* `TestLargeNoscanReuseAfterFree` — end-to-end free→realloc.
* `TestLargeNoscanExhaustionFallback` — transparent fallback to the heap.
* `TestSmallNoscanAllocFromRegion` / `TestSmallNoscanSurvivesGC` — small noscan routing and GC survival.
* `TestSmallNoscanFilteredByMinSize` — verifies that minSize threshold keeps small objects on the heap.
* `TestFreeRegionZeroAndPageout` — verifies that freed region pages are zeroed and evicted.
* `TestTinyNoscanFromRegion` — tiny (sub-16-byte) noscan routing.
* `TestNoscanMixedStress` — tiny/small/large together under GC.
* `TestLargeNoscanNotScavenged` — the scavenger leaves the region resident.
* `TestLargeNoscanFinalizer` — finalizers on region objects.
* `TestLargeNoscanStress` — many sizes under GC pressure.
* `TestNoscanFileEnvIntegration` — runs a subprocess with the env vars set and
  verifies both a large and a small noscan allocation are file-backed end-to-end.

Run with:

```sh
bin/go test ./src/runtime -run 'TestLargeNoscan|TestSmallNoscan|TestTinyNoscan|TestNoscanMixed|TestNoscanFile|TestMapSharedFilePrimitive|TestParseMemSize'
```

## 9. Limitations

* **Architectures.** Only `linux/amd64` and `linux/arm64`. Other platforms use
  the stub (feature disabled).
* **Fully resident.** Region pages are never scavenged. For zram this is usually
  fine (zram compresses transparently); for disk files, an optional mode that
  `MADV_DONTNEED`s cold region pages could be added.
* **Writeback churn.** Because the region is file-backed, small/tiny noscan
  churn generates page-cache writeback (see §2 caveat). Prefer zram or a large
  region for disk-backed deployments.
* **Mapping stats.** The total file-mapping size is not added to
  `gcController.mappedReady`; in-use span bytes are accounted normally.
* **Single region.** One region per process, sized by `GONOSCANFILESIZE`; it
  does not grow. Exhaustion spills to the heap.
* **Lifecycle.** The backing file and mapping live for the whole process. The
  runtime does not unlink or shrink the file when objects are freed.

## 10. Future improvements

A tracked list of useful follow-ups for the feature (not yet implemented):

1. **Selective annotation.** A `//go:noscanfile` pragma and/or an arena-style
   API (`FileArena`) to route only specific functions'/objects' noscan to the
   region. This cuts writeback churn for disk-backed deployments (where routing
   *all* noscan may be too aggressive) and gives explicit lifetime control.
2. **Growable region.** Allow the region to extend (map additional file-backed
   chunks) on exhaustion instead of always falling back to the regular heap.
3. **Scavenging policy.** An optional mode that `MADV_DONTNEED`s cold region
   pages (mainly useful for disk-backed regions) to reclaim clean file pages.
4. **Accurate global stats.** Add the region mapping to
   `gcController.mappedReady`, and expose region usage (bytes in use, exhaustion
   count) via `runtime.ReadMemStats` or a `GODEBUG` readout.
5. **More architectures.** Extend beyond `linux/amd64` and `linux/arm64`
   (e.g. `riscv64`, `loong64`, `ppc64le`). Note the 32-bit `off_t` complication
   for `ftruncate` on `linux/386` and `linux/arm`.
6. **Huge-page hint.** Apply `MADV_HUGEPAGE` to the region mapping where it
   would reduce TLB pressure.
7. **File reclamation.** Hole-punch (`fallocate` with
   `FALLOC_FL_PUNCH_HOLE`) or shrink the backing file as region spans free,
   instead of holding a fixed-size file for the process lifetime.

## 11. Quick start

```sh
# 1) Build the toolchain (once).
cd src && GOROOT_BOOTSTRAP=/path/to/go1.24.6+ ./make.bash

# 2) Run any program with the feature enabled.
GONOSCANFILE=/tmp/noscan.bin GONOSCANFILESIZE=512MiB ../bin/go run ./yourprogram
```

Noscan allocations in `yourprogram` (`[]byte`, `[]float64`, `string`, etc.,
large, small, and tiny) now reside in `/tmp/noscan.bin` (or `/dev/zramN`)
instead of anonymous RAM, with no code changes and full garbage-collection
safety.
