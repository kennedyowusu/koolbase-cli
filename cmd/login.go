package cmd

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"syscall"

	"github.com/kennedyowusu/koolbase-cli/internal/api"
	"github.com/kennedyowusu/koolbase-cli/internal/config"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var loginCmd = &cobra.Command{
	Use:   "login",
	Short: "Authenticate with your Koolbase account",
	RunE: func(cmd *cobra.Command, args []string) error {
		reader := bufio.NewReader(os.Stdin)

		fmt.Print("Email: ")
		email, _ := reader.ReadString('\n')
		email = strings.TrimSpace(email)

		fmt.Print("Password: ")
		passwordBytes, err := term.ReadPassword(int(syscall.Stdin))
		fmt.Println()
		if err != nil {
			return fmt.Errorf("failed to read password: %w", err)
		}
		password := strings.TrimSpace(string(passwordBytes))

		client := api.NewClient("", "")
		resp, err := client.Login(email, password)
		if err != nil {
			return err
		}

		cfg := &config.Config{
			APIKey:  resp.AccessToken,
			BaseURL: "https://api.koolbase.com",
		}

		if err := config.Save(cfg); err != nil {
			return fmt.Errorf("failed to save config: %w", err)
		}

		fmt.Printf("\n Logged in as %s\n", resp.User.Email)
		fmt.Println("Run `koolbase functions list --project <project_id>` to see your functions.")
		return nil
	},
}
