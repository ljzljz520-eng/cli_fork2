// Copyright 2023 Vic Shóstak and Create Go App Contributors. All rights reserved.
// Use of this source code is governed by Apache 2.0 license
// that can be found in the LICENSE file.

//go:build darwin || linux || freebsd || netbsd || openbsd

package genplan

import (
	"os"
	"syscall"
)

// sameDevice reports whether two paths reside on the same file system, which
// is a hard requirement for the atomic rename commit protocol.
func sameDevice(a, b string) (bool, error) {
	ai, err := os.Stat(a)
	if err != nil {
		return false, err
	}
	bi, err := os.Stat(b)
	if err != nil {
		return false, err
	}
	as, ok := ai.Sys().(*syscall.Stat_t)
	if !ok {
		return false, nil
	}
	bs, ok := bi.Sys().(*syscall.Stat_t)
	if !ok {
		return false, nil
	}
	return as.Dev == bs.Dev, nil
}
