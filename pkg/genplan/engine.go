// Copyright 2023 Vic Shóstak and Create Go App Contributors. All rights reserved.
// Use of this source code is governed by Apache 2.0 license
// that can be found in the LICENSE file.

package genplan

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Options configures one engine execution.
type Options struct {
	// Target is the directory to publish. Created when absent.
	Target string
	// StateRoot holds journals/staging/bundles. Defaults to
	// "<parent(target)>/.cgapp-gen" and must share target's file system.
	StateRoot string
	// Resume continues an interrupted run from its journal.
	Resume bool
	// Force discards an existing committed state and runs the plan again.
	Force bool
	// Log receives human-readable progress lines (nil = silent).
	Log io.Writer
}

// Result summarizes an engine invocation.
type Result struct {
	Committed        bool
	RecoveredCommit  bool
	AlreadyCommitted bool
	Resumed          bool
	Changes          []FileChange
	Effects          []ExternalEffect
	BundlePath       string
}

// Engine executes a single plan against a single target.
type Engine struct {
	plan             *Plan
	opts             Options
	layout           layout
	journal          *Journal
	records          []Record
	baselineManifest *manifest

	// currentLogPrefix names stdout/stderr capture files for the active step.
	currentLogPrefix string
}

// unsafeName matches characters not safe in a log file name.
var unsafeName = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

// New compiles the plan and prepares an engine. Nothing is executed.
func New(plan *Plan, opts Options) (*Engine, error) {
	if plan == nil {
		return nil, fmt.Errorf("genplan: nil plan")
	}
	if err := plan.Compile(); err != nil {
		return nil, err
	}
	target, err := filepath.Abs(opts.Target)
	if err != nil {
		return nil, err
	}
	opts.Target = target
	if opts.StateRoot == "" {
		opts.StateRoot = filepath.Join(filepath.Dir(target), ".cgapp-gen")
	} else {
		opts.StateRoot, err = filepath.Abs(opts.StateRoot)
		if err != nil {
			return nil, err
		}
	}
	return &Engine{plan: plan, opts: opts, layout: newLayout(opts.StateRoot, target, plan.ID)}, nil
}

// rec appends a journal record and keeps the in-memory slice in sync, so
// compensation/pre-commit decisions observe records written during the run.
func (en *Engine) rec(kind RecKind, mutate func(*Record)) (Record, error) {
	r, err := en.journal.append(kind, mutate)
	if err == nil {
		en.records = append(en.records, r)
	}
	return r, err
}

// logf emits one progress line.
func (en *Engine) logf(format string, args ...interface{}) {
	if en.opts.Log == nil {
		return
	}
	fmt.Fprintln(en.opts.Log, fmt.Sprintf(format, args...))
}

// Preview computes the complete file/command/network preview without touching
// the target. The staging directory is not required to exist.
func (en *Engine) Preview() (*Preview, error) {
	baseline, err := en.readBaseline()
	if err != nil {
		return nil, err
	}
	p := &Preview{
		PlanID:   en.plan.ID,
		Target:   en.layout.target,
		Staging:  en.layout.staging,
		PolicyOK: true,
		Files:    en.projectedChanges(baseline),
	}
	for i := range en.plan.Steps {
		if fx, ok := externalOf(&en.plan.Steps[i]); ok {
			p.External = append(p.External, fx)
		}
	}
	for i := range en.plan.Verify {
		gate := en.plan.Verify[i]
		p.Verify = append(p.Verify, externalEffect("verify", &gate))
	}
	return p, nil
}

// readBaseline returns the current target manifest (or the persisted baseline
// when a state directory already exists).
func (en *Engine) readBaseline() (*manifest, error) {
	if en.baselineManifest != nil {
		return en.baselineManifest, nil
	}
	if data, err := os.ReadFile(filepath.Join(en.layout.root, "manifest-baseline.json")); err == nil {
		var m manifest
		if jerr := json.Unmarshal(data, &m.Entries); jerr == nil {
			en.baselineManifest = &m
			return &m, nil
		}
	}
	m, err := buildManifest(en.layout.target)
	if err != nil {
		if os.IsNotExist(err) {
			m = &manifest{}
		} else {
			return nil, err
		}
	}
	en.baselineManifest = m
	return m, nil
}

