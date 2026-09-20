// Copyright 2023 Vic Shóstak and Create Go App Contributors. All rights reserved.
// Use of this source code is governed by Apache 2.0 license
// that can be found in the LICENSE file.

package genplan

import (
	"fmt"
	"os"
	"path/filepath"
)

// commit publishes staging to target with a journaled, crash-safe protocol:
//
//  1. target (if present) -> backup   [record: target_backed_up]
//  2. staging            -> target   [record: target_published]
//  3. remove backup                  [record: backup_removed]
//  4. mark committed                 [record: committed]
//
// A crash at any position is resolved deterministically by recoverCommit:
//   - after (2): the publish succeeded -> finish cleanup;
//   - after (1) only: restore the backup so the target is byte-identical to
//     its pre-run state;
//   - before (1): the target was never touched.
func (en *Engine) commit() error {
	if err := saveState(
		en.layout.root,
		&State{PlanID: en.plan.ID, Status: StatusCommitting, Target: en.layout.target, Staging: en.layout.staging, Backup: en.layout.backup},
	); err != nil {
		return err
	}
	if _, err := en.rec(recCommitStart, nil); err != nil {
		return err
	}

	targetExists := true
	if _, err := os.Lstat(en.layout.target); err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		targetExists = false
	}

	if targetExists {
		if err := os.Rename(en.layout.target, en.layout.backup); err != nil {
			return fmt.Errorf("backup target: %w", err)
		}
		if err := syncDir(en.layout.backup); err != nil {
			return err
		}
		if _, err := en.rec(recTargetBackedUp, func(r *Record) { r.Path = en.layout.backup }); err != nil {
			return err
		}
	}

	if err := os.Rename(en.layout.staging, en.layout.target); err != nil {
		return fmt.Errorf("publish staging: %w", err)
	}
	if err := syncDir(en.layout.target); err != nil {
		return err
	}
	if _, err := en.rec(recTargetPublished, func(r *Record) { r.Path = en.layout.target }); err != nil {
		return err
	}

	if targetExists {
		if err := os.RemoveAll(en.layout.backup); err != nil {
			return err
		}
		if err := syncDir(en.layout.backup); err != nil {
			return err
		}
	}
	if _, err := en.rec(recBackupRemoved, nil); err != nil {
		return err
	}

	if err := saveState(en.layout.root, &State{
		PlanID: en.plan.ID, Status: StatusCommitted, Target: en.layout.target,
	}); err != nil {
		return err
	}
	_, err := en.rec(recCommitted, nil)
	return err
}

// commitRecovery resolves an interrupted commit using only the journal and the
// on-disk positions of target/backup/staging. The second return value reports
// whether a commit-phase record existed (so callers can distinguish a rollback
// from a pre-commit abort).
func commitRecovery(l *layout, records []Record, j *Journal) (committed, commitWasStarted bool, err error) {
	_, published := lastOfKind(records, recTargetPublished)
	_, backedUp := lastOfKind(records, recTargetBackedUp)
	commitWasStarted = hasKind(records, recCommitStart) || published || backedUp || hasKind(records, recCommitted)

	switch {
	case hasKind(records, recCommitted):
		// Fully done; just ensure leftovers disappear.
		_ = os.RemoveAll(l.backup)
		_ = os.RemoveAll(l.staging)
		return true, commitWasStarted, nil

	case published:
		// Publish happened: the new content is authoritative. Drop the backup.
		if err := os.RemoveAll(l.backup); err != nil {
			return false, commitWasStarted, err
		}
		if err := syncDir(l.backup); err != nil {
			return false, commitWasStarted, err
		}
		if _, err := j.append(recBackupRemoved, nil); err != nil {
			return false, commitWasStarted, err
		}
		if err := saveState(l.root, &State{PlanID: rootPlanID(l.root), Status: StatusCommitted, Target: l.target}); err != nil {
			return false, commitWasStarted, err
		}
		if _, err := j.append(recCommitted, nil); err != nil {
			return false, commitWasStarted, err
		}
		_ = os.RemoveAll(l.staging)
		return true, commitWasStarted, nil

	case backedUp:
		// Publish did not happen: restore the original target byte-for-byte.
		if err := os.RemoveAll(l.staging); err != nil {
			return false, commitWasStarted, err
		}
		if _, err := os.Lstat(l.target); os.IsNotExist(err) {
			if err := os.Rename(l.backup, l.target); err != nil {
				return false, commitWasStarted, fmt.Errorf("restore target: %w", err)
			}
			if err := syncDir(l.target); err != nil {
				return false, commitWasStarted, err
			}
		}
		if err := saveState(l.root, &State{
			PlanID: rootPlanID(l.root), Status: StatusAborted, Target: l.target,
			Error: "interrupted during commit; original target restored",
		}); err != nil {
			return false, commitWasStarted, err
		}
		return false, commitWasStarted, nil

	default:
		// Commit never touched the target; nothing to restore.
		_ = os.RemoveAll(l.staging)
		return false, commitWasStarted, nil
	}
}

// rootPlanID extracts the plan ID from a state root path .../<stateRoot>/<id>.
func rootPlanID(root string) string {
	return filepath.Base(root)
}
