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

func NoscanFileRegionSetupForTest(path string, size uintptr) (base unsafe.Pointer, fd int32, regionSize uintptr, errmsg string) {
	var r noscanFileRegion
	errmsg = r.setup(path, size)
	if errmsg != "" {
		return nil, -1, 0, errmsg
	}
	return unsafe.Pointer(r.base), r.fd, r.size, ""
}

const (
	MAP_SHARED = _MAP_SHARED
	PROT_READ  = _PROT_READ
	PROT_WRITE = _PROT_WRITE
)
