// Copyright 2023 Vic Shóstak and Create Go App Contributors. All rights reserved.
// Use of this source code is governed by Apache 2.0 license
// that can be found in the LICENSE file.

package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"

	"github.com/AlecAivazis/survey/v2"
	"github.com/spf13/cobra"

	"github.com/create-go-app/cli/v4/pkg/cgapp"
	"github.com/create-go-app/cli/v4/pkg/genplan"
)

var (
	genTarget    string
	genStateRoot string
	genPlanFile  string
	genDemoURL   string
	genResume    bool
	genForce     bool
	genAssumeYes bool
	genDryRun    bool
)

func init() {
	rootCmd.AddCommand(genCmd)

	genCmd.PersistentFlags().StringVar(&genTarget, "target", "", "directory to publish (created if absent; same file system as --state-root)")
	genCmd.PersistentFlags().StringVar(&genStateRoot, "state-root", "", "journal/staging state root (default: <target-parent>/.cgapp-gen)")
	genCmd.PersistentFlags().BoolVar(&genResume, "resume", false, "resume an interrupted run from its journal without repeating side effects")
	genCmd.PersistentFlags().BoolVar(&genForce, "force", false, "discard an existing committed state and run again")
	genCmd.PersistentFlags().BoolVarP(&genAssumeYes, "yes", "y", false, "skip the confirmation prompt (required in non-interactive shells)")
	genCmd.PersistentFlags().BoolVar(&genDryRun, "dry-run", false, "print file/command/network preview and exit without executing")

	genRunCmd.Flags().StringVar(&genPlanFile, "plan", "", "path to the typed generation plan (JSON)")
	_ = genRunCmd.MarkFlagRequired("plan")
	_ = genRunCmd.MarkFlagRequired("target")

	_ = genDemoCmd.MarkFlagRequired("target")
	genDemoCmd.Flags().StringVar(&genDemoURL, "demo-url", "", "optional URL for the demo HTTP step")

	genRecoverCmd.Flags().StringVar(&genStateRoot, "state-dir", "", "plan state directory (<state-root>/<plan-id>)")
	_ = genRecoverCmd.MarkFlagRequired("state-dir")
	genStatusCmd.Flags().StringVar(&genStateRoot, "state-dir", "", "plan state directory (<state-root>/<plan-id>)")
	_ = genStatusCmd.MarkFlagRequired("state-dir")

	genCmd.AddCommand(genRunCmd, genDemoCmd, genRecoverCmd, genStatusCmd)
}

// genCmd groups transactional generation commands.
var genCmd = &cobra.Command{
	Use:   "gen",
	Short: "Transactionally execute a typed generation plan",
	Long: `
Compile a typed generation plan, execute it in an isolated staging directory
on the same file system as the target, journal every side effect, verify the
build and policy, then publish with an atomic rename. Failed or killed runs
leave the target untouched or deterministically recoverable (--resume).`,
}

var genRunCmd = &cobra.Command{
	Use:   "run",
	Short: "Run a generation plan from a JSON file",
	RunE:  runGenPlan(false),
}

var genDemoCmd = &cobra.Command{
	Use:   "demo",
	Short: "Run the built-in demo generation plan",
	RunE:  runGenPlan(true),
}

var genRecoverCmd = &cobra.Command{
	Use:   "recover",
	Short: "Recover (finish or roll back) an interrupted commit",
	RunE: func(cmd *cobra.Command, args []string) error {
		outcome, err := genplan.Recover(genStateRoot, cgapp.Stdout)
		if err != nil {
			return cgapp.ShowError(err.Error())
		}
		switch outcome {
		case genplan.RecoveryCommitted:
			cgapp.ShowMessage("success", "interrupted commit finished; target published", true, true)
		case genplan.RecoveryRolledBack:
			cgapp.ShowMessage("info", "interrupted commit rolled back; target restored to its original state", true, true)
		default:
			cgapp.ShowMessage("info", "no interrupted commit found; target unchanged (use 'gen run --resume' for step-level recovery)", true, true)
		}
		return nil
	},
}

var genStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show the persisted state and journal summary",
	RunE: func(cmd *cobra.Command, args []string) error {
		state, records, err := genplan.Inspect(genStateRoot)
		if err != nil {
			return cgapp.ShowError(err.Error())
		}
		fmt.Fprintf(cgapp.Stdout, "plan:     %s\n", state.PlanID)
		fmt.Fprintf(cgapp.Stdout, "status:   %s\n", state.Status)
		fmt.Fprintf(cgapp.Stdout, "target:   %s\n", state.Target)
		fmt.Fprintf(cgapp.Stdout, "step:     %s\n", state.CurrentStep)
		fmt.Fprintf(cgapp.Stdout, "updated:  %s\n", state.UpdatedAt.Format("2006-01-02 15:04:05"))
		if state.Error != "" {
			fmt.Fprintf(cgapp.Stdout, "error:    %s\n", state.Error)
		}
		fmt.Fprintf(cgapp.Stdout, "journal:  %d record(s)\n", len(records))
		tail := records
		if len(tail) > 8 {
			tail = tail[len(tail)-8:]
		}
		for _, r := range tail {
			note := r.StepID
			if note == "" {
				note = r.Reason
			}
			fmt.Fprintf(cgapp.Stdout, "  #%-3d %-22s %s\n", r.Seq, r.Kind, note)
		}
		return nil
	},
}

