#!/bin/bash
#
# etcd_noscan_bench.sh — Reproduce the noscan file region etcd benchmark.
#
# Builds etcd + benchmark tool with the in-tree Go toolchain, then runs
# five benchmark scenarios under two configurations (baseline vs noscan
# with zram backing), collecting throughput, latency, and memory metrics.
#
# Usage:
#   ./etcd_noscan_bench.sh [OPTIONS]
#
# Options:
#   --goroot DIR       Go source tree root        (default: inferred)
#   --etcd-root DIR    etcd source tree           (default: ~/dev/etcd)
#   --results DIR      Output directory            (default: ./results)
#   --region SIZE      GONOSCANFILESIZE            (default: 128MiB)
#   --device PATH      zram block device           (default: /dev/zram0)
#   --conns N          gRPC connections            (default: 100)
#   --clients N        gRPC clients                (default: 500)
#   --skip-build       Use existing binaries       (skip build step)
#   --only CONFIG      Run only one config         (baseline|noscan|zram)
#   -h, --help         Show this help
#
# Prerequisites:
#   - The Go source tree (GOROOT) with noscan file region patches.
#   - The etcd source tree (checked out to a tag compatible with the
#     GOROOT's Go version).
#   - A zram block device, not in use as swap, with disksize >= region.
#
# Example:
#   ./etcd_noscan_bench.sh --goroot ~/dev/go_upstream --etcd-root ~/dev/etcd
#
set -euo pipefail

# ---------------------------------------------------------------------------
# Defaults
# ---------------------------------------------------------------------------
GOROOT=""
ETCD_ROOT="$HOME/dev/etcd"
RESULTS="./results"
REGION_SIZE="128MiB"
DEVICE="/dev/zram0"
CONNS=100
CLIENTS=500
SKIP_BUILD=0
ONLY=""

# ---------------------------------------------------------------------------
# Argument parsing
# ---------------------------------------------------------------------------
while [[ $# -gt 0 ]]; do
  case "$1" in
    --goroot)      GOROOT="$2"; shift 2 ;;
    --etcd-root)   ETCD_ROOT="$2"; shift 2 ;;
    --results)     RESULTS="$2"; shift 2 ;;
    --region)      REGION_SIZE="$2"; shift 2 ;;
    --device)      DEVICE="$2"; shift 2 ;;
    --conns)       CONNS="$2"; shift 2 ;;
    --clients)     CLIENTS="$2"; shift 2 ;;
    --skip-build)  SKIP_BUILD=1; shift ;;
    --only)        ONLY="$2"; shift 2 ;;
    -h|--help)
      sed -n '3,/^$/s/^# \?//p' "$0"
      exit 0 ;;
    *) echo "Unknown option: $1"; exit 1 ;;
  esac
done

# Infer GOROOT from the script location if not set.
if [[ -z "$GOROOT" ]]; then
  SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
  if [[ -f "$SCRIPT_DIR/../src/runtime/memfile.go" ]]; then
    GOROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
  else
    echo "ERROR: cannot infer GOROOT; pass --goroot" >&2
    exit 1
  fi
fi

GO_BIN="$GOROOT/bin/go"
GO_ENV="PATH=$GOROOT/bin:/usr/local/go/bin:/usr/bin:/bin"
GO_ENV+=" GOROOT=$GOROOT GOTOOLCHAIN=local"
GO_ENV+=" GOCACHE=${GOCACHE:-$HOME/.cache/go-build}"
GO_ENV+=" GOMODCACHE=${GOMODCACHE:-$HOME/go/pkg/mod}"
GO_ENV+=" GOPATH=${GOPATH:-$HOME/go}"
GO_ENV+=" CGO_ENABLED=0"

ETCD_BIN="$ETCD_ROOT/bin/etcd"
ETCDCTL_BIN="$ETCD_ROOT/bin/etcdctl"
BENCH_BIN="$ETCD_ROOT/bin/benchmark"
DATADIR="/tmp/etcd-bench-data"

mkdir -p "$RESULTS"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------
log()  { echo "[$(date +%H:%M:%S)] $*"; }

