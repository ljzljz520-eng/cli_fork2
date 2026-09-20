// Copyright 2023 Vic Shóstak and Create Go App Contributors. All rights reserved.
// Use of this source code is governed by Apache 2.0 license
// that can be found in the LICENSE file.

package genplan

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// ---- helpers -------------------------------------------------------------

// newTestPaths returns target/state paths under one temp dir (one file system).
func newTestPaths(t *testing.T) (target, stateRoot string) {
	t.Helper()
	tmp := t.TempDir()
	return filepath.Join(tmp, "target"), filepath.Join(tmp, "state")
}

func newEngine(t *testing.T, p *Plan, target, stateRoot string, opts ...func(*Options)) *Engine {
	t.Helper()
	o := Options{Target: target, StateRoot: stateRoot}
	for _, fn := range opts {
		fn(&o)
	}
	en, err := New(p, o)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return en
}

func runOK(t *testing.T, en *Engine) *Result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := en.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return res
}

func runExpectErr(t *testing.T, en *Engine, want error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	_, err := en.Run(ctx)
	if want == nil {
		if err == nil {
			t.Fatal("expected a failure, got nil")
		}
		return
	}
	if !errors.Is(err, want) {
		t.Fatalf("expected %v, got %v", want, err)
	}
}

func osReadDirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func stateDirOf(target, stateRoot, planID string) string {
	return filepath.Join(stateRoot, planID)
}

// markerCmd returns a command that appends a line to $MARKER.
func markerCmd(line string, compensate bool) *CommandSpec {
	spec := &CommandSpec{
		Bin:  "sh",
		Args: []string{"-c", fmt.Sprintf("printf '%%s\\n' '%s' >> \"$MARKER\"", line)},
		Env:  map[string]string{},
	}
	if compensate {
		spec.Compensate = &CommandSpec{
			Bin:  "sh",
			Args: []string{"-c", "printf '%s\\n' 'compensated' >> \"$MARKER\""},
			Env:  map[string]string{},
		}
	}
	return spec
}

// failingCmd exits 7.
func failingCmd() *CommandSpec {
	return &CommandSpec{Bin: "sh", Args: []string{"-c", "exit 7"}, Env: map[string]string{}}
}

func basePolicy() Policy {
	return Policy{AllowCommands: []string{"sh", "go"}}
}

// ---- plan compile / determinism ------------------------------------------

func TestCompileIsContentAddressed(t *testing.T) {
	p1 := DemoPlan("")
	if err := p1.Compile(); err != nil {
		t.Fatal(err)
	}
	p2 := DemoPlan("")
	if err := p2.Compile(); err != nil {
		t.Fatal(err)
	}
	if p1.ID != p2.ID {
		t.Fatalf("identical plans got different IDs: %s vs %s", p1.ID[:12], p2.ID[:12])
	}

	// Target-independence: ID does not depend on where the plan executes.
	en1 := newEngine(t, DemoPlan(""), "/tmp/a/target", "/tmp/a/state")
	en2 := newEngine(t, DemoPlan(""), "/tmp/b/target", "/tmp/b/state")
	if en1.plan.ID != en2.plan.ID {
		t.Fatal("plan ID must be target-independent")
	}

	// A changed content changes the ID.
	p3 := DemoPlan("")
	p3.Steps[0].File.Content += "\n// changed\n"
	_ = p3.Compile()
	if p3.ID == p1.ID {
		t.Fatal("content change did not change plan ID")
	}
}

