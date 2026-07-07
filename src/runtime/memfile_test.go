// Copyright 2024 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build linux && (amd64 || arm64)

package runtime_test

import (
	"bytes"
	"internal/testenv"
	"os"
	"os/exec"
	"testing"
	"time"
	"unsafe"

	"runtime"
	"runtime/debug"
)

// TestMapSharedFilePrimitive validates the low-level primitives that the
// noscan file region will rely on: the ftruncate wrapper and a MAP_SHARED
// file-backed mmap whose writes persist to the backing file. No allocator
// integration is exercised here.
func TestMapSharedFilePrimitive(t *testing.T) {
	const size = 1 << 20 // 1 MiB, larger than a single page

	f, err := os.CreateTemp("", "noscanfile")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	path := f.Name()
	defer os.Remove(path)

	fd := int32(f.Fd())

	// Extend the file to `size` bytes using the runtime's ftruncate wrapper.
	if r := runtime.Ftruncate(fd, int64(size)); r != 0 {
		f.Close()
		t.Fatalf("ftruncate returned %d", r)
	}

	// Map it MAP_SHARED, read/write.
	p, errc := runtime.Mmap(nil, size, runtime.PROT_READ|runtime.PROT_WRITE, runtime.MAP_SHARED, fd, 0)
	if errc != 0 {
		f.Close()
		t.Fatalf("mmap returned error %d", errc)
	}
	if p == nil {
		f.Close()
		t.Fatal("mmap returned nil pointer")
	}

	// Write known bytes at the first and last offset. MAP_SHARED writes must
	// propagate to the backing file.
	*(*byte)(p) = 0xAB
	*(*byte)(unsafe.Add(p, size-1)) = 0xCD

	// Drop the mapping before reading the file back.
	runtime.Munmap(p, size)
	if err := f.Sync(); err != nil {
		f.Close()
		t.Fatalf("Sync: %v", err)
	}
	f.Close()

	// Verify the writes persisted to the file.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(data) != size {
		t.Fatalf("file size = %d, want %d", len(data), size)
	}
	if data[0] != 0xAB {
		t.Fatalf("data[0] = %#x, want 0xAB", data[0])
	}
	if data[size-1] != 0xCD {
		t.Fatalf("data[size-1] = %#x, want 0xCD", data[size-1])
	}
}