get_rss() { awk '/^VmRSS:/{print $2}' /proc/$1/status 2>/dev/null; }
get_hwm() { awk '/^VmHWM:/{print $2}' /proc/$1/status 2>/dev/null; }
get_mm()  { awk '{print $3}' "${DEVICE/mm_stat}" 2>/dev/null \
              <<< "$(cat /sys/block/$(basename $DEVICE)/mm_stat 2>/dev/null)" \
              || echo 0; }

# Simpler mm_stat reader
zram_mm() {
  local stat_file="/sys/block/$(basename "$DEVICE")/mm_stat"
  if [[ -f "$stat_file" ]]; then
    awk '{print $3}' "$stat_file"
  else
    echo 0
  fi
}

start_etcd() {
  local label=$1
  rm -rf "$DATADIR"; mkdir -p "$DATADIR"
  env -i PATH=/usr/bin:/bin ${NOSCAN_ENV:-} \
    "$ETCD_BIN" --name single \
    --listen-client-urls http://127.0.0.1:2379 \
    --advertise-client-urls http://127.0.0.1:2379 \
    --listen-peer-urls http://127.0.0.1:12380 \
    --initial-advertise-peer-urls http://127.0.0.1:12380 \
    --initial-cluster 'single=http://127.0.0.1:12380' \
    --initial-cluster-state new --initial-cluster-token etcd-bench \
    --data-dir "$DATADIR" --logger=zap --log-outputs=discard \
    > "$RESULTS/${label}_etcd.log" 2>&1 &
  ETCD_PID=$!
  for i in $(seq 1 60); do
    if "$ETCDCTL_BIN" --endpoints=127.0.0.1:2379 endpoint health \
        >/dev/null 2>&1; then return 0; fi
    kill -0 "$ETCD_PID" 2>/dev/null \
      || { log "FAIL: etcd died ($label)"; tail -10 "$RESULTS/${label}_etcd.log"; return 1; }
    sleep 0.5
  done
  log "FAIL: etcd not ready ($label)"; return 1
}

stop_etcd() {
  kill "$ETCD_PID" 2>/dev/null || true
  wait "$ETCD_PID" 2>/dev/null || true
  sleep 1
}

run_bench() {
  local label=$1 desc=$2; shift 2
  log "START $label: $desc"
  start_etcd "$label" || return 1

  local rss_b hwm_b mm_b rss_a hwm_a mm_a
  rss_b=$(get_rss "$ETCD_PID"); hwm_b=$(get_hwm "$ETCD_PID"); mm_b=$(zram_mm)

  "$BENCH_BIN" "$@" > "$RESULTS/${label}_bench.txt" 2>&1 || true

  rss_a=$(get_rss "$ETCD_PID"); hwm_a=$(get_hwm "$ETCD_PID"); mm_a=$(zram_mm)
  cat > "$RESULTS/${label}_metrics.txt" << EOF
label=$label
desc=$desc
rss_before=$rss_b
rss_after=$rss_a
hwm_after=$hwm_a
mm_before=$mm_b
mm_after=$mm_a
EOF
  log "DONE $label  RSS ${rss_b}->${rss_a} kB (peak ${hwm_a})  zram ${mm_b}->${mm_a}"
  stop_etcd
}

run_range() {
  local label=$1
  log "START $label: Range (after 10 K preload)"
  start_etcd "$label" || return 1

  "$BENCH_BIN" --conns=$CONNS --clients=$CLIENTS --target-leader \
    put --key-size=8 --val-size=256 --total=10000 \
    --key-space-size=10000 --sequential-keys \
    > "$RESULTS/${label}_preload.txt" 2>&1 || true

  local rss_b mm_b rss_a hwm_a mm_a
  rss_b=$(get_rss "$ETCD_PID"); mm_b=$(zram_mm)

  "$BENCH_BIN" --conns=$CONNS --clients=$CLIENTS \
    range single-key --total=100000 \
    > "$RESULTS/${label}_bench.txt" 2>&1 || true

  rss_a=$(get_rss "$ETCD_PID"); hwm_a=$(get_hwm "$ETCD_PID"); mm_a=$(zram_mm)
  cat > "$RESULTS/${label}_metrics.txt" << EOF
label=$label
desc=Range-after-10K-preload
rss_before=$rss_b
rss_after=$rss_a
hwm_after=$hwm_a
mm_before=$mm_b
mm_after=$mm_a
EOF
  log "DONE $label  RSS ${rss_b}->${rss_a} kB (peak ${hwm_a})  zram ${mm_b}->${mm_a}"
  stop_etcd
}

