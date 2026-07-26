package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/kennedyowusu/koolbase-cli/internal/api"
	"github.com/kennedyowusu/koolbase-cli/internal/config"
	"github.com/spf13/cobra"
)

const defaultManifest = "koolbase.json"

var snapshotCmd = &cobra.Command{
	Use:   "snapshot",
	Short: "Manage your project's backend definition as code",
	Long: `Treat your backend as code.

'koolbase snapshot pull' exports a project's structural definition — collections
and access rules, storage buckets, secret NAMES, and per-environment flags /
config / version policy — into a single versioned file (koolbase.json) that you
commit to git.

'koolbase snapshot apply' reconciles a target project to that file, idempotently.
Use --dry-run to preview the diff, and run it in CI to keep projects in sync from
one reviewed source of truth.

Secret VALUES, OAuth/SMS credentials, and records are never included. Apply
reports which secrets the target still needs so you can set them yourself.`,
}

var (
	snapshotProject       string
	snapshotOutput        string
	snapshotFile          string
	snapshotDryRun        bool
	snapshotPrune         bool
	snapshotForce         bool
	snapshotVerbose       bool
	snapshotIgnorePartial bool
)

var snapshotPullCmd = &cobra.Command{
	Use:   "pull",
	Short: "Export a project's backend definition to a file you commit (default: koolbase.json)",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		client := api.NewClient(cfg.BaseURL, cfg.APIKey)

		data, err := client.SnapshotPull(snapshotProject)
		if err != nil {
			return err
		}

		// pretty-print so committed definitions diff cleanly in version control
		var pretty bytes.Buffer
		if json.Indent(&pretty, data, "", "  ") == nil {
			data = pretty.Bytes()
		}

		// "-" means stdout; empty means the default manifest file
		if snapshotOutput == "-" {
			fmt.Println(string(data))
			return nil
		}
		out := snapshotOutput
		if out == "" {
			out = defaultManifest
		}
		if err := os.WriteFile(out, data, 0600); err != nil {
			return fmt.Errorf("failed to write %s: %w", out, err)
		}
		fmt.Printf("Backend definition written to %s\n", out)
		return nil
	},
}

type applyError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type itemResult struct {
	Name    string                 `json:"name"`
	Action  string                 `json:"action"`
	Status  string                 `json:"status"`
	Enabled *bool                  `json:"enabled,omitempty"`
	Details map[string]interface{} `json:"details,omitempty"`
	Error   *applyError            `json:"error,omitempty"`
}

type envItemResult struct {
	Slug            string       `json:"slug"`
	Name            string       `json:"name"`
	Matched         bool         `json:"matched"`
	Flags           []itemResult `json:"flags"`
	Configs         []itemResult `json:"configs"`
	VersionPolicies []itemResult `json:"version_policies"`
}

type pruneCandidate struct {
	Name        string `json:"name"`
	ManagedBy   string `json:"managed_by"`
	RecordCount int    `json:"record_count"`
}

type applyResult struct {
	TargetProjectID string `json:"target_project_id"`
	SnapshotVersion int    `json:"snapshot_version"`
	DryRun          bool   `json:"dry_run"`
	Status          string `json:"status"`

	Collections  []itemResult    `json:"collections"`
	Buckets      []itemResult    `json:"buckets"`
	Secrets      []itemResult    `json:"secrets"`
	Functions    []itemResult    `json:"functions"`
	Crons        []itemResult    `json:"crons"`
	Triggers     []itemResult    `json:"triggers"`
	Environments []envItemResult `json:"environments"`

	SecretsNeedingValues []string `json:"secrets_needing_values"`
	SkippedEnvironments  []string `json:"skipped_environments"`

	Prune *struct {
		Pruned  []pruneCandidate `json:"pruned"`
		Blocked []pruneCandidate `json:"blocked"`
		Kept    []pruneCandidate `json:"kept"`
	} `json:"prune"`
}