// projectedChanges derives the pre-run file change list from plan file steps.
func (en *Engine) projectedChanges(baseline *manifest) []FileChange {
	old := baseline.byPath()
	var changes []FileChange
	for i := range en.plan.Steps {
		step := &en.plan.Steps[i]
		if step.File == nil || step.Type != StepWriteFile {
			continue
		}
		entry := manifestEntry{
			Path: step.File.Path,
			Mode: step.File.Perm,
			Hash: hashBytes([]byte(step.File.Content)),
		}
		prev, existed := old[entry.Path]
		switch {
		case !existed:
			changes = append(changes, FileChange{Path: entry.Path, Kind: "added", Mode: entry.Mode, NewHash: entry.Hash})
		case prev.Hash != entry.Hash || prev.Mode != entry.Mode:
			changes = append(changes, FileChange{Path: entry.Path, Kind: "modified", Mode: entry.Mode, OldHash: prev.Hash, NewHash: entry.Hash})
		default:
			changes = append(changes, FileChange{Path: entry.Path, Kind: "unchanged", Mode: entry.Mode, NewHash: entry.Hash})
		}
	}
	return changes
}

// Run executes the plan transactionally. It is safe to call against a state
// left by a killed process together with Options.Resume.
func (en *Engine) Run(ctx context.Context) (*Result, error) {
	resumed, err := en.openExisting()
	if err != nil {
		return nil, err
	}

	if err := en.layout.prepareStateDirs(); err != nil {
		return nil, err
	}
	if err := en.writePlanFile(); err != nil {
		return nil, err
	}

	j, records, err := openJournal(en.layout.root)
	if err != nil {
		return nil, err
	}
	en.journal = j
	en.records = records
	defer func() { _ = j.close() }()

	// A crashed commit is resolved first, before any other work.
	if hasKind(en.records, recCommitStart) || hasKind(en.records, recCommitted) {
		committed, _, rerr := commitRecovery(&en.layout, en.records, j)
		if rerr != nil {
			return nil, rerr
		}
		if committed {
			_ = en.layout.removeStaging()
			return &Result{Committed: true, RecoveredCommit: true, Resumed: resumed}, nil
		}
		// Rollback completed; fall through to step replay + compensation.
	}

	// Rebuild staging first (fresh snapshot or deterministic resume base),
	// so the plan_start record carries the true baseline hash.
	if err := en.rebuildStaging(); err != nil {
		return nil, en.fail(ctx, err)
	}

	if !resumed {
		if _, err := en.rec(recPlanStart, func(r *Record) {
			r.Manifest = en.baselineManifest.hash()
		}); err != nil {
			return nil, err
		}
		if err := saveState(en.layout.root, &State{
			PlanID: en.plan.ID, Status: StatusRunning,
			Target: en.layout.target, Staging: en.layout.staging,
		}); err != nil {
			return nil, err
		}
	}

	if err := en.executeSteps(ctx, resumed); err != nil {
		return nil, en.fail(ctx, err)
	}

	if err := saveState(en.layout.root, &State{
		PlanID: en.plan.ID, Status: StatusVerifying,
		Target: en.layout.target, Staging: en.layout.staging,
	}); err != nil {
		return nil, en.fail(ctx, err)
	}

	en.logf("verifying build gates...")
	if err := en.runVerify(ctx); err != nil {
		return nil, en.fail(ctx, err)
	}
	if err := noSymlinks(en.layout.staging); err != nil {
		return nil, en.fail(ctx, err)
	}

	finalManifest, err := buildManifest(en.layout.staging)
	if err != nil {
		return nil, en.fail(ctx, err)
	}
	changes := diffManifests(en.baselineManifest, finalManifest)
	effects := en.executedEffects()
	en.printCommitGate(changes, effects, finalManifest.hash())

	if err := en.commit(); err != nil {
		committed, _, rerr := commitRecovery(&en.layout, en.records, en.journal)
		if rerr != nil {
			return nil, rerr
		}
		if committed {
			_ = en.layout.removeStaging()
			return &Result{Committed: true, RecoveredCommit: true}, nil
		}
		return nil, en.fail(ctx, fmt.Errorf("commit failed and target was rolled back: %w", err))
	}

	if err := en.layout.removeStaging(); err != nil {
		en.logf("warning: cleanup staging failed: %v", err)
	}
	en.logf("committed plan %s to %s", en.plan.ID[:12], en.layout.target)
	return &Result{Committed: true, Resumed: resumed, Changes: changes, Effects: effects}, nil
}

