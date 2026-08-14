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
	APIKey types.String `tfsdk:"api_key"`
	URL    types.String `tfsdk:"url"`
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
		Description: "IPAM (IP Address Management) for infrastructure-as-code. Manages dynamic, " +
			"conflict-free IP CIDR subnets across multi-cloud and on-prem environments. " +
			"PRE-RELEASE: this provider and the nxip API behind it are still being validated with real " +
			"users. Resource schemas may change, and the Free tier backing this API carries no data-" +
			"durability guarantee. Do not use for production infrastructure yet.",
		Attributes: map[string]schema.Attribute{
			"api_key": schema.StringAttribute{
				Description: "API Key for nxip Control Plane. Can also be set via the NXIP_API_KEY " +
					"environment variable (used only if this attribute is left unset) - useful for CI/CD " +
					"pipelines that shouldn't have a real credential written into .tf files at all.",
				Optional:  true,
				Sensitive: true,
			},
			"url": schema.StringAttribute{
				Description: "Optional base URL for API (defaults to https://nxip.dev). Can also be set " +
					"via the NXIP_URL environment variable.",
				Optional: true,
			},
		},
	}
}

func (p *NxipProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var data NxipProviderModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// The attribute wins if set; NXIP_API_KEY/NXIP_URL are fallbacks for
	// CI/CD pipelines that shouldn't have a real credential written into
	// .tf files at all. Resolved here, once, rather than in every
	// resource's own Configure - the client is what actually reads these,
	// so data must carry the final values, not the possibly-empty ones
	// straight out of the parsed config.
	if data.APIKey.IsNull() || data.APIKey.ValueString() == "" {
		if envKey := os.Getenv("NXIP_API_KEY"); envKey != "" {
			data.APIKey = types.StringValue(envKey)
		}
	}
	if data.URL.IsNull() || data.URL.ValueString() == "" {
		if envURL := os.Getenv("NXIP_URL"); envURL != "" {
			data.URL = types.StringValue(envURL)
		}
	}

	if data.APIKey.ValueString() == "" {
		resp.Diagnostics.AddError(
			"Missing API Key",
			"Set the api_key attribute in the provider block, or the NXIP_API_KEY environment variable.",
		)
		return
	}

	// Pass API key/client to resource configure methods
	resp.ResourceData = &data
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
