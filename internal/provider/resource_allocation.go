package provider

import (
	"context"
	"fmt"
	"net/http"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var _ resource.Resource = &AllocationResource{}
var _ resource.ResourceWithImportState = &AllocationResource{}

type AllocationResource struct {
	client *nxipClient
}

func NewAllocationResource() resource.Resource {
	return &AllocationResource{}
}

type AllocationResourceModel struct {
	ID             types.String `tfsdk:"id"`
	Environment    types.String `tfsdk:"environment"`
	Region         types.String `tfsdk:"region"`
	Family         types.String `tfsdk:"family"`
	PrefixLength   types.Int64  `tfsdk:"prefix_length"`
	ParentSubnetID types.String `tfsdk:"parent_subnet_id"`
	Kind           types.String `tfsdk:"kind"`
	Name           types.String `tfsdk:"name"`
	Description    types.String `tfsdk:"description"`
	CIDR           types.String `tfsdk:"cidr"`
}

func (r *AllocationResource) Metadata(ctx context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_allocation"
}

func (r *AllocationResource) Schema(ctx context.Context, req resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Allocates a dynamic, non-overlapping CIDR subnet block. Either routes to a matching " +
			"nxip_pool by environment/region/family — auto-resolving onto a kind-tagged subnet under that " +
			"pool first if one exists for that exact key, so this resource's config never has to change " +
			"whether or not that structure exists yet — or, given parent_subnet_id, nests directly under " +
			"an existing subnet instead, bypassing auto-resolution entirely. Requires a pool to already " +
			"exist for the target environment/region/family (see nxip_pool) — there is no implicit pool " +
			"creation. See subnet-hierarchy.html in the nxip repo for the full reasoning.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:    true,
				Description: "Unique identifier for the allocation record.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"environment": schema.StringAttribute{
				Optional: true,
				Computed: true,
				Description: "Target environment (e.g. production, staging). Required unless parent_subnet_id " +
					"is set — a nested allocation inherits environment/region from its parent, so this is " +
					"populated automatically after apply rather than needing to be supplied. Allocations are " +
					"immutable: changing this forces a new resource.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"region": schema.StringAttribute{
				Optional: true,
				Computed: true,
				Description: "Target region (e.g. uksouth, us-east-1). Required unless parent_subnet_id is " +
					"set — see environment's description. Allocations are immutable: changing this forces a " +
					"new resource.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"family": schema.StringAttribute{
				Required: true,
				Description: "Address family: \"IPV4\" or \"IPV6\". Determines which pool this routes to — a pool " +
					"is scoped to exactly one family per environment/region. Allocations are immutable: changing " +
					"this forces a new resource. Validated server-side; an invalid value returns an API error.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"prefix_length": schema.Int64Attribute{
				Required:    true,
				Description: "Desired CIDR prefix size (e.g. 24 for a /24 IPv4 subnet, or 64 for a /64 IPv6 subnet). Allocations are immutable: changing this forces a new resource.",
				PlanModifiers: []planmodifier.Int64{
					int64planmodifier.RequiresReplace(),
				},
			},
			"parent_subnet_id": schema.StringAttribute{
				Optional: true,
				Computed: true,
				Description: "Nest this allocation directly under an existing subnet (e.g. another " +
					"nxip_allocation's id) instead of routing to a pool by environment/region/family — the " +
					"deliberate, admin-facing path for building structure beyond what environment/region/" +
					"family alone can disambiguate (e.g. a second VPC-scoped subnet under the same region). " +
					"When set, environment/region are inherited from the parent, not read from this config. " +
					"Computed as well as Optional: even a config that never sets this can end up nested, via " +
					"auto-resolution onto a kind-tagged subnet matching environment/region/family — this is " +
					"populated automatically after apply in that case, not left null. " +
					"Allocations are immutable: changing this forces a new resource.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"kind": schema.StringAttribute{
				Optional: true,
				Description: "Free-text label for what this level of an address plan represents (\"region\", " +
					"\"vpc\", \"site\", \"vlan\"...) — not validated against a fixed list. Only meaningful on a " +
					"top-level allocation (no parent_subnet_id): it's what makes this allocation eligible as " +
					"the auto-resolution landing point for later requests matching the same environment/" +
					"region/family. Allocations are immutable: changing this forces a new resource.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"name": schema.StringAttribute{
				Optional:    true,
				Description: "Human-readable name for this allocation (e.g. \"App Team A\"). Allocations are immutable: changing this forces a new resource.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"description": schema.StringAttribute{
				Optional:    true,
				Description: "Free-text notes for why this allocation exists. Allocations are immutable: changing this forces a new resource.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"cidr": schema.StringAttribute{
				Computed:    true,
				Description: "The allocated non-overlapping CIDR block returned by nxip API (e.g. 10.240.12.0/24).",
			},
		},
	}
}

func (r *AllocationResource) Configure(ctx context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	r.client = newNxipClient(req.ProviderData.(*NxipProviderModel))
}

// allocationResponse mirrors the JSON shape returned by the nxip API for a
// single allocation (POST /v1/allocations and GET /v1/allocations/:id).
// The allocated block comes back under "allocatedCidr" — there is no
// separate "cidr" field server-side. ParentSubnetID/Kind/Name/Description
// are pointers since the API returns JSON null, not an empty string, for
// an allocation that doesn't have one.
type allocationResponse struct {
	ID             string  `json:"id"`
	AllocatedCIDR  string  `json:"allocatedCidr"`
	PrefixLength   int64   `json:"prefixLength"`
	Family         string  `json:"family"`
	Environment    string  `json:"environment"`
	Region         string  `json:"region"`
	ParentSubnetID *string `json:"parentSubnetId"`
	Kind           *string `json:"kind"`
	Name           *string `json:"name"`
	Description    *string `json:"description"`
}

