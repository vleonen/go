// Copyright 2024 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// File-backed memory region for large noscan spans.
//
// This is currently supported only on linux/amd64 and linux/arm64.

//go:build linux && (amd64 || arm64)

package runtime

// ftruncate calls the ftruncate system call. It is implemented in assembly.
// It returns 0 on success or a negative errno on failure.
//
//go:nosplit
func ftruncate(fd int32, length int64) int32