// summarize renders "3 created · 1 conflict" for a section, omitting zeroes.
//
// Items that need attention are counted by STATUS, not action: every one of them
// carries action "skipped", so counting by action would report "3 skipped" for a
// mix of conflicts and pending values and tell the operator nothing. The summary
// line alone should say whether a section needs looking at.
func summarize(items []itemResult) string {
	order := []string{"created", "updated", "unchanged", "conflict", "pending", "failed", "skipped"}
	counts := map[string]int{}
	for _, i := range items {
		switch i.Status {
		case "conflict", "pending", "failed":
			counts[i.Status]++
		default:
			counts[i.Action]++
		}
	}
	parts := []string{}
	for _, a := range order {
		if counts[a] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", counts[a], a))
		}
	}
	if len(parts) == 0 {
		return "nothing to do"
	}
	return strings.Join(parts, " · ")
}

// printSection prints a section's summary, then every item that needs a human:
// failures, conflicts, and pending values. Successful items are counted, not
// listed — a 30-function apply must not bury its one failure in a wall of
// output. --verbose lists everything.
func printSection(label string, items []itemResult, verbose bool) {
	if len(items) == 0 {
		return
	}
	fmt.Printf("%s: %s\n", label, summarize(items))
	for _, i := range items {
		switch {
		case i.Status == "failed":
			fmt.Printf("  ✗ %s — %s\n", i.Name, errText(i))
		case i.Status == "conflict":
			fmt.Printf("  ! %s — %s\n", i.Name, errText(i))
			if d, ok := i.Details["immutable_mismatch"].([]interface{}); ok {
				for _, m := range d {
					fmt.Printf("      %v\n", m)
				}
			}
		case i.Status == "pending":
			fmt.Printf("  · %s — awaiting a value\n", i.Name)
		case verbose:
			fmt.Printf("  %s %s\n", actionGlyph(i.Action), i.Name)
		}
	}
}

func actionGlyph(action string) string {
	switch action {
	case "created":
		return "+"
	case "updated":
		return "~"
	case "unchanged":
		return "="
	default:
		return "·"
	}
}

func errText(i itemResult) string {
	if i.Error != nil && i.Error.Message != "" {
		return i.Error.Message
	}
	return i.Status
}

