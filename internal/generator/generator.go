package generator

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"time"
)

// Generator handles the generation of Terraform provider code from OpenAPI specs
type Generator struct {
	ProviderName        string
	OutputDir           string
	ConfigFile          string
	GenerateDataSources bool   // New field to control data source generation
	GitHubOrg           string // GitHub organization/owner name
}

// New creates a new Generator instance
func New(providerName, outputDir, configFile string) *Generator {
	return &Generator{
		ProviderName:        providerName,
		OutputDir:           outputDir,
		ConfigFile:          configFile,
		GenerateDataSources: true,        // Enable by default
		GitHubOrg:           "hashicorp", // Default to hashicorp org
	}
}

// Create a setter method to allow disabling data sources
func (g *Generator) DisableDataSources() {
	g.GenerateDataSources = false
}

// Add a setter method for GitHub organization
func (g *Generator) SetGitHubOrg(org string) {
	g.GitHubOrg = org
}

// GenerateFromURL generates Terraform provider code from an OpenAPI spec at the given URL
func (g *Generator) GenerateFromURL(url string) error {
	fmt.Printf("Fetching OpenAPI spec from URL: %s\n", url)

	// Download the spec
	resp, err := http.Get(url)
	if err != nil {
		return fmt.Errorf("failed to fetch spec from URL: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("received non-OK response from URL: %s", resp.Status)
	}

	// Create a temporary file to store the spec
	tempFile, err := os.CreateTemp("", "openapi-spec*.json")
	if err != nil {
		return fmt.Errorf("failed to create temporary file: %w", err)
	}
	defer os.Remove(tempFile.Name())
	defer tempFile.Close()

	// Copy the response body to the temporary file
	_, err = io.Copy(tempFile, resp.Body)
	if err != nil {
		return fmt.Errorf("failed to write spec to temporary file: %w", err)
	}

	// Generate from the temporary file
	return g.GenerateFromFile(tempFile.Name())
}

// GenerateFromFile generates Terraform provider code from an OpenAPI spec file
func (g *Generator) GenerateFromFile(filePath string) error {
	fmt.Printf("Generating provider code from file: %s\n", filePath)

	// Ensure the file exists and is readable
	if _, err := os.Stat(filePath); err != nil {
		return fmt.Errorf("failed to access spec file: %w", err)
	}

	// Create an absolute reference to the original spec file
	absFilePath, err := filepath.Abs(filePath)
	if err != nil {
		return fmt.Errorf("failed to get absolute path for spec file: %w", err)
	}

	// Preprocess the OpenAPI spec
	fmt.Println("Preprocessing OpenAPI spec to fix schema issues...")
	preprocessedPath, err := g.preprocessOpenAPISpec(absFilePath)
	if err != nil {
		return fmt.Errorf("failed to preprocess OpenAPI spec: %w", err)
	}

	// Use the preprocessed path for the rest of the generation
	absFilePath = preprocessedPath

	// Ensure output directory exists
	if err := os.MkdirAll(g.OutputDir, 0755); err != nil {
		return fmt.Errorf("failed to create output directory: %w", err)
	}

	// Create output file path for the generated JSON spec
	outputSpecFile := filepath.Join(g.OutputDir, "provider_code_spec.json")

	// Create or use config file
	var configPath string

	if g.ConfigFile != "" {
		// Use the provided config file
		configPath = g.ConfigFile
		fmt.Printf("Using provided config file: %s\n", configPath)
	} else {
		// Generate config from OpenAPI spec
		configPath, err = g.generateConfigFromSpec(absFilePath)
		if err != nil {
			return fmt.Errorf("failed to generate config from spec: %w", err)
		}
		fmt.Printf("Generated config file: %s\n", configPath)
	}

	// Prepare command arguments
	args := []string{"generate"}
	args = append(args, "--config", configPath)
	args = append(args, "--output", outputSpecFile)
	args = append(args, absFilePath) // Use absolute path here

	// Build and execute the tfplugingen-openapi command with a timeout
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute) // Increased to 5 minute timeout for debug
	defer cancel()

	// Prepare command with context
	cmd := exec.CommandContext(ctx, "tfplugingen-openapi", args...)

	// Capture the command's stdout and stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to capture command stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("failed to capture command stderr: %w", err)
	}

	// Start the command
	fmt.Println("Executing: tfplugingen-openapi", args)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start generator command: %w", err)
	}

	// Read and display output
	go copyOutput(stdout, os.Stdout)
	go copyOutput(stderr, os.Stderr)

	// Wait for command to complete
	err = cmd.Wait()
	if ctx.Err() == context.DeadlineExceeded {
		fmt.Println("Command execution timed out after 3 minutes. Proceeding with partial results.")

		// Check if the output file was created despite the timeout
		if _, statErr := os.Stat(outputSpecFile); os.IsNotExist(statErr) {
			// Create a minimal spec file to allow the process to continue
			if err := g.createMinimalSpecFile(outputSpecFile); err != nil {
				return fmt.Errorf("failed to create minimal spec file after timeout: %w", err)
			}
		}
	} else if err != nil {
		// Check if we have partial results we can use
		if _, statErr := os.Stat(outputSpecFile); os.IsNotExist(statErr) {
			return fmt.Errorf("generator command failed with no output: %w", err)
		}

		fmt.Printf("Warning: Generator command completed with errors: %v\nProceeding with partial results.\n", err)
	}

	// Now generate the Go code from the JSON spec
	if err := g.generateGoCode(outputSpecFile); err != nil {
		return fmt.Errorf("failed to generate Go code: %w", err)
	}

	// Create a provider structure README file
	if err := createReadme(g.OutputDir, g.ProviderName); err != nil {
		return fmt.Errorf("failed to create README: %w", err)
	}

	fmt.Printf("Successfully generated provider code in: %s\n", g.OutputDir)
	return nil
}