// openExisting handles a previously created state directory.
func (en *Engine) openExisting() (resumed bool, err error) {
	if _, statErr := os.Stat(en.layout.root); os.IsNotExist(statErr) {
		return false, nil
	}
	state, _, statErr := loadStateOrJournal(en.layout.root)
	if statErr != nil {
		return false, statErr
	}
	if state.Status == StatusCommitted && !en.opts.Force {
		return false, ErrAlreadyCommitted
	}
	if en.opts.Force {
		if err := os.RemoveAll(en.layout.root); err != nil {
			return false, err
		}
		return false, nil
	}
	if !en.opts.Resume {
		return false, fmt.Errorf("%w (state=%s, plan=%s)", ErrResumeRequired, state.Status, en.plan.ID[:12])
	}
	en.logf("resuming plan %s from journal (state=%s)", en.plan.ID[:12], state.Status)
	return true, nil
}

// writePlanFile persists the canonical plan; content is identical across runs.
func (en *Engine) writePlanFile() error {
	data, err := en.plan.CanonicalJSON()
	if err != nil {
		return err
	}
	if err := os.WriteFile(en.layout.planFile, data, 0o600); err != nil {
		return err
	}
	return syncDir(en.layout.planFile)
}

// rebuildStaging wipes and reconstructs staging from the target baseline.
// Called on both fresh runs and resumes, so the tree is always derived
// deterministically before external steps are replayed.
func (en *Engine) rebuildStaging() error {
	if err := os.RemoveAll(en.layout.staging); err != nil {
		return err
	}
	if err := en.layout.prepareStaging(); err != nil {
		return err
	}
	hadBaseline, err := en.layout.snapshotBaseline()
	if err != nil {
		return err
	}
	if !hadBaseline {
		// A freshly created project directory is published with standard
		// restrictive permissions (staging itself is created 0o700).
		if err := os.Chmod(en.layout.staging, 0o750); err != nil { // #nosec G302 -- directories require the execute/traverse bit
			return err
		}
	}
	baseline := &manifest{}
	if hadBaseline {
		baseline, err = buildManifest(en.layout.staging)
		if err != nil {
			return err
		}
	}
	en.baselineManifest = baseline
	data, err := json.Marshal(baseline.Entries)
	if err != nil {
		return err
	}
	path := filepath.Join(en.layout.root, "manifest-baseline.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return err
	}
	return syncDir(path)
}

// executeSteps runs steps in order, consulting the journal for idempotency.
func (en *Engine) executeSteps(ctx context.Context, resumed bool) error {
	states := stepStates(en.records)
	for i := range en.plan.Steps {
		step := &en.plan.Steps[i]
		en.currentLogPrefix = fmt.Sprintf("step-%02d-%s", i+1, unsafeName.ReplaceAllString(step.ID, "_"))

		if err := ctx.Err(); err != nil {
			return err
		}

		if !step.Type.IsExternal() {
			// File steps are deterministic: rebuilding staging always replays
			// them, with or without a journal record.
			if err := en.layout.applyFileStep(*step); err != nil {
				return err
			}
			if states[step.ID] == stepAbsent {
				if _, err := en.rec(recStepStart, func(r *Record) { r.StepID = step.ID }); err != nil {
					return err
				}
				if _, err := en.rec(recStepDone, func(r *Record) { r.StepID = step.ID }); err != nil {
					return err
				}
			}
			continue
		}

		switch states[step.ID] {
		case stepDone:
			en.logf("- skip %s: already completed (journal idempotency)", step.ID)
			if _, err := en.rec(recStepSkipped, func(r *Record) { r.StepID = step.ID }); err != nil {
				return err
			}
			continue
		case stepStarted:
			adopt, err := en.resolveAmbiguity(ctx, step)
			if err != nil {
				return fmt.Errorf("%w: step %q", err, step.ID)
			}
			if adopt {
				en.logf("- adopt %s: probe confirms the side effect exists", step.ID)
				if _, err := en.rec(recStepAdopted, func(r *Record) { r.StepID = step.ID }); err != nil {
					return err
				}
				continue
			}
			en.logf("- retry %s: probe reports the side effect is absent", step.ID)
		case stepCompensated:
			en.logf("- retry %s: previously compensated", step.ID)
		}

		if err := en.executeExternal(ctx, step); err != nil {
			return err
		}
	}
	return nil
}

