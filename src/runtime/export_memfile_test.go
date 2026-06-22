// Copyright 2024 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build linux && (amd64 || arm64)

package runtime

import "unsafe"

// Exports for the external noscan-file-region tests.

var Ftruncate = ftruncate

func ParseMemSize(s string) (uintptr, bool) { return parseMemSize(s) }

func NoscanFileConfigFromEnv(path, sizeStr string) (p string, size uintptr, ok bool) {
	c := noscanFileConfigFromEnv(path, sizeStr)
	return c.path, c.size, c.ok
}

// NoscanFileRegionSetupForTest creates a region, backs it with file at path,
// stores it as the process-wide fileRegion, and returns its base, fd, and size.
func NoscanFileRegionSetupForTest(path string, size uintptr) (base unsafe.Pointer, fd int32, regionSize uintptr, errmsg string) {
	r := new(noscanFileRegion)
	errmsg = r.setup(path, size)
	if errmsg != "" {
		return nil, -1, 0, errmsg
	}
	setFileRegion(r)
	return unsafe.Pointer(r.base), r.fd, r.size, ""
}

// NoscanFileRegionArenaRegistered reports whether the arena containing addr has
// been registered by the file region.
func NoscanFileRegionArenaRegistered(addr uintptr) bool {
	ri := arenaIndex(addr)
	l2 := mheap_.arenas[ri.l1()]
	if l2 == nil {
		return false
	}
	return l2[ri.l2()] != nil
}

func NoscanFileRegionAllocPages(npages uintptr) (uintptr, bool) {
	if fileRegion == nil {
		return 0, false
	}
	return fileRegion.allocPages(npages)
}

func NoscanFileRegionFreePages(base, npages uintptr) {
	if fileRegion != nil {
		fileRegion.freePages(base, npages)
	}
}

const (
	MAP_SHARED = _MAP_SHARED
	PROT_READ  = _PROT_READ
	PROT_WRITE = _PROT_WRITE
)