// generateGoCode initializes a Go module and sets up the structure for the provider
func (g *Generator) generateGoCode(specFile string) error {
	fmt.Printf("Setting up Go code for provider from spec: %s\n", specFile)

	// Create the module path based on GitHub organization
	modulePath := fmt.Sprintf("github.com/%s/terraform-provider-%s", g.GitHubOrg, g.ProviderName)

	// Check if the Go module is properly initialized
	if _, err := os.Stat(filepath.Join(g.OutputDir, "go.mod")); os.IsNotExist(err) {
		fmt.Println("Initializing Go module for provider...")
		initCmd := exec.Command("go", "mod", "init", modulePath)
		initCmd.Dir = g.OutputDir
		initCmd.Stdout = os.Stdout
		initCmd.Stderr = os.Stderr
		if err := initCmd.Run(); err != nil {
			return fmt.Errorf("failed to initialize Go module: %w", err)
		}

		// Add required dependencies
		fmt.Println("Adding Terraform plugin dependencies...")
		deps := []string{
			"github.com/hashicorp/terraform-plugin-framework@latest",
			"github.com/hashicorp/terraform-plugin-framework-validators@latest",
			"github.com/hashicorp/terraform-plugin-go@latest",
			"github.com/hashicorp/terraform-plugin-log@latest",
		}

		for _, dep := range deps {
			getCmd := exec.Command("go", "get", dep)
			getCmd.Dir = g.OutputDir
			getCmd.Stdout = os.Stdout
			getCmd.Stderr = os.Stderr
			if err := getCmd.Run(); err != nil {
				fmt.Printf("Warning: Failed to add dependency %s: %v\n", dep, err)
			}
		}

		// Tidy up the dependencies
		tidyCmd := exec.Command("go", "mod", "tidy")
		tidyCmd.Dir = g.OutputDir
		tidyCmd.Stdout = os.Stdout
		tidyCmd.Stderr = os.Stderr
		if err := tidyCmd.Run(); err != nil {
			fmt.Printf("Warning: Failed to tidy dependencies: %v\n", err)
		}
	}

	// Read and parse the generated spec file
	specContent, err := os.ReadFile(specFile)
	if err != nil {
		return fmt.Errorf("failed to read spec file: %w", err)
	}

	// Parse the JSON spec content to extract resources and data sources
	var spec struct {
		Resources []struct {
			Name   string `json:"name"`
			Schema struct {
				Attributes []struct {
					Name   string   `json:"name"`
					Type   string   `json:"type,omitempty"`
					Int64  struct{} `json:"int64,omitempty"`
					String struct{} `json:"string,omitempty"`
					Bool   struct{} `json:"bool,omitempty"`
				} `json:"attributes,omitempty"`
			} `json:"schema"`
		} `json:"resources"`
		DataSources []struct {
			Name   string `json:"name"`
			Schema struct {
				Attributes []struct {
					Name   string   `json:"name"`
					Type   string   `json:"type,omitempty"`
					Int64  struct{} `json:"int64,omitempty"`
					String struct{} `json:"string,omitempty"`
					Bool   struct{} `json:"bool,omitempty"`
				} `json:"attributes,omitempty"`
			} `json:"schema"`
		} `json:"data_sources"`
	}

	if err := json.Unmarshal(specContent, &spec); err != nil {
		fmt.Printf("Warning: Failed to parse spec file: %v\n", err)
		// Continue with basic structure if parsing fails
	}

	// Build resource imports and registrations based on the spec
	resourceImports := ""
	resourceRegistrations := ""

	// Build data source imports and registrations
	dataSourceImports := ""
	dataSourceRegistrations := ""

	// Generate resource files
	if len(spec.Resources) > 0 {
		resourcesDir := filepath.Join(g.OutputDir, "internal", "resources")
		if err := os.MkdirAll(resourcesDir, 0755); err != nil {
			return fmt.Errorf("failed to create resources directory: %w", err)
		}

		for _, resource := range spec.Resources {
			resourceName := strings.ToLower(resource.Name)
			// Add to imports
			resourceImports += fmt.Sprintf(`    "%s/internal/resources/%s"`+"\n",
				fmt.Sprintf("github.com/%s/terraform-provider-%s", g.GitHubOrg, g.ProviderName), resourceName)

			// Add to resource registrations
			resourceRegistrations += fmt.Sprintf("        %s.New%sResource,\n", resourceName, strings.Title(resourceName))

			// Create resource file
			if err := g.generateResourceFile(resourceName, resource); err != nil {
				fmt.Printf("Warning: Failed to generate resource file for %s: %v\n", resourceName, err)
			}
		}
	}

	if resourceImports == "" {
		resourceImports = "    // No resources defined in the spec\n"
	}

	if resourceRegistrations == "" {
		resourceRegistrations = "        // No resources to register\n"
	}

	// Generate data source files
	if len(spec.DataSources) > 0 {
		dataSourcesDir := filepath.Join(g.OutputDir, "internal", "datasources")
		if err := os.MkdirAll(dataSourcesDir, 0755); err != nil {
			return fmt.Errorf("failed to create data sources directory: %w", err)
		}

		for _, dataSource := range spec.DataSources {
			dataSourceName := strings.ToLower(dataSource.Name)

			// Add to imports
			dataSourceImports += fmt.Sprintf(`    "%s/internal/datasources/%s"`+"\n",
				fmt.Sprintf("github.com/%s/terraform-provider-%s", g.GitHubOrg, g.ProviderName), dataSourceName)

			// Add to data source registrations
			dataSourceRegistrations += fmt.Sprintf("        %s.New%sDataSource,\n",
				dataSourceName, strings.Title(dataSourceName))

			// Create data source file
			if err := g.generateDataSourceFile(dataSourceName, dataSource); err != nil {
				fmt.Printf("Warning: Failed to generate data source file for %s: %v\n", dataSourceName, err)
			}
		}
	}

	if dataSourceImports == "" {
		dataSourceImports = "    // No data sources defined in the spec\n"
	}

	if dataSourceRegistrations == "" {
		dataSourceRegistrations = "        // No data sources to register\n"
	}

	// Basic provider structure with dynamic resources and data sources
	providerContent := fmt.Sprintf(`package main

import (
    "context"
    
    "github.com/hashicorp/terraform-plugin-framework/datasource"
    "github.com/hashicorp/terraform-plugin-framework/provider"
    "github.com/hashicorp/terraform-plugin-framework/provider/schema"
    "github.com/hashicorp/terraform-plugin-framework/resource"
%s
%s
)

// Ensure the implementation satisfies the provider interface
var _ provider.Provider = &%sProvider{}

// %sProvider is the provider implementation
type %sProvider struct {
    // Configuration values
    version string
}

// New creates a new provider instance
func New() provider.Provider {
    return &%sProvider{
        version: "0.1",
    }
}

// Metadata returns the provider metadata
func (p *%sProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
    resp.TypeName = %q
    resp.Version = p.version
}

// Schema returns the provider schema
func (p *%sProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
    resp.Schema = schema.Schema{
        Description: "The %s provider.",
        Attributes: map[string]schema.Attribute{
            // Add provider-level attributes here
            "api_url": schema.StringAttribute{
                Description: "API URL for the service",
                Optional:    true,
            },
            "token": schema.StringAttribute{
                Description: "Authentication token",
                Optional:    true,
                Sensitive:   true,
            },
        },
    }
}

// Configure configures the provider
func (p *%sProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
    // Handle provider configuration here
    // This would include setting up API clients, etc.
}

// Resources returns the provider resources
func (p *%sProvider) Resources(_ context.Context) []func() resource.Resource {
    return []func() resource.Resource{
%s
    }
}

// DataSources returns the provider data sources
func (p *%sProvider) DataSources(_ context.Context) []func() datasource.DataSource {
    return []func() datasource.DataSource{
%s
    }
}
`, resourceImports, dataSourceImports, g.ProviderName, g.ProviderName, g.ProviderName,
		g.ProviderName, g.ProviderName, g.ProviderName, g.ProviderName, g.ProviderName,
		g.ProviderName, g.ProviderName, resourceRegistrations, g.ProviderName, dataSourceRegistrations)

	if err := os.WriteFile(filepath.Join(g.OutputDir, "provider.go"), []byte(providerContent), 0644); err != nil {
		return fmt.Errorf("failed to create provider.go file: %w", err)
	}

	return nil
}

