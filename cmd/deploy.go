package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kennedyowusu/koolbase-cli/internal/api"
	"github.com/kennedyowusu/koolbase-cli/internal/config"
	"github.com/spf13/cobra"
)

var deployCmd = &cobra.Command{
	Use:   "deploy <function-name>",
	Short: "Deploy a function from a local file",
	Example: `  koolbase deploy send-email --file ./functions/send_email.ts --project proj_123
  koolbase deploy process-order --file ./functions/process.dart --runtime dart --project proj_123`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]

		cfg, err := config.Load()
		if err != nil {
			return err
		}

		file, _ := cmd.Flags().GetString("file")
		runtime, _ := cmd.Flags().GetString("runtime")
		projectID, _ := cmd.Flags().GetString("project")
		timeoutMs, _ := cmd.Flags().GetInt("timeout")

		if file == "" {
			return fmt.Errorf("--file is required")
		}
		if projectID == "" {
			if cfg.ProjectID != "" {
				projectID = cfg.ProjectID
			} else {
				return fmt.Errorf("--project is required")
			}
		}

		// Auto-detect runtime from file extension if not specified
		if runtime == "" {
			ext := strings.ToLower(filepath.Ext(file))
			switch ext {
			case ".dart":
				runtime = "dart"
			default:
				runtime = "deno"
			}
		}

		// Read file contents
		code, err := os.ReadFile(file)
		if err != nil {
			return fmt.Errorf("failed to read file %s: %w", file, err)
		}

		if timeoutMs <= 0 {
			timeoutMs = 10000
		}

		fmt.Printf("Deploying %s (%s runtime)...\n", name, runtime)

		client := api.NewClient(cfg.BaseURL, cfg.APIKey)
		fn, err := client.DeployFunction(projectID, api.DeployRequest{
			Name:      name,
			Code:      string(code),
			Runtime:   runtime,
			TimeoutMs: timeoutMs,
		})
		if err != nil {
			return err
		}

		fmt.Printf("\n✅ Deployed %s v%d\n", fn.Name, fn.Version)
		fmt.Printf("   Runtime:  %s\n", fn.Runtime)
		fmt.Printf("   Timeout:  %dms\n", fn.TimeoutMs)
		fmt.Printf("   Project:  %s\n", projectID)
		return nil
	},
}

func init() {
	deployCmd.Flags().StringP("file", "f", "", "Path to the function file (.ts, .dart)")
	deployCmd.Flags().StringP("runtime", "r", "", "Runtime: deno (default) or dart (auto-detected from file extension)")
	deployCmd.Flags().StringP("project", "p", "", "Project ID")
	deployCmd.Flags().Int("timeout", 10000, "Execution timeout in milliseconds (max 30000)")
}
