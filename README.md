# Provider Generator

A CLI tool to generate Terraform provider Go code from OpenAPI specifications.

## Installation

```bash
go get github.com/Ilhicas/provider-generator
```

## Dependencies

This tool uses the Terraform Plugin Framework code generator tools:
- [tfplugingen-openapi](https://github.com/hashicorp/terraform-plugin-codegen-openapi) - To convert OpenAPI specs to provider specs
- [tfplugingen-go](https://github.com/hashicorp/terraform-plugin-codegen) - To generate Go code from provider specs

You need to install these dependencies manually before using this tool:

```bash
go install github.com/hashicorp/terraform-plugin-codegen-openapi/cmd/tfplugingen-openapi@latest
go install github.com/hashicorp/terraform-plugin-codegen/cmd/tfplugingen-go@latest
```

Or you can use the `--install-deps` flag to install them automatically:

```bash
provider-generator --install-deps generate --file ./api-spec.json --provider-name example
```

## Usage

Generate Terraform provider code from an OpenAPI specification:

```bash
# Generate from a URL
provider-generator generate --url https://example.com/api-spec.json --provider-name example

# Generate from a local file
provider-generator generate --file ./api-spec.json --provider-name example

# Specify an output directory (defaults to ./generated)
provider-generator generate --file ./api-spec.json --provider-name example --output ./my-provider

# Specify a custom generator config file
provider-generator generate --file ./api-spec.json --provider-name example --config ./my-config.yml
```

## Configuration

You can customize the code generation using a YAML configuration file. Example:

```yaml
provider_name: myapi
provider_full_name: "Terraform Provider MyAPI"
provider_short_name: myapi
```

If no configuration file is specified, a basic one will be generated automatically.

## Generated Code

The generated code will be a complete Terraform provider implementation based on the OpenAPI specification. It includes:

- Resource definitions
- Data source definitions
- Schema definitions
- CRUD operations
