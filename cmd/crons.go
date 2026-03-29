package cmd

import (
	"encoding/json"
	"fmt"

	"github.com/kennedyowusu/koolbase-cli/internal/api"
	"github.com/kennedyowusu/koolbase-cli/internal/config"
	"github.com/spf13/cobra"
)

var cronsCmd = &cobra.Command{
	Use:   "crons",
	Short: "Manage cron schedules for your functions",
}

var cronsListCmd = &cobra.Command{
	Use:     "list",
	Short:   "List all cron schedules in a project",
	Example: `  koolbase crons list --project proj_123`,
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
		schedules, err := client.ListCrons(projectID)
		if err != nil {
			return err
		}

		if len(schedules) == 0 {
			fmt.Println("No cron schedules found.")
			fmt.Println("Add one with: koolbase crons add <function-name> --cron <expression> --project <id>")
			return nil
		}

		fmt.Printf("%-36s %-20s %-15s %-8s %s\n", "ID", "FUNCTION", "CRON", "ENABLED", "LAST RUN")
		fmt.Printf("%-36s %-20s %-15s %-8s %s\n", "--", "--------", "----", "-------", "--------")
		for _, s := range schedules {
			enabled := "yes"
			if !s.Enabled {
				enabled = "no"
			}
			lastRun := "never"
			if s.LastRunAt != "" && len(s.LastRunAt) >= 10 {
				lastRun = s.LastRunAt[:10]
			}
			fmt.Printf("%-36s %-20s %-15s %-8s %s\n", s.ID, s.FunctionName, s.CronExpression, enabled, lastRun)
		}
		return nil
	},
}

var cronsAddCmd = &cobra.Command{
	Use:   "add <function-name>",
	Short: "Add a cron schedule for a function",
	Example: `  koolbase crons add send-email --cron "0 9 * * *" --project proj_123
  koolbase crons add cleanup --cron "*/5 * * * *" --project proj_123`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		functionName := args[0]

		cfg, err := config.Load()
		if err != nil {
			return err
		}

		projectID, _ := cmd.Flags().GetString("project")
		cronExpr, _ := cmd.Flags().GetString("cron")

		if projectID == "" {
			if cfg.ProjectID != "" {
				projectID = cfg.ProjectID
			} else {
				return fmt.Errorf("--project is required")
			}
		}
		if cronExpr == "" {
			return fmt.Errorf("--cron is required (e.g. \"0 9 * * *\")")
		}

		client := api.NewClient(cfg.BaseURL, cfg.APIKey)
		schedule, err := client.CreateCron(projectID, functionName, cronExpr)
		if err != nil {
			return err
		}

		nextRun := "—"
		if schedule.NextRunAt != "" && len(schedule.NextRunAt) >= 19 {
			nextRun = schedule.NextRunAt[:19] + "Z"
		}

		fmt.Printf("\nCron schedule created\n")
		fmt.Printf("   ID:         %s\n", schedule.ID)
		fmt.Printf("   Function:   %s\n", schedule.FunctionName)
		fmt.Printf("   Expression: %s\n", schedule.CronExpression)
		fmt.Printf("   Next run:   %s\n", nextRun)
		return nil
	},
}

var cronsDeleteCmd = &cobra.Command{
	Use:     "delete <cron-id>",
	Short:   "Delete a cron schedule",
	Example: `  koolbase crons delete 18f79c3a-1965-44a5-8ff2-1d35ac58ede8 --project proj_123`,
	Args:    cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cronID := args[0]

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
		if err := client.DeleteCron(projectID, cronID); err != nil {
			return err
		}

		fmt.Printf("Cron schedule %s deleted\n", cronID)
		return nil
	},
}

var cronsToggleCmd = &cobra.Command{
	Use:     "toggle <cron-id>",
	Short:   "Enable or disable a cron schedule",
	Example: `  koolbase crons toggle 18f79c3a --enable --project proj_123
  koolbase crons toggle 18f79c3a --disable --project proj_123`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cronID := args[0]

		cfg, err := config.Load()
		if err != nil {
			return err
		}

		projectID, _ := cmd.Flags().GetString("project")
		enable, _ := cmd.Flags().GetBool("enable")
		disable, _ := cmd.Flags().GetBool("disable")

		if !enable && !disable {
			return fmt.Errorf("--enable or --disable is required")
		}

		enabled := enable

		client := api.NewClient(cfg.BaseURL, cfg.APIKey)
		schedule, err := client.UpdateCron(projectID, cronID, enabled)
		if err != nil {
			return err
		}

		status := "enabled"
		if !schedule.Enabled {
			status = "disabled"
		}
		fmt.Printf("Cron schedule %s %s\n", cronID, status)
		return nil
	},
}

func init() {
	cronsListCmd.Flags().StringP("project", "p", "", "Project ID")
	cronsAddCmd.Flags().StringP("project", "p", "", "Project ID")
	cronsAddCmd.Flags().String("cron", "", "Cron expression (e.g. \"0 9 * * *\")")
	cronsDeleteCmd.Flags().StringP("project", "p", "", "Project ID")
	cronsToggleCmd.Flags().StringP("project", "p", "", "Project ID")
	cronsToggleCmd.Flags().Bool("enable", false, "Enable the cron schedule")
	cronsToggleCmd.Flags().Bool("disable", false, "Disable the cron schedule")

	cronsCmd.AddCommand(cronsListCmd)
	cronsCmd.AddCommand(cronsAddCmd)
	cronsCmd.AddCommand(cronsDeleteCmd)
	cronsCmd.AddCommand(cronsToggleCmd)
}

// ensure json is used
var _ = json.Marshal
