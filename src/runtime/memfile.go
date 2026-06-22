// Copyright 2024 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// File-backed memory region for large noscan spans.
//
// This is currently supported only on linux/amd64 and linux/arm64.

//go:build linux && (amd64 || arm64)

package runtime

import (
	"internal/goarch"
	"internal/runtime/atomic"
	"unsafe"
)

// ftruncate calls the ftruncate system call. It is implemented in assembly.
// It returns 0 on success or a negative errno on failure.
//
//go:nosplit
func ftruncate(fd int32, length int64) int32

// noscanFileRegion describes a contiguous, file-backed address-space region
// used to back large noscan spans. The region is reserved as anonymous
// PROT_NONE memory and then overlaid with a single MAP_SHARED mapping of a
// backing file, so that the region's contents reside on a regular file or a
// block device (for example, zram).
type noscanFileRegion struct {
	// base is the first byte of the region; [base, base+size) is owned.
	base uintptr
	// size is the length of the region in bytes (a multiple of pallocChunkBytes).
	size uintptr
	// fd is the backing file descriptor, kept open for the life of the region.
	fd int32
	// enabled reports whether the region is mapped and ready to serve spans.
	enabled bool

	// lock guards pages, the dedicated page allocator for this region. It is
	// also passed to pages.init as the page allocator's mheapLock.
	lock mutex
	// pages manages free/used pages within [base, base+size). It is a separate
	// instance from mheap_.pages so the global scavenger never touches the
	// file-backed region.
	pages pageAlloc
	// arenas lists the arena indices registered for this region, so they can be
	// referenced or excluded (e.g. from scavenging) later.
	arenas []arenaIdx
}

// fileRegion is the process-wide file-backed noscan region, or nil if the
// feature is disabled. It is assigned during early initialization (see
// noscanFileRegionInit) and read-only thereafter.
var fileRegion *noscanFileRegion

// setup reserves an arena-aligned region, creates (or opens) the backing file
// at path, sizes it to size, and overlays a MAP_SHARED file mapping over the
// reservation.
//
// On success it returns the empty string and sets base/size/fd/enabled.
// On failure it returns a non-empty diagnostic message and leaves the region
// disabled, releasing any partial resources it acquired.
//
// size is rounded up to a multiple of pallocChunkBytes. The base is aligned to
// heapArenaBytes so that the region can later be registered as heap arenas
// (see registerArenas).
func (r *noscanFileRegion) setup(path string, size uintptr) string {
	size = alignUp(size, pallocChunkBytes)
	if size == 0 {
		return "noscan file region: zero size"
	}

	// Reserve an arena-aligned chunk of address space (PROT_NONE).
	base, rsize := sysReserveAligned(nil, size, heapArenaBytes)
	if base == nil {
		return "noscan file region: unable to reserve address space"
	}
	// sysReserveAligned may reserve more than requested to achieve alignment;
	// release the excess so we own exactly [base, base+size).
	if rsize > size {
		sysFree(unsafe.Add(base, size), rsize-size, &memstats.other_sys)
	}

	// Create/open the backing file (created if missing, never truncated here).
	b := make([]byte, len(path)+1)
	copy(b, path)
	fd := open(&b[0], _O_RDWR|_O_CREAT, 0600)
	if fd < 0 {
		sysFree(base, size, &memstats.other_sys)
		return "noscan file region: unable to open backing file"
	}

	// Size the file. (For block devices this may fail; for now we require a
	// regular file, which ftruncate succeeds on.)
	if rc := ftruncate(fd, int64(size)); rc != 0 {
		closefd(fd)
		sysFree(base, size, &memstats.other_sys)
		return "noscan file region: ftruncate failed"
	}

	// Overlay a MAP_SHARED file mapping on the reservation, replacing the
	// PROT_NONE placeholder at exactly the same address.
	p, err := mmap(base, size, _PROT_READ|_PROT_WRITE, _MAP_SHARED|_MAP_FIXED, fd, 0)
	if err != 0 || p != base {
		closefd(fd)
		sysFree(base, size, &memstats.other_sys)
		return "noscan file region: mmap MAP_SHARED failed"
	}

	r.base = uintptr(base)
	r.size = size
	r.fd = fd
	r.enabled = true

	// Set up the dedicated page allocator over the region and register the
	// region's arenas so that spanOf / pageIndexOf (and thus the GC) can
	// resolve addresses in [base, base+size).
	r.pages.init(&r.lock, &memstats.gcMiscSys, false)
	r.registerArenas()
	lock(&r.lock)
	r.pages.grow(r.base, r.size)
	unlock(&r.lock)

	return ""
}