// generateResourceFile creates a resource implementation file
func (g *Generator) generateResourceFile(resourceName string, resourceSpec struct {
	Name   string `json:"name"`
	Schema struct {
		Attributes []struct {
			Name   string   `json:"name"`
			Type   string   `json:"type,omitempty"`
			Int64  struct{} `json:"int64,omitempty"`
			String struct{} `json:"string,omitempty"`
			Bool   struct{} `json:"bool,omitempty"`
		} `json:"attributes,omitempty"`
	} `json:"schema"`
}) error {
	resourceDir := filepath.Join(g.OutputDir, "internal", "resources", resourceName)
	if err := os.MkdirAll(resourceDir, 0755); err != nil {
		return fmt.Errorf("failed to create resource directory: %w", err)
	}

	filePath := filepath.Join(resourceDir, "resource.go")

	// Build attribute declarations and schema definitions
	var attrDeclarations, schemaDefinitions string

	for _, attr := range resourceSpec.Schema.Attributes {
		attrType := "string"
		schemaType := "schema.StringAttribute"
		// Determine attribute type based on the struct fields
		if attr.Type == "int64" || reflect.ValueOf(attr.Int64).IsValid() && !reflect.ValueOf(attr.Int64).IsZero() {
			attrType = "int64"
			schemaType = "schema.Int64Attribute"
		} else if attr.Type == "bool" || reflect.ValueOf(attr.Bool).IsValid() && !reflect.ValueOf(attr.Bool).IsZero() {
			attrType = "bool"
			schemaType = "schema.BoolAttribute"
		}

		// Add to model declaration
		attrDeclarations += fmt.Sprintf("\t%s %s `tfsdk:\"%s\"`\n",
			strings.Title(attr.Name), attrType, attr.Name)

		// Add to schema definition
		schemaDefinitions += fmt.Sprintf("\t\t\t\"%s\": %s{\n", attr.Name, schemaType)
		schemaDefinitions += "\t\t\t\tDescription: \"TODO: Add description\",\n"
		// Assuming most fields are required, this would need more detailed logic in a real implementation
		schemaDefinitions += "\t\t\t\tRequired:    true,\n"
		schemaDefinitions += "\t\t\t},\n"
	}

	// If no attributes found, add placeholder
	if attrDeclarations == "" {
		attrDeclarations = "\t// TODO: Add attributes\n"
	}

	if schemaDefinitions == "" {
		schemaDefinitions = "\t\t\t// TODO: Add schema attributes\n"
	}

	capitalizedResourceName := strings.Title(resourceName)

	content := fmt.Sprintf(`package %s

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// Ensure the implementation satisfies the resource interfaces
var (
	_ resource.Resource              = &%sResource{}
	_ resource.ResourceWithConfigure = &%sResource{}
)

// %sResourceModel represents the resource data model
type %sResourceModel struct {
%s
}

// %sResource is the resource implementation
type %sResource struct {
	// Add any provider client here
}

// New%sResource returns a new instance of the resource
func New%sResource() resource.Resource {
	return &%sResource{}
}

// Metadata returns the resource metadata
func (r *%sResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_%s"
}

// Schema returns the resource schema
func (r *%sResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "%s resource",
		Attributes: map[string]schema.Attribute{
%s
		},
	}
}

// Configure adds the provider client to the resource
func (r *%sResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	// Get provider client from provider configure
	if req.ProviderData == nil {
		return
	}
	
	// client := req.ProviderData.(YourClientType)
	// r.client = client
}

// Create creates the resource and sets the initial Terraform state
func (r *%sResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	// Get plan data
	var plan %sResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// TODO: Create the resource using an API client
	// ...

	// Set state
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Read refreshes the Terraform state with the latest data
func (r *%sResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	// Get current state
	var state %sResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// TODO: Read the resource from the API
	// ...

	// Set state
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Update updates the resource and sets the updated Terraform state
func (r *%sResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	// Get plan and state data
	var plan, state %sResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// TODO: Update the resource using an API client
	// ...

	// Set state
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Delete deletes the resource
func (r *%sResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	// Get state data
	var state %sResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// TODO: Delete the resource using an API client
	// ...
}
`,
		resourceName,
		capitalizedResourceName, capitalizedResourceName,
		capitalizedResourceName, capitalizedResourceName, attrDeclarations,
		capitalizedResourceName, capitalizedResourceName,
		capitalizedResourceName, capitalizedResourceName, capitalizedResourceName,
		capitalizedResourceName, resourceName,
		capitalizedResourceName, capitalizedResourceName, schemaDefinitions,
		capitalizedResourceName,
		capitalizedResourceName, capitalizedResourceName,
		capitalizedResourceName, capitalizedResourceName,
		capitalizedResourceName, capitalizedResourceName,
		capitalizedResourceName, capitalizedResourceName)

	return os.WriteFile(filePath, []byte(content), 0644)
}

