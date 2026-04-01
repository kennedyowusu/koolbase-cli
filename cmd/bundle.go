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

var bundleCmd = &cobra.Command{
	Use:   "bundle",
	Short: "Manage runtime bundles for code push",
}

var bundleDeployCmd = &cobra.Command{
	Use:   "deploy",
	Short: "Package and deploy a runtime bundle",
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

		// Step 1 — validate
		fmt.Println("  Validating bundle directory...")
		if err := validateBundleDir(bundleDir); err != nil {
			return fmt.Errorf("invalid bundle directory: %w", err)
		}
		fmt.Println("  ✓ Bundle directory valid")

		// Step 2 — read payload
		payload, err := readPayload(bundleDir)
		if err != nil {
			return fmt.Errorf("could not read payload: %w", err)
		}

		if dryRun {
			fmt.Println("  Packaging bundle...")
			zipPath, checksum, size, err := packageBundle(bundleDir, payload, bundleMeta{
				bundleID: "dry-run", appID: appID, version: 0,
				baseAppVersion: baseAppVersion, maxAppVersion: maxAppVersion,
				platform: platform, channel: channel,
			})
			if err != nil {
				return fmt.Errorf("packaging failed: %w", err)
			}
			os.Remove(zipPath)
			fmt.Printf("  ✓ Packaged (%s)\n", humanizeBytes(size))
			fmt.Println("\n  Dry run complete — no upload performed")
			fmt.Printf("  Checksum: %s\n", checksum)
			return nil
		}

		client := api.NewClient(cfg.BaseURL, cfg.APIKey)

		// Step 3 — create draft to get real bundle ID + version
		fmt.Println("  Creating bundle draft...")
		bundle, err := client.CreateBundle(appID, api.CreateBundleRequest{
			BaseAppVersion:    baseAppVersion,
			MaxAppVersion:     maxAppVersion,
			Platform:          platform,
			Channel:           channel,
			RolloutPercentage: rollout,
			Checksum:          "pending",
			Signature:         "placeholder",
			SizeBytes:         0,
			Payload:           payload,
		})
		if err != nil {
			return err
		}
		fmt.Printf("  ✓ Draft created → %s (v%d)\n", bundle.ID, bundle.Version)

		// Step 4 — package with real bundle ID + version baked into manifest
		fmt.Println("  Packaging bundle...")
		zipPath, checksum, size, err := packageBundle(bundleDir, payload, bundleMeta{
			bundleID:       bundle.ID,
			appID:          appID,
			version:        bundle.Version,
			baseAppVersion: baseAppVersion,
			maxAppVersion:  maxAppVersion,
			platform:       platform,
			channel:        channel,
		})
		if err != nil {
			return fmt.Errorf("packaging failed: %w", err)
		}
		defer os.Remove(zipPath)
		fmt.Printf("  ✓ Packaged (%s)\n", humanizeBytes(size))

		// Step 5 — upload
		fmt.Println("  Uploading artifact...")
		if err := client.UploadBundleArtifact(appID, bundle.ID, zipPath); err != nil {
			return fmt.Errorf("upload failed: %w", err)
		}
		fmt.Println("  ✓ Artifact uploaded")

		// Step 6 — update checksum on the bundle record
		if err := client.UpdateBundleChecksum(appID, bundle.ID, checksum, size); err != nil {
			return fmt.Errorf("checksum update failed: %w", err)
		}

		// Step 7 — publish
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
		fmt.Printf("\n  %-10s %-8s %-10s %-12s %-12s %-8s %s\n",
			"VERSION", "PLATFORM", "CHANNEL", "STATUS", "ROLLOUT", "SIZE", "CREATED")
		for _, b := range bundles {
			fmt.Printf("  v%-9d %-8s %-10s %-12s %-12s %-8s %s\n",
				b.Version, b.Platform, b.Channel,
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

type bundleMeta struct {
	bundleID       string
	appID          string
	version        int
	baseAppVersion string
	maxAppVersion  string
	platform       string
	channel        string
}

func validateBundleDir(dir string) error {
	for _, name := range []string{"config.json", "flags.json", "directives.json"} {
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
	cfg, err := readJSON("config.json")
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
		"config":     cfg,
		"flags":      flags,
		"directives": directives,
		"assets":     map[string]interface{}{"images": []string{}, "json": []string{}, "fonts": []string{}},
	}, nil
}

func packageBundle(bundleDir string, payload map[string]interface{}, meta bundleMeta) (zipPath, checksum string, size int, err error) {
	zipPath = filepath.Join(os.TempDir(), fmt.Sprintf("kbl_bundle_%d.zip", time.Now().UnixNano()))
	f, err := os.Create(zipPath)
	if err != nil {
		return "", "", 0, err
	}
	defer f.Close()

	h := sha256.New()
	w := io.MultiWriter(f, h)
	zw := zip.NewWriter(w)

	manifestBytes, err := json.Marshal(map[string]interface{}{
		"bundle_id":        meta.bundleID,
		"app_id":           meta.appID,
		"version":          meta.version,
		"base_app_version": meta.baseAppVersion,
		"max_app_version":  meta.maxAppVersion,
		"platform":         meta.platform,
		"channel":          meta.channel,
		"checksum":         "pending",
		"signature":        "pending",
		"size_bytes":       0,
		"payload":          payload,
	})
	if err != nil {
		return "", "", 0, err
	}
	if err := writeZipEntry(zw, "manifest.json", manifestBytes); err != nil {
		return "", "", 0, err
	}

	for _, name := range []string{"config.json", "flags.json", "directives.json"} {
		data, err := os.ReadFile(filepath.Join(bundleDir, name))
		if err != nil {
			return "", "", 0, err
		}
		if err := writeZipEntry(zw, name, data); err != nil {
			return "", "", 0, err
		}
	}

	assetsDir := filepath.Join(bundleDir, "assets")
	err = filepath.WalkDir(assetsDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() {
			return walkErr
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

func init() {
	bundleDeployCmd.Flags().String("app", "", "App (project) ID (required)")
	bundleDeployCmd.Flags().String("platform", "", "ios or android (required)")
	bundleDeployCmd.Flags().String("channel", "stable", "Channel to deploy to")
	bundleDeployCmd.Flags().String("base-app-version", "", "Minimum app version (required)")
	bundleDeployCmd.Flags().String("max-app-version", "", "Maximum app version (required)")
	bundleDeployCmd.Flags().String("bundle-dir", "./bundle", "Path to bundle directory")
	bundleDeployCmd.Flags().Int("rollout", 100, "Rollout percentage 0-100")
	bundleDeployCmd.Flags().Bool("dry-run", false, "Validate and package without uploading")

	bundleRecallCmd.Flags().String("app", "", "App (project) ID (required)")
	bundleRecallCmd.Flags().String("bundle", "", "Bundle ID to recall (required)")

	bundleListCmd.Flags().String("app", "", "App (project) ID (required)")

	bundleCmd.AddCommand(bundleDeployCmd)
	bundleCmd.AddCommand(bundleRecallCmd)
	bundleCmd.AddCommand(bundleListCmd)
}
