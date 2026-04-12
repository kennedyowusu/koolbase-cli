package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var pushCmd = &cobra.Command{
	Use:   "push",
	Short: "Push a code patch to your Koolbase app",
	Long:  `Compile, sign, and upload a code patch to Koolbase. The patch will be delivered to your app at next launch without a store release.`,
	RunE:  runPush,
}

var (
	pushSnapshot   string
	pushTarget     string
	pushReplace    string
	pushAppID      string
	pushChannel    string
	pushPrivateKey string
	pushSlot       int64
)

func init() {
	pushCmd.Flags().StringVar(&pushSnapshot, "snapshot", "", "Path to the AOT snapshot (.so file)")
	pushCmd.Flags().StringVar(&pushTarget, "target", "", "Symbol to replace (e.g. StandardPricing.calculate)")
	pushCmd.Flags().StringVar(&pushReplace, "replace", "", "Symbol to use as replacement (e.g. DiscountPricing.calculate)")
	pushCmd.Flags().StringVar(&pushAppID, "app", "", "Your Koolbase app ID")
	pushCmd.Flags().StringVar(&pushChannel, "channel", "stable", "Release channel (stable, beta)")
	pushCmd.Flags().StringVar(&pushPrivateKey, "key", "private.key", "Path to Ed25519 private key")
	pushCmd.Flags().Int64Var(&pushSlot, "slot", 304, "Dispatch table slot index")

	pushCmd.MarkFlagRequired("snapshot")
	pushCmd.MarkFlagRequired("target")
	pushCmd.MarkFlagRequired("replace")
	pushCmd.MarkFlagRequired("app")
}

func runPush(cmd *cobra.Command, args []string) error {
	if _, err := os.Stat(pushSnapshot); os.IsNotExist(err) {
		return fmt.Errorf("snapshot file not found: %s", pushSnapshot)
	}

	fmt.Println("Analyzing snapshot...")

	info, err := analyzeSnapshot(pushSnapshot, pushTarget, pushReplace)
	if err != nil {
		return fmt.Errorf("snapshot analysis failed: %w", err)
	}

	fmt.Printf("  build_id:   0x%x\n", info.buildID)
	fmt.Printf("  nm_snap:    0x%x\n", info.nmSnap)
	fmt.Printf("  nm_target:  0x%x\n", info.nmTarget)
	fmt.Printf("  nm_replace: 0x%x\n", info.nmReplace)
	fmt.Printf("  instr_size: 0x%x\n", info.instrSize)

	fmt.Println("\nGenerating patch manifest...")

	patch, err := generatePatch(info, pushPrivateKey, pushSlot)
	if err != nil {
		return fmt.Errorf("patch generation failed: %w", err)
	}

	fmt.Printf("  Patch size: %d bytes\n", len(patch))
	fmt.Printf("  build_id:   0x%x\n", info.buildID)

	// Write patch locally for now
	outPath := pushAppID + "-" + pushChannel + ".kbpatch"
	if err := os.WriteFile(outPath, patch, 0644); err != nil {
		return fmt.Errorf("failed to write patch: %w", err)
	}

	fmt.Printf("\nPatch written to: %s\n", outPath)
	fmt.Println("Upload to Koolbase CDN: coming soon")

	return nil
}
