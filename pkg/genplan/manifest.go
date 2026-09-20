// Copyright 2023 Vic Shóstak and Create Go App Contributors. All rights reserved.
// Use of this source code is governed by Apache 2.0 license
// that can be found in the LICENSE file.

package genplan

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// manifestEntry is one regular file in a tree manifest.
type manifestEntry struct {
	Path string
	Mode uint32
	Hash string // sha256 of content, "-" for symlinks
}

// manifest is a sorted, content-addressed description of a tree.
type manifest struct {
	Entries []manifestEntry
}

// hash returns the deterministic digest of the whole manifest.
func (m *manifest) hash() string {
	h := sha256.New()
	for _, e := range m.Entries {
		_, _ = io.WriteString(h, e.Path)
		_, _ = h.Write([]byte{0})
		_, _ = io.WriteString(h, e.Hash)
		_, _ = h.Write([]byte{0})
		// Permission bits are always <= 0o777, so an octal string both avoids
		// integer narrowing and stays deterministic.
		_, _ = fmt.Fprintf(h, "%04o", e.Mode)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// byPath returns the entry index keyed by slash path.
func (m *manifest) byPath() map[string]manifestEntry {
	idx := make(map[string]manifestEntry, len(m.Entries))
	for _, e := range m.Entries {
		idx[e.Path] = e
	}
	return idx
}

// buildManifest walks root and records every regular file. Any symlink is
// recorded with a "-" hash (and later rejected by the policy scan), so the
// manifest stays complete even on trees that fail verification.
func buildManifest(root string) (*manifest, error) {
	m := &manifest{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == root {
			return nil
		}
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		rel, err := relToRoot(root, p)
		if err != nil {
			return err
		}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			m.Entries = append(m.Entries, manifestEntry{Path: rel, Mode: uint32(info.Mode().Perm()), Hash: "-"})
		case info.Mode().IsRegular():
			sum, err := hashFile(p)
			if err != nil {
				return err
			}
			m.Entries = append(m.Entries, manifestEntry{Path: rel, Mode: uint32(info.Mode().Perm()), Hash: sum})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(m.Entries, func(i, j int) bool { return m.Entries[i].Path < m.Entries[j].Path })
	return m, nil
}

// hashFile returns the hex sha256 of a file's content.
func hashFile(path string) (string, error) {
	f, err := os.Open(path) // #nosec G304 -- path is scoped to the engine staging/state directory
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// hashBytes returns the hex sha256 of b.
func hashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// diffManifests returns every changed/added/deleted file between baseline and
// current. Unchanged entries are omitted from the result.
func diffManifests(baseline, current *manifest) []FileChange {
	var changes []FileChange
	old := baseline.byPath()
	cur := current.byPath()

	for path, e := range cur {
		prev, existed := old[path]
		switch {
		case !existed:
			changes = append(changes, FileChange{Path: path, Kind: "added", Mode: e.Mode, NewHash: e.Hash})
		case prev.Hash != e.Hash || prev.Mode != e.Mode:
			changes = append(changes, FileChange{Path: path, Kind: "modified", Mode: e.Mode, OldHash: prev.Hash, NewHash: e.Hash})
		}
	}
	for path, e := range old {
		if _, ok := cur[path]; !ok {
			changes = append(changes, FileChange{Path: path, Kind: "deleted", Mode: e.Mode, OldHash: e.Hash})
		}
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].Path < changes[j].Path })
	return changes
}

// noSymlinks returns an error if the tree contains a symbolic link. Commands
// run inside staging must not be able to escape through pre-existing links.
func noSymlinks(root string) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 {
			rel, _ := relToRoot(root, p)
			return &PolicyViolation{Path: rel, Reason: "symlinks are not allowed in generated trees"}
		}
		return nil
	})
}

// PolicyViolation describes a rejected path during the post-execution scan.
type PolicyViolation struct {
	Path   string
	Reason string
}

func (e *PolicyViolation) Error() string {
	return "genplan: policy violation at " + e.Path + ": " + e.Reason
}
