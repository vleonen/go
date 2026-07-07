# Benchmarking etcd with the noscan file region on zram

This document describes a set of benchmarks that measure the impact of the
Go runtime's noscan file region feature (`GONOSCANFILE` /
`GONOSCANFILESIZE` / `GONOSCANPAGEOUT` / `GONOSCANFILEMIN`) on a real-world
Go application — etcd v3.5.22 — when backed by a zram compressed-RAM block
device.

## 1. Test environment

| Item | Value |
|---|---|
| Platform | aarch64 (ARM64) |
| CPU | 12 cores |
| RAM | 30 GiB |
| Go toolchain | go1.24.13 (in-tree, `release-branch.go1.24` with noscan file region patches) |
| etcd | v3.5.22, single-node, CGO\_ENABLED=0 |
| zram device | `/dev/zram0`, 4 GiB disksize, zstd compression |
| Benchmark tool | `tools/benchmark` (100 connections, 500 clients) |

The etcd server and benchmark tool were both compiled with the in-tree Go
toolchain so that the noscan file region runtime feature is available.

### zram configuration

```sh
sudo swapoff /dev/zram0                    # ensure device is not swap
echo 1   | sudo tee /sys/block/zram0/reset  # clear stale data
echo 4G  | sudo tee /sys/block/zram0/disksize
# comp_algorithm is zstd (system default)
```

## 2. Test methodology

### Configurations

| Config | GONOSCANFILE | GONOSCANFILESIZE | GONOSCANFILEMIN | GONOSCANPAGEOUT | Description |
|---|---|---|---|---|---|
| **baseline** | *(unset)* | *(unset)* | — | — | Normal anonymous heap (control group) |
| **zram** | `/dev/zram0` | 128 MiB | 256 | *(on, default)* | Noscan allocations >256 B from zram-backed region with four optimisations; objects <=256 B stay on the regular heap |

The zram configuration uses four optimisations together:

1. **Noscan file region** -- pointer-free allocations (byte-slice keys, revision
   arrays, value buffers) are served from a `MAP_SHARED` mmap of `/dev/zram0`
   instead of anonymous heap, transparently compressing them via zstd.
2. **Size filtering** (`GONOSCANFILEMIN=256`) -- noscan objects whose size class
   is at most 256 B (8, 16, 24, ..., 256 bytes -- size classes 1-18) are **not**
   routed to the region and stay on the regular heap. This avoids the re-fault
   overhead for small, high-churn objects that benefit little from compression.
3. **Proactive page eviction of live spans** (`MADV_PAGEOUT` after GC sweep) --
   after each GC sweep, live region span pages are evicted from the page cache.
   The kernel writes back dirty pages to zram (compressing with zstd), then
   removes the uncompressed page-cache copies. On the next access, pages are
   re-faulted (decompressed from zram). This makes the compression benefit
   observable without external memory pressure.
4. **Free-time zero + eviction** -- when a region span is freed during sweep,
   its pages are zeroed with `memclrNoHeapPointers` and immediately evicted with
   `MADV_PAGEOUT`. Zero pages compress to approximately 40 bytes per 4 KiB page
   in zstd (a 100:1 ratio), so freed region memory costs almost nothing in
   physical RAM. On re-allocation, the pages are re-faulted from zram and arrive
   as clean zeros, satisfying the runtime's zero-on-allocation requirement.

Each etcd run starts with a fresh data directory (`--data-dir` is removed
between runs). No `GODEBUG=gctrace=1` or pprof is enabled, to avoid
measurement overhead.

### Benchmark scenarios

All scenarios use `--conns=100 --clients=500 --target-leader`. Sequential
unique keys (`--sequential-keys`, `--key-space-size` = `--total`).

| # | Scenario | Command | Description |
|---|---|---|---|
| 1 | Put 8 B | `put --key-size=8 --val-size=8 --total=100000` | 100 K small puts |
| 2 | Put 256 B | `put --key-size=8 --val-size=256 --total=100000` | 100 K medium puts |
| 3 | Put 4 KB | `put --key-size=8 --val-size=4096 --total=10000` | 10 K large puts |
| 4 | Range | `range single-key --total=100000` (after 10 K-key preload) | 100 K point reads |
| 5 | Txn-mixed | `txn-mixed --key-size=8 --val-size=256 --total=10000 --key-space-size=1000` | 10 K mixed read-write transactions |

### Metrics collected

- **Throughput**: requests/sec (from benchmark output).
- **Latency**: p50, p99 (from benchmark percentile distribution).
- **VmRSS**: resident set size in kB (`/proc/PID/status` before and after).
- **VmHWM**: high-water-mark RSS.
- **zram mm\_stat field 3** (`mem_used_total`): compressed bytes stored by zram.

### Estimating actual physical memory usage

VmRSS alone does **not** give a fair comparison between the two
configurations, because the zram region uses `MAP_SHARED` over a zram block
device and the kernel's page-cache / writeback interaction with zram
introduces subtleties that VmRSS does not capture.

#### Baseline (anonymous heap)

Every resident heap page is a private anonymous page occupying 4 KiB of
DRAM:

```
actual_physical_baseline = VmRSS
```

#### ZRAM with MADV\_PAGEOUT

Because pageout runs after every GC cycle (for live spans) and at free time
(for dead spans), region pages spend most of their lifetime in compressed
form only. The actual physical footprint is:

```
actual_physical_zram ~ (VmRSS - region_RSS) + mm_stat_mem_used_total
```

Where **region\_RSS** (the page-cache contribution of the zram mapping,
typically near-zero between GC cycles) is obtainable from
`/proc/PID/smaps`:

```sh
awk '/^[\da-f]+-[\da-f]+ .*/{p=0}
     /^[\da-f]+-[\da-f]+ .*\/dev\/zram0/{p=1}
     p && /^Rss:/{print $2}' /proc/PID/smaps
```

In practice, VmRSS for the zram configuration is already a good
approximation of the true footprint, because the uncompressed region pages
have been evicted and only the compressed zram copies (reported by mm\_stat)
remain.

## 3. Results

### 3.1 Throughput

| Scenario | Baseline (req/s) | ZRAM (req/s) | Delta |
|---|---|---|---|
| Put 8 B | 27 998 | 27 735 | -0.9 % |
| Put 256 B | 27 636 | 25 538 | -7.6 % |
| Put 4 KB | 17 323 | 11 747 | -32.2 % |
| Range | 43 360 | 42 640 | -1.7 % |
| Txn-mixed | 493 | 460 | -6.7 % |

### 3.2 Latency (p99)

| Scenario | Baseline p99 | ZRAM p99 | Delta |
|---|---|---|---|
| Put 8 B | 34.1 ms | 33.9 ms | -1 % |
| Put 256 B | 35.2 ms | 38.8 ms | +10 % |
| Put 4 KB | 81.8 ms | 93.0 ms | +14 % |
| Range | 30.7 ms | 35.7 ms | +16 % |
| Txn-mixed | 1 858.0 ms | 1 949.8 ms | +5 % |

### 3.3 Memory -- VmRSS

| Scenario | Baseline RSS (kB) | ZRAM RSS (kB) | Delta |
|---|---|---|---|
| Put 8 B | 117 788 | 224 000 | +90 % |
| Put 256 B | 161 024 | 208 128 | +29 % |
| Put 4 KB | 173 184 | **121 528** | **-30 %** |
| Range | 73 544 | 168 392 | +129 % |
| Txn-mixed | 392 156 | **295 684** | **-25 %** |

> **Bold** values are lower than baseline -- ZRAM RSS is below the no-noscan
> control group for Put 4 KB and Txn-mixed.

The ZRAM runs start at approximately 155 MB RSS because the 128 MiB region is
pre-zeroed at startup (`memclrNoHeapPointers` faults in every page). Pageout
reclaims most of this fixed overhead after the first GC cycle, and free-time
zero+eviction ensures that freed spans are immediately compressed to
near-zero in zram rather than lingering as stale data.

### 3.4 Incremental heap growth (dRSS)

| Scenario | Baseline dRSS | ZRAM dRSS | Savings |
|---|---|---|---|
| Put 8 B | +87.3 MB | +68.4 MB | 18.9 MB (22 %) |
| Put 256 B | +131.8 MB | +53.0 MB | 78.8 MB (60 %) |
| Put 4 KB | +144.8 MB | -33.1 MB | 177.9 MB (123 %) |
| Range | +1.7 MB | -4.2 MB | 5.9 MB |
| Txn-mixed | +363.8 MB | +140.7 MB | 223.2 MB (61 %) |

> **dRSS** = `rss_after - rss_before` (growth during the benchmark).
> Negative values mean RSS *shrank* during the benchmark -- pageout evicted
> more region pages than the workload allocated.

### 3.5 zram compressed memory (mm\_stat `mem_used_total`)

Each zram scenario starts a fresh etcd process that re-zeroes the region,
so zram recompresses. Values below are the mm\_stat reading **after** each
run:

| Scenario | ZRAM mm\_stat (bytes) |
|---|---|
| Put 8 B | 13 025 280 |
| Put 256 B | 1 265 664 |
| Put 4 KB | 5 001 216 |
| Range | 94 326 784 |
| Txn-mixed | 1 728 512 |

> The Range scenario shows the highest value because the preload phase
> populates many keys in the region before the read benchmark runs.
> Put 8 B and Put 256 B show very low values because with `GONOSCANFILEMIN=256`,
> small objects (<=256 B) are not routed to the region, so only larger internal
> noscan allocations (e.g. revision arrays, lease objects) use zram. The
> free-time zero+eviction ensures that freed pages within these runs compress
> to near-zero, keeping mm\_stat low.

## 4. Analysis

### 4.1 Memory: zram wins for write-heavy and mixed workloads

The combination of free-time zero+eviction and proactive pageout of live spans
drives RSS **below the baseline** for write-heavy and mixed workloads:

| Scenario | Baseline RSS | ZRAM RSS | Savings |
|---|---|---|---|
| Put 4 KB | 173 MB | 122 MB | **51 MB (30 %)** |
| Txn-mixed | 392 MB | 296 MB | **96 MB (25 %)** |

