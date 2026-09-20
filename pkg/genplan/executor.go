// Copyright 2023 Vic Shóstak and Create Go App Contributors. All rights reserved.
// Use of this source code is governed by Apache 2.0 license
// that can be found in the LICENSE file.

package genplan

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// maxResponseBody bounds captured HTTP response bodies (10 MiB).
const maxResponseBody = 10 << 20

// execResult summarizes one external execution for the journal.
type execResult struct {
	exitCode   int
	status     int
	stdoutHash string
	stderrHash string
	bodyHash   string
}

// resolveWorkDir maps a root-relative work directory to an absolute staging
// path, rejecting symlink escapes.
func (en *Engine) resolveWorkDir(rel string) (string, error) {
	if strings.TrimSpace(rel) == "" {
		return en.layout.staging, nil
	}
	dir, err := secureRel(en.layout.staging, rel)
	if err != nil {
		return "", err
	}
	if err := rejectSymlinkPath(en.layout.staging, dir); err != nil {
		return "", err
	}
	return dir, nil
}

// runCommand executes a command with timeout, captures stdout/stderr to log
// files and returns content hashes for the journal.
func (en *Engine) runCommand(ctx context.Context, spec *CommandSpec) (*execResult, error) {
	dir, err := en.resolveWorkDir(spec.WorkDir)
	if err != nil {
		return nil, err
	}

	timeout := time.Duration(spec.TimeoutSeconds) * time.Second
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, spec.Bin, spec.Args...) // #nosec G204 -- policy-allow-listed, args no shell
	cmd.Dir = dir
	cmd.Env = commandEnv(spec.Env)

	var stdout, stderr bytes.Buffer
	stdoutPath := filepath.Join(en.layout.logs, en.logName(spec, "stdout"))
	stderrPath := filepath.Join(en.layout.logs, en.logName(spec, "stderr"))
	stdoutW, errCreateOut := os.Create(stdoutPath) // #nosec G304 -- fixed log file name inside the engine state directory
	if errCreateOut != nil {
		return nil, errCreateOut
	}
	defer func() { _ = stdoutW.Close() }()
	stderrW, errCreateErr := os.Create(stderrPath) // #nosec G304 -- fixed log file name inside the engine state directory
	if errCreateErr != nil {
		return nil, errCreateErr
	}
	defer func() { _ = stderrW.Close() }()
	cmd.Stdout = io.MultiWriter(&stdout, stdoutW)
	cmd.Stderr = io.MultiWriter(&stderr, stderrW)

	en.logf("  $ %s %s", spec.Bin, strings.Join(spec.Args, " "))
	startErr := cmd.Start()
	if startErr != nil {
		return nil, fmt.Errorf("start %q: %w", spec.Bin, startErr)
	}
	waitErr := cmd.Wait()

	res := &execResult{
		exitCode:   cmd.ProcessState.ExitCode(),
		stdoutHash: hashBytes(stdout.Bytes()),
		stderrHash: hashBytes(stderr.Bytes()),
	}
	if cctx.Err() == context.DeadlineExceeded {
		return res, fmt.Errorf("command %q timed out after %s", spec.Bin, timeout)
	}
	if waitErr != nil {
		if cctx.Err() != nil {
			return res, fmt.Errorf("command %q canceled: %w", spec.Bin, cctx.Err())
		}
		return res, &CommandError{Bin: spec.Bin, ExitCode: res.exitCode, Stderr: stderr.String()}
	}
	return res, nil
}

// logName derives a stable log file name for a command invocation.
func (en *Engine) logName(_ *CommandSpec, stream string) string {
	return en.currentLogPrefix + "." + stream
}

// commandEnv merges the inherited environment with deterministic overrides.
// Overrides are already sorted for plan hashing; the merged slice is sorted
// so spawned processes see a stable order.
func commandEnv(overrides map[string]string) []string {
	env := os.Environ()
	merged := make(map[string]string, len(env)+len(overrides))
	for _, kv := range env {
		if i := strings.IndexByte(kv, '='); i > 0 {
			merged[kv[:i]] = kv[i+1:]
		}
	}
	for k, v := range overrides {
		merged[k] = v
	}
	keys := make([]string, 0, len(merged))
	for k := range merged {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+merged[k])
	}
	return out
}

