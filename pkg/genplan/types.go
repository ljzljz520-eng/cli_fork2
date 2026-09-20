// Copyright 2023 Vic Shóstak and Create Go App Contributors. All rights reserved.
// Use of this source code is governed by Apache 2.0 license
// that can be found in the LICENSE file.

// Package genplan implements a transactional, journaled execution engine for
// typed generation plans.
//
// All actions are first compiled into a typed, content-addressed Plan. The
// plan is executed exclusively inside a staging directory on the same file
// system as the target. Every state transition is recorded in an append-only,
// fsync'ed journal. Only after build and policy verification pass is the
// staging tree published to the target through a journaled rename protocol
// that is recoverable after a crash (including SIGKILL/power loss).
//
// External actions (commands, HTTP requests) carry explicit compensation
// specifications. Replaying a journal never repeats a completed side effect:
// completed steps are adopted (optionally re-checked with a probe) instead of
// being executed again.
package genplan

import (
	"errors"
	"time"
)

// PlanVersion is the supported plan schema version.
const PlanVersion = 1

// DefaultMaxFileBytes bounds the size of a single generated file (10 MiB).
const DefaultMaxFileBytes = 10 << 20

// StepType enumerates the typed actions a plan may perform.
type StepType string

const (
	// StepWriteFile writes (or overwrites) a single file inside staging.
	StepWriteFile StepType = "write_file"
	// StepMkdir creates a directory (and parents) inside staging.
	StepMkdir StepType = "mkdir"
	// StepRunCommand executes an external process (a possible side effect).
	StepRunCommand StepType = "run_command"
	// StepHTTPRequest performs a single HTTP request (a possible side effect).
	StepHTTPRequest StepType = "http_request"
)

// IsExternal reports whether the step performs an action outside the staging
// tree and therefore participates in compensation/ambiguity handling.
func (t StepType) IsExternal() bool {
	return t == StepRunCommand || t == StepHTTPRequest
}

// FileSpec describes a write_file or mkdir operation.
type FileSpec struct {
	// Path is a slash-separated path relative to the plan root. It must not
	// escape the root (no absolute paths, no ".." traversal, no symlinks).
	Path string `json:"path"`
	// Content is the exact, deterministic file content. Directories omit it.
	Content string `json:"content,omitempty"`
	// Perm is the permission mode (permission bits only). Zero defaults to
	// 0o644 for files and 0o755 for directories.
	Perm uint32 `json:"perm,omitempty"`
}

// ProbeKind enumerates side-effect probes used during crash recovery.
type ProbeKind string

const (
	// ProbeCommandExit treats the probe as successful when the command exits
	// with the expected code (zero by default).
	ProbeCommandExit ProbeKind = "command_exit"
	// ProbeHTTPStatus treats the probe as successful when the response status
	// equals the expected status.
	ProbeHTTPStatus ProbeKind = "http_status"
)

// Probe deterministically checks whether an external side effect already
// exists. It resolves the ambiguity caused by a crash between starting and
// completing an external step.
type Probe struct {
	Kind         ProbeKind    `json:"kind"`
	Command      *CommandSpec `json:"command,omitempty"`
	HTTP         *HTTPSpec    `json:"http,omitempty"`
	ExpectExit   int          `json:"expect_exit,omitempty"`
	ExpectStatus int          `json:"expect_status,omitempty"`
}