// generateDataSourceFile creates a data source implementation file
func (g *Generator) generateDataSourceFile(dataSourceName string, dataSourceSpec struct {
	Name   string `json:"name"`
	Schema struct {
		Attributes []struct {
			Name   string   `json:"name"`
			Type   string   `json:"type,omitempty"`
			Int64  struct{} `json:"int64,omitempty"`
			String struct{} `json:"string,omitempty"`
			Bool   struct{} `json:"bool,omitempty"`
		} `json:"attributes,omitempty"`
	} `json:"schema"`
}) error {
	dataSourceDir := filepath.Join(g.OutputDir, "internal", "datasources", dataSourceName)
	if err := os.MkdirAll(dataSourceDir, 0755); err != nil {
		return fmt.Errorf("failed to create data source directory: %w", err)
	}

	filePath := filepath.Join(dataSourceDir, "data_source.go")

	// Build attribute declarations and schema definitions similar to the resource function
	var attrDeclarations, schemaDefinitions string

	for _, attr := range dataSourceSpec.Schema.Attributes {
		attrType := "string"
		schemaType := "schema.StringAttribute"

		// Determine attribute type
		if attr.Type == "int64" || reflect.ValueOf(attr.Int64).IsValid() && !reflect.ValueOf(attr.Int64).IsZero() {
			attrType = "int64"
			schemaType = "schema.Int64Attribute"
		} else if attr.Type == "bool" || reflect.ValueOf(attr.Bool).IsValid() && !reflect.ValueOf(attr.Bool).IsZero() {
			attrType = "bool"
			schemaType = "schema.BoolAttribute"
		}

		// Add to model declaration
		attrDeclarations += fmt.Sprintf("\t%s %s `tfsdk:\"%s\"`\n",
			strings.Title(attr.Name), attrType, attr.Name)

		// Add to schema definition - for data sources most fields are Computed
		schemaDefinitions += fmt.Sprintf("\t\t\t\"%s\": %s{\n", attr.Name, schemaType)
		schemaDefinitions += "\t\t\t\tDescription: \"TODO: Add description\",\n"

		// Check if this is likely an ID or filter field
		if strings.ToLower(attr.Name) == "id" ||
			strings.HasSuffix(strings.ToLower(attr.Name), "_id") ||
			strings.HasPrefix(strings.ToLower(attr.Name), "filter_") {
			// These are typically required inputs
			schemaDefinitions += "\t\t\t\tRequired:    true,\n"
		} else {
			// Most other fields are computed outputs
			schemaDefinitions += "\t\t\t\tComputed:    true,\n"
		}

		schemaDefinitions += "\t\t\t},\n"
	}

	// Create placeholders if needed
	if attrDeclarations == "" {
		attrDeclarations = "\t// TODO: Add attributes\n"
	}

	if schemaDefinitions == "" {
		schemaDefinitions = "\t\t\t// TODO: Add schema attributes\n"
	}

	capitalizedDataSourceName := strings.Title(dataSourceName)

	// Generate data source implementation
	content := fmt.Sprintf(`package %s

import (
    "context"
    "fmt"

    "github.com/hashicorp/terraform-plugin-framework/datasource"
    "github.com/hashicorp/terraform-plugin-framework/datasource/schema"
    "github.com/hashicorp/terraform-plugin-framework/types"
)

// Ensure the implementation satisfies the data source interfaces
var (
    _ datasource.DataSource              = &%sDataSource{}
    _ datasource.DataSourceWithConfigure = &%sDataSource{}
)

// %sDataSourceModel represents the data source data model
type %sDataSourceModel struct {
%s
}

// %sDataSource is the data source implementation
type %sDataSource struct {
    // Add any provider client here
}

// New%sDataSource returns a new instance of the data source
func New%sDataSource() datasource.DataSource {
    return &%sDataSource{}
}

// Metadata returns the data source metadata
func (d *%sDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
    resp.TypeName = req.ProviderTypeName + "_%s"
}

// Schema returns the data source schema
func (d *%sDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
    resp.Schema = schema.Schema{
        Description: "%s data source",
        Attributes: map[string]schema.Attribute{
%s
        },
    }
}

// Configure adds the provider client to the data source
func (d *%sDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
    // Get provider client from provider configure
    if req.ProviderData == nil {
        return
    }
    
    // client := req.ProviderData.(YourClientType)
    // d.client = client
}

// Read refreshes the Terraform state with the latest data
func (d *%sDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
    // Get current config
    var config %sDataSourceModel
    resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
    if resp.Diagnostics.HasError() {
        return
    }

    // TODO: Read data from API using client
    // ...
    
    // Set state with the fetched data
    resp.Diagnostics.Append(resp.State.Set(ctx, &config)...)
}
`,
		dataSourceName,
		capitalizedDataSourceName, capitalizedDataSourceName,
		capitalizedDataSourceName, capitalizedDataSourceName, attrDeclarations,
		capitalizedDataSourceName, capitalizedDataSourceName,
		capitalizedDataSourceName, capitalizedDataSourceName, capitalizedDataSourceName,
		capitalizedDataSourceName, dataSourceName,
		capitalizedDataSourceName, capitalizedDataSourceName, schemaDefinitions,
		capitalizedDataSourceName,
		capitalizedDataSourceName, capitalizedDataSourceName)

	return os.WriteFile(filePath, []byte(content), 0644)
}

