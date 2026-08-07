package cmd

import (
	"fmt"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/kennedyowusu/koolbase-cli/internal/version"
)

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the Koolbase CLI version",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Printf("Koolbase CLI %s (%s/%s)\n", version.Version, runtime.GOOS, runtime.GOARCH)
	},
}

func init() {
	rootCmd.AddCommand(versionCmd)
	// `--version` and `version` must not disagree about how to state the
	// same fact.
	rootCmd.SetVersionTemplate(
		fmt.Sprintf("Koolbase CLI %s (%s/%s)\n", version.Version, runtime.GOOS, runtime.GOARCH))
}
