package cmd

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/kennedyowusu/koolbase-cli/internal/api"
	"github.com/kennedyowusu/koolbase-cli/internal/scaffold"
	"github.com/spf13/cobra"
)

// `koolbase create` scaffolds a Flutter app that is Koolbase-native from the
// first commit: opinionated architecture, authentication generated, and the
// developer's own project wired in.
//
// It orchestrates and nothing more. Flutter's own platform scaffolding is
// produced by shelling out to `flutter create` — owning iOS/Android folders,
// gradle, Podfile, and package naming across Flutter versions would be
// disproportionate. Rendering lives in internal/scaffold, which is testable
// without a network or a Flutter install.
//
// The developer owns the generated code the moment it lands. There is no
// upgrade path and no ownership tracking: you cannot upgrade code someone has
// been editing for three months.

var (
	createProjectID string
	createOrg       string
	createFlavors   bool
	createSkipPub   bool
)

// dartPackageName enforces Dart's package rules: lowercase, digits, and
// underscores, not starting with a digit. `flutter create` refuses anything
// else, and failing here gives a better message than its output.
var dartPackageName = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

var createCmd = &cobra.Command{
	Use:   "create <app_name>",
	Short: "Create a new Flutter app wired to your Koolbase project",
	Long: `Scaffold a new Flutter app that talks to Koolbase from the first run.

Runs 'flutter create', then replaces lib/ with an opinionated architecture:
feature-first layout, Riverpod, go_router, and authentication screens already
wired to your project. Pick an existing Koolbase project or create one — no
dashboard trip required.

The generated code is yours. Edit it, restructure it, or throw the
architecture away; nothing tracks or upgrades it afterwards.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		appName := args[0]
		if !dartPackageName.MatchString(appName) {
			return fmt.Errorf("%q is not a valid Dart package name — use lowercase letters, digits and underscores, starting with a letter (e.g. my_app)", appName)
		}
		if _, err := os.Stat(appName); err == nil {
			return fmt.Errorf("a directory named %q already exists here", appName)
		}
		if _, err := exec.LookPath("flutter"); err != nil {
			return fmt.Errorf("flutter is not on your PATH — install Flutter first: https://docs.flutter.dev/get-started/install")
		}

		client, who, err := keysClient()
		if err != nil {
			return err
		}
		orgID := who.OrgID
		if createOrg != "" {
			orgID = createOrg
		}

		project, err := resolveProject(client, orgID)
		if err != nil {
			return err
		}

		env, err := resolveEnvironment(client, project.ID)
		if err != nil {
			return err
		}

		fmt.Printf("\nCreating Flutter app %s…\n", appName)
		flutter := exec.Command("flutter", "create",
			"--org", createOrgIdentifier(),
			"--project-name", appName,
			appName,
		)
		flutter.Stdout, flutter.Stderr = os.Stdout, os.Stderr
		if err := flutter.Run(); err != nil {
			return fmt.Errorf("flutter create failed: %w", err)
		}

		// Replace Flutter's counter app wholesale. Anything we do not write is
		// Flutter's own scaffolding and stays untouched.
		libDir := filepath.Join(appName, "lib")
		if err := os.RemoveAll(libDir); err != nil {
			return fmt.Errorf("could not clear lib/: %w", err)
		}
		// Flutter's generated widget test references MyApp, which went with
		// lib/. Remove it rather than ship a project that fails analysis.
		_ = os.Remove(filepath.Join(appName, "test", "widget_test.dart"))

		vars := scaffold.Vars{
			AppName:  appName,
			AppClass: strings.ReplaceAll(titleFromPackage(appName), " ", ""), AppTitle: titleFromPackage(appName),
			OrgIdentifier:   createOrgIdentifier(),
			ProjectID:       project.ID,
			PublicKey:       env.PublicKey,
			EnvironmentSlug: env.Slug,
			BaseURL:         client.BaseURL(),
			Flavors:         createFlavors,
			SDKVersion:      flutterSDKConstraint,
		}
		if err := scaffold.Render(templatesFS, "templates/skeleton", appName, vars); err != nil {
			return fmt.Errorf("scaffolding failed: %w", err)
		}
		if err := scaffold.Render(templatesFS, "templates/features/auth", appName, vars); err != nil {
			return fmt.Errorf("scaffolding auth failed: %w", err)
		}

		if !createSkipPub {
			fmt.Println("\nFetching packages…")
			pub := exec.Command("flutter", "pub", "get")
			pub.Dir = appName
			pub.Stdout, pub.Stderr = os.Stdout, os.Stderr
			if err := pub.Run(); err != nil {
				// Not fatal: the project exists and is correct; the developer
				// can run it themselves and see the real error.
				fmt.Printf("\n⚠ flutter pub get failed — run it yourself in %s to see why\n", appName)
			}
		}

		fmt.Printf(`
✓ %s is ready.

  Project      %s
  Environment  %s

  cd %s
  flutter run

Sign-in screens are generated and wired. Add a feature with:
  koolbase add <feature>
`, appName, project.Name, env.Slug, appName)
		return nil
	},
}

// resolveProject returns the project to wire the app to: the one named by
// --project, or one chosen interactively, or a newly created one.
func resolveProject(client *api.Client, orgID string) (*api.Project, error) {
	if createProjectID != "" {
		p, err := client.GetProject(createProjectID)
		if err != nil {
			return nil, err
		}
		return &api.Project{ID: p.ID, Name: p.Name}, nil
	}

	projects, err := client.ListProjects(orgID)
	if err != nil {
		return nil, err
	}

	fmt.Println("\nWhich Koolbase project should this app use?")
	for i, p := range projects {
		fmt.Printf("  %d) %s\n", i+1, p.Name)
	}
	fmt.Printf("  %d) Create a new project\n", len(projects)+1)
	fmt.Print("\nChoice: ")

	reader := bufio.NewReader(os.Stdin)
	line, _ := reader.ReadString('\n')
	choice, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || choice < 1 || choice > len(projects)+1 {
		return nil, fmt.Errorf("pick a number between 1 and %d", len(projects)+1)
	}

	if choice <= len(projects) {
		return &projects[choice-1], nil
	}

	fmt.Print("New project name: ")
	nameLine, _ := reader.ReadString('\n')
	name := strings.TrimSpace(nameLine)
	if name == "" {
		return nil, fmt.Errorf("a project name is required")
	}
	created, err := client.CreateProject(orgID, name)
	if err != nil {
		return nil, err
	}
	fmt.Printf("✓ Created project %s\n", created.Name)
	return created, nil
}

// resolveEnvironment returns the environment whose public key the app will
// use. A project created moments ago has none — the project-create path does
// not seed one — so this creates `development` when the list is empty.
func resolveEnvironment(client *api.Client, projectID string) (*api.Environment, error) {
	envs, err := client.ListEnvironments(projectID)
	if err != nil {
		return nil, err
	}
	if len(envs) == 0 {
		fmt.Println("This project has no environments — creating 'development'…")
		return client.CreateEnvironment(projectID, "development")
	}
	// Prefer a development environment. A scaffolded app must never silently
	// point at production — a first-run experiment writing to real data is a
	// worse outcome than an extra prompt.
	for i := range envs {
		if envs[i].Slug == "development" || envs[i].Slug == "dev" {
			return &envs[i], nil
		}
	}

	fmt.Println("\nThis project has no development environment. Existing:")
	for i, e := range envs {
		fmt.Printf("  %d) %s\n", i+1, e.Slug)
	}
	fmt.Printf("  %d) Create 'development' (recommended)\n", len(envs)+1)
	fmt.Print("\nWhich environment should the app use? ")

	reader := bufio.NewReader(os.Stdin)
	line, _ := reader.ReadString('\n')
	choice, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || choice < 1 || choice > len(envs)+1 {
		return nil, fmt.Errorf("pick a number between 1 and %d", len(envs)+1)
	}
	if choice <= len(envs) {
		return &envs[choice-1], nil
	}
	return client.CreateEnvironment(projectID, "development")
}

// createOrgIdentifier is the reverse-domain prefix passed to `flutter create`.
func createOrgIdentifier() string {
	if v := os.Getenv("KOOLBASE_ORG_IDENTIFIER"); v != "" {
		return v
	}
	return "com.example"
}

// titleFromPackage turns my_cool_app into "My Cool App" for display strings.
func titleFromPackage(pkg string) string {
	parts := strings.Split(pkg, "_")
	for i, p := range parts {
		if p == "" {
			continue
		}
		parts[i] = strings.ToUpper(p[:1]) + p[1:]
	}
	return strings.Join(parts, " ")
}

func init() {
	createCmd.Flags().StringVar(&createProjectID, "project", "", "Koolbase project ID to wire the app to (skips the picker)")
	createCmd.Flags().StringVar(&createOrg, "org", "", "organization ID (defaults to your own)")
	createCmd.Flags().BoolVar(&createFlavors, "flavors", false, "generate dev/staging/prod flavor configs")
	createCmd.Flags().BoolVar(&createSkipPub, "skip-pub-get", false, "do not run flutter pub get after scaffolding")
}