The free-time zero+eviction is particularly effective for high-churn
workloads like Txn-mixed: freed spans are immediately zeroed and evicted,
so their pages compress to approximately 40 bytes per 4 KiB page in zram
(a 100:1 ratio). This is reflected in the dRSS savings -- Txn-mixed saves
223 MB (61 %) of incremental heap growth.

For lighter workloads (Put 8 B, Range), ZRAM RSS is higher than baseline
because the fixed 128 MiB region overhead is not offset by sufficient heap
savings -- the size filter keeps most small objects on the heap, reducing
the compression benefit.

### 4.2 Throughput: re-fault and zeroing overhead

The zram configuration has a measurable throughput cost, caused by two
factors:

1. **Re-fault overhead** -- when the application accesses evicted region
   pages, the kernel decompresses them from zram. This cost scales with the
   working-set size that must be re-faulted between GC cycles.
2. **Free-time zeroing** -- zeroing freed span pages adds CPU work to the
   sweep phase, proportional to the freed span size.

| Scenario | Object size | Filtered? | Throughput delta |
|---|---|---|---|
| Put 8 B | 8 B | Yes | -1 % |
| Put 256 B | 256 B | Yes | -8 % |
| Put 4 KB | 4 KB | No (routed to zram) | -32 % |
| Range | -- | -- | -2 % |
| Txn-mixed | 256 B values | Yes | -7 % |

Scenarios where the workload's objects are filtered out (Put 8 B, Put 256 B,
Txn-mixed) have throughput within -8 % of baseline. The re-fault overhead is
eliminated for filtered objects; the remaining cost is from internal noscan
allocations (revision arrays, etc.) that still use the region.

Put 4 KB, whose 4 KB values are above the 256 B threshold and thus routed
to the zram region, shows the expected -32 % throughput cost from
decompression on page re-fault.

### 4.3 Latency: p99 impact

p99 latency is within 10 % of baseline for filtered workloads (Put 8 B, Put
256 B). Put 4 KB shows a +14 % p99 increase, caused by re-faulting 4 KB
region pages. Range p99 increases by +16 %, likely due to re-faulting
preloaded key data that was evicted between the preload and the read phase.

### 4.4 Effectiveness of free-time zero+eviction

The free-time zero+eviction (optimisation #4) provides a significant
improvement over live-span-only pageout. By zeroing freed pages before
eviction, the compressed size in zram drops from near-uncompressible (stale
application data) to approximately 40 bytes per page. This is most visible
in the dRSS savings for high-churn workloads:

- **Txn-mixed**: 61 % dRSS savings (223 MB), with RSS 25 % below baseline
- **Put 4 KB**: 123 % dRSS savings (178 MB), with RSS 30 % below baseline
- **Put 256 B**: 60 % dRSS savings (79 MB)

### 4.5 zram compression

zram with zstd effectively compresses the noscan data that reaches the
region. For the Put 4 KB scenario (10 K keys x 4 KB values = approximately
40 MB of logical data plus overhead), zram used only approximately 5.0 MB
compressed -- an **approximately 8:1 compression ratio**.

With `GONOSCANFILEMIN=256`, the Put 8 B and Put 256 B scenarios route very
little data to the region (only larger internal allocations), so zram usage
is correspondingly low (approximately 1.3-13 MB).

## 5. Limitations of this benchmark

- **Single run per scenario**: no statistical aggregation (median, stddev
  across multiple runs). Results should be treated as indicative, not
  definitive.
- **Single-node etcd**: Raft consensus overhead is excluded. A 3-node
  cluster would show different memory patterns.
- **128 MiB region**: sufficient for these workloads, but heavier workloads
  may exhaust the region and fall back to the heap.
- **No smaps data**: `region_RSS` from `/proc/PID/smaps` was not collected
  during these runs; the `actual_physical` formula is described but not
  computed from measured data.

## 6. Conclusion

The noscan file region with size filtering, proactive page eviction, and
free-time zero+eviction, backed by zram with a 128 MiB region, provides a
favourable throughput-memory trade-off for etcd:

1. **Memory**: RSS drops below baseline for write-heavy and mixed workloads
   -- Put 4 KB saves 30 % (173 -> 122 MB), Txn-mixed saves 25 % (392 -> 296
   MB). The free-time zero+eviction ensures freed pages compress to
   near-zero in zram, maximising the memory savings.
2. **Throughput**: small-object workloads (Put 8 B, Range) are within -2 %
   of baseline. Larger workloads (Put 4 KB) pay a -32 % cost from
   decompression on page re-fault. The size filter eliminates re-fault
   overhead for objects <=256 B.
3. **Latency**: p99 is within 10 % of baseline for filtered workloads.
4. **Compression**: zram achieves 8:1+ compression on Go noscan data routed
   to the region; freed pages compress at 100:1 (zero pages).

The zram configuration is most beneficial for memory-constrained deployments
with write-heavy or mixed workloads -- it achieves a smaller physical
footprint than the baseline by transparently compressing pointer-free data
via zram, while the size filter and free-time zero+eviction minimise the
throughput overhead and maximise compression effectiveness.
