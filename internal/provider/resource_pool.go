package provider

import (
	"context"
	"net/http"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var _ resource.Resource = &PoolResource{}
var _ resource.ResourceWithImportState = &PoolResource{}

type PoolResource struct {
	client *nxipClient
}

func NewPoolResource() resource.Resource {
	return &PoolResource{}
}

type PoolResourceModel struct {
	ID          types.String `tfsdk:"id"`
	Name        types.String `tfsdk:"name"`
	CIDR        types.String `tfsdk:"cidr"`
	Family      types.String `tfsdk:"family"`
	Environment types.String `tfsdk:"environment"`
	Region      types.String `tfsdk:"region"`
}

func (r *PoolResource) Metadata(ctx context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_pool"
}

func (r *PoolResource) Schema(ctx context.Context, req resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Registers a top-level IP pool — the parent CIDR block that nxip_subnet resources " +
			"carve non-overlapping subnets from. A pool is scoped to exactly one address family per " +
			"environment/region: to support both IPv4 and IPv6 for the same environment/region, create two " +
			"pools (one per family), not one pool with a mixed range.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:    true,
				Description: "Unique identifier for the pool.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"name": schema.StringAttribute{
				Required:    true,
				Description: "Human-readable name for the pool.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"cidr": schema.StringAttribute{
				Required:    true,
				Description: "The pool's own CIDR block (e.g. 10.240.0.0/16). Must be a valid block for the declared family. Immutable: changing this forces a new resource.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"family": schema.StringAttribute{
				Required:    true,
				Description: "Address family: \"IPV4\" or \"IPV6\". Validated server-side; an invalid value, or a cidr that doesn't match, returns an API error. Immutable: changing this forces a new resource.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"environment": schema.StringAttribute{
				Required:    true,
				Description: "Target environment (e.g. production, staging). Immutable: changing this forces a new resource.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"region": schema.StringAttribute{
				Required:    true,
				Description: "Target region (e.g. uksouth, us-east-1). Immutable: changing this forces a new resource.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
		},
	}
}

func (r *PoolResource) Configure(ctx context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	r.client = newNxipClient(req.ProviderData.(*NxipProviderModel))
}

// poolResponse mirrors the JSON shape returned by the nxip API for a single
// pool (POST /v1/pools and GET /v1/pools/:id). GET also includes a
// "utilization" object; deliberately not mapped here, unknown JSON fields
// are ignored on decode.
type poolResponse struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	CIDR        string `json:"cidr"`
	Family      string `json:"family"`
	Environment string `json:"environment"`
	Region      string `json:"region"`
}

func (r *PoolResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan PoolResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	payload := map[string]any{
		"name":        plan.Name.ValueString(),
		"cidr":        plan.CIDR.ValueString(),
		"family":      plan.Family.ValueString(),
		"environment": plan.Environment.ValueString(),
		"region":      plan.Region.ValueString(),
	}

	var result poolResponse
	status, apiMessage, err := r.client.do(ctx, http.MethodPost, "/v1/pools", payload, &result)
	if err != nil {
		resp.Diagnostics.AddError("Client Error", err.Error())
		return
	}
	if status != http.StatusCreated {
		resp.Diagnostics.AddError("API Error", apiErrorSummary("failed to create pool", status, apiMessage))
		return
	}

	plan.ID = types.StringValue(result.ID)

	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *PoolResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state PoolResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var result poolResponse
	status, apiMessage, err := r.client.do(ctx, http.MethodGet, "/v1/pools/"+state.ID.ValueString(), nil, &result)
	if err != nil {
		resp.Diagnostics.AddError("Client Error", err.Error())
		return
	}

	// If the pool was deleted outside of Terraform, drop it from state so
	// Terraform plans to recreate it rather than erroring on drift.
	if status == http.StatusNotFound {
		resp.State.RemoveResource(ctx)
		return
	}
	if status != http.StatusOK {
		resp.Diagnostics.AddError("API Error", apiErrorSummary("failed to fetch pool", status, apiMessage))
		return
	}

	state.ID = types.StringValue(result.ID)
	state.Name = types.StringValue(result.Name)
	state.CIDR = types.StringValue(result.CIDR)
	state.Family = types.StringValue(result.Family)
	state.Environment = types.StringValue(result.Environment)
	state.Region = types.StringValue(result.Region)

	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *PoolResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	// Every user-configurable attribute is RequiresReplace — there is no
	// PATCH endpoint for pools server-side. Terraform will destroy and
	// recreate rather than reach this method for any meaningful change;
	// kept only to satisfy the resource.Resource interface.
	var plan PoolResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *PoolResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state PoolResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	status, apiMessage, err := r.client.do(ctx, http.MethodDelete, "/v1/pools/"+state.ID.ValueString(), nil, nil)
	if err != nil {
		resp.Diagnostics.AddError("Client Error", err.Error())
		return
	}

	// A 404 means the pool is already gone server-side; treat as a
	// successful (idempotent) delete. Pool delete returns 204 No Content on
	// success — unlike subnet delete, which returns 200 with a body. A
	// 400 here means the pool still has subnets attached (the API refuses
	// to delete a non-empty pool) — the API's own message already names the
	// pool and the exact subnet count; appended Terraform-specific guidance
	// covers the usual cause (destroy ordering put the pool before a subnet
	// that still references it, or a subnet was created outside Terraform).
	if status == http.StatusBadRequest {
		resp.Diagnostics.AddError(
			"API Error",
			apiErrorSummary("failed to delete pool", status, apiMessage)+
				" — destroy any nxip_subnet resources referencing this pool first.",
		)
		return
	}
	if status != http.StatusNoContent && status != http.StatusNotFound {
		resp.Diagnostics.AddError("API Error", apiErrorSummary("failed to delete pool", status, apiMessage))
		return
	}
}

// ImportState allows an existing pool (created outside Terraform, or from a
// previous state file) to be brought under management with:
//
//	terraform import nxip_pool.example <pool-id>
//
// Only the ID is known at import time; Read (invoked automatically by the
// framework after ImportState) populates the remaining attributes from the API.
func (r *PoolResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}
