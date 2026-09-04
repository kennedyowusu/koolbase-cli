package cmd

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

// The export contract, enforced. The Designer produces a zip: a generated
// tree under lib/generated/, a handful of developer-owned files written
// once, and .koolbase/export.json recording a hash of every generated
// file. This command applies it to a project directory.
//
//   First apply    everything is written.
//   Re-apply       lib/generated/ is replaced transactionally; developer
//                  files are never touched; hand edits inside generated/
//                  are detected from the previous manifest and named
//                  before anything is overwritten; a pubspec whose
//                  dependencies changed is reported, not rewritten.
//
// No merge. Inside generated/ Koolbase owns it; outside, the developer
// does.

const manifestPath = ".koolbase/export.json"

type manifestFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type exportManifest struct {
	DocumentID       string         `json:"document_id"`
	DocumentName     string         `json:"document_name"`
	SchemaVersion    int            `json:"schema_version"`
	ExporterVersion  int            `json:"exporter_version"`
	GeneratedRoot    string         `json:"generated_root"`
	OwnedByDeveloper []string       `json:"owned_by_developer"`
	GeneratedFiles   []manifestFile `json:"generated_files"`
}

var exportCmd = &cobra.Command{
	Use:   "export",
	Short: "Apply a Designer export to a Flutter project",
}

