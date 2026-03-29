package cmd

import (
	"fmt"

	"github.com/kennedyowusu/koolbase-cli/internal/api"
	"github.com/kennedyowusu/koolbase-cli/internal/config"
	"github.com/spf13/cobra"
)

var functionsCmd = &cobra.Command{
	Use:   "functions",
	Short: "Manage your Koolbase functions",
}

var functionsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all deployed functions in a project",
	Example: `  koolbase functions list --project proj_123`,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load()
		if err != nil {
			return err
		}

		projectID, _ := cmd.Flags().GetString("project")
		if projectID == "" {
			if cfg.ProjectID != "" {
				projectID = cfg.ProjectID
			} else {
				return fmt.Errorf("--project is required")
			}
		}

		client := api.NewClient(cfg.BaseURL, cfg.APIKey)
		fns, err := client.ListFunctions(projectID)
		if err != nil {
			return err
		}

		if len(fns) == 0 {
			fmt.Println("No functions deployed yet.")
			fmt.Println("Deploy one with: koolbase deploy <name> --file <path> --project <id>")
			return nil
		}

		fmt.Printf("%-25s %-8s %-10s %-8s %s\n", "NAME", "VERSION", "RUNTIME", "TIMEOUT", "LAST DEPLOYED")
		fmt.Printf("%-25s %-8s %-10s %-8s %s\n", "----", "-------", "-------", "-------", "-------------")
		for _, fn := range fns {
			lastDeployed := "—"
			if fn.LastDeployedAt != "" && len(fn.LastDeployedAt) >= 10 {
				lastDeployed = fn.LastDeployedAt[:10]
			}
			fmt.Printf("%-25s v%-7d %-10s %-8d %s\n",
				fn.Name, fn.Version, fn.Runtime, fn.TimeoutMs, lastDeployed)
		}
		return nil
	},
}

func init() {
	functionsListCmd.Flags().StringP("project", "p", "", "Project ID")
	functionsCmd.AddCommand(functionsListCmd)
}
