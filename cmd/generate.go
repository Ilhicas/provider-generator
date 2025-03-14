package cmd

import (
	"fmt"
	"os"

	"github.com/Ilhicas/provider-generator/internal/generator"
	"github.com/spf13/cobra"
)

var (
	specURL      string
	specFile     string
	outputDir    string
	providerName string
	configFile   string
)

var generateCmd = &cobra.Command{
	Use:   "generate",
	Short: "Generate Terraform provider code from OpenAPI spec",
	Long:  `Generate Terraform provider Go code from an OpenAPI specification.`,
	PreRunE: func(cmd *cobra.Command, args []string) error {
		// Validate input: we need either a URL or a file path
		if specURL == "" && specFile == "" {
			return fmt.Errorf("either --url or --file must be specified")
		}

		if specURL != "" && specFile != "" {
			return fmt.Errorf("only one of --url or --file can be specified")
		}

		if providerName == "" {
			return fmt.Errorf("--provider-name is required")
		}

		// Ensure output directory exists
		if outputDir == "" {
			outputDir = "./generated"
		}

		if _, err := os.Stat(outputDir); os.IsNotExist(err) {
			if err := os.MkdirAll(outputDir, 0755); err != nil {
				return fmt.Errorf("failed to create output directory: %w", err)
			}
		}

		return nil
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		gen := generator.New(providerName, outputDir, configFile)

		if specURL != "" {
			return gen.GenerateFromURL(specURL)
		}

		return gen.GenerateFromFile(specFile)
	},
}

func init() {
	rootCmd.AddCommand(generateCmd)

	// Add flags for the generate command
	generateCmd.Flags().StringVar(&specURL, "url", "", "URL to the OpenAPI specification")
	generateCmd.Flags().StringVar(&specFile, "file", "", "File path to the OpenAPI specification")
	generateCmd.Flags().StringVar(&outputDir, "output", "./generated", "Directory where the generated provider code will be placed")
	generateCmd.Flags().StringVar(&providerName, "provider-name", "", "Name of the Terraform provider to generate")
	generateCmd.Flags().StringVar(&configFile, "config", "", "Path to generator config file (YAML)")
}