// resolveAmbiguity decides whether a crashed external step can be adopted.
func (en *Engine) resolveAmbiguity(ctx context.Context, step *Step) (adopt bool, err error) {
	var (
		idempotent bool
		probe      *Probe
	)
	switch step.Type {
	case StepRunCommand:
		idempotent, probe = step.Run.Idempotent, step.Run.Check
	case StepHTTPRequest:
		idempotent, probe = step.HTTP.Idempotent, step.HTTP.Check
	}
	if idempotent {
		return false, nil // safe to re-run
	}
	if probe == nil {
		return false, ErrAmbiguous
	}
	return en.runProbe(ctx, probe)
}

// executeExternal journals and performs one external step.
func (en *Engine) executeExternal(ctx context.Context, step *Step) error {
	if err := saveState(en.layout.root, &State{
		PlanID: en.plan.ID, Status: StatusRunning, CurrentStep: step.ID,
		Target: en.layout.target, Staging: en.layout.staging,
	}); err != nil {
		return err
	}
	if _, err := en.rec(recStepStart, func(r *Record) { r.StepID = step.ID }); err != nil {
		return err
	}

	var res *execResult
	var err error
	switch step.Type {
	case StepRunCommand:
		en.logf("+ %s", step.ID)
		res, err = en.runCommand(ctx, step.Run)
	case StepHTTPRequest:
		en.logf("+ %s", step.ID)
		res, err = en.runHTTP(ctx, step.HTTP)
	default:
		return fmt.Errorf("not an external step: %s", step.Type)
	}
	if err != nil {
		return err
	}
	_, err = en.rec(recStepDone, func(r *Record) {
		r.StepID = step.ID
		if res != nil {
			r.ExitCode = res.exitCode
			r.Status = res.status
			r.StdoutHash = res.stdoutHash
			r.StderrHash = res.stderrHash
			r.BodyHash = res.bodyHash
		}
	})
	return err
}

// runCompensations reverses completed external steps in reverse order. The
// step that triggered the abort is absent from the journal's done set, and
// already-compensated steps are skipped, so compensation never doubles up.
func (en *Engine) runCompensations(ctx context.Context) {
	states := stepStates(en.records)
	for i := len(en.plan.Steps) - 1; i >= 0; i-- {
		step := &en.plan.Steps[i]
		if !step.Type.IsExternal() {
			continue
		}
		if states[step.ID] != stepDone {
			continue
		}
		did, err := en.compensate(ctx, *step)
		if err != nil {
			en.logf("! compensation for %s failed: %v", step.ID, err)
			_, _ = en.rec(recCompFailed, func(r *Record) {
				r.StepID = step.ID
				r.Reason = err.Error()
			})
			continue
		}
		if did {
			_, _ = en.rec(recStepCompensated, func(r *Record) { r.StepID = step.ID })
		}
	}
}

// fail aborts the transaction: compensates external side effects, persists
// state and writes the diagnostic bundle.
func (en *Engine) fail(ctx context.Context, cause error) error {
	en.logf("failure: %v", cause)
	en.runCompensations(ctx)
	_ = saveState(en.layout.root, &State{
		PlanID: en.plan.ID, Status: StatusAborted,
		Target: en.layout.target, Staging: en.layout.staging, Error: cause.Error(),
	})
	_, _ = en.rec(recAborted, func(r *Record) { r.Reason = cause.Error() })
	bundle, err := en.writeDiagnosticBundle(cause.Error())
	if err != nil {
		en.logf("warning: diagnostic bundle: %v", err)
	} else {
		en.logf("diagnostic bundle retained: %s", bundle)
	}
	return cause
}

// executedEffects lists external actions recorded as done/adopted, i.e. the
// complete set of side effects actually performed before commit.
func (en *Engine) executedEffects() []ExternalEffect {
	states := stepStates(en.records)
	var out []ExternalEffect
	for i := range en.plan.Steps {
		step := &en.plan.Steps[i]
		if !step.Type.IsExternal() {
			continue
		}
		if states[step.ID] != stepDone {
			continue
		}
		fx, _ := externalOf(step)
		out = append(out, fx)
	}
	return out
}

