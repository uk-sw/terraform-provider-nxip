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
	Metadata    types.Map    `tfsdk:"metadata"`
}

func (r *PoolResource) Metadata(ctx context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_pool"
}

func (r *PoolResource) Schema(ctx context.Context, req resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Registers a top-level IP pool: the parent CIDR block that nxip_subnet resources " +
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
				Required: true,
				Description: "The pool's own CIDR block (e.g. 10.240.0.0/16). Must be a valid block for the declared family. " +
					"Updated in place: the API accepts a resize provided every subnet already carved from the pool still fits " +
					"inside the new block, and the new block does not overlap another pool. A resize that would strand a subnet " +
					"is rejected with an error rather than applied.",
				// Deliberately no RequiresReplace. It used to have one, which
				// made any CIDR change a destroy-and-create, and a pool
				// holding subnets cannot be destroyed: the apply failed and
				// left no way forward. Resizing is now a real API operation,
				// so this is an ordinary in-place update.
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
			"metadata": schema.MapAttribute{
				ElementType: types.StringType,
				Optional:    true,
				Computed:    true,
				Description: "Free-form key/value tags for this pool (e.g. owner, cost_center), not " +
					"interpreted by nxip, stored and returned as-is. Two keys are read by the nxip GUI: " +
					"\"latitude\" and \"longitude\" place this pool on the world map, which is how an on-prem " +
					"site whose region isn't a recognized cloud region gets plotted. Computed as well as " +
					"Optional so a config that never sets this reads back as an empty map rather than null, " +
					"matching what the API returns. Updated in place via PATCH /v1/pools/:id - a full replace " +
					"of the whole map, not a merge, same as the API itself, but does not force a new resource.",
				PlanModifiers: []planmodifier.Map{
					// Without this, a config that never sets metadata plans as
					// "(known after apply)" on every apply, not just the first,
					// since nothing tells the framework a value it already knows
					// is stable. On this resource that would be worse than plan
					// noise: every other attribute here is RequiresReplace, so a
					// spurious unknown would force a pool to be destroyed and
					// recreated - taking every subnet in it along with it.
					mapplanmodifier.UseStateForUnknown(),
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
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	CIDR        string            `json:"cidr"`
	Family      string            `json:"family"`
	Environment string            `json:"environment"`
	Region      string            `json:"region"`
	Metadata    map[string]string `json:"metadata"`
}

// applyPoolResponse copies API response fields into the resource model,
// shared by Create, Read, and Update so the three can't drift apart on which
// fields get synced back into state.
func applyPoolResponse(ctx context.Context, model *PoolResourceModel, result poolResponse) diag.Diagnostics {
	model.ID = types.StringValue(result.ID)
	model.Name = types.StringValue(result.Name)
	model.CIDR = types.StringValue(result.CIDR)
	model.Family = types.StringValue(result.Family)
	model.Environment = types.StringValue(result.Environment)
	model.Region = types.StringValue(result.Region)

	// Must be set from the response even when the config omitted metadata:
	// the attribute is Computed, so leaving it unknown after apply is the
	// "provider produced inconsistent result after apply" error, not an
	// empty map. MapValueFrom on a nil map yields an empty map, which is
	// exactly what the API returns for a pool with no metadata.
	metadataValue, diags := types.MapValueFrom(ctx, types.StringType, result.Metadata)
	model.Metadata = metadataValue
	return diags
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
	if !plan.Metadata.IsNull() && !plan.Metadata.IsUnknown() {
		var metadata map[string]string
		resp.Diagnostics.Append(plan.Metadata.ElementsAs(ctx, &metadata, false)...)
		if resp.Diagnostics.HasError() {
			return
		}
		payload["metadata"] = metadata
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

	resp.Diagnostics.Append(applyPoolResponse(ctx, &plan, result)...)
	if resp.Diagnostics.HasError() {
		return
	}

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

	// An organization-not-found 404 means the wrong organization was asked
	// about, not that this pool was deleted, so it is checked, and
	// reported as an error, before the ordinary 404 handling below - which
	// would otherwise silently drop a possibly still-live pool from state.
	if isOrganizationNotFound(status, apiMessage) {
		resp.Diagnostics.AddError("API Error", apiErrorSummary("failed to fetch pool", status, apiMessage))
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

	resp.Diagnostics.Append(applyPoolResponse(ctx, &state, result)...)
	if resp.Diagnostics.HasError() {
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Update is only ever reached for a change to metadata: name, cidr, family,
// environment and region are all still RequiresReplace, since those genuinely
// are immutable server-side. metadata alone is patchable via
// PATCH /v1/pools/:id, so tagging a pool with a latitude/longitude no longer
// means destroying it, and every subnet inside it, to record where it is.
func (r *PoolResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan PoolResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// The API requires metadata on this endpoint, so an unset map is sent as
	// an explicit empty object rather than omitted - which is also the
	// correct semantics: clearing the block in config means "no tags".
	metadata := map[string]string{}
	if !plan.Metadata.IsNull() && !plan.Metadata.IsUnknown() {
		resp.Diagnostics.Append(plan.Metadata.ElementsAs(ctx, &metadata, false)...)
		if resp.Diagnostics.HasError() {
			return
		}
	}
	// cidr travels with metadata now that pools can be resized. Sent
	// unconditionally: it is Required, so the plan always carries a value,
	// and the API treats an unchanged cidr as a no-op.
	payload := map[string]any{"metadata": metadata, "cidr": plan.CIDR.ValueString()}

	var result poolResponse
	status, apiMessage, err := r.client.do(ctx, http.MethodPatch, "/v1/pools/"+plan.ID.ValueString(), payload, &result)
	if err != nil {
		resp.Diagnostics.AddError("Client Error", err.Error())
		return
	}
	if status != http.StatusOK {
		resp.Diagnostics.AddError("API Error", apiErrorSummary("failed to update pool", status, apiMessage))
		return
	}

	resp.Diagnostics.Append(applyPoolResponse(ctx, &plan, result)...)
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

	// An organization-not-found 404 means the wrong organization was asked
	// about, not that the pool was successfully deleted from the right
	// one - checked before the ordinary 404-means-idempotent-delete
	// handling below, which would otherwise report success for a pool
	// this request never actually reached.
	if isOrganizationNotFound(status, apiMessage) {
		resp.Diagnostics.AddError("API Error", apiErrorSummary("failed to delete pool", status, apiMessage))
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
				" Destroy any nxip_subnet resources referencing this pool first.",
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
