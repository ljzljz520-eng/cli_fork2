// Copyright 2023 Vic Shóstak and Create Go App Contributors. All rights reserved.
// Use of this source code is governed by Apache 2.0 license
// that can be found in the LICENSE file.

package genplan

import (
	"fmt"
	"path"
	"path/filepath"
	"strings"
)

// cleanRelPath lexically validates a root-relative slash path and returns it
// in cleaned slash form. Absolute paths and any ".." traversal are rejected.
func cleanRelPath(rel string) (string, error) {
	rel = strings.TrimSpace(rel)
	if rel == "" {
		return "", fmt.Errorf("%w: empty path", ErrPolicyDenied)
	}

	if filepath.IsAbs(rel) || path.IsAbs(filepath.ToSlash(rel)) {
		return "", fmt.Errorf("%w: absolute path not allowed: %q", ErrPolicyDenied, rel)
	}

	clean := path.Clean(filepath.ToSlash(rel))
	if clean == ".." || strings.HasPrefix(clean, ".."+"/") {
		return "", fmt.Errorf("%w: path escapes root: %q", ErrPolicyDenied, rel)
	}
	if clean == "." || clean == "" {
		return "", fmt.Errorf("%w: empty path", ErrPolicyDenied)
	}

	return clean, nil
}

// secureRel converts a root-relative slash path into a cleaned OS path inside
// root. Callers must additionally reject existing symlinks before writes.
func secureRel(root, rel string) (string, error) {
	clean, err := cleanRelPath(rel)
	if err != nil {
		return "", err
	}
	return filepath.Join(root, filepath.FromSlash(clean)), nil
}

// relToRoot returns the slash-separated path of p relative to root.
func relToRoot(root, p string) (string, error) {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes root %q", p, root)
	}
	return filepath.ToSlash(rel), nil
}

// matchAny reports whether name matches at least one glob pattern.
func matchAny(patterns []string, name string) bool {
	for _, pattern := range patterns {
		if ok, err := path.Match(pattern, name); err == nil && ok {
			return true
		}
	}
	return false
}

// allowCommand checks a command (and its compensation chain) against the
// default-deny command policy.
func (p Policy) allowCommand(spec *CommandSpec) error {
	if spec == nil {
		return nil
	}
	base := filepath.Base(spec.Bin)
	if !matchAny(p.AllowCommands, base) {
		return fmt.Errorf("%w: command %q is not in allow-list %v", ErrPolicyDenied, base, p.AllowCommands)
	}
	if spec.Check != nil {
		if err := p.allowProbe(spec.Check); err != nil {
			return err
		}
	}
	return p.allowCommand(spec.Compensate)
}

// allowHTTP checks a request (and its compensation chain) against network
// policy. Matching is performed against host:port so local test servers on
// ephemeral ports can be expressed as "127.0.0.1:*".
func (p Policy) allowHTTP(spec *HTTPSpec) error {
	if spec == nil {
		return nil
	}
	host := httpHostPort(spec.URL)
	if host == "" {
		return fmt.Errorf("%w: invalid request URL %q", ErrPolicyDenied, spec.URL)
	}
	if !matchAny(p.AllowNetwork, host) {
		return fmt.Errorf("%w: network %q is not in allow-list %v", ErrPolicyDenied, host, p.AllowNetwork)
	}
	if spec.Check != nil {
		if err := p.allowProbe(spec.Check); err != nil {
			return err
		}
	}
	return p.allowHTTP(spec.Compensate)
}

// allowProbe validates the command/HTTP embedded in a recovery probe.
func (p Policy) allowProbe(probe *Probe) error {
	switch probe.Kind {
	case ProbeCommandExit:
		return p.allowCommand(probe.Command)
	case ProbeHTTPStatus:
		return p.allowHTTP(probe.HTTP)
	default:
		return fmt.Errorf("%w: unknown probe kind %q", ErrPolicyDenied, probe.Kind)
	}
}