// applyAllocationResponse copies API response fields into the resource
// model — shared by Create and Read so the two can't drift apart on which
// fields get synced back into state.
func applyAllocationResponse(model *AllocationResourceModel, result allocationResponse) {
	model.ID = types.StringValue(result.ID)
	model.CIDR = types.StringValue(result.AllocatedCIDR)
	model.PrefixLength = types.Int64Value(result.PrefixLength)
	model.Family = types.StringValue(result.Family)
	// Always synced from the response, even when the caller supplied them:
	// for a nested allocation (parent_subnet_id set) these are server-
	// derived, inherited from the parent rather than read from config, so
	// environment/region are Optional+Computed in the schema specifically
	// to allow this.
	model.Environment = types.StringValue(result.Environment)
	model.Region = types.StringValue(result.Region)

	if result.ParentSubnetID != nil {
		model.ParentSubnetID = types.StringValue(*result.ParentSubnetID)
	} else {
		model.ParentSubnetID = types.StringNull()
	}
	if result.Kind != nil {
		model.Kind = types.StringValue(*result.Kind)
	} else {
		model.Kind = types.StringNull()
	}
	if result.Name != nil {
		model.Name = types.StringValue(*result.Name)
	} else {
		model.Name = types.StringNull()
	}
	if result.Description != nil {
		model.Description = types.StringValue(*result.Description)
	} else {
		model.Description = types.StringNull()
	}
}

func (r *AllocationResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan AllocationResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	payload := map[string]any{
		"family":       plan.Family.ValueString(),
		"prefixLength": plan.PrefixLength.ValueInt64(),
	}
	// Optional fields are only included when actually known and set —
	// "not provided" must reach the API as an absent field (routes/auto-
	// resolves normally), not as an empty string, which its validation
	// rejects for kind/name/parentSubnetId. Checking IsNull() alone isn't
	// enough for environment/region/parent_subnet_id: since they're
	// Optional+Computed, a config that never sets them leaves the plan
	// value *unknown*, not null, until the provider's response fills it
	// in — and ValueString() on an unknown value silently returns "",
	// which is exactly the empty string the API rejects.
	if !plan.Environment.IsNull() && !plan.Environment.IsUnknown() {
		payload["environment"] = plan.Environment.ValueString()
	}
	if !plan.Region.IsNull() && !plan.Region.IsUnknown() {
		payload["region"] = plan.Region.ValueString()
	}
	if !plan.ParentSubnetID.IsNull() && !plan.ParentSubnetID.IsUnknown() {
		payload["parentSubnetId"] = plan.ParentSubnetID.ValueString()
	}
	if !plan.Kind.IsNull() {
		payload["kind"] = plan.Kind.ValueString()
	}
	if !plan.Name.IsNull() {
		payload["name"] = plan.Name.ValueString()
	}
	if !plan.Description.IsNull() {
		payload["description"] = plan.Description.ValueString()
	}

	var result allocationResponse
	status, err := r.client.do(ctx, http.MethodPost, "/v1/allocations", payload, &result)
	if err != nil {
		resp.Diagnostics.AddError("Client Error", err.Error())
		return
	}
	if status != http.StatusCreated {
		resp.Diagnostics.AddError("API Error", fmt.Sprintf("nxip API failed allocation request with status %d", status))
		return
	}

	applyAllocationResponse(&plan, result)

	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *AllocationResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state AllocationResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var result allocationResponse
	status, err := r.client.do(ctx, http.MethodGet, "/v1/allocations/"+state.ID.ValueString(), nil, &result)
	if err != nil {
		resp.Diagnostics.AddError("Client Error", err.Error())
		return
	}

	// If the allocation was released outside of Terraform (e.g. deleted
	// directly via the API), drop it from state so Terraform plans to
	// recreate it rather than erroring on drift.
	if status == http.StatusNotFound {
		resp.State.RemoveResource(ctx)
		return
	}
	if status != http.StatusOK {
		resp.Diagnostics.AddError("API Error", fmt.Sprintf("nxip API failed to fetch allocation with status %d", status))
		return
	}

	applyAllocationResponse(&state, result)

	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *AllocationResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	// All user-configurable attributes (environment, region, family,
	// prefix_length) are marked RequiresReplace in the schema, since
	// allocations are immutable server-side. Terraform will therefore
	// destroy and recreate the resource rather than reach this method for
	// any meaningful change. This is kept only to satisfy the
	// resource.Resource interface.
	var plan AllocationResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *AllocationResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state AllocationResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	status, err := r.client.do(ctx, http.MethodDelete, "/v1/allocations/"+state.ID.ValueString(), nil, nil)
	if err != nil {
		resp.Diagnostics.AddError("Client Error", err.Error())
		return
	}

	// A 404 means the allocation is already gone server-side. Treat this as
	// a successful (idempotent) delete rather than an error. Allocation
	// delete returns 200 (with a body), not 204 — unlike pool delete.
	if status != http.StatusOK && status != http.StatusNotFound {
		resp.Diagnostics.AddError("API Error", fmt.Sprintf("nxip API failed to release allocation with status %d", status))
		return
	}
}

// ImportState allows an existing allocation (created outside Terraform, or
// from a previous state file) to be brought under management with:
//
//	terraform import nxip_allocation.example <allocation-id>
//
// Only the ID is known at import time; Read (invoked automatically by the
// framework after ImportState) populates the remaining attributes from the API.
func (r *AllocationResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}