// TestParseMemSize checks the size parser used by GONOSCANFILESIZE.
func TestParseMemSize(t *testing.T) {
	tests := []struct {
		in   string
		want uintptr
		ok   bool
	}{
		{"1024", 1024, true},
		{"0", 0, true}, // parses; callers treat zero as "disabled"
		{"1k", 1 << 10, true},
		{"2K", 2 << 10, true},
		{"3MiB", 3 << 20, true},
		{"4GB", 4 << 30, true},
		{"5g", 5 << 30, true},
		{"16MiB", 16 << 20, true},
		{"", 0, false},
		{"abc", 0, false},
		{"12x", 0, false},
		{"KB12", 0, false},
	}
	for _, tc := range tests {
		got, ok := runtime.ParseMemSize(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("ParseMemSize(%q) = (%v, %v), want (%v, %v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

// TestNoscanFileConfigFromEnv checks the configuration derivation logic.
func TestNoscanFileConfigFromEnv(t *testing.T) {
	tests := []struct {
		name       string
		path       string
		sizeStr    string
		pageoutStr string
		minSizeStr string
		wantOk     bool
		wantSize   uintptr
		wantPage   bool
		wantMin    uintptr
	}{
		{"both set", "/tmp/x", "16MiB", "", "", true, 16 << 20, true, 0},
		{"path missing", "", "16MiB", "", "", false, 0, false, 0},
		{"size missing", "/tmp/x", "", "", "", false, 0, false, 0},
		{"bad size", "/tmp/x", "lots", "", "", false, 0, false, 0},
		{"zero size", "/tmp/x", "0", "", "", false, 0, false, 0},
		{"pageout off", "/tmp/x", "16MiB", "0", "", true, 16 << 20, false, 0},
		{"pageout on", "/tmp/x", "16MiB", "1", "", true, 16 << 20, true, 0},
		{"pageout default", "/tmp/x", "16MiB", "", "", true, 16 << 20, true, 0},
		{"min 256", "/tmp/x", "16MiB", "", "256", true, 16 << 20, true, 256},
		{"min 1KiB", "/tmp/x", "16MiB", "", "1KiB", true, 16 << 20, true, 1024},
		{"min bad", "/tmp/x", "16MiB", "", "lots", true, 16 << 20, true, 0},
		{"min zero", "/tmp/x", "16MiB", "", "0", true, 16 << 20, true, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p, size, pageout, minSize, ok := runtime.NoscanFileConfigFromEnv(tc.path, tc.sizeStr, tc.pageoutStr, tc.minSizeStr)
			if ok != tc.wantOk {
				t.Fatalf("ok = %v, want %v (path=%q)", ok, tc.wantOk, p)
			}
			if ok {
				if p != tc.path {
					t.Errorf("path = %q, want %q", p, tc.path)
				}
				if size != tc.wantSize {
					t.Errorf("size = %v, want %v", size, tc.wantSize)
				}
				if pageout != tc.wantPage {
					t.Errorf("pageout = %v, want %v", pageout, tc.wantPage)
				}
				if minSize != tc.wantMin {
					t.Errorf("minSize = %v, want %v", minSize, tc.wantMin)
				}
			}
		})
	}
}

// TestNoscanFileRegionSetup verifies that the region can be reserved, backed
// by a file, and that writes through the region persist to the file via a
// MAP_SHARED mapping. No allocator wiring is involved.
func TestNoscanFileRegionSetup(t *testing.T) {
	const size = 1 << 20 // 1 MiB

	dir, err := os.MkdirTemp("", "noscanregion")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	defer os.RemoveAll(dir)
	path := dir + "/backing.bin"

	base, fd, regionSize, errmsg := runtime.NoscanFileRegionSetupForTest(path, size)
	if errmsg != "" {
		t.Fatalf("setup failed: %s", errmsg)
	}
	if base == nil {
		t.Fatal("setup returned nil base")
	}
	if fd < 0 {
		t.Fatalf("setup returned bad fd %d", fd)
	}
	if regionSize < size {
		t.Fatalf("region size = %d, want >= %d", regionSize, size)
	}

	// Write known bytes at the first and last offset through the region.
	*(*byte)(base) = 0x11
	*(*byte)(unsafe.Add(base, regionSize-1)) = 0x22

	// Persistence check #1: a fresh MAP_SHARED mapping of the same file must
	// observe the writes (they share the page cache).
	p2, errc := runtime.Mmap(nil, regionSize, runtime.PROT_READ|runtime.PROT_WRITE, runtime.MAP_SHARED, fd, 0)
	if errc != 0 {
		t.Fatalf("second mmap returned error %d", errc)
	}
	if *(*byte)(p2) != 0x11 {
		t.Fatalf("second mapping [0] = %#x, want 0x11", *(*byte)(p2))
	}
	if *(*byte)(unsafe.Add(p2, regionSize-1)) != 0x22 {
		t.Fatalf("second mapping [end] = %#x, want 0x22", *(*byte)(unsafe.Add(p2, regionSize-1)))
	}
	runtime.Munmap(p2, regionSize)

	// Persistence check #2: the file on disk carries the data and the size.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(data) != int(regionSize) {
		t.Fatalf("file size = %d, want %d", len(data), regionSize)
	}
	if data[0] != 0x11 {
		t.Fatalf("data[0] = %#x, want 0x11", data[0])
	}
	if data[regionSize-1] != 0x22 {
		t.Fatalf("data[regionSize-1] = %#x, want 0x22", data[regionSize-1])
	}
}

// TestNoscanFileRegionArenaRegistered verifies that setup registers heapArena
// metadata for the region so that the arena-index machinery resolves the
// region's addresses.
func TestNoscanFileRegionArenaRegistered(t *testing.T) {
	const size = 1 << 20 // 1 MiB

	dir, err := os.MkdirTemp("", "noscanarena")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	defer os.RemoveAll(dir)
	path := dir + "/backing.bin"

	base, _, regionSize, errmsg := runtime.NoscanFileRegionSetupForTest(path, size)
	if errmsg != "" {
		t.Fatalf("setup failed: %s", errmsg)
	}
	baseAddr := uintptr(base)

	// First and last page of the region must resolve to a registered arena.
	if !runtime.NoscanFileRegionArenaRegistered(baseAddr) {
		t.Errorf("base %x not registered as an arena", baseAddr)
	}
	if !runtime.NoscanFileRegionArenaRegistered(baseAddr + regionSize - 1) {
		t.Errorf("last byte %x not registered as an arena", baseAddr+regionSize-1)
	}

	// An address well outside the region (a low address) must not be reported
	// as registered by our region.
	if runtime.NoscanFileRegionArenaRegistered(0x1000) {
		t.Errorf("address 0x1000 unexpectedly registered")
	}
}

// TestNoscanFileRegionPageAllocRoundTrip verifies the region's dedicated page
// allocator can hand pages out and take them back, and that allocation still
// works after a free (proving the bookkeeping stays consistent).
func TestNoscanFileRegionPageAllocRoundTrip(t *testing.T) {
	const size = 1 << 20 // 1 MiB

	dir, err := os.MkdirTemp("", "noscanpagealloc")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	defer os.RemoveAll(dir)
	path := dir + "/backing.bin"

	base, _, regionSize, errmsg := runtime.NoscanFileRegionSetupForTest(path, size)
	if errmsg != "" {
		t.Fatalf("setup failed: %s", errmsg)
	}
	lo := uintptr(base)
	hi := lo + regionSize

	inRange := func(x uintptr, what string) {
		if x < lo || x >= hi {
			t.Fatalf("%s = %x not in region [%x, %x)", what, x, lo, hi)
		}
	}

	// Allocate two disjoint runs of pages.
	a, ok := runtime.NoscanFileRegionAllocPages(4)
	if !ok {
		t.Fatal("first alloc failed")
	}
	inRange(a, "first alloc")
	b, ok := runtime.NoscanFileRegionAllocPages(4)
	if !ok {
		t.Fatal("second alloc failed")
	}
	inRange(b, "second alloc")
	if !(b >= a+4*runtime.PageSize || a >= b+4*runtime.PageSize) {
		t.Fatalf("allocs overlap: a=%x b=%x", a, b)
	}

	// Free the first and allocate again; it must still succeed and stay in range.
	runtime.NoscanFileRegionFreePages(a, 4)
	c, ok := runtime.NoscanFileRegionAllocPages(4)
	if !ok {
		t.Fatal("alloc after free failed")
	}
	inRange(c, "alloc after free")
}

// setupFileRegionForTest creates a file region for use by a test and returns
// its [base, limit) range. The region stays active after the test so that any
// spans it served can still be freed correctly by the GC.
func setupFileRegionForTest(t *testing.T, size uintptr) (lo, hi uintptr) {
	t.Helper()
	dir, err := os.MkdirTemp("", "noscanfile")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	base, _, regionSize, errmsg := runtime.NoscanFileRegionSetupForTest(dir+"/backing.bin", size)
	if errmsg != "" {
		t.Fatalf("setup failed: %s", errmsg)
	}
	return uintptr(base), uintptr(base) + regionSize
}

// TestLargeNoscanAllocFromRegion verifies that a large noscan allocation (a
// big []byte) is served from the file-backed region.
func TestLargeNoscanAllocFromRegion(t *testing.T) {
	lo, hi := setupFileRegionForTest(t, 8<<20)

	const n = 1 << 20 // 1 MiB, well above the large-object threshold
	b := make([]byte, n)
	ptr := uintptr(unsafe.Pointer(&b[0]))
	if ptr < lo || ptr >= hi {
		t.Fatalf("large noscan alloc %x not in region [%x, %x)", ptr, lo, hi)
	}
	// The allocation must be usable.
	for i := range b {
		b[i] = byte(i)
	}
	for i := range b {
		if b[i] != byte(i) {
			t.Fatalf("slice data mismatch at %d", i)
		}
	}
}

// TestLargeNoscanSurvivesGC verifies that a large noscan object allocated from
// the region survives garbage collection with its contents intact, and that
// freeing it later (once unreferenced) does not crash the runtime.
func TestLargeNoscanSurvivesGC(t *testing.T) {
	lo, hi := setupFileRegionForTest(t, 8<<20)

	const n = 1 << 20
	b := make([]byte, n)
	ptr := uintptr(unsafe.Pointer(&b[0]))
	if ptr < lo || ptr >= hi {
		t.Fatalf("large noscan alloc %x not in region [%x, %x)", ptr, lo, hi)
	}
	for i := range b {
		b[i] = byte(i)
	}

	// Run the marker and the sweeper. The object is live, so it must survive.
	runtime.GC()
	runtime.GC()
	// Force sweeping of any dead spans from the cycles above.
	runtime.GC()

	bad := 0
	for i := range b {
		if b[i] != byte(i) {
			bad++
		}
	}
	if bad != 0 {
		t.Fatalf("%d bytes corrupted after GC", bad)
	}

	// Drop the reference and force collection so the region span gets freed
	// through the routed free path; this must not crash.
	b = nil
	runtime.GC()
	runtime.GC()
}

// TestLargeNoscanReuseAfterFree verifies that after a large noscan object from
// the region is freed, a subsequent large noscan allocation is still served
// from the region. This exercises the end-to-end alloc -> sweep-free -> realloc
// path at the Go allocation API level (the low-level page reuse is covered by
// TestNoscanFileRegionPageAllocRoundTrip).
func TestLargeNoscanReuseAfterFree(t *testing.T) {
	lo, hi := setupFileRegionForTest(t, 8<<20)

	const n = 1 << 20
	b1 := make([]byte, n)
	p1 := uintptr(unsafe.Pointer(&b1[0]))
	if p1 < lo || p1 >= hi {
		t.Fatalf("first alloc %x not in region [%x, %x)", p1, lo, hi)
	}

	// Free b1 and let the sweeper return its span to the region allocator.
	b1 = nil
	runtime.GC()
	runtime.GC()

	// Allocate again. It must still come from the region (the region allocator
	// is healthy after a free cycle); whether it reuses b1's exact address is an
	// allocator detail.
	b2 := make([]byte, n)
	p2 := uintptr(unsafe.Pointer(&b2[0]))
	if p2 < lo || p2 >= hi {
		t.Fatalf("alloc after free %x not in region [%x, %x); region allocator not reused", p2, lo, hi)
	}

	// b2 must be usable.
	for i := range b2 {
		b2[i] = byte(i)
	}
	for i := range b2 {
		if b2[i] != byte(i) {
			t.Fatalf("reused slice data mismatch at %d", i)
		}
	}
}

// TestLargeNoscanExhaustionFallback verifies that once the file region is full,
// large noscan allocations transparently fall back to the regular heap, and
// that freeing a mix of region-served and heap-served spans works (each via its
// own free path).
func TestLargeNoscanExhaustionFallback(t *testing.T) {
	lo, hi := setupFileRegionForTest(t, 4<<20) // small region

	// Allocate 1 MiB noscan objects, keeping them live, until one no longer fits
	// in the region and is served from the heap.
	var regionLive [][]byte
	var heapOne []byte
	for i := 0; i < 64; i++ {
		b := make([]byte, 1<<20)
		p := uintptr(unsafe.Pointer(&b[0]))
		if p < lo || p >= hi {
			heapOne = b
			break
		}
		regionLive = append(regionLive, b)
	}
	if heapOne == nil {
		t.Fatalf("region never exhausted after %d allocs; fallback not exercised", len(regionLive))
	}

	// The heap-served allocation must be usable and survive GC.
	for i := range heapOne {
		heapOne[i] = byte(i)
	}
	runtime.GC()
	runtime.GC()
	for i := range heapOne {
		if heapOne[i] != byte(i) {
			t.Fatalf("heap fallback slice corrupted at %d", i)
		}
	}

	// Drop everything and collect, freeing a mix of region and heap spans. This
	// must not crash: region spans go through the region free path, the heap
	// span through the normal path.
	regionLive = nil
	heapOne = nil
	runtime.GC()
	runtime.GC()
}

// TestSmallNoscanAllocFromRegion verifies that small noscan allocations are
// served from the file-backed region once the per-P mcache/mcentral exhaust
// their existing (heap-sourced) spans and must grow new ones. The region is
// the preferred source for noscan span growth, so at least some of a large
// batch of small []byte must land in the region.
func TestSmallNoscanAllocFromRegion(t *testing.T) {
	lo, hi := setupFileRegionForTest(t, 8<<20)

	// Allocate many small noscan objects, keeping them live so their spans fill
	// and the mcentral is forced to grow fresh (region-sourced) spans.
	const n = 50000
	live := make([][]byte, 0, n)
	inRegion := 0
	for i := 0; i < n; i++ {
		b := make([]byte, 32)
		live = append(live, b)
		if p := uintptr(unsafe.Pointer(&b[0])); p >= lo && p < hi {
			inRegion++
		}
	}
	if inRegion == 0 {
		t.Fatalf("no small noscan allocations served from region after %d allocs", n)
	}
	t.Logf("%d/%d small noscan allocations served from region", inRegion, n)

	// Region-served objects must be usable.
	for i, b := range live {
		b[0] = byte(i)
		b[len(b)-1] = byte(i)
	}

	// Drop everything and collect, freeing small region spans through the routed
	// free path.
	live = nil
	runtime.GC()
	runtime.GC()
}

// TestSmallNoscanSurvivesGC verifies that small noscan objects served from the
// region survive garbage collection with their contents intact.
func TestSmallNoscanSurvivesGC(t *testing.T) {
	lo, hi := setupFileRegionForTest(t, 8<<20)

	// Collect a set of region-served small objects, filled with known data.
	var region [][]byte
	for i := 0; i < 50000; i++ {
		b := make([]byte, 32)
		if p := uintptr(unsafe.Pointer(&b[0])); p >= lo && p < hi {
			for j := range b {
				b[j] = byte(j)
			}
			region = append(region, b)
			if len(region) >= 2000 {
				break
			}
		}
	}
	if len(region) == 0 {
		t.Fatal("no small noscan allocations served from region")
	}

	runtime.GC()
	runtime.GC()

	bad := 0
	for _, b := range region {
		for j := range b {
			if b[j] != byte(j) {
				bad++
				break
			}
		}
	}
	if bad != 0 {
		t.Fatalf("%d/%d region small objects corrupted after GC", bad, len(region))
	}

	region = nil
	runtime.GC()
	runtime.GC()
}

// TestSmallNoscanFilteredByMinSize verifies that when the region's minSize
// threshold is set, small noscan objects whose size class does not exceed the
// threshold are allocated from the regular heap, while large noscan objects
// still use the region.
func TestSmallNoscanFilteredByMinSize(t *testing.T) {
	lo, hi := setupFileRegionForTest(t, 8<<20)
	runtime.SetNoscanFileMinSize(256)

	// Small 32-byte objects should NOT land in the region.
	const n = 50000
	live := make([][]byte, 0, n)
	inRegion := 0
	for i := 0; i < n; i++ {
		b := make([]byte, 32)
		live = append(live, b)
		if p := uintptr(unsafe.Pointer(&b[0])); p >= lo && p < hi {
			inRegion++
		}
	}
	if inRegion > 0 {
		t.Fatalf("%d/%d small (32 B) allocations leaked into region despite minSize=256", inRegion, n)
	}
	t.Logf("0/%d small (32 B) allocations in region (correctly filtered)", n)

	// Large allocations must still go to the region.
	big := make([]byte, 1<<20)
	if p := uintptr(unsafe.Pointer(&big[0])); p < lo || p >= hi {
		t.Fatalf("large (1 MiB) allocation %x not in region [%x, %x)", p, lo, hi)
	}

	live = nil
	runtime.GC()
	runtime.GC()
}

// TestTinyNoscanFromRegion verifies that tiny noscan allocations (sub-16-byte,
// coalesced into 16-byte slots of tinySpanClass / spanClass 5) are served from
// the file-backed region once the tiny span is grown there.
func TestTinyNoscanFromRegion(t *testing.T) {
	lo, hi := setupFileRegionForTest(t, 8<<20)

	// Tiny allocations are packed into 16-byte slots; once the spanClass-5 span
	// is grown from the region, the tiny block pointer lives in the region.
	const n = 100000
	live := make([][]byte, 0, n)
	inRegion := 0
	for i := 0; i < n; i++ {
		b := make([]byte, 8) // < maxTinySize (16) -> tiny noscan path
		live = append(live, b)
		if p := uintptr(unsafe.Pointer(&b[0])); p >= lo && p < hi {
			inRegion++
		}
	}
	if inRegion == 0 {
		t.Fatalf("no tiny noscan allocations served from region after %d allocs", n)
	}
	t.Logf("%d/%d tiny noscan allocations served from region", inRegion, n)

	live = nil
	runtime.GC()
	runtime.GC()
}

// TestNoscanMixedStress exercises tiny, small, and large noscan allocations
// together under GC pressure, forcing the region to fill and spill onto the
// heap, and freeing spans of all three sizes through their routed free paths.
func TestNoscanMixedStress(t *testing.T) {
	setupFileRegionForTest(t, 8<<20)

	keep := make([][]byte, 0, 64)
	for i := 0; i < 1500; i++ {
		var sz int
		switch i % 3 {
		case 0:
			sz = 8 // tiny
		case 1:
			sz = 200 // small
		case 2:
			sz = 100000 // large
		}
		b := make([]byte, sz)
		v := byte(i)
		b[0] = v
		b[len(b)-1] = v
		keep = append(keep, b)
		if len(keep) > 32 {
			keep = keep[1:] // drop oldest; freed later by the GC
		}
		if i%100 == 0 {
			runtime.GC()
		}
	}
	// Survivors must be internally consistent (b[0] == b[len-1], as written).
	for _, b := range keep {
		if b[0] != b[len(b)-1] {
			t.Fatal("mixed stress: survivor data inconsistent")
		}
	}
	keep = nil
	runtime.GC()
	runtime.GC()
}

// the heap's page allocator) never touches the file-backed region: a live
// region object's contents survive a forced full scavenge, and the region can
// still serve allocations afterward.
func TestLargeNoscanNotScavenged(t *testing.T) {
	lo, hi := setupFileRegionForTest(t, 8<<20)

	const n = 1 << 20
	b := make([]byte, n)
	if p := uintptr(unsafe.Pointer(&b[0])); p < lo || p >= hi {
		t.Fatalf("alloc %x not in region [%x, %x)", p, lo, hi)
	}
	for i := range b {
		b[i] = byte(i)
	}

	// Force a GC followed by a full scavenge of all free heap pages.
	runtime.GC()
	debug.FreeOSMemory()

	// The region object must be untouched.
	bad := 0
	for i := range b {
		if b[i] != byte(i) {
			bad++
		}
	}
	if bad != 0 {
		t.Fatalf("region object corrupted after scavenge: %d bytes", bad)
	}

	// And the region must still be able to serve a new allocation.
	b2 := make([]byte, n)
	if p := uintptr(unsafe.Pointer(&b2[0])); p < lo || p >= hi {
		t.Fatalf("alloc after scavenge %x not in region [%x, %x)", p, lo, hi)
	}

	b = nil
	b2 = nil
	runtime.GC()
	runtime.GC()
}

// TestLargeNoscanFinalizer verifies that a finalizer attached to a large
// noscan object allocated from the region runs when the object becomes
// unreachable. This exercises the markrootSpans finalizer path for a region
// span.
func TestLargeNoscanFinalizer(t *testing.T) {
	setupFileRegionForTest(t, 8<<20)

	const n = 1 << 20
	b := make([]byte, n)
	done := make(chan struct{}, 1)
	runtime.SetFinalizer(&b[0], func(*byte) {
		select {
		case done <- struct{}{}:
		default:
		}
	})
	// Drop all references so the object becomes collectible.
	b = nil
	runtime.GC()
	runtime.GC() // advance mark + sweep so the finalizer is queued/run

	select {
	case <-done:
		// finalizer ran
	case <-time.After(5 * time.Second):
		t.Fatal("finalizer on region object did not run")
	}
}

// TestLargeNoscanStress exercises many large noscan allocations of varying
// sizes under GC pressure, forcing the region to fill and spill onto the heap
// and freeing spans through both free paths concurrently.
func TestLargeNoscanStress(t *testing.T) {
	setupFileRegionForTest(t, 8<<20)

	keep := make([][]byte, 0, 16)
	for i := 0; i < 300; i++ {
		sz := (i%8 + 1) * 64 * 1024 // 64 KiB .. 512 KiB, all large noscan
		b := make([]byte, sz)
		v := byte(i)
		b[0] = v
		b[len(b)-1] = v
		keep = append(keep, b)
		if len(keep) > 16 {
			keep = keep[1:] // drop oldest; freed later by the GC
		}
		if i%50 == 0 {
			runtime.GC()
		}
	}
	// Survivors must be intact (b[0] == b[len-1], as written).
	for _, b := range keep {
		if b[0] != b[len(b)-1] {
			t.Fatal("stress: survivor data inconsistent")
		}
	}
	keep = nil
	runtime.GC()
	runtime.GC()
}

// TestNoscanFileEnvHelper is the in-process side of the env integration test.
// It only does work when GONOSCANFILE is set in its environment (i.e. when run
// as a subprocess by TestNoscanFileEnvIntegration).
func TestNoscanFileEnvHelper(t *testing.T) {
	path := os.Getenv("GONOSCANFILE")
	if path == "" {
		t.Skip("only runs as a subprocess with GONOSCANFILE set")
	}
	// The region was created at startup from the environment. A large noscan
	// allocation must therefore be file-backed: writes must appear in the file.
	const n = 1 << 20
	b := make([]byte, n)
	marker := []byte{0xDE, 0xAD, 0xBE, 0xEF}
	copy(b[:len(marker)], marker)
	copy(b[n-len(marker):], marker)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Contains(data, marker) {
		t.Fatalf("allocation marker not found in backing file; region not file-backed")
	}

	// A small noscan allocation must also be file-backed. Allocate a batch so
	// the mcache/mcentral grows a region-sourced span, then look for the marker
	// in the backing file. (Each small object's offset in the file depends on
	// where the region placed its span, so search the whole file.)
	smallMarker := []byte{0xCA, 0xFE, 0xBA, 0xBE}
	small := make([][]byte, 0, 20000)
	for i := 0; i < 20000; i++ {
		b := make([]byte, 64)
		copy(b, smallMarker)
		small = append(small, b)
	}
	data2, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Contains(data2, smallMarker) {
		t.Fatalf("small allocation marker not found in backing file; small noscan not file-backed")
	}
	_ = small
}

// TestNoscanFileEnvIntegration verifies the end-to-end env-driven startup: a
// subprocess with GONOSCANFILE/GONOSCANFILESIZE set creates the region at
// init, and a large noscan allocation in it is backed by the named file.
func TestNoscanFileEnvIntegration(t *testing.T) {
	testenv.MustHaveExec(t)

	dir := t.TempDir()
	path := dir + "/envregion.bin"

	cmd := testenv.CleanCmdEnv(exec.Command(os.Args[0],
		"-test.run=^TestNoscanFileEnvHelper$", "-test.v"))
	cmd.Env = append(cmd.Env,
		"GONOSCANFILE="+path,
		"GONOSCANFILESIZE=8MiB",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helper subprocess failed: %v\n%s", err, out)
	}
}