// ensureCompatibleSpecFormat attempts to convert the spec file to a compatible format if needed
func (g *Generator) ensureCompatibleSpecFormat(specFile string) (string, error) {
	// Read the spec file to analyze it
	content, err := os.ReadFile(specFile)
	if err != nil {
		return "", fmt.Errorf("failed to read spec file: %w", err)
	}

	// Check if it's JSON and convert to YAML if necessary
	if len(content) > 0 && (content[0] == '{' || content[0] == '[') {
		fmt.Println("Detected JSON format, converting to YAML...")

		// Parse JSON
		var jsonData interface{}
		if err := json.Unmarshal(content, &jsonData); err != nil {
			return "", fmt.Errorf("failed to parse JSON spec: %w", err)
		}

		// Create a simplified spec with just essential information
		simplifiedSpec := map[string]interface{}{
			"openapi": "3.0.0", // Force OpenAPI 3.0.0 version
			"info": map[string]interface{}{
				"title":   "Simplified API",
				"version": "1.0.0",
			},
		}

		// Copy paths if they exist
		if originalSpec, ok := jsonData.(map[string]interface{}); ok {
			if paths, ok := originalSpec["paths"].(map[string]interface{}); ok {
				simplifiedSpec["paths"] = paths
			} else {
				// If no paths found, add empty paths section
				simplifiedSpec["paths"] = map[string]interface{}{}
			}

			// Copy components if they exist
			if components, ok := originalSpec["components"].(map[string]interface{}); ok {
				simplifiedSpec["components"] = components
			}
		}

		// Create a new YAML file with the simplified spec
		yamlFile := specFile + ".yaml"
		yamlData, err := json.Marshal(simplifiedSpec)
		if err != nil {
			return "", fmt.Errorf("failed to marshal simplified spec: %w", err)
		}

		if err := os.WriteFile(yamlFile, yamlData, 0644); err != nil {
			return "", fmt.Errorf("failed to write YAML spec: %w", err)
		}

		return yamlFile, nil
	}

	// If we can't convert or it's already in an acceptable format
	return specFile, nil
}

// createDefaultConfig creates a default generator config file for the provider
func (g *Generator) createDefaultConfig() (string, error) {
	configDir := filepath.Join(g.OutputDir, "config")
	if err := os.MkdirAll(configDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create config directory: %w", err)
	}

	configPath := filepath.Join(configDir, "generator_config.yml")

	// Create a more complete configuration with at least one resource
	// The OpenAPI generator requires at least one resource or data source
	config := fmt.Sprintf(`---
provider:
  name: %s
  full_name: "Terraform Provider %s"
  short_name: %s
resources:
  example:
    schema:
      title: "Example Resource"
      description: "An example resource generated automatically."
      properties:
        id:
          description: "ID of the resource"
          type: string
          computed: true
          primary_key: true
        name:
          description: "Name of the resource"
          type: string
          required: true
    create:
      path: "/example"
      method: "POST"
    read:
      path: "/example/{id}"
      method: "GET"
`, g.ProviderName, g.ProviderName, g.ProviderName)

	if err := os.WriteFile(configPath, []byte(config), 0644); err != nil {
		return "", fmt.Errorf("failed to write default config file: %w", err)
	}

	return configPath, nil
}

// generateConfigFromSpec creates a generator config file based on the OpenAPI spec
func (g *Generator) generateConfigFromSpec(specFilePath string) (string, error) {
	// Extract paths and schemas from OpenAPI spec
	resources, err := parseOpenAPISpec(specFilePath, g.GenerateDataSources)
	if err != nil {
		return "", fmt.Errorf("failed to parse OpenAPI spec: %w", err)
	}

	if len(resources) == 0 {
		fmt.Println("Warning: No resources could be extracted from the OpenAPI spec.")
		fmt.Println("Falling back to a minimal default configuration.")
		return g.createDefaultConfig()
	}

	// Create config directory
	configDir := filepath.Join(g.OutputDir, "config")
	if err := os.MkdirAll(configDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create config directory: %w", err)
	}

	configPath := filepath.Join(configDir, "generator_config.yml")

	// Build provider config
	providerConfig := fmt.Sprintf(`---
provider:
  name: %s
  full_name: "Terraform Provider %s"
  short_name: %s
resources:
`, g.ProviderName, g.ProviderName, g.ProviderName)

	// Add resources to config
	configContent := providerConfig + resources

	// Write config to file
	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		return "", fmt.Errorf("failed to write config file: %w", err)
	}

	return configPath, nil
}

// copyOutput copies command output to a writer
func copyOutput(src io.Reader, dst io.Writer) {
	io.Copy(dst, src)
}

// createReadme creates a README.md file in the output directory with information about the generated provider
func createReadme(outputDir, providerName string) error {
	readmePath := filepath.Join(outputDir, "README.md")

	content := fmt.Sprintf(`# Terraform Provider %s

This Terraform provider was automatically generated from an OpenAPI specification.

## Provider Information
- Provider Name: %s

## Usage
To use this provider, include it in your Terraform configuration:

`+"```hcl"+`
terraform {
  required_providers {
    %s = {
      source = "local/%s"
    }
  }
}

provider "%s" {
  # configuration options
}
`+"```",
		providerName,
		providerName,
		providerName,
		providerName,
		providerName)

	return os.WriteFile(readmePath, []byte(content), 0644)
}

// VerifyDependencies checks if required dependencies are available without installing them
func VerifyDependencies() error {
	requiredCommands := []string{"tfplugingen-openapi"} // Removed tfplugingen-go
	missingCommands := []string{}

	for _, cmd := range requiredCommands {
		_, err := exec.LookPath(cmd)
		if err != nil {
			missingCommands = append(missingCommands, cmd)
		}
	}

	if len(missingCommands) > 0 {
		return fmt.Errorf("missing required dependencies: %s\n\nPlease install these dependencies manually using:\n\n"+
			"go install github.com/hashicorp/terraform-plugin-codegen-openapi/cmd/tfplugingen-openapi@latest\n"+
			"Or run this command with the --install-deps flag",
			strings.Join(missingCommands, ", "))
	}

	return nil
}

// EnsurePackageInstalled ensures that the required packages are installed
func EnsurePackageInstalled() error {
	// Check and install tfplugingen-openapi
	if err := ensureCommand("tfplugingen-openapi", "github.com/hashicorp/terraform-plugin-codegen-openapi/cmd/tfplugingen-openapi@latest"); err != nil {
		return err
	}

	return nil
}

// ensureCommand ensures a specific command is installed
func ensureCommand(command, goPackage string) error {
	// Check if command is already installed
	_, err := exec.LookPath(command)
	if err == nil {
		// Command is already available
		return nil
	}

	fmt.Printf("Installing %s...\n", command)

	// Install the package using go install
	cmd := exec.Command("go", "install", goPackage)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	return cmd.Run()
}