# ---------------------------------------------------------------------------
# Build step
# ---------------------------------------------------------------------------
if [[ "$SKIP_BUILD" -eq 0 ]]; then
  log "Building etcd, etcdctl, benchmark with in-tree Go ($GOROOT)..."

  ( cd "$ETCD_ROOT/server" && \
    env $GO_ENV go build -trimpath -installsuffix=cgo -o "$ETCD_BIN" . ) || {
      log "FAIL: cannot build etcd"; exit 1; }
  log "  etcd:     $ETCD_BIN"

  ( cd "$ETCD_ROOT/etcdctl" && \
    env $GO_ENV go build -trimpath -installsuffix=cgo -o "$ETCDCTL_BIN" . ) || {
      log "FAIL: cannot build etcdctl"; exit 1; }
  log "  etcdctl:  $ETCDCTL_BIN"

  ( cd "$ETCD_ROOT/tools/benchmark" && \
    env $GO_ENV go build -trimpath -installsuffix=cgo -o "$BENCH_BIN" . ) || {
      log "FAIL: cannot build benchmark"; exit 1; }
  log "  benchmark: $BENCH_BIN"
else
  for b in "$ETCD_BIN" "$ETCDCTL_BIN" "$BENCH_BIN"; do
    [[ -x "$b" ]] || { log "FAIL: missing binary $b (run without --skip-build)"; exit 1; }
  done
fi

# Sanity check
"$ETCD_BIN" --version 2>&1 | head -1

# ---------------------------------------------------------------------------
# Sanity check zram device
# ---------------------------------------------------------------------------
if [[ "$ONLY" == "" || "$ONLY" == "noscan" ]]; then
  if [[ ! -b "$DEVICE" ]]; then
    log "WARN: $DEVICE is not a block device; noscan runs will fail"
  fi
  if swapon --show 2>/dev/null | grep -q "$(basename "$DEVICE")"; then
    log "ERROR: $DEVICE is in use as swap. Run: sudo swapoff $DEVICE"
    exit 1
  fi
fi

# ---------------------------------------------------------------------------
# Run benchmarks
# ---------------------------------------------------------------------------
BENCH_ARGS="--conns=$CONNS --clients=$CLIENTS --target-leader"

# --- Baseline ---
if [[ "$ONLY" == "" || "$ONLY" == "baseline" ]]; then
  unset NOSCAN_ENV
  log "=== BASELINE (no noscan) ==="

  run_bench base_put_small  "Put 8B val" \
    $BENCH_ARGS put --key-size=8 --val-size=8 \
    --total=100000 --key-space-size=100000 --sequential-keys

  run_bench base_put_medium "Put 256B val" \
    $BENCH_ARGS put --key-size=8 --val-size=256 \
    --total=100000 --key-space-size=100000 --sequential-keys

  run_bench base_put_large  "Put 4KB val" \
    $BENCH_ARGS put --key-size=8 --val-size=4096 \
    --total=10000 --key-space-size=10000 --sequential-keys

  run_range base_range

  run_bench base_txn_mixed  "Txn-mixed" \
    $BENCH_ARGS txn-mixed aa --key-size=8 --val-size=256 \
    --total=10000 --key-space-size=1000
fi

