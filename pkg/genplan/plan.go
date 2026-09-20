// Copyright 2023 Vic Shóstak and Create Go App Contributors. All rights reserved.
// Use of this source code is governed by Apache 2.0 license
// that can be found in the LICENSE file.

package genplan

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
)

// Compile validates and normalizes the plan, enforces policy and assigns the
// content-addressed ID. The ID is independent of the target directory and of
// any execution-time data, so identical plans always produce identical IDs and
// identical output trees.
func (p *Plan) Compile() error {
	if p == nil {
		return fmt.Errorf("genplan: nil plan")
	}
	if p.Version != 0 && p.Version != PlanVersion {
		return fmt.Errorf("genplan: unsupported plan version %d (want %d)", p.Version, PlanVersion)
	}
	p.Version = PlanVersion

	if len(p.Steps) == 0 {
		return fmt.Errorf("genplan: plan contains no steps")
	}
	if p.Policy.MaxFileBytes == 0 {
		p.Policy.MaxFileBytes = DefaultMaxFileBytes
	}

	seen := make(map[string]struct{}, len(p.Steps))
	for i := range p.Steps {
		step := &p.Steps[i]
		if err := p.compileStep(step); err != nil {
			return fmt.Errorf("step %q: %w", step.ID, err)
		}
		if _, dup := seen[step.ID]; dup {
			return fmt.Errorf("genplan: duplicate step id %q", step.ID)
		}
		seen[step.ID] = struct{}{}
	}

	for i := range p.Verify {
		if err := p.Policy.allowCommand(&p.Verify[i]); err != nil {
			return fmt.Errorf("verify gate %d: %w", i, err)
		}
		normalizeCommand(&p.Verify[i])
	}

	// Compute the content-addressed ID from canonical JSON with a blank ID.
	savedID := p.ID
	p.ID = ""
	canonical, err := json.Marshal(p)
	if err != nil {
		p.ID = savedID
		return fmt.Errorf("genplan: cannot encode plan: %w", err)
	}
	sum := sha256.Sum256(canonical)
	p.ID = hex.EncodeToString(sum[:])
	return nil
}

// compileStep validates one step against its type and the policy.
func (p *Plan) compileStep(step *Step) error {
	id := strings.TrimSpace(step.ID)
	if id == "" {
		return fmt.Errorf("missing step id")
	}
	step.ID = id

	switch step.Type {
	case StepWriteFile, StepMkdir:
		if step.File == nil {
			return fmt.Errorf("file spec required for %s", step.Type)
		}
		clean, err := cleanRelPath(step.File.Path)
		if err != nil {
			return err
		}
		step.File.Path = clean
		if int64(len(step.File.Content)) > p.Policy.MaxFileBytes {
			return fmt.Errorf("%w: file %q exceeds %d bytes", ErrPolicyDenied, clean, p.Policy.MaxFileBytes)
		}
		if step.Type == StepWriteFile {
			if step.File.Perm == 0 {
				step.File.Perm = 0o644
			}
		} else {
			if step.File.Content != "" {
				return fmt.Errorf("mkdir step must not carry content: %q", clean)
			}
			if step.File.Perm == 0 {
				step.File.Perm = 0o755
			}
		}
		step.Run = nil
		step.HTTP = nil
	case StepRunCommand:
		if step.Run == nil {
			return fmt.Errorf("run spec required for run_command")
		}
		if strings.TrimSpace(step.Run.Bin) == "" {
			return fmt.Errorf("command bin is empty")
		}
		if step.Run.WorkDir != "" {
			clean, err := cleanRelPath(step.Run.WorkDir)
			if err != nil {
				return err
			}
			step.Run.WorkDir = clean
		}
		if err := p.Policy.allowCommand(step.Run); err != nil {
			return err
		}
		if step.Run.Check != nil {
			normalizeProbe(step.Run.Check)
		}
		normalizeCommand(step.Run)
		step.File = nil
		step.HTTP = nil
	case StepHTTPRequest:
		if step.HTTP == nil {
			return fmt.Errorf("http spec required for http_request")
		}
		if strings.TrimSpace(step.HTTP.URL) == "" {
			return fmt.Errorf("http url is empty")
		}
		if err := p.Policy.allowHTTP(step.HTTP); err != nil {
			return err
		}
		if step.HTTP.Check != nil {
			normalizeProbe(step.HTTP.Check)
		}
		normalizeHTTP(step.HTTP)
		step.File = nil
		step.Run = nil
	default:
		return fmt.Errorf("unknown step type %q", step.Type)
	}
	return nil
}

// normalizeCommand applies deterministic defaults and ordering.
func normalizeCommand(spec *CommandSpec) {
	if spec.TimeoutSeconds <= 0 {
		spec.TimeoutSeconds = 120
	}
	if spec.Env != nil {
		ordered := make(map[string]string, len(spec.Env))
		keys := make([]string, 0, len(spec.Env))
		for k := range spec.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			ordered[k] = spec.Env[k]
		}
		spec.Env = ordered
	}
	if spec.Args == nil {
		spec.Args = []string{}
	}
	if spec.Compensate != nil {
		normalizeCommand(spec.Compensate)
	}
}

// normalizeProbe applies deterministic defaults to a probe's embedded spec.
func normalizeProbe(probe *Probe) {
	switch probe.Kind {
	case ProbeCommandExit:
		if probe.Command != nil {
			normalizeCommand(probe.Command)
		}
	case ProbeHTTPStatus:
		if probe.HTTP != nil {
			normalizeHTTP(probe.HTTP)
		}
	}
}

// normalizeHTTP applies deterministic defaults and ordering.
func normalizeHTTP(spec *HTTPSpec) {
	if spec.Method == "" {
		spec.Method = "GET"
	}
	spec.Method = strings.ToUpper(spec.Method)
	if spec.TimeoutSeconds <= 0 {
		spec.TimeoutSeconds = 60
	}
	if spec.Headers != nil {
		ordered := make(map[string]string, len(spec.Headers))
		keys := make([]string, 0, len(spec.Headers))
		for k := range spec.Headers {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			ordered[k] = spec.Headers[k]
		}
		spec.Headers = ordered
	}
	if spec.Compensate != nil {
		normalizeHTTP(spec.Compensate)
	}
}

// httpHostPort extracts host:port from a URL for policy matching.
func httpHostPort(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Host
}

// CanonicalJSON returns the normalized JSON encoding of the plan (with ID).
func (p *Plan) CanonicalJSON() ([]byte, error) {
	return json.MarshalIndent(p, "", "  ")
}
