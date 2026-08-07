package cmd

import (
	"bufio"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/kennedyowusu/koolbase-cli/internal/scaffold"
	"github.com/kennedyowusu/koolbase-cli/internal/templates"
	"github.com/spf13/cobra"
)

// `koolbase add` installs a template into an EXISTING project.
//
// Different problem from create, which owns the whole tree: here the
// developer's code is already there, so nothing is overwritten without being
// named, and the values a template needs are discovered from the project
// rather than collected from a picker.
//
// As with create, the generated code is the developer's the moment it lands.
// Nothing tracks it and nothing upgrades it.

var (
	addForce   bool
	addProject string
	addDir     string
	addYes     bool
)

var addCmd = &cobra.Command{
	Use:   "add <template>",
	Short: "Add a template to an existing Flutter project",
	Long: `Install a template into a project you already have.

Reads the app's package name from pubspec.yaml and its Koolbase project from
lib/app/koolbase_config.dart, so a project scaffolded by 'koolbase create'
needs no arguments. Elsewhere, pass --project.

Existing files are never overwritten silently: a collision lists what would
be replaced and stops, and --force proceeds.

Reference a version explicitly with 'chat@1.2.0'; without one, the newest
version compatible with your Flutter and SDK is used.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		projectDir, err := filepath.Abs(addDir)
		if err != nil {
			return err
		}

		pubspec := filepath.Join(projectDir, "pubspec.yaml")
		if _, err := os.Stat(pubspec); err != nil {
			return fmt.Errorf("no pubspec.yaml in %s — run this from a Flutter project, or pass --dir", projectDir)
		}
		if _, err := os.Stat(filepath.Join(projectDir, "lib")); err != nil {
			return fmt.Errorf("%s has no lib/ directory — is this a Flutter project?", projectDir)
		}

		appName, err := packageNameFrom(pubspec)
		if err != nil {
			return err
		}

		vars, err := varsForExistingProject(projectDir, appName)
		if err != nil {
			return err
		}

		req, err := templates.ParseRef(args[0], templates.Flutter)
		if err != nil {
			return err
		}

		store, err := templates.NewStore()
		if err != nil {
			return err
		}
		store.SweepStaging()

		fmt.Printf("Resolving template %s…\n", args[0])
		catalog, err := store.FetchCatalog()
		if err != nil {
			return err
		}
		entry, err := templates.Resolve(catalog, req, templates.Environment{
			FlutterVersion: detectFlutterVersion(),
			SDKVersion:     strings.TrimPrefix(flutterSDKConstraint, "^"),
		})
		if err != nil {
			return err
		}
		fmt.Printf("    %s@%s\n", entry.ID, entry.Version)

		fsys, err := store.Prepare(*entry)
		if err != nil {
			return err
		}

		if !entry.Resources.IsEmpty() {
			client, _, err := keysClient()
			if err != nil {
				return err
			}
			if err := provisionTemplate(client, vars.ProjectID, entry, addYes); err != nil {
				return err
			}
		}

		// Collisions are listed and refused rather than merged: overwriting
		// a file the developer has been editing is the one unrecoverable
		// thing this command could do.
		clashes, err := collisions(fsys, projectDir)
		if err != nil {
			return err
		}
		if len(clashes) > 0 && !addForce {
			fmt.Fprintf(os.Stderr, "\n%s would overwrite existing files:\n", entry.ID)
			for _, c := range clashes {
				fmt.Fprintf(os.Stderr, "  %s\n", c)
			}
			return fmt.Errorf("nothing was written — re-run with --force to overwrite, or move these files first")
		}

		if err := scaffold.Render(fsys, ".", projectDir, vars); err != nil {
			return fmt.Errorf("adding %s@%s failed: %w", entry.ID, entry.Version, err)
		}

		fmt.Printf("\n✓ Added %s\n", entry.Title)
		if len(clashes) > 0 {
			fmt.Printf("  %d existing file(s) overwritten.\n", len(clashes))
		}
		fmt.Printf(`
Wire it up:
  · import its routes in lib/app/router.dart and spread them into the routes
    list, beside the ones already there

This code is yours now — nothing tracks or upgrades it.
`)
		return nil
	},
}

// collisions reports which of the template's files already exist, using the
// same .tmpl-stripping rule the renderer applies, so the list matches what
// would actually be written.
func collisions(fsys fs.FS, projectDir string) ([]string, error) {
	var found []string

	err := fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || path == "." {
			return nil
		}
		out := strings.TrimSuffix(path, ".tmpl")
		if _, err := os.Stat(filepath.Join(projectDir, filepath.FromSlash(out))); err == nil {
			found = append(found, out)
		}
		return nil
	})
	return found, err
}

var pubspecName = regexp.MustCompile(`(?m)^name:\s*([a-z][a-z0-9_]*)\s*$`)

func packageNameFrom(pubspecPath string) (string, error) {
	data, err := os.ReadFile(pubspecPath)
	if err != nil {
		return "", err
	}
	m := pubspecName.FindSubmatch(data)
	if m == nil {
		return "", fmt.Errorf("could not read the package name from %s", pubspecPath)
	}
	return string(m[1]), nil
}

var (
	configProjectID = regexp.MustCompile(`projectId\s*=\s*'([^']+)'`)
	configPublicKey = regexp.MustCompile(`publicKey\s*=\s*'([^']+)'`)
	configBaseURL   = regexp.MustCompile(`baseUrl\s*=\s*'([^']+)'`)
	configEnvSlug   = regexp.MustCompile(`environment\s*=\s*'([^']+)'`)
)

