// Copyright 2023 Vic Shóstak and Create Go App Contributors. All rights reserved.
// Use of this source code is governed by Apache 2.0 license
// that can be found in the LICENSE file.

package genplan

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// RecKind enumerates journal record types.
type RecKind string

const (
	recPlanStart       RecKind = "plan_start"
	recStepStart       RecKind = "step_start"
	recStepDone        RecKind = "step_done"
	recStepAdopted     RecKind = "step_adopted"
	recStepSkipped     RecKind = "step_skipped"
	recStepCompensated RecKind = "step_compensated"
	recCompFailed      RecKind = "compensation_failed"
	recVerifyStart     RecKind = "verify_start"
	recVerifyDone      RecKind = "verify_done"
	recCommitStart     RecKind = "commit_start"
	recTargetBackedUp  RecKind = "target_backed_up"
	recTargetPublished RecKind = "target_published"
	recBackupRemoved   RecKind = "backup_removed"
	recCommitted       RecKind = "committed"
	recAborted         RecKind = "aborted"
)

// Record is one append-only journal entry. Only relevant fields are set.
type Record struct {
	Seq        int64     `json:"seq"`
	Time       time.Time `json:"ts"`
	Kind       RecKind   `json:"kind"`
	StepID     string    `json:"step_id,omitempty"`
	ExitCode   int       `json:"exit_code,omitempty"`
	Status     int       `json:"status,omitempty"`
	StdoutHash string    `json:"stdout_hash,omitempty"`
	StderrHash string    `json:"stderr_hash,omitempty"`
	BodyHash   string    `json:"body_hash,omitempty"`
	Manifest   string    `json:"manifest,omitempty"`
	Path       string    `json:"path,omitempty"`
	Reason     string    `json:"reason,omitempty"`
}

// Journal is a durable, append-only JSONL log.
type Journal struct {
	mu   sync.Mutex
	f    *os.File
	path string
	seq  int64
}

// openJournal opens (creating) the journal and replays existing records.
func openJournal(dir string) (*Journal, []Record, error) {
	path := filepath.Join(dir, "journal.jsonl")
	records, _ := readJournal(path) // a torn tail line is simply ignored

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) // #nosec G304 -- path is the engine-managed journal inside the state directory
	if err != nil {
		return nil, nil, err
	}
	j := &Journal{f: f, path: path, seq: int64(len(records))}
	return j, records, nil
}

// readJournal parses all complete JSON lines; a partial final line (crash
// during write) is ignored rather than treated as a committed record.
func readJournal(path string) ([]Record, error) {
	f, err := os.Open(path) // #nosec G304 -- path is the engine-managed journal inside the state directory
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer func() { _ = f.Close() }()

	var records []Record
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec Record
		if err := json.Unmarshal(line, &rec); err != nil {
			// Torn write at crash time: stop here, subsequent lines cannot exist.
			break
		}
		records = append(records, rec)
	}
	return records, nil
}

// append writes one record, fsyncs the file and returns the stored record.
func (j *Journal) append(kind RecKind, mutate func(*Record)) (Record, error) {
	j.mu.Lock()
	defer j.mu.Unlock()

	j.seq++
	rec := Record{Seq: j.seq, Time: time.Now().UTC(), Kind: kind}
	if mutate != nil {
		mutate(&rec)
	}
	line, err := json.Marshal(rec)
	if err != nil {
		j.seq--
		return Record{}, err
	}
	line = append(line, '\n')
	if _, err := j.f.Write(line); err != nil {
		return Record{}, err
	}
	if err := j.f.Sync(); err != nil {
		return Record{}, err
	}
	return rec, nil
}

func (j *Journal) close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.f.Close()
}

// hasKind reports whether any loaded record has the given kind.
func hasKind(records []Record, kind RecKind) bool {
	for i := range records {
		if records[i].Kind == kind {
			return true
		}
	}
	return false
}

// lastOfKind returns the last record of the kind and whether one exists.
func lastOfKind(records []Record, kind RecKind) (Record, bool) {
	for i := len(records) - 1; i >= 0; i-- {
		if records[i].Kind == kind {
			return records[i], true
		}
	}
	return Record{}, false
}

// stepState describes the journaled state of one step.
type stepState string

const (
	stepAbsent      stepState = "absent"
	stepStarted     stepState = "started"
	stepDone        stepState = "done"
	stepCompensated stepState = "compensated"
)

// stepStates summarizes step records keyed by step ID. Done followed by
// compensated is reported as compensated (compensation happens in reverse
// order after failure, so a later compensation wins).
func stepStates(records []Record) map[string]stepState {
	states := make(map[string]stepState)
	for i := range records {
		r := &records[i]
		if r.StepID == "" {
			continue
		}
		switch r.Kind {
		case recStepStart:
			if states[r.StepID] == "" {
				states[r.StepID] = stepStarted
			}
		case recStepDone, recStepAdopted:
			states[r.StepID] = stepDone
		case recStepCompensated:
			states[r.StepID] = stepCompensated
		}
	}
	return states
}

// saveState atomically writes state.json (tmp file + fsync + rename + fsync
// of the directory), so it survives a crash in any position.
func saveState(dir string, state *State) error {
	state.UpdatedAt = time.Now().UTC()
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, ".state.json.tmp")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := syncDir(tmp); err != nil {
		return err
	}
	final := filepath.Join(dir, "state.json")
	if err := os.Rename(tmp, final); err != nil {
		return fmt.Errorf("state rename: %w", err)
	}
	return syncDir(final)
}

// loadState reads state.json. A missing file returns (zero,false,nil).
func loadState(dir string) (State, bool, error) {
	data, err := os.ReadFile(filepath.Join(dir, "state.json")) // #nosec G304 -- fixed file name inside the engine state directory
	if err != nil {
		if os.IsNotExist(err) {
			return State{}, false, nil
		}
		return State{}, false, err
	}
	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return State{}, false, err
	}
	return state, true, nil
}

// syncDir fsyncs the directory containing path, required to persist renames.
func syncDir(path string) error {
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}
