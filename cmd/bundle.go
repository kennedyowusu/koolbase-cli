package cmd

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/kennedyowusu/koolbase-cli/internal/api"
	"github.com/kennedyowusu/koolbase-cli/internal/config"
	"github.com/spf13/cobra"
)

// ─── Root bundle command ────────────────────────────────────────────────────

var bundleCmd = &cobra.Command{
	Use:   "bundle",
	Short: "Manage runtime bundles for code push",
}

// ─── koolbase bundle deploy ─────────────────────────────────────────────────

var bundleDeployCmd = &cobra.Command{
	Use:   "deploy",
	Short: "Package and deploy a runtime bundle",
	Example: `  koolbase bundle deploy \
    --app d2bc2c2c-fb42-4891-b758-45e48a1cd871 \
    --platform ios \
    --channel stable \
    --base-app-version 1.0.0 \
    --max-app-version 1.0.99 \
    --bundle-dir ./bundle \
    --rollout 100`,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load()
		if err != nil {
			return err
		}

		appID, _ := cmd.Flags().GetString("app")
		platform, _ := cmd.Flags().GetString("platform")
		channel, _ := cmd.Flags().GetString("channel")
		baseAppVersion, _ := cmd.Flags().GetString("base-app-version")
		maxAppVersion, _ := cmd.Flags().GetString("max-app-version")
		bundleDir, _ := cmd.Flags().GetString("bundle-dir")
		rollout, _ := cmd.Flags().GetInt("rollout")
		dryRun, _ := cmd.Flags().GetBool("dry-run")

		if appID == "" {
			return fmt.Errorf("--app is required")
		}
		if platform != "ios" && platform != "android" {
			return fmt.Errorf("--platform must be ios or android")
		}
		if baseAppVersion == "" {
			return fmt.Errorf("--base-app-version is required")
		}
		if maxAppVersion == "" {
			return fmt.Errorf("--max-app-version is required")
		}

		// Step 1 — validate bundle directory
		fmt.Println("  Validating bundle directory...")
		if err := validateBundleDir(bundleDir); err != nil {
			return fmt.Errorf("invalid bundle directory: %w", err)
		}
		fmt.Println("  ✓ Bundle directory valid")

		// Step 2 — read payload from directory
		payload, err := readPayload(bundleDir)
		if err != nil {
			return fmt.Errorf("could not read payload: %w", err)
		}

		// Step 3 — package into zip
		fmt.Println("  Packaging bundle...")
		zipPath, checksum, size, err := packageBundle(bundleDir, payload)
		if err != nil {
			return fmt.Errorf("packaging failed: %w", err)
		}
		defer os.Remove(zipPath)
		fmt.Printf("  ✓ Packaged (%s)\n", humanizeBytes(size))

		if dryRun {
			fmt.Println("\n  Dry run complete — no upload performed")
			fmt.Printf("  Checksum: %s\n", checksum)
			return nil
		}

		client := api.NewClient(cfg.BaseURL, cfg.APIKey)

		// Step 4 — create draft bundle
		fmt.Println("  Creating bundle draft...")
		bundle, err := client.CreateBundle(appID, api.CreateBundleRequest{
			BaseAppVersion:    baseAppVersion,
			MaxAppVersion:     maxAppVersion,
			Platform:          platform,
			Channel:           channel,
			RolloutPercentage: rollout,
			Checksum:          checksum,
			Signature:         "placeholder", // real signing in future phase
			SizeBytes:         size,
			Payload:           payload,
		})
		if err != nil {
			return err
		}
		fmt.Printf("  ✓ Draft created → %s (v%d)\n", bundle.ID, bundle.Version)

		// Step 5 — upload artifact
		fmt.Println("  Uploading artifact...")
		if err := client.UploadBundleArtifact(appID, bundle.ID, zipPath); err != nil {
			return fmt.Errorf("upload failed: %w", err)
		}
		fmt.Println("  ✓ Artifact uploaded")

		// Step 6 — publish
		fmt.Println("  Publishing...")
		if err := client.PublishBundle(appID, bundle.ID); err != nil {
			return fmt.Errorf("publish failed: %w", err)
		}

		fmt.Printf("\n  Bundle v%d live on %s/%s → %d%% of devices\n",
			bundle.Version, platform, channel, rollout)
		fmt.Printf("  Bundle ID: %s\n", bundle.ID)
		fmt.Printf("  Run `koolbase bundle recall --app %s --bundle %s` to roll back\n",
			appID, bundle.ID)

		return nil
	},
}