// varsForExistingProject discovers what a template needs.
//
// A project scaffolded by `koolbase create` already carries its Koolbase
// details in lib/app/koolbase_config.dart, so add works there with no
// arguments. Elsewhere they come from --project, which requires a lookup.
func varsForExistingProject(projectDir, appName string) (scaffold.Vars, error) {
	vars := scaffold.Vars{
		AppName:    appName,
		AppClass:   strings.ReplaceAll(titleFromPackage(appName), " ", ""),
		AppTitle:   titleFromPackage(appName),
		SDKVersion: flutterSDKConstraint,
		BaseURL:    "https://api.koolbase.com",
	}

	configPath := filepath.Join(projectDir, "lib", "app", "koolbase_config.dart")
	if data, err := os.ReadFile(configPath); err == nil {
		if m := configProjectID.FindSubmatch(data); m != nil {
			vars.ProjectID = string(m[1])
		}
		if m := configPublicKey.FindSubmatch(data); m != nil {
			vars.PublicKey = string(m[1])
		}
		if m := configBaseURL.FindSubmatch(data); m != nil {
			vars.BaseURL = string(m[1])
		}
		if m := configEnvSlug.FindSubmatch(data); m != nil {
			vars.EnvironmentSlug = string(m[1])
		}
		// A flavored project carries per-flavor keys instead of one; the
		// single-value regexes above will not match, and --project fills the
		// gap.
	}

	if addProject != "" {
		client, _, err := keysClient()
		if err != nil {
			return vars, err
		}
		envs, err := client.ListEnvironments(addProject)
		if err != nil {
			return vars, err
		}
		if len(envs) == 0 {
			return vars, fmt.Errorf("project %s has no environments", addProject)
		}
		env := &envs[0]
		for i := range envs {
			if envs[i].Slug == "development" || envs[i].Slug == "dev" {
				env = &envs[i]
				break
			}
		}
		vars.ProjectID = addProject
		vars.PublicKey = env.PublicKey
		vars.EnvironmentSlug = env.Slug
		vars.BaseURL = client.BaseURL()
	}

	if vars.ProjectID == "" || vars.PublicKey == "" {
		return vars, fmt.Errorf(
			"could not determine which Koolbase project this app uses — pass --project <id>, or run this inside a project created by `koolbase create`")
	}
	return vars, nil
}

// confirmOverwrite is unused for now: --force is the deliberate act. Kept as
// the seam if an interactive prompt is ever wanted.
var _ = bufio.NewReader

func init() {
	addCmd.Flags().BoolVar(&addForce, "force", false, "overwrite existing files")
	addCmd.Flags().StringVar(&addProject, "project", "", "Koolbase project ID (when the app has no koolbase_config.dart)")
	addCmd.Flags().StringVar(&addDir, "dir", ".", "the Flutter project to add to")
	addCmd.Flags().BoolVarP(&addYes, "yes", "y", false, "create backend resources without confirming")
}
