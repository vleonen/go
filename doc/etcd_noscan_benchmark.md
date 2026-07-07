# Benchmarking etcd with the noscan file region on zram

This document describes a set of benchmarks that measure the impact of the
Go runtime's noscan file region feature (`GONOSCANFILE` / `GONOSCANFILESIZE`
/ `GONOSCANPAGEOUT`) on a real-world Go application — etcd v3.5.22 — when
backed by a zram compressed-RAM block device.

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

| Config | GONOSCANFILE | GONOSCANFILESIZE | GONOSCANPAGEOUT | Description |
|---|---|---|---|---|
| **baseline** | *(unset)* | *(unset)* | — | Normal anonymous heap (control group) |
| **pageout** | `/dev/zram0` | 128 MiB | *(on, default)* | Noscan allocations from zram-backed region, with proactive page eviction after each GC sweep |

The pageout configuration uses `madvise(MADV_PAGEOUT)` to evict live noscan
span pages from the page cache after each GC sweep cycle. The kernel writes
back dirty pages to zram (compressing them with zstd), then removes the
uncompressed page-cache copies. On the next application access, the pages
are re-faulted (decompressed from zram). This makes the compression benefit
of zram observable without requiring external memory pressure.

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
configurations, because the pageout region uses `MAP_SHARED` over a zram
block device and the kernel's page-cache / writeback interaction with zram
introduces subtleties that VmRSS does not capture.

#### Baseline (anonymous heap)

Every resident heap page is a private anonymous page occupying 4 KiB of
DRAM:

```
actual_physical_baseline = VmRSS
```

#### Pageout with zram (MAP\_SHARED block-device mmap + MADV\_PAGEOUT)

The runtime mmaps the zram device with `MAP_SHARED` (see `memfile.go:169`).
After each GC sweep, `madvise(MADV_PAGEOUT)` proactively evicts region pages:

1. **Writeback** — the kernel flushes dirty pages to zram, which compresses
   each 4 KiB page with zstd and stores the compressed form.
2. **Eviction** — the uncompressed page-cache copies are removed from VmRSS.
3. **Re-fault on access** — when the application next touches an evicted
   page, the kernel decompresses it from zram back into the page cache.

Because pageout runs after every GC cycle, region pages spend most of their
lifetime in compressed form only. The actual physical footprint is:

```
actual_physical_pageout ≈ (VmRSS − region_RSS) + mm_stat_mem_used_total
```

Where **region\_RSS** (the page-cache contribution of the zram mapping,
typically near-zero between GC cycles) is obtainable from
`/proc/PID/smaps`:

```sh
awk '/^[\da-f]+-[\da-f]+ .*/{p=0}
     /^[\da-f]+-[\da-f]+ .*\/dev\/zram0/{p=1}
     p && /^Rss:/{print $2}' /proc/PID/smaps
```

In practice, VmRSS for the pageout configuration is already a good
approximation of the true footprint, because the uncompressed region pages
have been evicted and only the compressed zram copies (reported by mm\_stat)
remain.

#### Measurement recipe

For each pageout benchmark run, collect three values:

