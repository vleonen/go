// Copyright 2024 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// File-backed memory region for large noscan spans.
//
// This is currently supported only on linux/amd64 and linux/arm64.

//go:build linux && (amd64 || arm64)

package runtime

import "unsafe"

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
	// size is the length of the region in bytes (a multiple of pageSize).
	size uintptr
	// fd is the backing file descriptor, kept open for the life of the region.
	fd int32
	// enabled reports whether the region is mapped and ready to serve spans.
	enabled bool
}

// setup reserves an arena-aligned region, creates (or opens) the backing file
// at path, sizes it to size, and overlays a MAP_SHARED file mapping over the
// reservation.
//
// On success it returns the empty string and sets base/size/fd/enabled.
// On failure it returns a non-empty diagnostic message and leaves the region
// disabled, releasing any partial resources it acquired.
//
// size is rounded up to a multiple of pageSize. The base is aligned to
// heapArenaBytes so that the region can later be registered as heap arenas
// (see registerArenas).
func (r *noscanFileRegion) setup(path string, size uintptr) string {
	size = alignUp(size, pageSize)
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
	return ""
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
