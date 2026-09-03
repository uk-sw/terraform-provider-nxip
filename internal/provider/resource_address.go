package provider

import (
	"context"
	"net/http"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/mapplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var _ resource.Resource = &AddressResource{}
var _ resource.ResourceWithImportState = &AddressResource{}

type AddressResource struct {
	client *nxipClient
}

func NewAddressResource() resource.Resource {
	return &AddressResource{}
}

type AddressResourceModel struct {
	ID       types.String `tfsdk:"id"`
	SubnetID types.String `tfsdk:"subnet_id"`
	Address  types.String `tfsdk:"address"`
	Family   types.String `tfsdk:"family"`
	Status   types.String `tfsdk:"status"`
	Hostname types.String `tfsdk:"hostname"`
	Metadata types.Map    `tfsdk:"metadata"`
}

func (r *AddressResource) Metadata(ctx context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_address"
}

func (r *AddressResource) Schema(ctx context.Context, req resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Registers or reserves a specific IP address within an already-allocated nxip_subnet. This is " +
			"the individual-address layer beneath the subnet itself. There is no auto-pick mode: unlike " +
			"nxip_subnet, which address to use is normally chosen by whoever is deploying the host (or by " +
			"DHCP), not requested blind from IPAM, so `address` is always explicit here.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:    true,
				Description: "Unique identifier for the address record.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"subnet_id": schema.StringAttribute{
				Required:    true,
				Description: "The nxip_subnet this address belongs to (e.g. another nxip_subnet's id). Immutable: changing this forces a new resource.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"address": schema.StringAttribute{
				Required:    true,
				Description: "The exact IP address to register (e.g. 10.240.12.5). Must fall within subnet_id's CIDR block. Immutable: changing this forces a new resource.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"family": schema.StringAttribute{
				Computed:    true,
				Description: "Address family (\"IPV4\" or \"IPV6\"), inherited from the parent subnet. Not user-supplied.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"status": schema.StringAttribute{
				Optional: true,
				Computed: true,
				Description: "\"ACTIVE\" (in use) or \"RESERVED\" (held but not yet in use). Defaults to " +
					"\"ACTIVE\" server-side if omitted. Immutable: changing this forces a new resource.",
				PlanModifiers: []planmodifier.String{
					// UseStateForUnknown as well as RequiresReplace, not one or
					// the other. Optional+Computed with no UseStateForUnknown
					// re-plans as unknown on every apply that leaves status
					// unset in config, not just the first - which, combined
					// with RequiresReplace, forces a destroy+recreate on any
					// unrelated change at all. Invisible until metadata above
					// stopped force-replacing, since every attribute on this
					// resource used to force replace anyway - the same bug
					// already found and fixed on nxip_subnet's
					// environment/region/parent_subnet_id this week.
					stringplanmodifier.UseStateForUnknown(),
					stringplanmodifier.RequiresReplace(),
				},
			},
			"hostname": schema.StringAttribute{
				Optional:    true,
				Description: "Human-readable hostname for whatever holds this address (e.g. \"web-01\"). Immutable: changing this forces a new resource.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"metadata": schema.MapAttribute{
				ElementType: types.StringType,
				Optional:    true,
				Computed:    true,
				Description: "Free-form key/value tags for this address (e.g. owner, asset_tag), not " +
					"interpreted by nxip, stored and returned as-is. Computed as well as Optional so a config " +
					"that never sets this reads back as an empty map rather than null, matching what the API " +
					"returns. Updated in place via PATCH /v1/addresses/:id - a full replace of the whole map, " +
					"not a merge, same as the API itself, but does not force a new resource.",
				PlanModifiers: []planmodifier.Map{
					// Without this, a config that never sets metadata plans as
					// "(known after apply)" on every apply, not just the first,
					// since nothing tells the framework a value it already
					// knows is stable. On this resource that would be worse
					// than plan noise: every other attribute here is still
					// RequiresReplace, so a spurious unknown would force a
					// destroy and recreate on any unrelated change - the same
					// bug already found and fixed on nxip_subnet and nxip_pool.
					mapplanmodifier.UseStateForUnknown(),
				},
			},
		},
	}
}

func (r *AddressResource) Configure(ctx context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	r.client = newNxipClient(req.ProviderData.(*NxipProviderModel))
}

// addressResponse mirrors the JSON shape returned by the nxip API for a
// single address (POST/GET /v1/subnets/:id/addresses[/:addressId]).
// Hostname is a pointer since the API returns JSON null, not an empty
// string, for an address with none set.
type addressResponse struct {
	ID       string            `json:"id"`
	SubnetID string            `json:"subnetId"`
	Address  string            `json:"address"`
	Family   string            `json:"family"`
	Status   string            `json:"status"`
	Hostname *string           `json:"hostname"`
	Metadata map[string]string `json:"metadata"`
}