func TestPolicyRejectsUnsafePlan(t *testing.T) {
	cases := []struct {
		name string
		step Step
	}{
		{"path escape", Step{ID: "x", Type: StepWriteFile, File: &FileSpec{Path: "../escape.txt", Content: "x"}}},
		{"absolute path", Step{ID: "x", Type: StepWriteFile, File: &FileSpec{Path: "/etc/x", Content: "x"}}},
		{"command denied", Step{ID: "x", Type: StepRunCommand, Run: &CommandSpec{Bin: "rm", Args: []string{"-rf", "/"}}}},
		{"http denied", Step{ID: "x", Type: StepHTTPRequest, HTTP: &HTTPSpec{URL: "http://evil.example.com/", Method: "GET"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &Plan{Policy: basePolicy(), Steps: []Step{tc.step}}
			if tc.step.Type == StepHTTPRequest {
				p.Policy.AllowCommands = nil
			}
			err := p.Compile()
			if !errors.Is(err, ErrPolicyDenied) {
				t.Fatalf("expected ErrPolicyDenied, got %v", err)
			}
		})
	}
}

// ---- happy path, preview, idempotent replay ------------------------------

func happyPlan(marker string) *Plan {
	return &Plan{
		Policy: basePolicy(),
		Steps: []Step{
			{ID: "mod", Type: StepWriteFile, File: &FileSpec{Path: "go.mod", Content: "module demo\n\ngo 1.23\n"}},
			{ID: "main", Type: StepWriteFile, File: &FileSpec{Path: "main.go", Content: "package main\nfunc main() {}\n"}},
			{ID: "marker", Type: StepRunCommand, Run: withMarkerEnv(markerCmd("ran", false), marker)},
		},
		Verify: []CommandSpec{{Bin: "go", Args: []string{"build", "-o", os.DevNull, "."}}},
	}
}

func withMarkerEnv(spec *CommandSpec, marker string) *CommandSpec {
	if spec.Env == nil {
		spec.Env = map[string]string{}
	}
	spec.Env["MARKER"] = marker
	if spec.Compensate != nil {
		spec.Compensate.Env = map[string]string{"MARKER": marker}
	}
	return spec
}

func TestDryRunDoesNotTouchTarget(t *testing.T) {
	target, stateRoot := newTestPaths(t)
	marker := filepath.Join(t.TempDir(), "marker.log")
	en := newEngine(t, happyPlan(marker), target, stateRoot)

	pv, err := en.Preview()
	if err != nil {
		t.Fatal(err)
	}
	if !pv.PolicyOK || len(pv.Files) != 2 || len(pv.External) != 1 || len(pv.Verify) != 1 {
		t.Fatalf("bad preview: %+v", pv)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatal("target must not exist after preview")
	}
	if _, err := os.Stat(stateRoot); !os.IsNotExist(err) {
		t.Fatal("state must not exist after preview")
	}
}

func TestHappyPathAndIdempotentReplay(t *testing.T) {
	target, stateRoot := newTestPaths(t)
	markerDir := t.TempDir()
	marker := filepath.Join(markerDir, "marker.log")
	plan := happyPlan(marker)

	res := runOK(t, newEngine(t, plan, target, stateRoot))
	if !res.Committed || len(res.Changes) != 2 {
		t.Fatalf("bad result: %+v", res)
	}
	if got := readFile(t, filepath.Join(target, "main.go")); !strings.Contains(got, "package main") {
		t.Fatalf("target content wrong: %q", got)
	}
	if got := readFile(t, marker); got != "ran\n" {
		t.Fatalf("marker = %q, want ran\\n", got)
	}

	// Committed state is persisted for inspection.
	state, records, err := Inspect(stateDirOf(target, stateRoot, plan.ID))
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != StatusCommitted {
		t.Fatalf("state = %s", state.Status)
	}
	if !hasKind(records, recCommitted) || !hasKind(records, recTargetPublished) {
		t.Fatal("journal misses commit records")
	}

	// Re-running the same plan is a no-op: no second command side effect.
	runExpectErr(t, newEngine(t, plan, target, stateRoot), ErrAlreadyCommitted)
	if got := readFile(t, marker); got != "ran\n" {
		t.Fatalf("replay duplicated side effect: marker = %q", got)
	}
}

func TestOutputsAreDeterministic(t *testing.T) {
	tmp := t.TempDir()
	plan := happyPlan(filepath.Join(tmp, "m1"))
	en1 := newEngine(t, plan, filepath.Join(tmp, "t1"), filepath.Join(tmp, "s1"))
	runOK(t, en1)
	// Recompile a fresh, equal plan for the second target.
	plan2 := happyPlan(filepath.Join(tmp, "m2"))
	en2 := newEngine(t, plan2, filepath.Join(tmp, "t2"), filepath.Join(tmp, "s2"))
	runOK(t, en2)

	m1, _ := buildManifest(filepath.Join(tmp, "t1"))
	m2, _ := buildManifest(filepath.Join(tmp, "t2"))
	if m1.hash() != m2.hash() {
		t.Fatalf("generated trees differ:\n%s\n%s", manifestText(m1), manifestText(m2))
	}
}

// ---- failure, compensation, target preservation, diagnostics -------------

func failingPlan(marker string, withCompensation bool) *Plan {
	cmd := markerCmd("ran", withCompensation)
	return &Plan{
		Policy: basePolicy(),
		Steps: []Step{
			{ID: "f", Type: StepWriteFile, File: &FileSpec{Path: "f.txt", Content: "x\n"}},
			{ID: "external", Type: StepRunCommand, Run: withMarkerEnv(cmd, marker)},
			{ID: "boom", Type: StepRunCommand, Run: failingCmd()},
		},
	}
}

func TestFailureCompensatesAndPreservesTarget(t *testing.T) {
	tmp := t.TempDir()
	target := filepath.Join(tmp, "target")
	stateRoot := filepath.Join(tmp, "state")
	// Existing, non-empty target must remain byte-identical after failure.
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "old.txt"), []byte("keep me\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(tmp, "marker.log")

	en := newEngine(t, failingPlan(marker, true), target, stateRoot)
	runExpectErr(t, en, nil) // any failure; assert specifics below

	if got := readFile(t, filepath.Join(target, "old.txt")); got != "keep me\n" {
		t.Fatalf("target mutated after failure: %q", got)
	}
	if _, err := os.Stat(filepath.Join(target, "f.txt")); !os.IsNotExist(err) {
		t.Fatal("generated file leaked into target after failure")
	}
	for _, e := range osReadDirNames(t, filepath.Dir(target)) {
		if strings.HasPrefix(e, ".cgapp-old-") {
			t.Fatalf("target backup leaked after failed run: %s", e)
		}
	}
	if got := readFile(t, marker); got != "ran\ncompensated\n" {
		t.Fatalf("marker = %q, want ran+compensated", got)
	}

	// Diagnostic bundle retained.
	bundle := filepath.Join(stateRoot, en.plan.ID, "diagnostics", "bundle.zip")
	zr, err := zip.OpenReader(bundle)
	if err != nil {
		t.Fatalf("bundle missing: %v", err)
	}
	defer func() { _ = zr.Close() }()
	names := map[string]bool{}
	for _, f := range zr.File {
		names[f.Name] = true
	}
	for _, want := range []string{"plan.json", "journal.jsonl", "state.json", "error.txt"} {
		if !names[want] {
			t.Fatalf("bundle lacks %s (has %v)", want, names)
		}
	}
	hasLog := false
	for name := range names {
		if strings.HasPrefix(name, "logs/") {
			hasLog = true
		}
	}
	if !hasLog {
		t.Fatalf("bundle lacks captured logs (has %v)", names)
	}

	// Rerunning without --resume is refused.
	runExpectErr(t, newEngine(t, failingPlan(marker, true), target, stateRoot), ErrResumeRequired)
}

func TestResumeAfterCompensatedFailure(t *testing.T) {
	target, stateRoot := newTestPaths(t)
	tmp := filepath.Dir(target)
	marker := filepath.Join(tmp, "marker.log")
	plan := failingPlan(marker, true)

	runExpectErr(t, newEngine(t, plan, target, stateRoot), nil)

	// Same failing plan resumed: compensated external step re-executes and
	// fails again, compensation runs again (journal prevents double
	// compensation within one abort).
	en := newEngine(t, plan, target, stateRoot, func(o *Options) { o.Resume = true })
	runExpectErr(t, en, nil)
	if got := readFile(t, marker); got != "ran\ncompensated\nran\ncompensated\n" {
		t.Fatalf("marker = %q", got)
	}
}

// ---- crash recovery: ambiguous external step via probe --------------------

func crashPlan(marker string, withProbe bool) *Plan {
	cmd := markerCmd("ran", false)
	cmd.Idempotent = false
	if withProbe {
		cmd.Check = &Probe{
			Kind: ProbeCommandExit,
			Command: &CommandSpec{
				Bin:  "sh",
				Args: []string{"-c", "test -f \"$MARKER\""},
				Env:  map[string]string{"MARKER": marker},
			},
		}
	}
	return &Plan{
		Policy: basePolicy(),
		Steps: []Step{
			{ID: "f", Type: StepWriteFile, File: &FileSpec{Path: "f.txt", Content: "x\n"}},
			{ID: "external", Type: StepRunCommand, Run: withMarkerEnv(cmd, marker)},
			{ID: "after", Type: StepWriteFile, File: &FileSpec{Path: "after.txt", Content: "y\n"}},
		},
	}
}

// simulateCrashAfterSideEffect rewrites the journal to look as if the process
// was killed after the external step's side effect happened but before its
// step_done record was persisted.
func simulateCrashAfterSideEffect(t *testing.T, root, stepID, target string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "journal.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var kept []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var rec Record
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		switch rec.Kind {
		case recVerifyStart, recVerifyDone, recCommitStart, recTargetBackedUp,
			recTargetPublished, recBackupRemoved, recCommitted:
			continue
		case recStepDone:
			if rec.StepID == stepID {
				continue
			}
		}
		kept = append(kept, line)
	}
	kept = append(kept, "")
	if err := os.WriteFile(filepath.Join(root, "journal.jsonl"), []byte(strings.Join(kept, "\n")), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = saveState(root, &State{PlanID: filepath.Base(root), Status: StatusRunning, Target: target})
	// Remove the committed target, the commit records were stripped.
	_ = os.RemoveAll(target)
}

func TestResumeAdoptsSideEffectViaProbe(t *testing.T) {
	target, stateRoot := newTestPaths(t)
	marker := filepath.Join(t.TempDir(), "marker.log")
	plan := crashPlan(marker, true)

	// First run completes; then forge a crash after the external side effect.
	runOK(t, newEngine(t, plan, target, stateRoot))
	root := stateDirOf(target, stateRoot, plan.ID)
	simulateCrashAfterSideEffect(t, root, "external", target)

	// Resume: probe confirms marker exists -> adopt, command must NOT re-run.
	en := newEngine(t, plan, target, stateRoot, func(o *Options) { o.Resume = true })
	res := runOK(t, en)
	if !res.Committed {
		t.Fatal("resume did not commit")
	}
	if got := readFile(t, marker); got != "ran\n" {
		t.Fatalf("adopted step was re-executed: marker = %q", got)
	}
	if got := readFile(t, filepath.Join(target, "after.txt")); got != "y\n" {
		t.Fatal("later step did not run after resume")
	}
	if !hasKind(en.records, recStepAdopted) {
		t.Fatal("journal missing step_adopted record")
	}
}

func TestResumeAmbiguousWithoutProbe(t *testing.T) {
	target, stateRoot := newTestPaths(t)
	marker := filepath.Join(t.TempDir(), "marker.log")
	plan := crashPlan(marker, false)

	runOK(t, newEngine(t, plan, target, stateRoot))
	root := stateDirOf(target, stateRoot, plan.ID)
	simulateCrashAfterSideEffect(t, root, "external", target)

	en := newEngine(t, plan, target, stateRoot, func(o *Options) { o.Resume = true })
	runExpectErr(t, en, ErrAmbiguous)
}

// ---- commit protocol crash positions --------------------------------------

func seedCommitState(t *testing.T, stateDir, target string) (layout, *Journal) {
	t.Helper()
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	planID := filepath.Base(stateDir)
	l := newLayout(filepath.Dir(stateDir), target, planID)
	if err := saveState(stateDir, &State{PlanID: planID, Status: StatusCommitting, Target: target}); err != nil {
		t.Fatal(err)
	}
	j, _, err := openJournal(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	return l, j
}

func TestCommitRecoveryAfterPublish(t *testing.T) {
	tmp := t.TempDir()
	target := filepath.Join(tmp, "target")
	stateDir := filepath.Join(tmp, "state", "commitrecoveryxx")

	l, j := seedCommitState(t, stateDir, target)
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "old.txt"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(l.staging, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(l.staging, "new.txt"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Reproduce crash position: both renames durable, backup not removed.
	if err := os.Rename(target, l.backup); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(l.staging, target); err != nil {
		t.Fatal(err)
	}
	if _, err := j.append(recTargetBackedUp, func(r *Record) { r.Path = l.backup }); err != nil {
		t.Fatal(err)
	}
	if _, err := j.append(recTargetPublished, func(r *Record) { r.Path = target }); err != nil {
		t.Fatal(err)
	}
	_ = j.close()

	outcome, err := Recover(stateDir, io.Discard)
	if err != nil || outcome != RecoveryCommitted {
		t.Fatalf("Recover = %v, %v", outcome, err)
	}
	if got := readFile(t, filepath.Join(target, "new.txt")); got != "new" {
		t.Fatal("published content missing")
	}
	if _, err := os.Stat(l.backup); !os.IsNotExist(err) {
		t.Fatal("backup not cleaned up")
	}
}

func TestCommitRollbackAfterBackupOnly(t *testing.T) {
	tmp := t.TempDir()
	target := filepath.Join(tmp, "target")
	stateDir := filepath.Join(tmp, "state", "rollbackyyyyyy")

	l, j := seedCommitState(t, stateDir, target)
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "old.txt"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(l.staging, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.Rename(target, l.backup); err != nil {
		t.Fatal(err)
	}
	if _, err := j.append(recTargetBackedUp, func(r *Record) { r.Path = l.backup }); err != nil {
		t.Fatal(err)
	}
	_ = j.close()

	outcome, err := Recover(stateDir, io.Discard)
	if err != nil || outcome != RecoveryRolledBack {
		t.Fatalf("Recover = %v, %v", outcome, err)
	}
	if got := readFile(t, filepath.Join(target, "old.txt")); got != "old" {
		t.Fatal("target not restored byte-for-byte")
	}
	if _, err := os.Stat(l.backup); !os.IsNotExist(err) {
		t.Fatal("backup left behind")
	}
}

// ---- network step + compensation over a real local server -----------------

func TestHTTPStepCompensation(t *testing.T) {
	var created, canceled int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/resources" && r.Method == http.MethodPost:
			atomic.AddInt32(&created, 1)
			w.WriteHeader(http.StatusCreated)
		case strings.HasPrefix(r.URL.Path, "/cancel") && r.Method == http.MethodPost:
			atomic.AddInt32(&canceled, 1)
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	target, stateRoot := newTestPaths(t)
	p := &Plan{
		Policy: Policy{AllowCommands: []string{"sh"}, AllowNetwork: []string{httpHostPort(srv.URL)}},
		Steps: []Step{
			{ID: "create", Type: StepHTTPRequest, HTTP: &HTTPSpec{
				Method: http.MethodPost, URL: srv.URL + "/resources", ExpectStatus: http.StatusCreated,
				Compensate: &HTTPSpec{Method: http.MethodPost, URL: srv.URL + "/cancel", ExpectStatus: http.StatusOK},
			}},
			{ID: "boom", Type: StepRunCommand, Run: failingCmd()},
		},
	}
	runExpectErr(t, newEngine(t, p, target, stateRoot), nil)
	if atomic.LoadInt32(&created) != 1 || atomic.LoadInt32(&canceled) != 1 {
		t.Fatalf("created=%d canceled=%d", created, canceled)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatal("target must not exist after aborted network plan")
	}
}

// ---- post-execution policy scan: symlink rejection -------------------------

func TestSymlinkInStagingRejected(t *testing.T) {
	tmp := t.TempDir()
	target := filepath.Join(tmp, "target")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(tmp, "outside.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(target, "link.txt")); err != nil {
		t.Fatal(err)
	}
	stateRoot := filepath.Join(tmp, "state")
	plan := &Plan{
		Policy: Policy{},
		Steps:  []Step{{ID: "f", Type: StepWriteFile, File: &FileSpec{Path: "f.txt", Content: "x\n"}}},
	}
	en := newEngine(t, plan, target, stateRoot)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := en.Run(ctx)
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("expected symlink policy violation, got %v", err)
	}
	// Target untouched.
	if _, err := os.Lstat(filepath.Join(target, "link.txt")); err != nil {
		t.Fatalf("target altered: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, "f.txt")); !os.IsNotExist(err) {
		t.Fatal("generated file leaked into target")
	}
}

// ---- misc ------------------------------------------------------------------

func TestJournalTornTailIgnored(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "journal.jsonl")
	good := []byte(`{"seq":1,"ts":"2026-01-01T00:00:00Z","kind":"plan_start"}` + "\n")
	torn := []byte(`{"seq":2,"ts":"2026-01-01T00:0`)
	if err := os.WriteFile(path, append(good, torn...), 0o600); err != nil {
		t.Fatal(err)
	}
	recs, err := readJournal(path)
	if err != nil || len(recs) != 1 || recs[0].Kind != recPlanStart {
		t.Fatalf("torn tail not handled: %v %v", recs, err)
	}
}

func TestBundleZipDeterministic(t *testing.T) {
	// Regression guard for fixed zip timestamps.
	var b1, b2 bytes.Buffer
	for _, b := range []*bytes.Buffer{&b1, &b2} {
		zw := zip.NewWriter(b)
		hdr := &zip.FileHeader{Name: "x", Method: zip.Deflate}
		hdr.Modified = time.Unix(0, 0).UTC()
		w, _ := zw.CreateHeader(hdr)
		_, _ = io.WriteString(w, "data")
		_ = zw.Close()
	}
	if !bytes.Equal(b1.Bytes(), b2.Bytes()) {
		t.Fatal("bundle encoding is not deterministic")
	}
}
