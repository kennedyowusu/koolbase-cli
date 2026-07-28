package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kennedyowusu/koolbase-cli/internal/api"
	"github.com/kennedyowusu/koolbase-cli/internal/config"
	"github.com/spf13/cobra"
)

const defaultSeedDir = "koolbase/seeds"

var seedCmd = &cobra.Command{
	Use:   "seed",
	Short: "Manage reference data as code",
	Long: `Reference data is the fixed rows your application's logic depends on to
work at all — category enums, country lists, plan definitions.

A snapshot carries no records, which is right for user data and leaves a cloned
project functionally dead when its functions validate against an empty lookup
table. Seed files close that gap, and stay a separate artifact from the snapshot
deliberately: a snapshot describes a project, a seed file declares intended data,
and neither can quietly become the other.

'koolbase seed validate' checks your files without contacting the server.
'koolbase seed plan' shows what would change in a project.
'koolbase seed apply' makes it so.

Rows are identified by a key you declare, which must correspond to a real unique
constraint on the collection — that is what makes re-applying safe rather than
duplicating.`,
}

var (
	seedProject        string
	seedDir            string
	seedCollectionName string
	seedOnConflict     string
	seedForceConflicts bool
	seedAdoptExisting  bool
	seedVerbose        bool
)

// loadSeedSet reads the manifest and every file it lists.
//
// Read together rather than lazily so a manifest pointing at a missing file
// fails before anything is planned, rather than halfway through a project.
func loadSeedSet(dir string) (api.SeedManifest, map[string]api.SeedFile, error) {
	manifestPath := filepath.Join(dir, "manifest.json")
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		return api.SeedManifest{}, nil, fmt.Errorf("read %s: %w", manifestPath, err)
	}
	var m api.SeedManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return api.SeedManifest{}, nil, fmt.Errorf("%s is not valid JSON: %w", manifestPath, err)
	}

	files := map[string]api.SeedFile{}
	for _, c := range m.Collections {
		path := filepath.Join(dir, c.File)
		b, err := os.ReadFile(path)
		if err != nil {
			return m, nil, fmt.Errorf("%s: %w", c.Name, err)
		}
		var f api.SeedFile
		if err := json.Unmarshal(b, &f); err != nil {
			return m, nil, fmt.Errorf("%s is not valid JSON: %w", path, err)
		}
		files[c.Name] = f
	}
	return m, files, nil
}

// selected filters to one collection when --collection is given, so a large seed
// set can be applied a piece at a time.
func selected(m api.SeedManifest) []api.SeedCollection {
	if seedCollectionName == "" {
		return m.Collections
	}
	for _, c := range m.Collections {
		if c.Name == seedCollectionName {
			return []api.SeedCollection{c}
		}
	}
	return nil
}

var seedValidateCmd = &cobra.Command{
	Use:   "validate",
	Short: "Check seed files without contacting the server",
	Long: `Structural checks only: that the manifest and files agree, that every row
carries its declared key, and that no two rows share one.

Whether the declared key corresponds to a real unique constraint is checked by
'plan', because that is a fact about the target project rather than the file.`,
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		m, files, err := loadSeedSet(seedDir)
		if err != nil {
			return err
		}
		problems := 0
		for _, c := range m.Collections {
			f := files[c.Name]
			for _, p := range validateSeedLocally(c, f) {
				fmt.Printf("  ✗ %s: %s\n", c.Name, p)
				problems++
			}
			if problems == 0 {
				fmt.Printf("  ✓ %s — %d row(s), keyed by %s\n", c.Name, len(f.Rows), strings.Join(c.Key, ", "))
			}
		}
		if problems > 0 {
			return fmt.Errorf("%d problem(s) found", problems)
		}
		fmt.Printf("\n%d collection(s) valid.\n", len(m.Collections))
		return nil
	},
}

var seedPlanCmd = &cobra.Command{
	Use:   "plan",
	Short: "Show what would change in a project, without writing",
	Long: `Classifies every row against the target and the seed ledger, which records
what was last applied. That is what lets a plan distinguish a row you changed in
the file from one someone changed in the project — and say which.`,
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runSeed(true)
	},
}

var seedApplyCmd = &cobra.Command{
	Use:   "apply",
	Short: "Install reference data into a project",
	Long: `Applies the plan. Rows that changed in both the file and the target are
refused by default: guessing which is right is worse than stopping.

  --on-conflict=keep-existing   the project's version wins
  --on-conflict=overwrite       the file's version wins
  --force-conflicts             required to overwrite rows that changed in the
                                project as well; overwriting those discards a
                                deliberate edit, permanently
  --adopt-existing              bring rows that already exist under a seeded key
                                under seed management`,
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runSeed(false)
	},
}

