package cmd

import (
	"encoding/json"
	"fmt"

	"github.com/kennedyowusu/koolbase-cli/internal/api"
	"github.com/kennedyowusu/koolbase-cli/internal/config"
	"github.com/spf13/cobra"
)

var invokeCmd = &cobra.Command{
	Use:   "invoke <function-name>",
	Short: "Invoke a deployed function",
	Example: `  koolbase invoke send-email --project proj_123
  koolbase invoke send-email --project proj_123 --data '{"email":"user@example.com"}'`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]

		cfg, err := config.Load()
		if err != nil {
			return err
		}

		projectID, _ := cmd.Flags().GetString("project")
		dataStr, _ := cmd.Flags().GetString("data")

		if projectID == "" {
			if cfg.ProjectID != "" {
				projectID = cfg.ProjectID
			} else {
				return fmt.Errorf("--project is required")
			}
		}

		var body map[string]interface{}
		if dataStr != "" {
			if err := json.Unmarshal([]byte(dataStr), &body); err != nil {
				return fmt.Errorf("invalid JSON in --data: %w", err)
			}
		}

		fmt.Printf("Invoking %s...\n\n", name)

		client := api.NewClient(cfg.BaseURL, cfg.APIKey)
		resp, err := client.InvokeFunction(projectID, name, body)
		if err != nil {
			return err
		}

		if resp.Error != "" {
			fmt.Printf(" Error: %s\n", resp.Error)
			return nil
		}

		output, _ := json.MarshalIndent(resp.Body, "", "  ")
		fmt.Printf(" Status: %d\n", resp.Status)
		fmt.Printf("Response:\n%s\n", string(output))
		if resp.LogID != "" {
			fmt.Printf("\nLog ID: %s\n", resp.LogID)
		}
		return nil
	},
}

func init() {
	invokeCmd.Flags().StringP("project", "p", "", "Project ID")
	invokeCmd.Flags().StringP("data", "d", "", "JSON body to send to the function")
}