var exportApplyCmd = &cobra.Command{
	Use:   "apply <export.zip>",
	Short: "Write or update a project from a Designer export",
	Long: `Applies a Koolbase Designer export to a project directory.

On a fresh directory every file is written. On an existing project only
lib/generated/ is replaced; main.dart, pubspec.yaml and anything you have
added are never touched. If generated files were edited by hand since the
last export, they are listed and the apply stops unless --force is given.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		into, _ := cmd.Flags().GetString("into")
		force, _ := cmd.Flags().GetBool("force")
		if into == "" {
			into = "."
		}
		return applyExport(args[0], into, force, cmd.OutOrStdout())
	},
}

func init() {
	exportApplyCmd.Flags().String("into", ".", "Project directory to apply into")
	exportApplyCmd.Flags().Bool("force", false, "Replace generated files even if they were edited by hand")
	exportCmd.AddCommand(exportApplyCmd)
	rootCmd.AddCommand(exportCmd)
}

func applyExport(zipPath, into string, force bool, out io.Writer) error {
	incoming, err := readZip(zipPath)
	if err != nil {
		return err
	}
	raw, ok := incoming[manifestPath]
	if !ok {
		return fmt.Errorf("%s is not a Koolbase export: no %s", zipPath, manifestPath)
	}
	var next exportManifest
	if err := json.Unmarshal(raw, &next); err != nil {
		return fmt.Errorf("export manifest is unreadable: %w", err)
	}
	if next.GeneratedRoot == "" {
		return fmt.Errorf("export manifest names no generated root")
	}

	prevPath := filepath.Join(into, manifestPath)
	prevRaw, err := os.ReadFile(prevPath)
	fresh := os.IsNotExist(err)
	if err != nil && !fresh {
		return fmt.Errorf("reading previous manifest: %w", err)
	}

	if fresh {
		return applyFresh(incoming, into, out)
	}

	var prev exportManifest
	if err := json.Unmarshal(prevRaw, &prev); err != nil {
		return fmt.Errorf("previous manifest is unreadable: %w", err)
	}
	if prev.DocumentID != next.DocumentID {
		return fmt.Errorf("this directory was exported from document %s; the zip is from %s — apply into a different directory",
			prev.DocumentID, next.DocumentID)
	}

	// Hand edits inside generated/ since the last apply. Detected against
	// the PREVIOUS manifest's hashes: a file whose bytes differ from what
	// Koolbase last wrote was touched by someone. Named, never merged.
	edited := detectEdits(prev, into)
	if len(edited) > 0 && !force {
		fmt.Fprintln(out, "Generated files were edited by hand since the last export:")
		for _, p := range edited {
			fmt.Fprintf(out, "  %s\n", p)
		}
		fmt.Fprintln(out, "\nRe-export replaces them. Move your changes outside lib/generated/,")
		fmt.Fprintln(out, "or run again with --force to discard them.")
		return fmt.Errorf("stopped: %d generated file(s) edited", len(edited))
	}

	return applyReplace(incoming, next, prev, into, edited, out)
}

func applyFresh(incoming map[string][]byte, into string, out io.Writer) error {
	paths := sortedKeys(incoming)
	for _, p := range paths {
		if err := writeFile(filepath.Join(into, p), incoming[p]); err != nil {
			return err
		}
	}
	fmt.Fprintf(out, "Exported %d files into %s\n", len(paths), into)
	fmt.Fprintln(out, "\nlib/generated/ is Koolbase's and is replaced on re-export.")
	fmt.Fprintln(out, "Everything else is yours; build outside lib/generated/.")
	return nil
}

func applyReplace(incoming map[string][]byte, next, prev exportManifest, into string, edited []string, out io.Writer) error {
	root := next.GeneratedRoot

	// Transactional: build the new generated tree beside the old one, and
	// swap only after every file is written. A failure part-way leaves
	// the existing tree untouched.
	liveDir := filepath.Join(into, filepath.Clean(root))
	stageDir := liveDir + ".koolbase-next"
	backupDir := liveDir + ".koolbase-prev"
	_ = os.RemoveAll(stageDir)
	_ = os.RemoveAll(backupDir)

	written := 0
	for _, p := range sortedKeys(incoming) {
		if !strings.HasPrefix(p, root) {
			continue
		}
		rel := strings.TrimPrefix(p, root)
		if err := writeFile(filepath.Join(stageDir, rel), incoming[p]); err != nil {
			_ = os.RemoveAll(stageDir)
			return err
		}
		written++
	}

	if err := os.Rename(liveDir, backupDir); err != nil && !os.IsNotExist(err) {
		_ = os.RemoveAll(stageDir)
		return fmt.Errorf("moving old generated tree aside: %w", err)
	}
	if err := os.Rename(stageDir, liveDir); err != nil {
		_ = os.Rename(backupDir, liveDir)
		return fmt.Errorf("swapping in new generated tree: %w", err)
	}
	_ = os.RemoveAll(backupDir)

	if err := writeFile(filepath.Join(into, manifestPath), incoming[manifestPath]); err != nil {
		return err
	}

	// Removed screens: files the previous manifest listed that the new one
	// does not. The swap above already dropped them; say so.
	nextSet := map[string]bool{}
	for _, f := range next.GeneratedFiles {
		nextSet[f.Path] = true
	}
	var removed []string
	for _, f := range prev.GeneratedFiles {
		if !nextSet[f.Path] {
			removed = append(removed, f.Path)
		}
	}

	fmt.Fprintf(out, "Replaced %s (%d files)\n", root, written)
	if len(removed) > 0 {
		fmt.Fprintf(out, "Removed %d generated file(s) no longer in the design:\n", len(removed))
		for _, p := range removed {
			fmt.Fprintf(out, "  %s\n", p)
		}
	}
	if len(edited) > 0 {
		fmt.Fprintf(out, "Discarded hand edits in %d file(s) (--force)\n", len(edited))
	}

	// Developer-owned files: never written. But pubspec dependencies can
	// change between exports, and silently rewriting it would break the
	// rule the moment it was convenient. Report the delta instead.
	reportPubspecDelta(incoming, into, out)
	fmt.Fprintln(out, "\nDeveloper files untouched: "+strings.Join(next.OwnedByDeveloper, ", "))
	return nil
}

func detectEdits(prev exportManifest, into string) []string {
	var edited []string
	for _, f := range prev.GeneratedFiles {
		b, err := os.ReadFile(filepath.Join(into, f.Path))
		if err != nil {
			continue // deleted by hand: not an edit to preserve
		}
		sum := sha256.Sum256(b)
		if hex.EncodeToString(sum[:]) != f.SHA256 {
			edited = append(edited, f.Path)
		}
	}
	sort.Strings(edited)
	return edited
}

func reportPubspecDelta(incoming map[string][]byte, into string, out io.Writer) {
	want, ok := incoming["pubspec.yaml"]
	if !ok {
		return
	}
	have, err := os.ReadFile(filepath.Join(into, "pubspec.yaml"))
	if err != nil {
		return
	}
	wantDeps := depsOf(string(want))
	haveDeps := depsOf(string(have))
	var missing []string
	for d := range wantDeps {
		if _, ok := haveDeps[d]; !ok {
			missing = append(missing, d+": "+wantDeps[d])
		}
	}
	if len(missing) == 0 {
		return
	}
	sort.Strings(missing)
	fmt.Fprintln(out, "\nGenerated code now needs dependencies your pubspec.yaml does not list:")
	for _, m := range missing {
		fmt.Fprintf(out, "  %s\n", m)
	}
	fmt.Fprintln(out, "pubspec.yaml is yours; add them and run `flutter pub get`.")
}

// depsOf reads the `dependencies:` block of a pubspec — name -> constraint.
// Deliberately not a YAML parser: the block the exporter writes is flat,
// and a developer's additions in the same block are flat too.
func depsOf(pubspec string) map[string]string {
	deps := map[string]string{}
	in := false
	for _, line := range strings.Split(pubspec, "\n") {
		if strings.HasPrefix(line, "dependencies:") {
			in = true
			continue
		}
		if in && len(line) > 0 && line[0] != ' ' {
			break
		}
		if !in {
			continue
		}
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") || strings.HasPrefix(t, "sdk:") {
			continue
		}
		if k, v, ok := strings.Cut(t, ":"); ok {
			deps[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return deps
}

func readZip(path string) (map[string][]byte, error) {
	r, err := zip.OpenReader(path)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	defer r.Close()
	files := map[string][]byte{}
	for _, f := range r.File {
		if f.FileInfo().IsDir() {
			continue
		}
		name := filepath.ToSlash(f.Name)
		// Exports may be rooted in a single top-level folder; strip it so
		// paths match the manifest.
		if i := strings.Index(name, "/"); i > 0 && !strings.HasPrefix(name, "lib/") &&
			!strings.HasPrefix(name, ".koolbase/") && name != "pubspec.yaml" && name != "README.md" {
			name = name[i+1:]
		}
		if strings.Contains(name, "..") {
			return nil, fmt.Errorf("refusing zip entry with path traversal: %s", f.Name)
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		b, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return nil, err
		}
		files[name] = b
	}
	return files, nil
}

func writeFile(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

func sortedKeys(m map[string][]byte) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