// ─── koolbase bundle recall ─────────────────────────────────────────────────

var bundleRecallCmd = &cobra.Command{
	Use:   "recall",
	Short: "Recall a published bundle",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load()
		if err != nil {
			return err
		}

		appID, _ := cmd.Flags().GetString("app")
		bundleID, _ := cmd.Flags().GetString("bundle")

		if appID == "" {
			return fmt.Errorf("--app is required")
		}
		if bundleID == "" {
			return fmt.Errorf("--bundle is required")
		}

		client := api.NewClient(cfg.BaseURL, cfg.APIKey)
		if err := client.RecallBundle(appID, bundleID); err != nil {
			return err
		}

		fmt.Printf("  ✓ Bundle %s recalled\n", bundleID)
		fmt.Println("  Devices will revert on next cold launch")
		return nil
	},
}

// ─── koolbase bundle list ───────────────────────────────────────────────────

var bundleListCmd = &cobra.Command{
	Use:   "list",
	Short: "List bundles for an app",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load()
		if err != nil {
			return err
		}

		appID, _ := cmd.Flags().GetString("app")
		if appID == "" {
			return fmt.Errorf("--app is required")
		}

		client := api.NewClient(cfg.BaseURL, cfg.APIKey)
		bundles, err := client.ListBundles(appID)
		if err != nil {
			return err
		}

		if len(bundles) == 0 {
			fmt.Println("No bundles found")
			return nil
		}

		fmt.Printf("\n  %-10s %-8s %-10s %-10s %-12s %-8s %s\n",
			"VERSION", "PLATFORM", "CHANNEL", "STATUS", "ROLLOUT", "SIZE", "CREATED")
		fmt.Println("  " + string(make([]byte, 80)))

		for _, b := range bundles {
			fmt.Printf("  v%-9d %-8s %-10s %-10s %-12s %-8s %s\n",
				b.Version,
				b.Platform,
				b.Channel,
				statusIcon(b.Status),
				fmt.Sprintf("%d%%", b.RolloutPercentage),
				humanizeBytes(b.SizeBytes),
				formatTime(b.CreatedAt),
			)
		}
		fmt.Println()
		return nil
	},
}

// ─── Helpers ────────────────────────────────────────────────────────────────

func validateBundleDir(dir string) error {
	required := []string{"config.json", "flags.json", "directives.json"}
	for _, name := range required {
		path := filepath.Join(dir, name)
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return fmt.Errorf("missing required file: %s", name)
		}
		if err := validateJSONFile(path); err != nil {
			return fmt.Errorf("%s is not valid JSON: %w", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "assets")); os.IsNotExist(err) {
		return fmt.Errorf("missing assets/ directory")
	}
	return nil
}

func validateJSONFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var v interface{}
	return json.Unmarshal(data, &v)
}

func readPayload(dir string) (map[string]interface{}, error) {
	readJSON := func(name string) (map[string]interface{}, error) {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		var v map[string]interface{}
		return v, json.Unmarshal(data, &v)
	}

	config, err := readJSON("config.json")
	if err != nil {
		return nil, fmt.Errorf("config.json: %w", err)
	}
	flags, err := readJSON("flags.json")
	if err != nil {
		return nil, fmt.Errorf("flags.json: %w", err)
	}
	directives, err := readJSON("directives.json")
	if err != nil {
		return nil, fmt.Errorf("directives.json: %w", err)
	}

	return map[string]interface{}{
		"config":     config,
		"flags":      flags,
		"directives": directives,
		"assets": map[string]interface{}{
			"images": []string{},
			"json":   []string{},
			"fonts":  []string{},
		},
	}, nil
}