var snapshotApplyCmd = &cobra.Command{
	Use:   "apply",
	Short: "Reconcile a target project to your backend definition (use --dry-run to preview)",
	// A partial apply exits non-zero; printing usage on top of that buries the
	// result the operator needs to read.
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load()
		if err != nil {
			return err
		}

		raw, err := os.ReadFile(snapshotFile)
		if err != nil {
			return fmt.Errorf("failed to read %s: %w", snapshotFile, err)
		}

		client := api.NewClient(cfg.BaseURL, cfg.APIKey)
		data, err := client.SnapshotApply(snapshotProject, json.RawMessage(raw), snapshotDryRun, snapshotPrune, snapshotForce)
		if err != nil {
			return err
		}

		var res applyResult
		if err := json.Unmarshal(data, &res); err != nil {
			return fmt.Errorf("could not read the server's response: %w", err)
		}
		// Go zero-fills absent fields, so a shape mismatch parses cleanly and
		// prints an empty apply rather than failing. An absent status means the
		// server predates the typed result.
		if res.Status == "" {
			return fmt.Errorf("the server returned an unrecognised response shape; this CLI and the server are out of step")
		}

		mode := "Applied to"
		if res.DryRun {
			mode = "Dry run against"
		}
		fmt.Printf("%s project %s (snapshot v%d)\n\n", mode, res.TargetProjectID, res.SnapshotVersion)

		printSection("Collections", res.Collections, snapshotVerbose)
		printSection("Buckets", res.Buckets, snapshotVerbose)
		printSection("Secrets", res.Secrets, snapshotVerbose)
		printSection("Functions", res.Functions, snapshotVerbose)
		printSection("Crons", res.Crons, snapshotVerbose)
		printSection("Triggers", res.Triggers, snapshotVerbose)

		for _, env := range res.Environments {
			if !env.Matched {
				fmt.Printf("Environment %s: no matching environment in the target — skipped\n", env.Slug)
				continue
			}
			if len(env.Flags)+len(env.Configs)+len(env.VersionPolicies) == 0 {
				continue
			}
			fmt.Printf("Environment %s:\n", env.Slug)
			printSection("  Flags", env.Flags, snapshotVerbose)
			printSection("  Configs", env.Configs, snapshotVerbose)
			printSection("  Version policies", env.VersionPolicies, snapshotVerbose)
		}
		if len(res.SkippedEnvironments) > 0 {
			fmt.Println("  → create matching environments (by slug) in the target to promote their config.")
		}

		if res.Prune != nil {
			fmt.Println("\nPrune:")
			verb := "deleted"
			if res.DryRun {
				verb = "will delete"
			}
			for _, p := range res.Prune.Pruned {
				fmt.Printf("  - %s — %s (%d records)\n", p.Name, verb, p.RecordCount)
			}
			for _, p := range res.Prune.Blocked {
				fmt.Printf("  ! %s — BLOCKED: holds %d records (re-run with --force to delete)\n", p.Name, p.RecordCount)
			}
			for _, p := range res.Prune.Kept {
				fmt.Printf("  · %s — kept (dashboard-owned, not manifest-managed)\n", p.Name)
			}
		}

		if len(res.SecretsNeedingValues) > 0 {
			fmt.Printf("\nSecrets needing values (%d) — the clone is ready except these:\n", len(res.SecretsNeedingValues))
			for _, n := range res.SecretsNeedingValues {
				fmt.Printf("  koolbase secrets set %s --project %s\n", n, res.TargetProjectID)
			}
		}

		fmt.Printf("\nStatus: %s\n", res.Status)

		// Non-zero exit on anything short of success: a CI job that treats a
		// partial apply as a pass is worse than no CI job at all.
		switch res.Status {
		case "failed":
			return fmt.Errorf("apply failed")
		case "partial":
			if snapshotIgnorePartial {
				return nil
			}
			return fmt.Errorf("apply completed with failures or conflicts (pass --ignore-partial to exit 0 anyway)")
		}
		return nil
	},
}

func init() {
	snapshotPullCmd.Flags().StringVar(&snapshotProject, "project", "", "project ID to export from (required)")
	snapshotPullCmd.Flags().StringVarP(&snapshotOutput, "output", "o", "", "output file (default: koolbase.json; use '-' for stdout)")
	snapshotPullCmd.MarkFlagRequired("project")

	snapshotApplyCmd.Flags().StringVar(&snapshotProject, "project", "", "target project ID to apply to (required)")
	snapshotApplyCmd.Flags().StringVarP(&snapshotFile, "file", "f", defaultManifest, "backend definition file to apply (default: koolbase.json)")
	snapshotApplyCmd.Flags().BoolVar(&snapshotDryRun, "dry-run", false, "preview the diff without writing")
	snapshotApplyCmd.Flags().BoolVarP(&snapshotVerbose, "verbose", "v", false, "list every item, not just those needing attention")
	snapshotApplyCmd.Flags().BoolVar(&snapshotIgnorePartial, "ignore-partial", false, "exit 0 even when some items failed or conflicted")
	snapshotApplyCmd.Flags().BoolVar(&snapshotPrune, "prune", false, "delete manifest-owned collections that are absent from the file")
	snapshotApplyCmd.Flags().BoolVar(&snapshotForce, "force", false, "allow prune to delete collections that still hold records")
	snapshotApplyCmd.MarkFlagRequired("project")

	snapshotCmd.AddCommand(snapshotPullCmd, snapshotApplyCmd)
}