// printCommitGate is the mandatory pre-commit listing of every file change and
// every external command/request.
func (en *Engine) printCommitGate(changes []FileChange, effects []ExternalEffect, manifestHash string) {
	en.logf("")
	en.logf("commit gate (%d file change(s), manifest %s):", len(changes), manifestHash[:12])
	for _, c := range changes {
		en.logf("  %-8s %s", c.Kind, c.Path)
	}
	if len(changes) == 0 {
		en.logf("  (no file changes)")
	}
	en.logf("external actions performed (%d):", len(effects))
	for _, fx := range effects {
		if fx.Kind == "command" {
			en.logf("  $ %s %s", fx.Bin, fx.Args)
		} else {
			en.logf("  %s %s", fx.Method, fx.URL)
		}
	}
	if len(effects) == 0 {
		en.logf("  (none)")
	}
	en.logf("")
}

// externalOf converts a step into its preview effect descriptor.
func externalOf(step *Step) (ExternalEffect, bool) {
	switch step.Type {
	case StepRunCommand:
		return externalEffect(step.ID, step.Run), true
	case StepHTTPRequest:
		fx := ExternalEffect{
			StepID: step.ID, Kind: "http", Method: step.HTTP.Method, URL: step.HTTP.URL,
			Idempotent: step.HTTP.Idempotent, Compensable: step.HTTP.Compensate != nil,
		}
		return fx, true
	default:
		return ExternalEffect{}, false
	}
}

// externalEffect describes a command step or verify gate.
func externalEffect(id string, spec *CommandSpec) ExternalEffect {
	return ExternalEffect{
		StepID:      id,
		Kind:        "command",
		Bin:         spec.Bin,
		Args:        strings.Join(spec.Args, " "),
		WorkDir:     spec.WorkDir,
		Idempotent:  spec.Idempotent,
		Compensable: spec.Compensate != nil,
	}
}

// Inspect reads persisted state and journal of a state directory.
func Inspect(stateDir string) (State, []Record, error) {
	return loadStateOrJournal(stateDir)
}

// loadStateOrJournal loads state.json (falling back to journal-derived state).
func loadStateOrJournal(stateDir string) (State, []Record, error) {
	state, found, err := loadState(stateDir)
	if err != nil {
		return State{}, nil, err
	}
	records, _ := readJournal(filepath.Join(stateDir, "journal.jsonl"))
	if !found {
		state = State{PlanID: filepath.Base(stateDir), Status: StatusRunning}
	}
	return state, records, nil
}

// RecoveryOutcome describes what Recover resolved.
type RecoveryOutcome int

const (
	// RecoveryNone means no interrupted commit was found (the target was
	// never touched by a commit; use Run with Resume for step-level resume).
	RecoveryNone RecoveryOutcome = iota
	// RecoveryRolledBack means an interrupted commit was undone and the
	// original target restored (or left untouched if the crash preceded it).
	RecoveryRolledBack
	// RecoveryCommitted means the interrupted commit was finished.
	RecoveryCommitted
)

// Recover resolves an interrupted execution purely from a state directory.
// It finishes or rolls back a crashed commit; step-level recovery happens
// through Run with Resume=true.
func Recover(stateDir string, log io.Writer) (RecoveryOutcome, error) {
	state, _, err := loadStateOrJournal(stateDir)
	if err != nil {
		return RecoveryNone, err
	}
	target := state.Target
	if target == "" {
		return RecoveryNone, fmt.Errorf("genplan: state %s records no target; rerun with --resume", stateDir)
	}
	planID := filepath.Base(stateDir)
	stateRoot := filepath.Dir(stateDir)
	l := newLayout(stateRoot, target, planID)
	if err := l.prepareStateDirs(); err != nil {
		return RecoveryNone, err
	}
	en := &Engine{opts: Options{Log: log}, layout: l}
	j, existing, err := openJournal(stateDir)
	if err != nil {
		return RecoveryNone, err
	}
	en.journal = j
	defer func() { _ = j.close() }()
	committed, commitWasStarted, err := commitRecovery(&l, existing, j)
	if err != nil {
		return RecoveryNone, err
	}
	switch {
	case committed:
		_ = l.removeStaging()
		return RecoveryCommitted, nil
	case commitWasStarted:
		return RecoveryRolledBack, nil
	default:
		return RecoveryNone, nil
	}
}
