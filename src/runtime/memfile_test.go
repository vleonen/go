// Copyright 2024 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build linux && (amd64 || arm64)

package runtime_test

import (
	"os"
	"testing"
	"unsafe"

	"runtime"
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
		name     string
		path     string
		sizeStr  string
		wantOk   bool
		wantSize uintptr
	}{
		{"both set", "/tmp/x", "16MiB", true, 16 << 20},
		{"path missing", "", "16MiB", false, 0},
		{"size missing", "/tmp/x", "", false, 0},
		{"bad size", "/tmp/x", "lots", false, 0},
		{"zero size", "/tmp/x", "0", false, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p, size, ok := runtime.NoscanFileConfigFromEnv(tc.path, tc.sizeStr)
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