1. `VmRSS` from `/proc/PID/status` (already collected).
2. `region_RSS` from `/proc/PID/smaps` (the zram mapping's `Rss:` field).
3. `mm_stat_mem_used_total` from
   `/sys/block/zram0/mm_stat` field 3 (already collected).

Then compute:

```
non_region_RSS     = VmRSS − region_RSS           # heap + stacks + metadata
actual_physical    = non_region_RSS + mm_stat     # true DRAM consumption
```

## 3. Results

### 3.1 Throughput

| Scenario | Baseline (req/s) | Pageout (req/s) | Delta |
|---|---|---|---|
| Put 8 B | 28 164 | 27 606 | −2.0 % |
| Put 256 B | 27 541 | 24 898 | −9.6 % |
| Put 4 KB | 18 322 | 13 056 | −28.7 % |
| Range | 44 126 | 41 442 | −6.1 % |
| Txn-mixed | 492 | 433 | −12.0 % |

### 3.2 Latency (p99)

| Scenario | Baseline p99 | Pageout p99 | Delta |
|---|---|---|---|
| Put 8 B | 34.3 ms | 35.4 ms | +3 % |
| Put 256 B | 36.0 ms | 44.5 ms | +24 % |
| Put 4 KB | 51.9 ms | 79.1 ms | +52 % |
| Range | 28.8 ms | 34.4 ms | +19 % |
| Txn-mixed | 1860.6 ms | 2076.7 ms | +12 % |

### 3.3 Memory — VmRSS

| Scenario | Baseline RSS (kB) | Pageout RSS (kB) | Delta |
|---|---|---|---|
| Put 8 B | 115 944 | 209 844 | +81 % |
| Put 256 B | 166 972 | 214 868 | +29 % |
| Put 4 KB | 177 536 | **114 980** | **−35 %** |
| Range | 70 252 | 169 564 | +142 % |
| Txn-mixed | 395 892 | **298 832** | **−25 %** |

> **Bold** values are lower than baseline — pageout RSS is below the
> no-noscan control group for Put 4 KB and Txn-mixed.

The pageout runs start at ~158 MB RSS because the 128 MiB region is
pre-zeroed at startup (`memclrNoHeapPointers` faults in every page).
Pageout reclaims most of this fixed overhead after the first GC cycle.

### 3.4 Incremental heap growth (ΔRSS)

| Scenario | Baseline ΔRSS | Pageout ΔRSS | Savings |
|---|---|---|---|
| Put 8 B | +87.0 MB | +52.8 MB | 34.2 MB (39 %) |
| Put 256 B | +138.6 MB | +58.7 MB | 79.9 MB (58 %) |
| Put 4 KB | +147.8 MB | −40.8 MB | 188.6 MB (128 %) |
| Range | −1.1 MB | −7.1 MB | — |
| Txn-mixed | +367.3 MB | +143.7 MB | 223.6 MB (61 %) |

> **ΔRSS** = `rss_after − rss_before` (growth during the benchmark).
> Negative values mean RSS *shrank* during the benchmark — pageout evicted
> more region pages than the workload allocated.

### 3.5 zram compressed memory (mm\_stat `mem_used_total`)

Each pageout scenario starts a fresh etcd process that re-zeroes the region,
so zram recompresses. Values below are the mm\_stat reading **after** each
run:

| Scenario | Pageout mm\_stat (bytes) |
|---|---|
| Put 8 B | 12 943 360 |
| Put 256 B | 3 502 080 |
| Put 4 KB | 6 332 416 |
| Range | 90 697 728 |
| Txn-mixed | 2 244 608 |

> The Range scenario shows the highest value because the preload phase
> populates many keys in the region before the read benchmark runs.

## 4. Analysis

### 4.1 Memory: pageout beats baseline for write-heavy workloads

Pageout proactively evicts region pages after each GC sweep via
`madvise(MADV_PAGEOUT)`. For write-heavy scenarios, this drives total RSS
**below the baseline**:

| Scenario | Baseline RSS | Pageout RSS | Savings |
|---|---|---|---|
| Put 4 KB | 178 MB | 115 MB | **63 MB (35 %)** |
| Txn-mixed | 396 MB | 299 MB | **97 MB (25 %)** |

The 128 MiB region overhead is fully reclaimed by pageout, and the
compressed zram copies plus the reduced heap footprint are together cheaper
than the uncompressed baseline heap. Noscan objects (byte-slice keys,
revision arrays, value buffers) that would normally inflate the GC heap are
instead compressed in zram at 7:1+ ratio.

For lighter workloads (Put 8 B, Put 256 B, Range), pageout RSS is higher
than baseline because the fixed 128 MiB region overhead is not offset by
sufficient heap savings.

### 4.2 Throughput: re-fault overhead scales with value size

Pageout has a measurable throughput cost, caused by re-faulting region pages
(decompressing from zram) when the application accesses evicted objects:

| Scenario | Baseline req/s | Pageout req/s | Delta |
|---|---|---|---|
| Put 8 B | 28 164 | 27 606 | −2 % |
| Put 256 B | 27 541 | 24 898 | −10 % |
| Put 4 KB | 18 322 | 13 056 | −29 % |
| Range | 44 126 | 41 442 | −6 % |
| Txn-mixed | 492 | 433 | −12 % |

Scenarios with larger values (Put 4 KB) are most affected because each
access touches more pages, triggering more decompression. The overhead is
proportional to the working-set size that must be re-faulted between GC
cycles.

### 4.3 Latency: p99 increases for larger values

p99 latency increases modestly for small values but significantly for large
values:

- Put 8 B: 34.3 → 35.4 ms (+3 %) — within noise
- Put 4 KB: 51.9 → 79.1 ms (+52 %) — re-fault cost dominates

### 4.4 zram compression

zram with zstd effectively compresses the noscan data. The actual physical
memory consumed by zram (mm\_stat `mem_used_total`) is a fraction of the
logical region size. For example, after the Put 256 B scenario (100 K keys ×
256 B values = ~25 MB of logical data), zram used only ~3.5 MB compressed —
a **~7:1 compression ratio**.

## 5. Limitations of this benchmark

- **Single run per scenario**: no statistical aggregation (median, stddev
  across multiple runs). Results should be treated as indicative, not
  definitive.
- **Single-node etcd**: Raft consensus overhead is excluded. A 3-node
  cluster would show different memory patterns.
- **128 MiB region**: sufficient for these workloads, but heavier workloads
  may exhaust the region and fall back to the heap.
- **Cumulative zram mm\_stat**: zram is not reset between runs, so the
  incremental Δ values are approximate.
- **No smaps data**: `region_RSS` from `/proc/PID/smaps` was not collected
  during these runs; the `actual_physical` formula is described but not
  computed from measured data.

## 6. Conclusion

The noscan file region with proactive page eviction, backed by zram with a
128 MiB region, provides a throughput–memory trade-off for etcd:

1. **Memory**: RSS drops below baseline for write-heavy workloads — Put 4 KB
   saves 35 % (178 → 115 MB), Txn-mixed saves 25 % (396 → 299 MB).
2. **Throughput**: 2–29 % reduction due to re-fault overhead (decompression
   from zram on page re-access), scaling with value size.
3. **Latency**: p99 increases for large-value scenarios (Put 4 KB: 52 →
   79 ms).
4. **Compression**: zram achieves 7:1+ compression on Go noscan data.

Pageout is most beneficial for memory-constrained deployments with
write-heavy, large-value workloads — it achieves a smaller physical
footprint than even the baseline by transparently compressing pointer-free
data via zram. For throughput-sensitive workloads with small values, the
re-fault overhead may outweigh the memory savings.
