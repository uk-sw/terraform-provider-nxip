package provider

import (
	"context"

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
		Description: "Manages dynamic, conflict-free IP CIDR allocations across multi-cloud environments.",
		Attributes: map[string]schema.Attribute{
			"api_key": schema.StringAttribute{
				Description: "API Key for nxip Control Plane.",
				Required:    true,
				Sensitive:   true,
			},
			"url": schema.StringAttribute{
				Description: "Optional base URL for API (defaults to https://nxip.dev).",
				Optional:    true,
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

	// Pass API key/client to resource configure methods
	resp.ResourceData = &data
}

func (p *NxipProvider) Resources(ctx context.Context) []func() resource.Resource {
	return []func() resource.Resource{
		NewAllocationResource,
		NewPoolResource,
	}
}

func (p *NxipProvider) DataSources(ctx context.Context) []func() datasource.DataSource {
	return []func() datasource.DataSource{}
}
