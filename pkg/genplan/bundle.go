// Copyright 2023 Vic Shóstak and Create Go App Contributors. All rights reserved.
// Use of this source code is governed by Apache 2.0 license
// that can be found in the LICENSE file.

package genplan

import (
	"archive/zip"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// writeDiagnosticBundle collects everything needed to inspect a failed run —
// the canonical plan, journal, state, captured command output and manifests —
// into a single deterministic zip retained under the state directory.
func (en *Engine) writeDiagnosticBundle(reason string) (string, error) {
	if err := os.MkdirAll(en.layout.diagDir, 0o700); err != nil {
		return "", err
	}
	bundlePath := filepath.Join(en.layout.diagDir, "bundle.zip")
	if err := os.WriteFile(filepath.Join(en.layout.diagDir, "error.txt"), []byte(reason+"\n"), 0o600); err != nil {
		return "", err
	}

	f, err := os.Create(bundlePath) // #nosec G304 -- fixed bundle path inside the engine state directory
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()

	zw := zip.NewWriter(f)

	// Fixed modification time keeps bundle bytes reproducible across runs.
	fixedTime := time.Unix(0, 0).UTC()

	add := func(name, path string) error {
		info, err := os.Lstat(path)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if info.IsDir() {
			return addDir(zw, name, path, fixedTime)
		}
		return addFile(zw, name, path, info.Mode(), fixedTime)
	}

	_ = add("plan.json", en.layout.planFile)
	_ = add("journal.jsonl", filepath.Join(en.layout.root, "journal.jsonl"))
	_ = add("state.json", filepath.Join(en.layout.root, "state.json"))
	_ = add("logs", en.layout.logs)
	if en.baselineManifest != nil {
		_ = addBytes(zw, "manifest-baseline.txt", []byte(manifestText(en.baselineManifest)), fixedTime)
	}
	_ = addBytes(zw, "error.txt", []byte(reason+"\n"), fixedTime)

	if err := zw.Close(); err != nil {
		return "", err
	}
	if err := f.Sync(); err != nil {
		return "", err
	}
	return bundlePath, nil
}

// addFile writes a single file into the zip.
func addFile(zw *zip.Writer, name, path string, mode os.FileMode, t time.Time) error {
	in, err := os.Open(path) // #nosec G304 -- path is enumerated from the engine state directory
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	hdr := &zip.FileHeader{Name: name, Method: zip.Deflate}
	hdr.SetMode(mode.Perm())
	hdr.Modified = t
	out, err := zw.CreateHeader(hdr)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	return err
}

// addDir recursively adds a directory in sorted order.
func addDir(zw *zip.Writer, prefix, dir string, t time.Time) error {
	var names []string
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, name := range names {
		full := filepath.Join(dir, name)
		info, err := os.Lstat(full)
		if err != nil {
			return err
		}
		zipName := strings.TrimPrefix(prefix+"/"+filepath.ToSlash(name), "./")
		if info.IsDir() {
			if err := addDir(zw, zipName, full, t); err != nil {
				return err
			}
			continue
		}
		if err := addFile(zw, zipName, full, info.Mode(), t); err != nil {
			return err
		}
	}
	return nil
}

// addBytes stores an in-memory artifact in the bundle.
func addBytes(zw *zip.Writer, name string, data []byte, t time.Time) error {
	hdr := &zip.FileHeader{Name: name, Method: zip.Deflate}
	hdr.SetMode(0o600)
	hdr.Modified = t
	out, err := zw.CreateHeader(hdr)
	if err != nil {
		return err
	}
	_, err = out.Write(data)
	return err
}

// manifestText renders a stable, human-readable manifest listing.
func manifestText(m *manifest) string {
	var b strings.Builder
	for _, e := range m.Entries {
		b.WriteString(e.Hash)
		b.WriteString("  ")
		b.WriteString(e.Path)
		b.WriteByte('\n')
	}
	return b.String()
}
