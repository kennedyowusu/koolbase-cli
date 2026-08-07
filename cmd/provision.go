package cmd

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/kennedyowusu/koolbase-cli/internal/api"
	"github.com/kennedyowusu/koolbase-cli/internal/templates"
)

// provisionTemplate creates the backend a template needs, before any file is
// written into the developer's project.
//
// Order is the point. Generated screens that reference collections the
// installer failed to create are a half-made state whose failure is invisible
// until something runs — the same reasoning that moved template resolution
// ahead of `flutter create`.
//
// Returns nil for a template that declares nothing, which is most of them
// today: auth's backend exists on every project already.
func provisionTemplate(
	client *api.Client,
	projectID string,
	entry *templates.Entry,
	assumeYes bool,
) error {
	if entry.Resources.IsEmpty() {
		return nil
	}

	fmt.Printf("\n%s needs backend resources in your project:\n\n", entry.Title)
	fmt.Print(entry.Resources.Summarise())

	// Pull first: it is both the base for the merge and the collision check.
	// A template cannot be planned against a project whose shape is unknown.
	current, err := client.SnapshotPull(projectID)
	if err != nil {
		return fmt.Errorf("could not read your project's backend definition: %w", err)
	}

	merged, err := templates.MergeIntoSnapshot(current, entry.Resources, entry.ID)
	if err != nil {
		// A ConflictError already explains itself, including that nothing was
		// created. Wrapping it would bury that.
		return err
	}

	// Dry run before anything changes. The server produces the plan; showing
	// our own guess at it would risk describing something different from what
	// apply actually does.
	plan, err := client.SnapshotApply(projectID, json.RawMessage(merged), true, false, false)
	if err != nil {
		return fmt.Errorf("could not plan the backend changes: %w", err)
	}
	fmt.Printf("\nPlan:\n%s\n", indentLines(strings.TrimSpace(string(plan)), "  "))

	if !assumeYes {
		fmt.Print("Create these in your project? [Y/n]: ")
		reader := bufio.NewReader(os.Stdin)
		answer, _ := reader.ReadString('\n')
		answer = strings.ToLower(strings.TrimSpace(answer))
		if answer != "" && answer != "y" && answer != "yes" {
			return fmt.Errorf("cancelled — nothing was created and no files were written")
		}
	}

	// prune=false, always. Provisioning adds; it must never remove something
	// the developer has that this template does not know about.
	if _, err := client.SnapshotApply(projectID, json.RawMessage(merged), false, false, false); err != nil {
		return fmt.Errorf("creating the backend resources failed: %w", err)
	}

	fmt.Println("✓ Backend resources created")
	return nil
}

// indentLines shifts a block of server output so it reads as nested under the
// heading above it rather than as a new topic.
func indentLines(s, prefix string) string {
	if s == "" {
		return s
	}
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n")
}