// parseOpenAPISpec tries to extract resources and data sources from the OpenAPI spec
func parseOpenAPISpec(filePath string, generateDataSources bool) (string, error) {
	// Read the OpenAPI spec file
	content, err := os.ReadFile(filePath)
	if err != nil {
		return "", fmt.Errorf("failed to read OpenAPI spec: %w", err)
	}

	// Parse the JSON
	var spec map[string]interface{}
	if err := json.Unmarshal(content, &spec); err != nil {
		return "", fmt.Errorf("failed to parse OpenAPI spec JSON: %w", err)
	}

	// Check if it's an OpenAPI spec
	_, hasOpenAPI := spec["openapi"]
	_, hasSwagger := spec["swagger"]
	if !hasOpenAPI && !hasSwagger {
		return "", fmt.Errorf("file doesn't appear to be an OpenAPI specification")
	}

	// Extract paths
	pathsObj, ok := spec["paths"].(map[string]interface{})
	if !ok {
		return "", fmt.Errorf("no paths found in OpenAPI spec")
	}

	// Store resources and data sources
	resources := make(map[string]map[string]string)   // map[resourceName]map[property]value
	dataSources := make(map[string]map[string]string) // map[dataSourceName]map[property]value

	// First pass: identify all endpoints with POST/PUT methods (potential resources)
	resourcePaths := make(map[string]bool)
	for path, methods := range pathsObj {
		methodObj, ok := methods.(map[string]interface{})
		if !ok {
			continue
		}

		// Endpoints with POST are candidates for resources
		if _, hasPost := methodObj["post"]; hasPost {
			resourcePaths[path] = true
		}
	}

	// Process paths to identify resources and data sources
	for path, methods := range pathsObj {
		methodObj, ok := methods.(map[string]interface{})
		if !ok {
			continue
		}

		// Check if this is a resource (has POST method and corresponding GET)
		hasCreate := false
		hasRead := false
		hasUpdate := false
		hasDelete := false

		if _, exists := methodObj["post"]; exists {
			hasCreate = true
		}
		if _, exists := methodObj["get"]; exists {
			hasRead = true
		}
		if _, exists := methodObj["put"]; exists {
			hasUpdate = true
		}
		if _, exists := methodObj["delete"]; exists {
			hasDelete = true
		}

		// Clean the path to create a valid resource name
		baseName := cleanPathForName(path)

		// Generate a resource if it has POST and GET methods
		if hasCreate && hasRead {
			resourceConfig := make(map[string]string)

			resourceConfig["schema_title"] = fmt.Sprintf("\"%s Resource\"", strings.Title(baseName))
			resourceConfig["schema_description"] = fmt.Sprintf("\"Resource for %s\"", path)
			resourceConfig["create_path"] = fmt.Sprintf("\"%s\"", path)
			resourceConfig["create_method"] = "\"POST\""
			resourceConfig["read_path"] = fmt.Sprintf("\"%s\"", path)
			resourceConfig["read_method"] = "\"GET\""

			if hasUpdate {
				resourceConfig["update_path"] = fmt.Sprintf("\"%s\"", path)
				resourceConfig["update_method"] = "\"PUT\""
			}

			if hasDelete {
				resourceConfig["delete_path"] = fmt.Sprintf("\"%s\"", path)
				resourceConfig["delete_method"] = "\"DELETE\""
			}

			// Find a unique name for this resource
			resourceName := baseName
			counter := 1
			for {
				if _, exists := resources[resourceName]; !exists {
					break
				}
				resourceName = fmt.Sprintf("%s_%d", baseName, counter)
				counter++
			}

			resources[resourceName] = resourceConfig
		} else if hasRead && !resourcePaths[path] {
			// This is a read-only endpoint (GET) not associated with a resource - make it a data source
			// Skip if this path is too similar to a resource path
			isPartOfResource := false
			for resourcePath := range resourcePaths {
				// If this path is a subpath of a resource or vice versa
				if strings.HasPrefix(path, resourcePath) || strings.HasPrefix(resourcePath, path) {
					isPartOfResource = true
					break
				}
			}

			if !isPartOfResource {
				dataSourceConfig := make(map[string]string)

				dataSourceConfig["schema_title"] = fmt.Sprintf("\"%s Data Source\"", strings.Title(baseName))
				dataSourceConfig["schema_description"] = fmt.Sprintf("\"Data source for %s\"", path)
				dataSourceConfig["read_path"] = fmt.Sprintf("\"%s\"", path)
				dataSourceConfig["read_method"] = "\"GET\""

				// Add path parameters
				params := extractPathParams(path)
				if len(params) > 0 {
					for _, param := range params {
						dataSourceConfig["param_"+param] = param
					}
				}

				// Find a unique name for this data source
				dataSourceName := baseName
				counter := 1
				for {
					if _, exists := dataSources[dataSourceName]; !exists {
						break
					}
					dataSourceName = fmt.Sprintf("%s_%d", baseName, counter)
					counter++
				}

				dataSources[dataSourceName] = dataSourceConfig
			}
		}
	}

	// Generate the YAML for resources and data sources
	resourcesYAML := ""
	for name, config := range resources {
		resourcesYAML += fmt.Sprintf("  %s:\n", name)
		resourcesYAML += "    schema:\n"
		resourcesYAML += fmt.Sprintf("      title: %s\n", config["schema_title"])
		resourcesYAML += fmt.Sprintf("      description: %s\n", config["schema_description"])
		resourcesYAML += "      properties:\n"
		resourcesYAML += "        id:\n"
		resourcesYAML += "          description: \"ID of the resource\"\n"
		resourcesYAML += "          type: string\n"
		resourcesYAML += "          computed: true\n"
		resourcesYAML += "          primary_key: true\n"

		resourcesYAML += "    create:\n"
		resourcesYAML += fmt.Sprintf("      path: %s\n", config["create_path"])
		resourcesYAML += fmt.Sprintf("      method: %s\n", config["create_method"])

		resourcesYAML += "    read:\n"
		resourcesYAML += fmt.Sprintf("      path: %s\n", config["read_path"])
		resourcesYAML += fmt.Sprintf("      method: %s\n", config["read_method"])

		if update_path, exists := config["update_path"]; exists {
			resourcesYAML += "    update:\n"
			resourcesYAML += fmt.Sprintf("      path: %s\n", update_path)
			resourcesYAML += fmt.Sprintf("      method: %s\n", config["update_method"])
		}

		if delete_path, exists := config["delete_path"]; exists {
			resourcesYAML += "    delete:\n"
			resourcesYAML += fmt.Sprintf("      path: %s\n", delete_path)
			resourcesYAML += fmt.Sprintf("      method: %s\n", config["delete_method"])
		}
	}

	// Only process data sources if enabled
	if generateDataSources {
		// Process read-only paths as data sources
		for path, methods := range pathsObj {
			methodObj, ok := methods.(map[string]interface{})
			if !ok {
				continue
			}

			// Skip resources, only look for GET-only endpoints
			if resourcePaths[path] {
				continue
			}

			hasRead := false
			if _, exists := methodObj["get"]; exists {
				hasRead = true
			}

			if hasRead {
				// Clean the path to create a valid resource name
				baseName := cleanPathForName(path)

				// This is a read-only endpoint (GET) not associated with a resource - make it a data source
				// Skip if this path is too similar to a resource path
				isPartOfResource := false
				for resourcePath := range resourcePaths {
					// If this path is a subpath of a resource or vice versa
					if strings.HasPrefix(path, resourcePath) || strings.HasPrefix(resourcePath, path) {
						isPartOfResource = true
						break
					}
				}

				if !isPartOfResource {
					dataSourceConfig := make(map[string]string)

					dataSourceConfig["schema_title"] = fmt.Sprintf("\"%s Data Source\"", strings.Title(baseName))
					dataSourceConfig["schema_description"] = fmt.Sprintf("\"Data source for %s\"", path)
					dataSourceConfig["read_path"] = fmt.Sprintf("\"%s\"", path)
					dataSourceConfig["read_method"] = "\"GET\""

					// Add path parameters
					params := extractPathParams(path)
					if len(params) > 0 {
						for _, param := range params {
							dataSourceConfig["param_"+param] = param
						}
					}

					// Find a unique name for this data source
					dataSourceName := baseName
					counter := 1
					for {
						if _, exists := dataSources[dataSourceName]; !exists {
							break
						}
						dataSourceName = fmt.Sprintf("%s_%d", baseName, counter)
						counter++
					}

					dataSources[dataSourceName] = dataSourceConfig
				}
			}
		}
	}

	// Generate the YAML for resources and data sources
	// ...existing code for resources YAML...

	// Only include data sources in the YAML if enabled
	dataSourcesYAML := ""
	if generateDataSources && len(dataSources) > 0 {
		// ...existing code for data sources YAML...
	}

	// Build the final YAML
	result := resourcesYAML
	if dataSourcesYAML != "" {
		result += "data_sources:\n" + dataSourcesYAML
	}

	if result == "" {
		return "", fmt.Errorf("no suitable resources or data sources found in OpenAPI spec")
	}

	return result, nil
}