func runSeed(dryRun bool) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	m, files, err := loadSeedSet(seedDir)
	if err != nil {
		return err
	}
	cols := selected(m)
	if len(cols) == 0 {
		return fmt.Errorf("no collection named %q in %s/manifest.json", seedCollectionName, seedDir)
	}

	client := api.NewClient(cfg.BaseURL, cfg.APIKey)
	verb := "Applying"
	if dryRun {
		verb = "Planning"
	}
	fmt.Printf("%s %d collection(s) into project %s\n\n", verb, len(cols), seedProject)

	var failed int
	for _, c := range cols {
		res, err := client.SeedApply(seedProject, api.SeedApplyRequest{
			Collection:     c,
			File:           files[c.Name],
			OnConflict:     seedOnConflict,
			ForceConflicts: seedForceConflicts,
			AdoptExisting:  seedAdoptExisting,
		}, dryRun)
		if err != nil {
			fmt.Printf("%s: %v\n\n", c.Name, err)
			failed++
			continue
		}
		printSeedResult(c, res, dryRun)
		if res.Status != "ok" {
			failed++
		}
	}
	if failed > 0 {
		if dryRun {
			return fmt.Errorf("%d collection(s) need a decision before they can be applied", failed)
		}
		return fmt.Errorf("%d collection(s) did not fully apply", failed)
	}
	return nil
}

// printSeedResult leads with the shape of the change and then lists only what a
// human must act on. A two hundred row country list should not bury its one
// conflict.
func printSeedResult(c api.SeedCollection, res api.SeedApplyResult, dryRun bool) {
	counts := map[string]int{}
	for _, p := range res.Plan {
		counts[p.Action]++
	}
	parts := []string{}
	for _, a := range []string{"create", "recreate", "update", "unchanged", "drifted", "conflict", "adopt", "invalid"} {
		if counts[a] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", counts[a], a))
		}
	}
	fmt.Printf("%s: %s\n", c.Name, strings.Join(parts, " · "))

	for _, p := range res.Plan {
		needs := p.Action == "conflict" || p.Action == "drifted" || p.Action == "adopt" || p.Action == "invalid"
		if !needs && !seedVerbose {
			continue
		}
		glyph := map[string]string{
			"create": "+", "recreate": "+", "update": "~", "unchanged": "=",
			"conflict": "!", "drifted": "!", "adopt": "!", "invalid": "✗",
		}[p.Action]
		fmt.Printf("  %s %s\n", glyph, strings.Join(p.KeyValue, " / "))
		if p.Detail != "" {
			fmt.Printf("      %s\n", p.Detail)
		}
		for _, d := range p.Divergences {
			fmt.Printf("      %s: file=%v  target=%v\n", d.Field, d.InFile, d.InTarget)
		}
	}
	if dryRun && counts["conflict"] > 0 {
		fmt.Printf("  → decide with --on-conflict=keep-existing or --on-conflict=overwrite --force-conflicts\n")
	}
	if dryRun && counts["adopt"] > 0 {
		fmt.Printf("  → these already exist in the target; --adopt-existing brings them under seed management\n")
	}
	fmt.Println()
}

func init() {
	for _, c := range []*cobra.Command{seedPlanCmd, seedApplyCmd} {
		c.Flags().StringVar(&seedProject, "project", "", "target project ID (required)")
		c.Flags().StringVar(&seedDir, "dir", defaultSeedDir, "directory holding manifest.json")
		c.Flags().StringVar(&seedCollectionName, "collection", "", "apply only this collection")
		c.Flags().BoolVarP(&seedVerbose, "verbose", "v", false, "list every row, not just those needing attention")
		c.MarkFlagRequired("project")
	}
	seedValidateCmd.Flags().StringVar(&seedDir, "dir", defaultSeedDir, "directory holding manifest.json")

	seedApplyCmd.Flags().StringVar(&seedOnConflict, "on-conflict", "fail", "fail, keep-existing, or overwrite")
	seedApplyCmd.Flags().BoolVar(&seedForceConflicts, "force-conflicts", false, "overwrite rows that changed in the target too")
	seedApplyCmd.Flags().BoolVar(&seedAdoptExisting, "adopt-existing", false, "take ownership of rows already present under a seeded key")

	seedCmd.AddCommand(seedValidateCmd, seedPlanCmd, seedApplyCmd)
	rootCmd.AddCommand(seedCmd)
}

// validateSeedLocally repeats the server's structural checks so a broken file is
// caught before a request is made. The server validates again — this is
// convenience, not the boundary.
func validateSeedLocally(c api.SeedCollection, f api.SeedFile) []string {
	var problems []string
	if len(c.Key) == 0 {
		problems = append(problems, "no key declared: a row with no identity cannot be re-applied safely")
		return problems
	}
	if f.Collection != c.Name {
		problems = append(problems, fmt.Sprintf("file says collection %q, manifest says %q", f.Collection, c.Name))
	}
	if !equalStringSlices(f.Key, c.Key) {
		problems = append(problems, fmt.Sprintf("key mismatch: manifest %v, file %v", c.Key, f.Key))
	}
	if len(f.Rows) == 0 {
		problems = append(problems, "no rows")
	}
	seen := map[string]int{}
	for i, row := range f.Rows {
		parts := make([]string, 0, len(c.Key))
		ok := true
		for _, field := range c.Key {
			v, present := row[field]
			if !present || v == nil || v == "" {
				problems = append(problems, fmt.Sprintf("row %d is missing key field %q", i, field))
				ok = false
				break
			}
			parts = append(parts, fmt.Sprintf("%v", v))
		}
		if !ok {
			continue
		}
		joined := strings.Join(parts, "\x00")
		if prev, dup := seen[joined]; dup {
			problems = append(problems, fmt.Sprintf("rows %d and %d share the same key (%v)", prev, i, parts))
			continue
		}
		seen[joined] = i
	}
	return problems
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
