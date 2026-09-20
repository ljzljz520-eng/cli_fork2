// Copyright 2023 Vic Shóstak and Create Go App Contributors. All rights reserved.
// Use of this source code is governed by Apache 2.0 license
// that can be found in the LICENSE file.

//go:build !darwin && !linux && !freebsd && !netbsd && !openbsd

package genplan

// sameDevice cannot be inspected portably on this platform; the rename call
// itself remains the final guarantee.
func sameDevice(_, _ string) (bool, error) {
	return true, nil
}
