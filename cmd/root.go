package cmd

import (
	"fmt"
	"os"
	"strings"

	"github.com/Ilhicas/provider-generator/internal/generator"
	"github.com/spf13/cobra"
)

// Flag variables
var installDependencies bool

var rootCmd = &cobra.Command{
	Use:   "provider-generator",
	Short: "Generate Terraform provider code from OpenAPI specs",
	Long: `A CLI tool to generate Terraform provider Go code from OpenAPI specifications.
The tool accepts OpenAPI specs from either a URL or a local file.`,
}

// Execute adds all child commands to the root command and sets flags appropriately.
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}

func init() {
	// Global flags
	rootCmd.PersistentFlags().BoolVar(&installDependencies, "install-deps", false, "Automatically install required dependencies if missing")

	// Add a pre-run hook to check dependencies
	rootCmd.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		// Skip dependency check for help and completion commands
		if cmd.Name() == "help" || strings.HasPrefix(cmd.Name(), "completion") {
			return nil
		}

		// Only install dependencies if --install-deps flag is explicitly passed
		if installDependencies {
			fmt.Println("Installing missing dependencies because --install-deps flag was provided...")
			return generator.EnsurePackageInstalled()
		}

		// Otherwise, just verify dependencies exist without installing them
		return generator.VerifyDependencies()
	}
}