// registerArenas creates heapArena metadata for every arena overlapping the
// region and publishes it into the global arenas map, mirroring mheap.sysAlloc.
// After this, spanOf and pageIndexOf resolve addresses inside the region.
//
// Region addresses are disjoint from the regular heap's, so the arena slots
// written here do not collide with the heap's slots; stores use atomic writes
// to stay safe against concurrent lockless spanOf readers.
func (r *noscanFileRegion) registerArenas() {
	for ri := arenaIndex(r.base); ri <= arenaIndex(r.base+r.size-1); ri++ {
		l2 := mheap_.arenas[ri.l1()]
		if l2 == nil {
			l2 = (*[1 << arenaL2Bits]*heapArena)(sysAllocOS(unsafe.Sizeof(*l2)))
			if l2 == nil {
				throw("noscan file region: out of memory allocating arena map")
			}
			atomic.StorepNoWB(unsafe.Pointer(&mheap_.arenas[ri.l1()]), unsafe.Pointer(l2))
		}
		if l2[ri.l2()] != nil {
			throw("noscan file region: arena already initialized")
		}
		ha := (*heapArena)(persistentalloc(unsafe.Sizeof(heapArena{}), goarch.PtrSize, &memstats.gcMiscSys))
		if ha == nil {
			throw("noscan file region: out of memory allocating heap arena metadata")
		}
		atomic.StorepNoWB(unsafe.Pointer(&l2[ri.l2()]), unsafe.Pointer(ha))
		r.arenas = append(r.arenas, ri)
	}
}

// allocPages returns the base address of npages contiguous free pages in the
// region, or (0, false) if the region is exhausted.
//
// pageAlloc.alloc is go:systemstack, so the call is dispatched there.
func (r *noscanFileRegion) allocPages(npages uintptr) (uintptr, bool) {
	var base uintptr
	systemstack(func() {
		lock(&r.lock)
		base, _ = r.pages.alloc(npages)
		unlock(&r.lock)
	})
	return base, base != 0
}

// freePages returns npages starting at base to the region's page allocator.
//
// pageAlloc.free is go:systemstack, so the call is dispatched there.
func (r *noscanFileRegion) freePages(base, npages uintptr) {
	systemstack(func() {
		lock(&r.lock)
		r.pages.free(base, npages)
		unlock(&r.lock)
	})
}

// noscanFileConfig holds the parsed GONOSCANFILE configuration.
type noscanFileConfig struct {
	path string
	size uintptr
	ok   bool
}

// parseNoscanFileConfig reads GONOSCANFILE and GONOSCANFILESIZE from the
// environment. The feature is considered enabled only when both are set and
// the size parses to a positive value.
func parseNoscanFileConfig() noscanFileConfig {
	return noscanFileConfigFromEnv(gogetenv("GONOSCANFILE"), gogetenv("GONOSCANFILESIZE"))
}

// noscanFileConfigFromEnv derives the configuration from the path and sizeStr
// values (typically obtained from the environment). It is split out so that
// the parsing logic can be tested without manipulating the process
// environment.
func noscanFileConfigFromEnv(path, sizeStr string) noscanFileConfig {
	if path == "" || sizeStr == "" {
		return noscanFileConfig{}
	}
	size, ok := parseMemSize(sizeStr)
	if !ok || size == 0 {
		return noscanFileConfig{}
	}
	return noscanFileConfig{path: path, size: size, ok: true}
}

// parseMemSize parses a non-negative size with an optional binary suffix
// (K/KiB, M/MiB, G/GiB). Multipliers are power-of-two (KiB = 1024). It returns
// the size in bytes and whether parsing succeeded.
func parseMemSize(s string) (uintptr, bool) {
	if len(s) == 0 {
		return 0, false
	}
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i == 0 {
		return 0, false
	}
	n, ok := atoi(s[:i])
	if !ok || n < 0 {
		return 0, false
	}
	var mul uintptr = 1
	switch s[i:] {
	case "", "B", "b":
		mul = 1
	case "K", "k", "KB", "kb", "KiB", "kib":
		mul = 1 << 10
	case "M", "m", "MB", "mb", "MiB", "mib":
		mul = 1 << 20
	case "G", "g", "GB", "gb", "GiB", "gib":
		mul = 1 << 30
	default:
		return 0, false
	}
	return uintptr(n) * mul, true
}
