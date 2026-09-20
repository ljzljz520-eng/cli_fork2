// Copyright 2023 Vic Shóstak and Create Go App Contributors. All rights reserved.
// Use of this source code is governed by Apache 2.0 license
// that can be found in the LICENSE file.

package genplan

import (
	"context"
	"fmt"
)

// runVerify executes every build verification gate inside staging. Gates are
// local, deterministic checks and are always re-run on resume.
func (en *Engine) runVerify(ctx context.Context) error {
	if len(en.plan.Verify) == 0 {
		return nil
	}
	if _, err := en.rec(recVerifyStart, nil); err != nil {
		return err
	}
	for i := range en.plan.Verify {
		gate := &en.plan.Verify[i]
		en.currentLogPrefix = fmt.Sprintf("verify-%02d", i+1)
		en.logf("verify: $ %s %v", gate.Bin, gate.Args)
		if _, err := en.runCommand(ctx, gate); err != nil {
			return fmt.Errorf("build verification failed: %w", err)
		}
	}
	_, err := en.rec(recVerifyDone, nil)
	return err
}
