// Copyright 2024 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !(linux && (amd64 || arm64))

// Stubs for platforms without a file-backed noscan region. The feature is
// disabled there, so these always report "not a file region" and never
// allocate.

package runtime

func noscanFileRegionEnabled() bool { return false }

func noscanFileRegionAccepts(spanclass spanClass) bool { return false }

func noscanFileRegionAlloc(npages uintptr, spanclass spanClass) *mspan { return nil }

func isFileRegionAddr(addr uintptr) bool { return false }

func noscanFileRegionFreeLocked(s *mspan, typ spanAllocType) {}

func noscanFileRegionInit() {}

func pageoutFileRegion() {}