// runHTTP performs one policy-checked HTTP request and fingerprints the result.
func (en *Engine) runHTTP(ctx context.Context, spec *HTTPSpec) (*execResult, error) {
	timeout := time.Duration(spec.TimeoutSeconds) * time.Second
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var body io.Reader
	if spec.Body != "" {
		body = strings.NewReader(spec.Body)
	}
	req, err := http.NewRequestWithContext(cctx, spec.Method, spec.URL, body)
	if err != nil {
		return nil, err
	}
	for k, v := range spec.Headers {
		req.Header.Set(k, v)
	}

	en.logf("  %s %s", spec.Method, spec.URL)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http %s %s: %w", spec.Method, spec.URL, err)
	}
	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return nil, err
	}
	res := &execResult{status: resp.StatusCode, bodyHash: hashBytes(data)}
	if !statusAccepted(spec.ExpectStatus, resp.StatusCode) {
		return res, fmt.Errorf("http %s %s: unexpected status %d", spec.Method, spec.URL, resp.StatusCode)
	}
	return res, nil
}

// statusAccepted implements the expected-status rule: zero means any 2xx.
func statusAccepted(expected, actual int) bool {
	if expected == 0 {
		return actual >= 200 && actual < 300
	}
	return expected == actual
}

// runProbe reports whether the external side effect already exists.
func (en *Engine) runProbe(ctx context.Context, probe *Probe) (bool, error) {
	if probe == nil {
		return false, fmt.Errorf("missing probe")
	}
	switch probe.Kind {
	case ProbeCommandExit:
		res, err := en.runCommand(ctx, probe.Command)
		if err != nil {
			if _, ok := err.(*CommandError); ok {
				return res.exitCode == probe.ExpectExit, nil // wrong exit means "absent"
			}
			return false, err // infrastructure error: do not guess
		}
		return res.exitCode == probe.ExpectExit, nil
	case ProbeHTTPStatus:
		spec := probe.HTTP
		expected := probe.ExpectStatus
		if expected == 0 {
			expected = http.StatusOK
		}
		if spec.ExpectStatus == 0 {
			spec.ExpectStatus = expected
		}
		res, err := en.runHTTP(ctx, spec)
		if err != nil {
			// A non-2xx probe response means "absent"; transport errors abort.
			if res != nil {
				return false, nil
			}
			return false, err
		}
		return statusAccepted(expected, res.status), nil
	default:
		return false, fmt.Errorf("unknown probe kind %q", probe.Kind)
	}
}

// compensate reverses one already-applied external step. Compensation is
// itself journaled, so replaying an abort never compensates twice.
func (en *Engine) compensate(ctx context.Context, step Step) (bool, error) {
	switch step.Type {
	case StepRunCommand:
		if step.Run.Compensate == nil {
			return false, nil
		}
		en.logf("  compensating %s via $ %s %s", step.ID, step.Run.Compensate.Bin, strings.Join(step.Run.Compensate.Args, " "))
		_, err := en.runCommand(ctx, step.Run.Compensate)
		return true, err
	case StepHTTPRequest:
		if step.HTTP.Compensate == nil {
			return false, nil
		}
		en.logf("  compensating %s via %s %s", step.ID, step.HTTP.Compensate.Method, step.HTTP.Compensate.URL)
		_, err := en.runHTTP(ctx, step.HTTP.Compensate)
		return true, err
	default:
		return false, nil
	}
}

// CommandError carries a non-zero process exit for deterministic reporting.
type CommandError struct {
	Bin      string
	ExitCode int
	Stderr   string
}

func (e *CommandError) Error() string {
	return fmt.Sprintf("genplan: command %q exited with %d: %s", e.Bin, e.ExitCode, strings.TrimSpace(e.Stderr))
}