// runGenPlan returns the cobra runner shared by `run` and `demo`.
func runGenPlan(isDemo bool) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		plan, err := loadPlan(isDemo)
		if err != nil {
			return cgapp.ShowError(err.Error())
		}

		engine, err := genplan.New(plan, genplan.Options{
			Target:    genTarget,
			StateRoot: genStateRoot,
			Resume:    genResume,
			Force:     genForce,
			Log:       cgapp.Stdout,
		})
		if err != nil {
			return cgapp.ShowError(err.Error())
		}

		preview, err := engine.Preview()
		if err != nil {
			return cgapp.ShowError(err.Error())
		}
		renderPreview(preview)

		if genDryRun {
			cgapp.ShowMessage("info", "dry-run: nothing was executed and the target was not touched", true, true)
			return nil
		}

		if !genResume && !genAssumeYes {
			if err := confirmExecution(); err != nil {
				if errors.Is(err, errAbortedByUser) {
					return nil
				}
				return err
			}
		}

		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		defer stop()

		result, err := engine.Run(ctx)
		switch {
		case errors.Is(err, genplan.ErrAlreadyCommitted):
			cgapp.ShowMessage("info", "this plan is already committed for the target; the journal prevented any duplicate side effect", true, true)
			return nil
		case errors.Is(err, genplan.ErrResumeRequired):
			cgapp.ShowMessage("error", err.Error(), true, false)
			cgapp.ShowMessage("", "rerun with --resume to continue, or inspect with `cgapp gen status`", false, true)
			return err
		case err != nil:
			return cgapp.ShowError(err.Error())
		}

		if result.RecoveredCommit {
			cgapp.ShowMessage("success", "recovered interrupted commit and published the target", true, true)
		} else {
			cgapp.ShowMessage("success", fmt.Sprintf("committed %d file change(s) to %s", len(result.Changes), genTarget), true, true)
		}
		return nil
	}
}

// loadPlan reads the JSON plan file or builds the demo plan.
func loadPlan(isDemo bool) (*genplan.Plan, error) {
	if isDemo {
		return genplan.DemoPlan(genDemoURL), nil
	}
	data, err := os.ReadFile(genPlanFile) // #nosec G304 -- the plan file path is an explicit user-supplied CLI argument
	if err != nil {
		return nil, fmt.Errorf("read plan: %w", err)
	}
	var plan genplan.Plan
	if err := json.Unmarshal(data, &plan); err != nil {
		return nil, fmt.Errorf("parse plan: %w", err)
	}
	return &plan, nil
}

// confirmExecution asks for confirmation; non-interactive shells must pass
// --yes to avoid accidentally generating projects.
func confirmExecution() error {
	if !isInteractive() {
		return cgapp.ShowError("non-interactive shell: re-run with --yes (or --dry-run) to proceed")
	}
	var agreed bool
	prompt := &survey.Confirm{
		Message: "Execute the plan and publish to the target directory?",
		Default: false,
	}
	if err := survey.AskOne(prompt, &agreed, survey.WithIcons(surveyIconsConfig)); err != nil {
		return cgapp.ShowError(err.Error())
	}
	if !agreed {
		cgapp.ShowMessage("", "Aborted by user; nothing was executed.", true, true)
		return errAbortedByUser
	}
	return nil
}

// isInteractive reports whether stdin is a terminal.
func isInteractive() bool {
	info, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// errAbortedByUser is a sentinel returned when the confirmation is declined.
var errAbortedByUser = errors.New("aborted by user")

// renderPreview prints the complete file/command/network preview.
func renderPreview(p *genplan.Preview) {
	cgapp.ShowMessage("", fmt.Sprintf("Plan %s", p.PlanID[:12]), true, false)
	fmt.Fprintf(cgapp.Stdout, "  target:  %s\n", p.Target)
	fmt.Fprintf(cgapp.Stdout, "  staging: %s\n", p.Staging)

	fmt.Fprintf(cgapp.Stdout, "\nfiles (%d):\n", len(p.Files))
	for _, c := range p.Files {
		fmt.Fprintf(cgapp.Stdout, "  %-9s %s  (%s)\n", c.Kind, c.Path, shortHash(c.NewHash))
	}
	if len(p.Files) == 0 {
		fmt.Fprintln(cgapp.Stdout, "  (none)")
	}

	fmt.Fprintf(cgapp.Stdout, "\nexternal actions (%d):\n", len(p.External))
	for _, fx := range p.External {
		switch fx.Kind {
		case "command":
			flags := []string{}
			if fx.Idempotent {
				flags = append(flags, "idempotent")
			}
			if fx.Compensable {
				flags = append(flags, "compensable")
			}
			fmt.Fprintf(cgapp.Stdout, "  $ %s %s  [%s]\n", fx.Bin, fx.Args, strings.Join(flags, ","))
		case "http":
			fmt.Fprintf(cgapp.Stdout, "  %s %s  [network policy: allowed]\n", fx.Method, fx.URL)
		}
	}
	if len(p.External) == 0 {
		fmt.Fprintln(cgapp.Stdout, "  (none)")
	}

	fmt.Fprintf(cgapp.Stdout, "\nverification gates (%d):\n", len(p.Verify))
	for _, fx := range p.Verify {
		fmt.Fprintf(cgapp.Stdout, "  $ %s %s\n", fx.Bin, fx.Args)
	}
	if len(p.Verify) == 0 {
		fmt.Fprintln(cgapp.Stdout, "  (none)")
	}
	fmt.Fprintln(cgapp.Stdout)
}

// shortHash trims a digest for compact display.
func shortHash(h string) string {
	if len(h) <= 12 {
		return h
	}
	return h[:12]
}