func packageBundle(bundleDir string, payload map[string]interface{}) (zipPath, checksum string, size int, err error) {
	zipPath = filepath.Join(os.TempDir(),
		fmt.Sprintf("kbl_bundle_%d.zip", time.Now().UnixNano()))

	f, err := os.Create(zipPath)
	if err != nil {
		return "", "", 0, err
	}
	defer f.Close()

	h := sha256.New()
	w := io.MultiWriter(f, h)
	zw := zip.NewWriter(w)

	// Write manifest.json
	manifestBytes, err := json.Marshal(map[string]interface{}{
		"payload": payload,
	})
	if err != nil {
		return "", "", 0, err
	}
	if err := writeZipEntry(zw, "manifest.json", manifestBytes); err != nil {
		return "", "", 0, err
	}

	// Write config.json, flags.json, directives.json
	for _, name := range []string{"config.json", "flags.json", "directives.json"} {
		data, err := os.ReadFile(filepath.Join(bundleDir, name))
		if err != nil {
			return "", "", 0, err
		}
		if err := writeZipEntry(zw, name, data); err != nil {
			return "", "", 0, err
		}
	}

	// Walk assets/
	assetsDir := filepath.Join(bundleDir, "assets")
	err = filepath.WalkDir(assetsDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(bundleDir, path)
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return writeZipEntry(zw, rel, data)
	})
	if err != nil {
		return "", "", 0, err
	}

	if err := zw.Close(); err != nil {
		return "", "", 0, err
	}

	info, _ := f.Stat()
	checksum = fmt.Sprintf("sha256:%x", h.Sum(nil))
	return zipPath, checksum, int(info.Size()), nil
}

func writeZipEntry(zw *zip.Writer, name string, data []byte) error {
	fw, err := zw.Create(name)
	if err != nil {
		return err
	}
	_, err = fw.Write(data)
	return err
}

func humanizeBytes(b int) string {
	if b < 1024 {
		return fmt.Sprintf("%d B", b)
	}
	if b < 1024*1024 {
		return fmt.Sprintf("%.1f KB", float64(b)/1024)
	}
	return fmt.Sprintf("%.1f MB", float64(b)/(1024*1024))
}

func statusIcon(status string) string {
	switch status {
	case "published":
		return "published ✓"
	case "recalled":
		return "recalled ✗"
	default:
		return status
	}
}

func formatTime(t string) string {
	parsed, err := time.Parse(time.RFC3339, t)
	if err != nil {
		return t
	}
	return parsed.Format("2006-01-02 15:04")
}

// ─── Flag registration ──────────────────────────────────────────────────────

func init() {
	// deploy flags
	bundleDeployCmd.Flags().String("app", "", "App (project) ID (required)")
	bundleDeployCmd.Flags().String("platform", "", "ios or android (required)")
	bundleDeployCmd.Flags().String("channel", "stable", "Channel to deploy to")
	bundleDeployCmd.Flags().String("base-app-version", "", "Minimum app version (required)")
	bundleDeployCmd.Flags().String("max-app-version", "", "Maximum app version (required)")
	bundleDeployCmd.Flags().String("bundle-dir", "./bundle", "Path to bundle directory")
	bundleDeployCmd.Flags().Int("rollout", 100, "Rollout percentage 0-100")
	bundleDeployCmd.Flags().Bool("dry-run", false, "Validate and package without uploading")

	// recall flags
	bundleRecallCmd.Flags().String("app", "", "App (project) ID (required)")
	bundleRecallCmd.Flags().String("bundle", "", "Bundle ID to recall (required)")

	// list flags
	bundleListCmd.Flags().String("app", "", "App (project) ID (required)")

	// register subcommands
	bundleCmd.AddCommand(bundleDeployCmd)
	bundleCmd.AddCommand(bundleRecallCmd)
	bundleCmd.AddCommand(bundleListCmd)
}
