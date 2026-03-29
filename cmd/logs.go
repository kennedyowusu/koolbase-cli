package cmd

import (
	"fmt"

	"strings"

	"github.com/kennedyowusu/koolbase-cli/internal/api"
	"github.com/kennedyowusu/koolbase-cli/internal/config"
	"github.com/spf13/cobra"
)

var logsCmd = &cobra.Command{
	Use:   "logs <function-name>",
	Short: "View execution logs for a function",
	Example: `  koolbase logs send-email --project proj_123
  koolbase logs send-email --project proj_123 --limit 50`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]

		cfg, err := config.Load()
		if err != nil {
			return err
		}

		projectID, _ := cmd.Flags().GetString("project")
		limit, _ := cmd.Flags().GetInt("limit")

		if projectID == "" {
			if cfg.ProjectID != "" {
				projectID = cfg.ProjectID
			} else {
				return fmt.Errorf("--project is required")
			}
		}

		if limit <= 0 {
			limit = 20
		}

		// Get function ID from name
		client := api.NewClient(cfg.BaseURL, cfg.APIKey)
		fns, err := client.ListFunctions(projectID)
		if err != nil {
			return err
		}

		var functionID string
		for _, fn := range fns {
			if fn.Name == name {
				functionID = fn.ID
				break
			}
		}

		if functionID == "" {
			return fmt.Errorf("function %q not found in project %s", name, projectID)
		}

		logs, err := client.GetFunctionLogs(projectID, functionID, limit)
		if err != nil {
			return err
		}

		if len(logs) == 0 {
			fmt.Printf("No logs found for %s\n", name)
			return nil
		}

		fmt.Printf("Logs for %s (last %d):\n\n", name, len(logs))
		for _, log := range logs {
			status := "✅"
			if log.Status == "error" || log.Status == "timeout" {
				status = "❌"
			}
			fmt.Printf("%s [%s] %dms — %s\n", status, log.Status, log.DurationMs, log.CreatedAt)
			if log.Error != "" {
				fmt.Printf("   Error: %s\n", log.Error)
			}
			if log.Output != "" {
					output := log.Output
					marker := "__KOOLBASE_RESULT__"
					if idx := strings.Index(output, marker); idx >= 0 {
						output = output[idx+len(marker):]
					}
					output = strings.TrimSpace(output)
					if output != "" {
						fmt.Printf("   Output: %s\n", output)
					}
				}
			fmt.Println()
		}
		return nil
	},
}

func init() {
	logsCmd.Flags().StringP("project", "p", "", "Project ID")
	logsCmd.Flags().Int("limit", 20, "Number of logs to fetch")
}