// CommandSpec describes an external process invocation.
type CommandSpec struct {
	// Bin is the executable (looked up in PATH). Must match the command
	// policy allow-list.
	Bin string `json:"bin"`
	// Args are the literal arguments (no shell expansion).
	Args []string `json:"args,omitempty"`
	// Env are deterministic KEY=VALUE overrides applied on top of the
	// engine environment. Serialized sorted by key.
	Env map[string]string `json:"env,omitempty"`
	// WorkDir is a root-relative working directory (default: staging root).
	WorkDir string `json:"workdir,omitempty"`
	// TimeoutSeconds bounds execution. Zero defaults to 120 seconds.
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`
	// Idempotent marks the command safe to re-run blindly after a crash.
	Idempotent bool `json:"idempotent,omitempty"`
	// Check, when present, is executed on resume to adopt an already-applied
	// side effect instead of re-running the command.
	Check *Probe `json:"check,omitempty"`
	// Compensate, when present, reverses the side effect on abort.
	Compensate *CommandSpec `json:"compensate,omitempty"`
}

// HTTPSpec describes one HTTP request.
type HTTPSpec struct {
	Method         string            `json:"method,omitempty"` // default GET
	URL            string            `json:"url"`
	Headers        map[string]string `json:"headers,omitempty"`
	Body           string            `json:"body,omitempty"`
	ExpectStatus   int               `json:"expect_status,omitempty"` // default: 2xx
	Idempotent     bool              `json:"idempotent,omitempty"`
	Check          *Probe            `json:"check,omitempty"`
	Compensate     *HTTPSpec         `json:"compensate,omitempty"`
	TimeoutSeconds int               `json:"timeout_seconds,omitempty"`
}

// Step is one typed, uniquely identified plan action.
type Step struct {
	// ID is the stable, unique identifier used as the journal idempotency key.
	ID string `json:"id"`
	// Type selects which spec is active.
	Type StepType `json:"type"`
	// Description is an optional human-readable note.
	Description string `json:"description,omitempty"`
	// File is set for write_file and mkdir steps.
	File *FileSpec `json:"file,omitempty"`
	// Run is set for run_command steps.
	Run *CommandSpec `json:"run,omitempty"`
	// HTTP is set for http_request steps.
	HTTP *HTTPSpec `json:"http,omitempty"`
}

// Policy is the default-deny gate applied while compiling a plan.
type Policy struct {
	// AllowCommands lists glob patterns matched against the command base name
	// (e.g. "go", "sh"). Empty denies every command.
	AllowCommands []string `json:"allow_commands,omitempty"`
	// AllowNetwork lists glob patterns matched against host:port of request
	// URLs (e.g. "127.0.0.1:*", "api.example.com"). Empty denies all network.
	AllowNetwork []string `json:"allow_network,omitempty"`
	// MaxFileBytes bounds a single generated file size.
	MaxFileBytes int64 `json:"max_file_bytes,omitempty"`
}

// Plan is the complete typed generation plan. It is target-independent: the
// target directory is supplied at execution time, so the same plan yields the
// same content-addressed ID everywhere.
type Plan struct {
	// Version must equal PlanVersion.
	Version int `json:"version"`
	// ID is the content hash assigned at compile time (zero before Compile).
	ID string `json:"id,omitempty"`
	// Steps execute in declared order; IDs must be unique.
	Steps []Step `json:"steps"`
	// Policy gates file, command and network actions (default deny).
	Policy Policy `json:"policy"`
	// Verify are build verification gates executed in staging before commit.
	Verify []CommandSpec `json:"verify,omitempty"`
}

// Status enumerates persisted engine states.
type Status string

const (
	// StatusRunning: steps are executing in staging.
	StatusRunning Status = "running"
	// StatusVerifying: all steps applied, gates are running.
	StatusVerifying Status = "verifying"
	// StatusCommitting: the atomic rename protocol is in progress.
	StatusCommitting Status = "committing"
	// StatusCommitted: target was published; the plan is complete.
	StatusCommitted Status = "committed"
	// StatusAborted: execution failed; external side effects were compensated.
	StatusAborted Status = "aborted"
)

// State is the atomically persisted engine status.
type State struct {
	PlanID      string    `json:"plan_id"`
	Status      Status    `json:"status"`
	UpdatedAt   time.Time `json:"updated_at"`
	CurrentStep string    `json:"current_step,omitempty"`
	Target      string    `json:"target,omitempty"`
	Staging     string    `json:"staging,omitempty"`
	Backup      string    `json:"backup,omitempty"`
	Error       string    `json:"error,omitempty"`
}

// FileChange describes one projected or realized change against the baseline.
type FileChange struct {
	Path    string `json:"path"`
	Kind    string `json:"kind"` // added, modified, deleted, unchanged
	Mode    uint32 `json:"mode,omitempty"`
	OldHash string `json:"old_hash,omitempty"`
	NewHash string `json:"new_hash,omitempty"`
}

// ExternalEffect is a command or network action visible in a preview/journal.
type ExternalEffect struct {
	StepID      string `json:"step_id"`
	Kind        string `json:"kind"` // command, http
	Bin         string `json:"bin,omitempty"`
	Args        string `json:"args,omitempty"`
	Method      string `json:"method,omitempty"`
	URL         string `json:"url,omitempty"`
	WorkDir     string `json:"workdir,omitempty"`
	Idempotent  bool   `json:"idempotent"`
	Compensable bool   `json:"compensable"`
}

// Preview is the complete pre-execution description of a plan.
type Preview struct {
	PlanID     string           `json:"plan_id"`
	Target     string           `json:"target"`
	Staging    string           `json:"staging"`
	Files      []FileChange     `json:"files"`
	External   []ExternalEffect `json:"external"`
	Verify     []ExternalEffect `json:"verify"`
	PolicyOK   bool             `json:"policy_ok"`
	PolicyNote string           `json:"policy_note,omitempty"`
}

// Sentinel errors.
var (
	// ErrAlreadyCommitted is returned when an already committed plan is run
	// again without Force.
	ErrAlreadyCommitted = errors.New("genplan: plan already committed; journal prevents duplicate side effects")
	// ErrResumeRequired is returned for an aborted/running state found without
	// Resume being set.
	ErrResumeRequired = errors.New("genplan: interrupted state found; rerun with --resume to continue deterministically")
	// ErrAmbiguous is returned when an external step was started but not
	// recorded as done, and the plan cannot determine whether the side
	// effect happened (neither Idempotent nor a Check probe is available).
	ErrAmbiguous = errors.New("genplan: ambiguous external step after crash; add idempotent:true or a check probe to resume")
	// ErrCrossDevice is returned when the state directory is not on the same
	// file system as the target (atomic rename would be impossible).
	ErrCrossDevice = errors.New("genplan: state directory must be on the same file system as the target")
	// ErrPolicyDenied is returned when a plan action violates policy.
	ErrPolicyDenied = errors.New("genplan: policy denied")
)
