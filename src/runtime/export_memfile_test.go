// Copyright 2024 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build linux && (amd64 || arm64)

package runtime

// Exports for the external noscan-file-region tests.

var Ftruncate = ftruncate

const (
	MAP_SHARED = _MAP_SHARED
	PROT_READ  = _PROT_READ
	PROT_WRITE = _PROT_WRITE
)