// applyAddressResponse copies API response fields into the resource model -
// shared by Create and Read so the two can't drift apart on which fields
// get synced back into state.
func applyAddressResponse(ctx context.Context, model *AddressResourceModel, result addressResponse) diag.Diagnostics {
	model.ID = types.StringValue(result.ID)
	model.SubnetID = types.StringValue(result.SubnetID)
	model.Address = types.StringValue(result.Address)
	model.Family = types.StringValue(result.Family)
	model.Status = types.StringValue(result.Status)

	if result.Hostname != nil {
		model.Hostname = types.StringValue(*result.Hostname)
	} else {
		model.Hostname = types.StringNull()
	}

	metadataValue, diags := types.MapValueFrom(ctx, types.StringType, result.Metadata)
	model.Metadata = metadataValue
	return diags
}

func (r *AddressResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan AddressResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	payload := map[string]any{
		"address": plan.Address.ValueString(),
	}
	if !plan.Status.IsNull() && !plan.Status.IsUnknown() {
		payload["status"] = plan.Status.ValueString()
	}
	if !plan.Hostname.IsNull() {
		payload["hostname"] = plan.Hostname.ValueString()
	}
	if !plan.Metadata.IsNull() && !plan.Metadata.IsUnknown() {
		var metadata map[string]string
		resp.Diagnostics.Append(plan.Metadata.ElementsAs(ctx, &metadata, false)...)
		if resp.Diagnostics.HasError() {
			return
		}
		payload["metadata"] = metadata
	}

	var result addressResponse
	status, apiMessage, err := r.client.do(ctx, http.MethodPost, "/v1/subnets/"+plan.SubnetID.ValueString()+"/addresses", payload, &result)
	if err != nil {
		resp.Diagnostics.AddError("Client Error", err.Error())
		return
	}
	if status != http.StatusCreated {
		resp.Diagnostics.AddError("API Error", apiErrorSummary("failed to register address", status, apiMessage))
		return
	}

	resp.Diagnostics.Append(applyAddressResponse(ctx, &plan, result)...)
	if resp.Diagnostics.HasError() {
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *AddressResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state AddressResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var result addressResponse
	status, apiMessage, err := r.client.do(ctx, http.MethodGet, "/v1/addresses/"+state.ID.ValueString(), nil, &result)
	if err != nil {
		resp.Diagnostics.AddError("Client Error", err.Error())
		return
	}

	// If the address was released outside of Terraform (e.g. deleted
	// directly via the API), drop it from state so Terraform plans to
	// recreate it rather than erroring on drift.
	if status == http.StatusNotFound {
		resp.State.RemoveResource(ctx)
		return
	}
	if status != http.StatusOK {
		resp.Diagnostics.AddError("API Error", apiErrorSummary("failed to fetch address", status, apiMessage))
		return
	}

	resp.Diagnostics.Append(applyAddressResponse(ctx, &state, result)...)
	if resp.Diagnostics.HasError() {
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Update is only ever reached for a change to metadata - every other
// attribute (subnet_id, address, family, status, hostname) is still
// RequiresReplace, since those genuinely are immutable server-side.
// metadata alone is patchable via PATCH /v1/addresses/:id, so tagging an
// address with an owner or an asset tag no longer means releasing it and
// registering a new one at the same IP.
func (r *AddressResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan AddressResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	payload := map[string]any{}
	if !plan.Metadata.IsNull() && !plan.Metadata.IsUnknown() {
		var metadata map[string]string
		resp.Diagnostics.Append(plan.Metadata.ElementsAs(ctx, &metadata, false)...)
		if resp.Diagnostics.HasError() {
			return
		}
		payload["metadata"] = metadata
	}

	var result addressResponse
	status, apiMessage, err := r.client.do(ctx, http.MethodPatch, "/v1/addresses/"+plan.ID.ValueString(), payload, &result)
	if err != nil {
		resp.Diagnostics.AddError("Client Error", err.Error())
		return
	}
	if status != http.StatusOK {
		resp.Diagnostics.AddError("API Error", apiErrorSummary("failed to update address", status, apiMessage))
		return
	}

	resp.Diagnostics.Append(applyAddressResponse(ctx, &plan, result)...)
	if resp.Diagnostics.HasError() {
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *AddressResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state AddressResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	status, apiMessage, err := r.client.do(ctx, http.MethodDelete, "/v1/addresses/"+state.ID.ValueString(), nil, nil)
	if err != nil {
		resp.Diagnostics.AddError("Client Error", err.Error())
		return
	}

	// A 404 means the address is already gone server-side. Treat this as
	// a successful (idempotent) delete rather than an error. Address
	// delete returns 200 (with a body), matching subnet delete.
	if status != http.StatusOK && status != http.StatusNotFound {
		resp.Diagnostics.AddError("API Error", apiErrorSummary("failed to release address", status, apiMessage))
		return
	}
}

// ImportState allows an existing address (registered outside Terraform, or
// from a previous state file) to be brought under management with:
//
//	terraform import nxip_address.example <address-id>
//
// Same single-ID form as nxip_pool/nxip_subnet, now that GET/PATCH/DELETE
// /v1/addresses/:id exist - previously this needed a composite
// <subnet-id>/<address-id> identifier, since the address's own ID alone
// wasn't enough to fetch it while its only route was nested under its
// parent subnet. Read (invoked automatically by the framework after
// ImportState) populates subnet_id and every other attribute from the API.
func (r *AddressResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}