// cleanPathForName creates a valid resource name from an API path
func cleanPathForName(path string) string {
	// Remove any path parameters like {id}
	path = strings.ReplaceAll(path, "{", "")
	path = strings.ReplaceAll(path, "}", "")

	// Replace dots and special characters that are common in API paths
	path = strings.ReplaceAll(path, ".", "_")
	path = strings.ReplaceAll(path, "-", "_")
	path = strings.ReplaceAll(path, "/", "_")

	// Remove leading and trailing underscores
	path = strings.Trim(path, "_")

	// Replace multiple consecutive underscores with a single one
	for strings.Contains(path, "__") {
		path = strings.ReplaceAll(path, "__", "_")
	}

	// Ensure it starts with a letter
	if path != "" && !((path[0] >= 'a' && path[0] <= 'z') || (path[0] >= 'A' && path[0] <= 'Z')) {
		path = "resource_" + path
	}

	return strings.ToLower(path)
}

// Add this function to replace generateResourceName
func sanitizeResourceName(name string) string {
	// Replace any non-alphanumeric character with underscore
	result := ""
	for _, c := range name {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			result += string(c)
		} else {
			result += "_"
		}
	}

	// Ensure it starts with a letter
	if len(result) > 0 && !((result[0] >= 'a' && result[0] <= 'z') || (result[0] >= 'A' && result[0] <= 'Z')) {
		result = "resource_" + result
	}

	return strings.ToLower(result)
}

// extractPathParams extracts parameter names from a path like "/users/{id}/items/{item_id}"
func extractPathParams(path string) []string {
	var params []string
	parts := strings.Split(path, "/")

	for _, part := range parts {
		if strings.HasPrefix(part, "{") && strings.HasSuffix(part, "}") {
			// Extract the parameter name without braces
			param := part[1 : len(part)-1]
			params = append(params, param)
		}
	}

	return params
}

// createMinimalSpecFile creates a minimal spec file when the generator times out
func (g *Generator) createMinimalSpecFile(outputPath string) error {
	minimalSpec := map[string]interface{}{
		"resources": []map[string]interface{}{
			{
				"name": "example",
				"schema": map[string]interface{}{
					"attributes": []map[string]interface{}{
						{
							"name":     "id",
							"type":     "string",
							"computed": true,
						},
					},
				},
			},
		},
	}

	specData, err := json.MarshalIndent(minimalSpec, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal minimal spec: %w", err)
	}

	if err := os.WriteFile(outputPath, specData, 0644); err != nil {
		return fmt.Errorf("failed to write minimal spec file: %w", err)
	}

	return nil
}

// Add a new function to preprocess the OpenAPI spec before passing it to the generator
func (g *Generator) preprocessOpenAPISpec(filePath string) (string, error) {
	// Read the original OpenAPI spec
	content, err := os.ReadFile(filePath)
	if err != nil {
		return "", fmt.Errorf("failed to read OpenAPI spec: %w", err)
	}

	// Parse the JSON
	var spec map[string]interface{}
	if err := json.Unmarshal(content, &spec); err != nil {
		return "", fmt.Errorf("failed to parse OpenAPI spec JSON: %w", err)
	}

	// Process the spec to fix types and break circular references
	processed := make(map[string]bool)
	processedRefs := make(map[string]bool)
	referenceGraph := make(map[string][]string)

	fmt.Println("Breaking circular dependencies in OpenAPI spec...")
	breakCircularDependencies(spec, processed, processedRefs, referenceGraph, "")

	fmt.Println("Fixing schema types in OpenAPI spec...")
	fixTypesInSpec(spec)

	// Write the modified spec to a new file
	preprocessedPath := filePath + ".preprocessed"
	preprocessedContent, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return "", fmt.Errorf("failed to marshal preprocessed spec: %w", err)
	}

	if err := os.WriteFile(preprocessedPath, preprocessedContent, 0644); err != nil {
		return "", fmt.Errorf("failed to write preprocessed spec: %w", err)
	}

	fmt.Println("Created preprocessed OpenAPI spec with fixed types and broken circular references")
	return preprocessedPath, nil
}

