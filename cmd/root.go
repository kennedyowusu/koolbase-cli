package cmd

import (
	"os"

	"github.com/kennedyowusu/koolbase-cli/internal/api"
	"github.com/kennedyowusu/koolbase-cli/internal/config"
	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:          "koolbase",
	Short:        "Koolbase CLI — manage your Koolbase project from the terminal",
	SilenceUsage: true,
	PersistentPreRun: func(cmd *cobra.Command, args []string) {
		// Best-effort: label auth failures with the logged-in account.
		if cfg, err := config.Load(); err == nil && cfg.Email != "" {
			api.SetIdentityHint(cfg.Email)
		}
	},
	Long: `
██╗  ██╗ ██████╗  ██████╗ ██╗     ██████╗  █████╗ ███████╗███████╗
██║ ██╔╝██╔═══██╗██╔═══██╗██║     ██╔══██╗██╔══██╗██╔════╝██╔════╝
█████╔╝ ██║   ██║██║   ██║██║     ██████╔╝███████║███████╗█████╗
██╔═██╗ ██║   ██║██║   ██║██║     ██╔══██╗██╔══██║╚════██║██╔══╝
██║  ██╗╚██████╔╝╚██████╔╝███████╗██████╔╝██║  ██║███████║███████╗
╚═╝  ╚═╝ ╚═════╝  ╚═════╝ ╚══════╝╚═════╝ ╚═╝  ╚═╝╚══════╝╚══════╝

Backend as a Service for mobile developers.
Docs: https://docs.koolbase.com
`,
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func init() {
	rootCmd.AddCommand(loginCmd, snapshotCmd)
	rootCmd.AddCommand(deployCmd)
	rootCmd.AddCommand(invokeCmd)
	rootCmd.AddCommand(logsCmd)
	rootCmd.AddCommand(functionsCmd)
	rootCmd.AddCommand(cronsCmd)
	rootCmd.AddCommand(secretsCmd)
	rootCmd.AddCommand(bundleCmd)
	rootCmd.AddCommand(dlqCmd)
	rootCmd.AddCommand(triggersCmd)
	rootCmd.AddCommand(uniqueConstraintsCmd)
	rootCmd.AddCommand(vectorFieldsCmd)
	rootCmd.AddCommand(pushCmd)
	rootCmd.AddCommand(engineCmd)
	rootCmd.AddCommand(buildCmd)
	rootCmd.AddCommand(createCmd)
}