# --- Noscan (no pageout) ---
if [[ "$ONLY" == "" || "$ONLY" == "noscan" ]]; then
  export NOSCAN_ENV="GONOSCANFILE=$DEVICE GONOSCANFILESIZE=$REGION_SIZE GONOSCANPAGEOUT=0"
  log "=== NOSCAN ($DEVICE, $REGION_SIZE, pageout=off) ==="

  run_bench noscan_put_small  "Put 8B val (noscan)" \
    $BENCH_ARGS put --key-size=8 --val-size=8 \
    --total=100000 --key-space-size=100000 --sequential-keys

  run_bench noscan_put_medium "Put 256B val (noscan)" \
    $BENCH_ARGS put --key-size=8 --val-size=256 \
    --total=100000 --key-space-size=100000 --sequential-keys

  run_bench noscan_put_large  "Put 4KB val (noscan)" \
    $BENCH_ARGS put --key-size=8 --val-size=4096 \
    --total=10000 --key-space-size=10000 --sequential-keys

  run_range noscan_range

  run_bench noscan_txn_mixed  "Txn-mixed (noscan)" \
    $BENCH_ARGS txn-mixed aa --key-size=8 --val-size=256 \
    --total=10000 --key-space-size=1000
fi

# --- Noscan + pageout + minSize filter (zram) ---
if [[ "$ONLY" == "" || "$ONLY" == "zram" ]]; then
  export NOSCAN_ENV="GONOSCANFILE=$DEVICE GONOSCANFILESIZE=$REGION_SIZE GONOSCANFILEMIN=256"
  log "=== ZRAM ($DEVICE, $REGION_SIZE, pageout=on, min=256) ==="

  run_bench zram_put_small  "Put 8B val (zram)" \
    $BENCH_ARGS put --key-size=8 --val-size=8 \
    --total=100000 --key-space-size=100000 --sequential-keys

  run_bench zram_put_medium "Put 256B val (zram)" \
    $BENCH_ARGS put --key-size=8 --val-size=256 \
    --total=100000 --key-space-size=100000 --sequential-keys

  run_bench zram_put_large  "Put 4KB val (zram)" \
    $BENCH_ARGS put --key-size=8 --val-size=4096 \
    --total=10000 --key-space-size=10000 --sequential-keys

  run_range zram_range

  run_bench zram_txn_mixed  "Txn-mixed (zram)" \
    $BENCH_ARGS txn-mixed aa --key-size=8 --val-size=256 \
    --total=10000 --key-space-size=1000
fi

# ---------------------------------------------------------------------------
# Summary
# ---------------------------------------------------------------------------
log "=== GENERATING SUMMARY ==="
SUMMARY="$RESULTS/summary.txt"
{
  echo "# etcd noscan benchmark summary"
  echo "# $(date)"
  echo "# GOROOT=$GOROOT  ETCD_ROOT=$ETCD_ROOT"
  echo "# region=$REGION_SIZE  device=$DEVICE  conns=$CONNS  clients=$CLIENTS"
  echo ""
  printf "%-24s %14s %10s %10s %10s %10s\n" \
    "scenario" "req/s" "p50(ms)" "p99(ms)" "rss(kB)" "zram(B)"
  printf '%0.s-' {1..80}; echo ""

  for label in base_put_small base_put_medium base_put_large \
               base_range base_txn_mixed \
               noscan_put_small noscan_put_medium noscan_put_large \
               noscan_range noscan_txn_mixed \
               pgout_put_small pgout_put_medium pgout_put_large \
               pgout_range pgout_txn_mixed \
               zram_put_small zram_put_medium zram_put_large \
               zram_range zram_txn_mixed; do
    bench_file="$RESULTS/${label}_bench.txt"
    metrics_file="$RESULTS/${label}_metrics.txt"
    [[ -f "$bench_file" && -f "$metrics_file" ]] || continue

    reqs=$(grep -m1 'Requests/sec' "$bench_file" | awk '{printf "%.0f", $2}')
    p50=$(grep -m1 '50% in' "$bench_file" | awk '{printf "%.1f", $3*1000}')
    p99=$(grep -m1 '99% in' "$bench_file" | awk '{printf "%.1f", $3*1000}')
    rss=$(awk -F= '/rss_after/{print $2}' "$metrics_file")
    mm=$(awk -F= '/mm_after/{print $2}' "$metrics_file")

    printf "%-24s %14s %10s %10s %10s %10s\n" \
      "$label" "${reqs:-N/A}" "${p50:-N/A}" "${p99:-N/A}" \
      "${rss:-N/A}" "${mm:-N/A}"
  done
} | tee "$SUMMARY"

log "=== ALL DONE ==="
log "Results:   $RESULTS/"
log "Summary:   $SUMMARY"