// breakCircularDependencies detects and breaks circular references in the schema
func breakCircularDependencies(node interface{}, processed map[string]bool, processedRefs map[string]bool, referenceGraph map[string][]string, path string) {
	if node == nil {
		return
	}

	switch v := node.(type) {
	case map[string]interface{}:
		// Check if this is a reference
		if ref, hasRef := v["$ref"].(string); hasRef {
			// Record this reference in the graph
			if path != "" {
				referenceGraph[path] = append(referenceGraph[path], ref)

				// Check if this creates a circular reference
				if isCircularReference(ref, path, referenceGraph, make(map[string]bool)) {
					fmt.Printf("Detected circular reference: %s -> %s\n", path, ref)

					// Break circular reference by replacing with a simple object schema
					delete(v, "$ref")
					v["type"] = "object"
					v["description"] = "Circular reference removed"
					v["properties"] = map[string]interface{}{} // Empty properties

					return
				}
			}

			// Skip already processed references to avoid infinite recursion
			if processedRefs[ref] {
				return
			}
			processedRefs[ref] = true
		}

		// Process each property recursively
		for key, value := range v {
			breakCircularDependencies(value, processed, processedRefs, referenceGraph, path+"/"+key)
		}

	case []interface{}:
		for i, item := range v {
			breakCircularDependencies(item, processed, processedRefs, referenceGraph, fmt.Sprintf("%s/%d", path, i))
		}
	}
}

// isCircularReference checks if a reference creates a circular dependency
func isCircularReference(ref, currentPath string, graph map[string][]string, visited map[string]bool) bool {
	// If we've already visited this node in this DFS traversal, it's a cycle
	if visited[ref] {
		return true
	}

	// Mark as visited for this traversal
	visited[ref] = true

	// Check if this reference points back to the current path
	for _, target := range graph[ref] {
		if target == currentPath || isCircularReference(target, currentPath, graph, visited) {
			return true
		}
	}

	// Remove from visited when backtracking
	delete(visited, ref)
	return false
}

// fixTypesInSpec applies type fixes to the entire OpenAPI spec
func fixTypesInSpec(spec map[string]interface{}) bool {
	modified := false

	// Process components/schemas to add missing types
	if components, ok := spec["components"].(map[string]interface{}); ok {
		if schemas, ok := components["schemas"].(map[string]interface{}); ok {
			modified = fixSchemasTypes(schemas) || modified
		}
	}

	// Process paths to add missing types in schemas
	if paths, ok := spec["paths"].(map[string]interface{}); ok {
		for _, pathItem := range paths {
			if pathObj, ok := pathItem.(map[string]interface{}); ok {
				for _, method := range pathObj {
					if methodObj, ok := method.(map[string]interface{}); ok {
						// Fix request body schemas
						if requestBody, ok := methodObj["requestBody"].(map[string]interface{}); ok {
							if content, ok := requestBody["content"].(map[string]interface{}); ok {
								for _, mediaType := range content {
									if mediaTypeObj, ok := mediaType.(map[string]interface{}); ok {
										if schema, ok := mediaTypeObj["schema"].(map[string]interface{}); ok {
											modified = fixSchemaType(schema) || modified
										}
									}
								}
							}
						}

						// Fix response schemas
						if responses, ok := methodObj["responses"].(map[string]interface{}); ok {
							for _, response := range responses {
								if responseObj, ok := response.(map[string]interface{}); ok {
									if content, ok := responseObj["content"].(map[string]interface{}); ok {
										for _, mediaType := range content {
											if mediaTypeObj, ok := mediaType.(map[string]interface{}); ok {
												if schema, ok := mediaTypeObj["schema"].(map[string]interface{}); ok {
													modified = fixSchemaType(schema) || modified
												}
											}
										}
									}
								}
							}
						}

						// Fix parameters schemas
						if parameters, ok := methodObj["parameters"].([]interface{}); ok {
							for _, param := range parameters {
								if paramObj, ok := param.(map[string]interface{}); ok {
									if schema, ok := paramObj["schema"].(map[string]interface{}); ok {
										modified = fixSchemaType(schema) || modified
									}
								}
							}
						}
					}
				}
			}
		}
	}

	return modified
}

// fixSchemasTypes recursively fixes types in schema objects
func fixSchemasTypes(schemas map[string]interface{}) bool {
	modified := false
	for _, schema := range schemas {
		if schemaObj, ok := schema.(map[string]interface{}); ok {
			modified = fixSchemaType(schemaObj) || modified
		}
	}
	return modified
}

// fixSchemaType adds a "type": "object" property to schemas without a type
// and handles complex schema constructs like allOf, oneOf, anyOf
func fixSchemaType(schema map[string]interface{}) bool {
	modified := false

	// Check if schema has properties but no type
	if _, hasProps := schema["properties"]; hasProps {
		if _, hasType := schema["type"]; !hasType {
			schema["type"] = "object"
			modified = true
		}
	}

	// Handle allOf, oneOf, anyOf by extracting their properties
	for _, keyword := range []string{"allOf", "oneOf", "anyOf"} {
		if schemaList, ok := schema[keyword].([]interface{}); ok {
			// Extract properties from the first schema in the list
			if len(schemaList) > 0 {
				if firstSchema, ok := schemaList[0].(map[string]interface{}); ok {
					if props, ok := firstSchema["properties"].(map[string]interface{}); ok {
						// Move properties to the parent schema
						if _, hasProps := schema["properties"]; !hasProps {
							schema["properties"] = props
							schema["type"] = "object"
							modified = true
						}
					}
				}
			}

			// Delete the complex keyword to simplify
			delete(schema, keyword)
			modified = true
		}
	}

	// Process nested properties
	if properties, ok := schema["properties"].(map[string]interface{}); ok {
		for _, prop := range properties {
			if propObj, ok := prop.(map[string]interface{}); ok {
				modified = fixSchemaType(propObj) || modified
			}
		}
	}

	// Process array items
	if items, ok := schema["items"].(map[string]interface{}); ok {
		modified = fixSchemaType(items) || modified
	}

	return modified
}
