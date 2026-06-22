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
