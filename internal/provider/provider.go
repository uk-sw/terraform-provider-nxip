package provider

import (
	"context"
	"os"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// Ensure NxipProvider satisfies the provider.Provider interface.
var _ provider.Provider = &NxipProvider{}

type NxipProvider struct {
	version string
}

type NxipProviderModel struct {
	APIKey       types.String `tfsdk:"api_key"`
	URL          types.String `tfsdk:"url"`
	Organization types.String `tfsdk:"organization"`
}

func New(version string) func() provider.Provider {
	return func() provider.Provider {
		return &NxipProvider{version: version}
	}
}

func (p *NxipProvider) Metadata(ctx context.Context, req provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "nxip"
	resp.Version = p.version
}

func (p *NxipProvider) Schema(ctx context.Context, req provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		// Plain-text fallback for tooling that can't render Markdown (e.g.
		// the language server, terraform plan/validate output) - kept
		// free of links/formatting on purpose. MarkdownDescription below
		// is what the Registry's own docs generator actually renders.
		Description: "IPAM (IP Address Management) for infrastructure-as-code. Manages dynamic, " +
			"conflict-free IP CIDR subnets across multi-cloud and on-prem environments. See nxip.dev " +
			"for the product itself and the underlying REST API reference, and nx-ip.com to create " +
			"an account.",
		MarkdownDescription: "IPAM (IP Address Management) for infrastructure-as-code. Manages dynamic, " +
			"conflict-free IP CIDR subnets across multi-cloud and on-prem environments. See " +
			"[nxip.dev](https://nxip.dev) for the product itself and the underlying REST API reference, " +
			"and [nx-ip.com](https://nx-ip.com) to create an account.",
		Attributes: map[string]schema.Attribute{
			"api_key": schema.StringAttribute{
				Description: "API Key for nxip Control Plane. Recommended: leave this unset and provide " +
					"it via the NXIP_API_KEY environment variable instead (used only if this attribute " +
					"is left unset), so a real credential never ends up written into a .tf file.",
				Optional:  true,
				Sensitive: true,
			},
			"url": schema.StringAttribute{
				Description: "Optional base URL for API (defaults to https://nxip.dev). Can also be set " +
					"via the NXIP_URL environment variable.",
				Optional: true,
			},
			"organization": schema.StringAttribute{
				Description: "Customer organization ID to manage on behalf of, for a provider (MSP, ISP, " +
					"or acquirer) whose API key belongs to an Enterprise organization with linked " +
					"customers. Every request this configuration makes is sent with the " +
					"x-nxip-organization header set to this value, so it acts as if it were that " +
					"customer's own key, up to the role ceiling the customer's admin set when linking. " +
					"Can also be set via the NXIP_ORGANIZATION environment variable; this attribute wins " +
					"when both are set. If you do not set organization, requests go to the organization " +
					"your API key belongs to - this is the default for everyone, and nothing changes for " +
					"an organization with no customers. Setting it to your own organization's ID is valid " +
					"and changes nothing. Provider-level only: one configuration manages one organization, " +
					"so use provider aliases to manage several customers side by side. Creating, linking or " +
					"unlinking customer organizations is not done through Terraform.",
				Optional: true,
			},
		},
	}
}

// resolveConfigValue returns the config attribute's value when it is set to
// a non-empty string, otherwise the named environment variable (which may
// itself be empty). The attribute always wins over the environment - the
// same precedence for api_key, url and organization, so a value that is
// only ever meant to travel through the environment (a real credential, for
// example) still gets overridden by an explicit attribute someone put in a
// .tf file by mistake, rather than being silently shadowed.
func resolveConfigValue(attr types.String, envVar string) string {
	if !attr.IsNull() && attr.ValueString() != "" {
		return attr.ValueString()
	}
	return os.Getenv(envVar)
}

func (p *NxipProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var data NxipProviderModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// The attribute wins if set; NXIP_API_KEY/NXIP_URL/NXIP_ORGANIZATION are
	// the recommended path for everyone, not just CI/CD - it's how a real
	// credential avoids ever being written into a .tf file at all. Resolved
	// here, once, rather than in every resource's own Configure - the
	// client is what actually reads these, so data must carry the final
	// values, not the possibly-empty ones straight out of the parsed
	// config.
	data.APIKey = types.StringValue(resolveConfigValue(data.APIKey, "NXIP_API_KEY"))
	data.URL = types.StringValue(resolveConfigValue(data.URL, "NXIP_URL"))
	data.Organization = types.StringValue(resolveConfigValue(data.Organization, "NXIP_ORGANIZATION"))

	if data.APIKey.ValueString() == "" {
		resp.Diagnostics.AddError(
			"Missing API Key",
			"Set the api_key attribute in the provider block, or the NXIP_API_KEY environment variable. "+
				"If you don't have a key yet, get one for free at https://nx-ip.com/signup.",
		)
		return
	}

	// Pass API key/client to resource configure methods
	resp.ResourceData = &data

	// Advisory only: when organization is left unset, warn a provider whose
	// key already has customers that this configuration is, as ever,
	// managing the key's own organization - not one of them. Never an
	// error, and never allowed to block a plan or apply: a key whose role
	// cannot list customers, or a network hiccup during Configure, simply
	// skips the warning (see hasAnyCustomers). Skipped entirely once
	// organization is set, since there is then nothing to warn about.
	if data.Organization.ValueString() == "" {
		client := newNxipClient(&data)
		if hasCustomers, ok := client.hasAnyCustomers(ctx); ok && hasCustomers {
			resp.Diagnostics.AddWarning(
				"organization not set",
				"`organization` is not set, so this configuration manages your own organization. Set "+
					"`organization` to a customer organization's ID to manage that customer instead.",
			)
		}
	}
}

func (p *NxipProvider) Resources(ctx context.Context) []func() resource.Resource {
	return []func() resource.Resource{
		NewSubnetResource,
		NewPoolResource,
		NewAddressResource,
	}
}

func (p *NxipProvider) DataSources(ctx context.Context) []func() datasource.DataSource {
	return []func() datasource.DataSource{}
}
