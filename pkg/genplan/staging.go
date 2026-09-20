// Copyright 2023 Vic Shóstak and Create Go App Contributors. All rights reserved.
// Use of this source code is governed by Apache 2.0 license
// that can be found in the LICENSE file.

package genplan

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// layout groups the well-known paths of one plan execution.
type layout struct {
	root     string // state root: <stateRoot>/<planID>
	staging  string // working tree
	logs     string // captured stdout/stderr per step
	diagDir  string // diagnostic bundle output
	planFile string // canonical plan copy
	target   string // final directory (absolute)
	backup   string // target rename backup during commit
}

// newLayout resolves all paths for a plan execution.
func newLayout(stateRoot, target, planID string) layout {
	root := filepath.Join(stateRoot, planID)
	return layout{
		root:     root,
		staging:  filepath.Join(root, "staging"),
		logs:     filepath.Join(root, "logs"),
		diagDir:  filepath.Join(root, "diagnostics"),
		planFile: filepath.Join(root, "plan.json"),
		target:   target,
		backup:   filepath.Join(filepath.Dir(target), ".cgapp-old-"+planID[:12]),
	}
}

// prepareStateDirs creates the state hierarchy and checks same-filesystem.
func (l *layout) prepareStateDirs() error {
	if err := os.MkdirAll(l.root, 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(l.logs, 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(l.diagDir, 0o700); err != nil {
		return err
	}
	same, err := sameDevice(l.root, filepath.Dir(l.target))
	if err != nil {
		return err
	}
	if !same {
		return ErrCrossDevice
	}
	return nil
}

// prepareStaging creates an empty staging tree.
func (l *layout) prepareStaging() error {
	if err := os.MkdirAll(l.staging, 0o700); err != nil {
		return err
	}
	return syncDir(l.root)
}

// snapshotBaseline copies an existing target into staging so plans may target
// non-empty directories. The target itself is never modified before commit.
func (l *layout) snapshotBaseline() (bool, error) {
	info, err := os.Lstat(l.target)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil // fresh project: nothing to snapshot
		}
		return false, err
	}
	if !info.IsDir() {
		return false, fmt.Errorf("genplan: target %q exists and is not a directory", l.target)
	}
	if err := copyTree(l.target, l.staging); err != nil {
		return false, fmt.Errorf("baseline snapshot: %w", err)
	}
	// Publish must preserve the original target directory's permission bits.
	if err := os.Chmod(l.staging, info.Mode().Perm()|0o700); err != nil {
		return false, err
	}
	return true, nil
}

// copyTree recursively copies src into dst, preserving content and permission
// bits. Symlinks are recreated as links; the verification scan rejects them
// before commit, but keeping them makes the snapshot faithful for inspection.
func copyTree(src, dst string) error {
	return filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		out := dst
		if rel != "." {
			out = filepath.Join(dst, rel)
		}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			// #nosec G122 -- p is enumerated by Walk from the trusted local target being snapshotted; the pre-commit symlink scan rejects every link before publish.
			link, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return os.Symlink(link, out) // #nosec G122 -- links are recreated faithfully in staging and rejected by noSymlinks before commit
		case info.IsDir():
			return os.MkdirAll(out, info.Mode().Perm()|0o700)
		case info.Mode().IsRegular():
			return copyFile(p, out, info.Mode().Perm())
		default:
			return nil // sockets, devices etc. are skipped
		}
	})
}

// copyFile copies one regular file and sets its mode explicitly (umask-free).
func copyFile(src, dst string, perm os.FileMode) error {
	in, err := os.Open(src) // #nosec G304 -- src is enumerated from the local target snapshot root
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm) // #nosec G304 -- dst is policy-confined to the staging root
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chmod(dst, perm)
}

// applyFileStep performs a write_file/mkdir step inside staging. Existing
// symlinks at the destination are rejected so writes cannot escape the root.
func (l *layout) applyFileStep(step Step) error {
	dst, err := secureRel(l.staging, step.File.Path)
	if err != nil {
		return err
	}
	if err := rejectSymlinkPath(l.staging, dst); err != nil {
		return err
	}
	switch step.Type {
	case StepMkdir:
		return os.MkdirAll(dst, os.FileMode(step.File.Perm))
	case StepWriteFile:
		if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
			return err
		}
		if err := os.WriteFile(dst, []byte(step.File.Content), os.FileMode(step.File.Perm)); err != nil {
			return err
		}
		if err := os.Chmod(dst, os.FileMode(step.File.Perm)); err != nil {
			return err
		}
		return nil
	default:
		return fmt.Errorf("genplan: not a file step: %s", step.Type)
	}
}

// rejectSymlinkPath walks every existing component of dst relative to root
// and fails if a component is a symlink (write-through escape protection).
func rejectSymlinkPath(root, dst string) error {
	rel, err := filepath.Rel(root, dst)
	if err != nil {
		return err
	}
	parts := splitParts(rel)
	cur := root
	for _, part := range parts {
		cur = filepath.Join(cur, part)
		info, err := os.Lstat(cur)
		if err != nil {
			if os.IsNotExist(err) {
				return nil // remaining components do not exist yet
			}
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			rel, _ := relToRoot(root, cur)
			return &PolicyViolation{Path: rel, Reason: "refusing to write through a symlink"}
		}
	}
	return nil
}

// splitParts splits an OS-relative path into its components.
func splitParts(rel string) []string {
	var parts []string
	for {
		dir, base := filepath.Dir(rel), filepath.Base(rel)
		if base != "." && base != string(filepath.Separator) {
			parts = append([]string{base}, parts...)
		}
		if dir == "." || dir == string(filepath.Separator) || dir == rel {
			break
		}
		rel = dir
	}
	return parts
}

// removeStaging deletes the working tree (called after a successful publish).
func (l *layout) removeStaging() error {
	if err := os.RemoveAll(l.staging); err != nil {
		return err
	}
	return syncDir(l.root)
}
