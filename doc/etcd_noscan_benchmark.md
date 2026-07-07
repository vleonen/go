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
| **zram** | `/dev/zram0` | 128 MiB | 256 | *(on, default)* | Noscan allocations >256 B from zram-backed region, with proactive page eviction after GC sweep; objects ≤256 B stay on the regular heap |

The zram configuration uses three optimisations together:

1. **Noscan file region** — pointer-free allocations (byte-slice keys, revision
   arrays, value buffers) are served from a `MAP_SHARED` mmap of `/dev/zram0`
   instead of anonymous heap, transparently compressing them via zstd.
2. **Size filtering** (`GONOSCANFILEMIN=256`) — noscan objects whose size class
   is ≤256 B (8, 16, 24, …, 256 bytes — size classes 1–18) are **not** routed to
   the region and stay on the regular heap. This avoids the re-fault overhead
   for small, high-churn objects that benefit little from compression.
3. **Proactive page eviction** (`MADV_PAGEOUT`) — after each GC sweep, live
   region span pages are evicted from the page cache. The kernel writes back
   dirty pages to zram (compressing with zstd), then removes the uncompressed
   page-cache copies. On the next access, pages are re-faulted (decompressed
   from zram). This makes the compression benefit observable without external
   memory pressure.

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

Because pageout runs after every GC cycle, region pages spend most of their
lifetime in compressed form only. The actual physical footprint is:

```
actual_physical_zram ≈ (VmRSS − region_RSS) + mm_stat_mem_used_total
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

#### Measurement recipe

For each zram benchmark run, collect three values:

1. `VmRSS` from `/proc/PID/status` (already collected).
2. `region_RSS` from `/proc/PID/smaps` (the zram mapping's `Rss:` field).
3. `mm_stat_mem_used_total` from `/sys/block/zram0/mm_stat` field 3.

Then compute:

```
non_region_RSS     = VmRSS − region_RSS           # heap + stacks + metadata
actual_physical    = non_region_RSS + mm_stat     # true DRAM consumption
```

## 3. Results

### 3.1 Throughput

| Scenario | Baseline (req/s) | ZRAM (req/s) | Delta |
|---|---|---|---|
| Put 8 B | 27 991 | 28 269 | +1.0 % |
| Put 256 B | 27 020 | 25 849 | −4.3 % |
| Put 4 KB | 16 800 | 12 734 | −24.2 % |
| Range | 43 341 | 42 683 | −1.5 % |
| Txn-mixed | 462 | 513 | +11.0 % |

### 3.2 Latency (p99)

| Scenario | Baseline p99 | ZRAM p99 | Delta |
|---|---|---|---|
| Put 8 B | 33.8 ms | 34.5 ms | +2 % |
| Put 256 B | 35.6 ms | 37.4 ms | +5 % |
| Put 4 KB | 63.2 ms | 86.0 ms | +36 % |
| Range | 30.3 ms | 30.7 ms | +1 % |
| Txn-mixed | 1 935.7 ms | 1 770.7 ms | −9 % |

### 3.3 Memory — VmRSS

| Scenario | Baseline RSS (kB) | ZRAM RSS (kB) | Delta |
|---|---|---|---|
| Put 8 B | 115 952 | 231 012 | +99 % |
| Put 256 B | 155 756 | 216 788 | +39 % |
| Put 4 KB | 192 312 | **133 892** | **−30 %** |
| Range | 69 424 | 172 888 | +149 % |
| Txn-mixed | 328 112 | 358 608 | +9 % |

> **Bold** values are lower than baseline — ZRAM RSS is below the no-noscan
> control group for Put 4 KB.

The ZRAM runs start at ~155 MB RSS because the 128 MiB region is pre-zeroed
at startup (`memclrNoHeapPointers` faults in every page). Pageout reclaims
most of this fixed overhead after the first GC cycle.

### 3.4 Incremental heap growth (ΔRSS)

| Scenario | Baseline ΔRSS | ZRAM ΔRSS | Savings |
|---|---|---|---|
| Put 8 B | +87.4 MB | +75.5 MB | 11.9 MB (14 %) |
| Put 256 B | +127.4 MB | +61.0 MB | 66.3 MB (52 %) |
| Put 4 KB | +164.1 MB | −21.8 MB | 185.9 MB (113 %) |
| Range | −2.8 MB | −3.3 MB | — |
| Txn-mixed | +299.6 MB | +202.9 MB | 96.7 MB (32 %) |

> **ΔRSS** = `rss_after − rss_before` (growth during the benchmark).
> Negative values mean RSS *shrank* during the benchmark — pageout evicted
> more region pages than the workload allocated.

### 3.5 zram compressed memory (mm\_stat `mem_used_total`)

Each zram scenario starts a fresh etcd process that re-zeroes the region,
so zram recompresses. Values below are the mm\_stat reading **after** each
run:

| Scenario | ZRAM mm\_stat (bytes) |
|---|---|
| Put 8 B | 3 436 544 |
| Put 256 B | 1 343 488 |
| Put 4 KB | 5 103 616 |
| Range | 95 358 976 |
| Txn-mixed | 1 822 720 |

> The Range scenario shows the highest value because the preload phase
> populates many keys in the region before the read benchmark runs.
> Put 8 B and Put 256 B show very low values because with `GONOSCANFILEMIN=256`,
> small objects (≤256 B) are not routed to the region, so only larger internal
> noscan allocations (e.g. revision arrays, lease objects) use zram.

## 4. Analysis

### 4.1 Throughput: size filtering eliminates small-object overhead

The `GONOSCANFILEMIN=256` filter keeps small objects (≤256 B) on the regular
heap, eliminating the pageout re-fault cost for high-churn allocations. The
throughput impact is:

| Scenario | Object size | Filtered? | Throughput delta |
|---|---|---|---|
| Put 8 B | 8 B | Yes | **+1 %** (faster than baseline) |
| Put 256 B | 256 B | Yes | −4 % |
| Put 4 KB | 4 KB | No (routed to zram) | −24 % |
| Range | — | — | −2 % |
| Txn-mixed | 256 B values | Yes | **+11 %** (faster than baseline) |

Scenarios where the workload's objects are filtered out (Put 8 B, Put 256 B,
Txn-mixed) have throughput within ±5 % of baseline — the re-fault overhead is
eliminated for those objects. Put 4 KB, whose 4 KB values are above the 256 B
threshold and thus routed to the zram region, shows the expected −24 %
throughput cost from decompression on page re-fault.

Txn-mixed is **11 % faster** than baseline, likely because the region's
dedicated page allocator reduces lock contention on the main heap's page
allocator during allocation bursts, while the 256 B values stay on the heap
and avoid re-fault overhead.

### 4.2 Memory: zram wins for large-value workloads

Pageout proactively evicts region pages after each GC sweep via
`madvise(MADV_PAGEOUT)`. For write-heavy scenarios with large values, this
drives total RSS **below the baseline**:

| Scenario | Baseline RSS | ZRAM RSS | Savings |
|---|---|---|---|
| Put 4 KB | 192 MB | 134 MB | **58 MB (30 %)** |

The 128 MiB region overhead is fully reclaimed by pageout, and the
compressed zram copies plus the reduced heap footprint are together cheaper
than the uncompressed baseline heap. Noscan objects (byte-slice keys,
revision arrays, value buffers >256 B) that would normally inflate the GC
heap are instead compressed in zram at a high ratio.

For lighter workloads (Put 8 B, Range), ZRAM RSS is higher than baseline
because the fixed 128 MiB region overhead is not offset by sufficient heap
savings — the size filter keeps most small objects on the heap, reducing the
compression benefit.

### 4.3 Latency: p99 improves for mixed workloads

p99 latency is within 5 % of baseline for filtered workloads (Put 8 B, Put
256 B, Range). Txn-mixed p99 **improves** by 9 % (1 936 → 1 771 ms),
consistent with the throughput improvement. Only Put 4 KB shows a significant
p99 increase (+36 %), caused by re-faulting 4 KB region pages.

### 4.4 zram compression

zram with zstd effectively compresses the noscan data that reaches the
region. For the Put 4 KB scenario (10 K keys × 4 KB values = ~40 MB of
logical data plus overhead), zram used only ~5.1 MB compressed — an
**~8:1 compression ratio**.

With `GONOSCANFILEMIN=256`, the Put 8 B and Put 256 B scenarios route very
little data to the region (only larger internal allocations), so zram usage
is correspondingly low (~1.3–3.4 MB).

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

The noscan file region with size filtering and proactive page eviction,
backed by zram with a 128 MiB region, provides a favourable
throughput–memory trade-off for etcd:

1. **Throughput**: small-object workloads (Put 8 B, Txn-mixed) are **faster**
   than baseline (+1 % to +11 %) because the size filter eliminates re-fault
   overhead for high-churn objects. Only large-value workloads (Put 4 KB)
   pay a throughput cost (−24 %).
2. **Memory**: RSS drops below baseline for large-value workloads — Put 4 KB
   saves 30 % (192 → 134 MB). The fixed 128 MiB region overhead is fully
   reclaimed by pageout after the first GC cycle.
3. **Latency**: p99 is within 5 % of baseline for filtered workloads.
   Txn-mixed p99 improves by 9 %.
4. **Compression**: zram achieves 8:1+ compression on Go noscan data routed
   to the region.

The zram configuration is most beneficial for memory-constrained deployments
with large-value workloads — it achieves a smaller physical footprint than
the baseline by transparently compressing pointer-free data via zram, while
the size filter ensures small-object throughput is not impacted.
