package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// Unit tests for the hints that link a failed request to the
// troubleshooting page (docs/specs/troubleshooting-page.md in
// net-saas-monorepo). Each drives a real resource method against an
// httptest server that answers the way the API does, and checks the
// diagnostic Terraform would print.
//
// The expected links are written out in full here on purpose, not built
// from troubleshootingLink or the anchor constants: a released provider
// prints these exact URLs and the page promises to keep them, so a test
// that rebuilt them from the same constants would pass straight through an
// accidental rename.
const (
	wantNoMatchingPoolLink       = "https://nx-ip.com/docs/troubleshooting#no-matching-pool"
	wantPoolReplacedLink         = "https://nx-ip.com/docs/troubleshooting#pool-replaced"
	wantOrganizationNotFoundLink = "https://nx-ip.com/docs/troubleshooting#organization-not-found"
)

// The API's own messages, copied from net-saas-monorepo's apps/api/src
// (routes/subnets.ts for the two subnet 404s, routes/pools.ts for the
// pool delete refusal) with example values filled in.
const (
	apiNoMatchingPoolMessage = "No matching IPV4 IP pool found for environment 'preprod' in region 'ukwest'. Please create a pool first."
	apiParentNotFoundMessage = "parentSubnetId 'sub_missing' does not reference an existing subnet in your organization."
	apiPoolHasSubnetsMessage = "Cannot delete IP Pool 'preprod-uk-west'. It currently has 2 active subnet(s). Please release the subnets first."
)

// newErrorServer answers every request with one status and one API error
// message, in the API's ErrorResponse shape.
func newErrorServer(t *testing.T, status int, message string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"statusCode":` + strconv.Itoa(status) + `,"error":"` + http.StatusText(status) + `","message":"` + message + `"}`))
	}))
}

// subnetCreateFixture builds a SubnetResource plus the Plan and empty State
// Terraform would pass into Create for a new subnet, built from the
// resource's own Schema() the same way poolReadDeleteFixture builds a pool.
// Computed attributes the config leaves unset are unknown, exactly as they
// are in a real plan. parentSubnetID is "" for a subnet routed by
// environment/region/family.
func subnetCreateFixture(t *testing.T, parentSubnetID string) (*SubnetResource, tfsdk.Plan, tfsdk.State) {
	t.Helper()
	ctx := context.Background()
	r := &SubnetResource{}

	schemaResp := &resource.SchemaResponse{}
	r.Schema(ctx, resource.SchemaRequest{}, schemaResp)
	objectType := schemaResp.Schema.Type().TerraformType(ctx)

	parent := tftypes.NewValue(tftypes.String, tftypes.UnknownValue)
	environment := tftypes.NewValue(tftypes.String, "preprod")
	region := tftypes.NewValue(tftypes.String, "ukwest")
	if parentSubnetID != "" {
		parent = tftypes.NewValue(tftypes.String, parentSubnetID)
		environment = tftypes.NewValue(tftypes.String, tftypes.UnknownValue)
		region = tftypes.NewValue(tftypes.String, tftypes.UnknownValue)
	}

	raw := tftypes.NewValue(objectType, map[string]tftypes.Value{
		"id":               tftypes.NewValue(tftypes.String, tftypes.UnknownValue),
		"environment":      environment,
		"region":           region,
		"family":           tftypes.NewValue(tftypes.String, "IPV4"),
		"prefix_length":    tftypes.NewValue(tftypes.Number, 24),
		"parent_subnet_id": parent,
		"kind":             tftypes.NewValue(tftypes.String, nil),
		"name":             tftypes.NewValue(tftypes.String, "legacy-app"),
		"description":      tftypes.NewValue(tftypes.String, nil),
		"cidr":             tftypes.NewValue(tftypes.String, tftypes.UnknownValue),
		"metadata":         tftypes.NewValue(tftypes.Map{ElementType: tftypes.String}, tftypes.UnknownValue),
	})

	plan := tfsdk.Plan{Raw: raw, Schema: schemaResp.Schema}
	state := tfsdk.State{Raw: tftypes.NewValue(objectType, nil), Schema: schemaResp.Schema}
	return r, plan, state
}

func runSubnetCreate(t *testing.T, r *SubnetResource, plan tfsdk.Plan, state tfsdk.State) diag.Diagnostics {
	t.Helper()
	createResp := &resource.CreateResponse{State: state}
	r.Create(context.Background(), resource.CreateRequest{Plan: plan}, createResp)
	if !createResp.Diagnostics.HasError() {
		t.Fatalf("expected an error diagnostic, got none")
	}
	return createResp.Diagnostics
}

func diagsContainSummary(diags diag.Diagnostics, substr string) bool {
	for _, d := range diags {
		if strings.Contains(d.Summary(), substr) {
			return true
		}
	}
	return false
}

// diagsMention checks summary and detail together, for assertions that a
// hint appears nowhere at all.
func diagsMention(diags diag.Diagnostics, substr string) bool {
	return diagsContainSummary(diags, substr) || diagsContainDetail(diags, substr)
}

func TestSubnetCreate_NoMatchingPoolHint(t *testing.T) {
	server := newErrorServer(t, http.StatusNotFound, apiNoMatchingPoolMessage)
	defer server.Close()

	r, plan, state := subnetCreateFixture(t, "")
	r.client = &nxipClient{baseURL: server.URL, apiKey: "key", http: server.Client()}
	diags := runSubnetCreate(t, r, plan, state)

	if !diagsContainSummary(diags, "No matching pool for `preprod` / `ukwest` / `IPV4`") {
		t.Fatalf("expected the summary to name environment, region and family, got: %v", diags)
	}
	// The API's own message must survive verbatim, so nothing someone
	// already searches for is lost.
	if !diagsContainDetail(diags, apiNoMatchingPoolMessage) {
		t.Fatalf("expected the API message verbatim in the detail, got: %v", diags)
	}
	if !diagsContainDetail(diags, "environment = nxip_pool.<name>.environment") {
		t.Fatalf("expected the reference hint in the detail, got: %v", diags)
	}
	if !diagsContainDetail(diags, wantNoMatchingPoolLink) {
		t.Fatalf("expected the %s link in the detail, got: %v", wantNoMatchingPoolLink, diags)
	}
}

func TestSubnetCreate_ParentNotFoundHasNoPoolHint(t *testing.T) {
	server := newErrorServer(t, http.StatusNotFound, apiParentNotFoundMessage)
	defer server.Close()

	r, plan, state := subnetCreateFixture(t, "sub_missing")
	r.client = &nxipClient{baseURL: server.URL, apiKey: "key", http: server.Client()}
	diags := runSubnetCreate(t, r, plan, state)

	// No pool is involved when parent_subnet_id is set, so neither the pool
	// summary, the reference hint nor the pool link may appear.
	for _, forbidden := range []string{"No matching pool", "nxip_pool.<name>", wantNoMatchingPoolLink} {
		if diagsMention(diags, forbidden) {
			t.Fatalf("expected no pool hint (%q) for a parent_subnet_id 404, got: %v", forbidden, diags)
		}
	}
	if !diagsContainDetail(diags, apiParentNotFoundMessage) {
		t.Fatalf("expected the API message verbatim in the detail, got: %v", diags)
	}
	if !diagsContainDetail(diags, `parent_subnet_id ("sub_missing") must be the id of an existing subnet in the organization this API key belongs to`) {
		t.Fatalf("expected the parent_subnet_id hint, got: %v", diags)
	}
}

func TestSubnetCreate_ParentNotFoundNamesConfiguredOrganization(t *testing.T) {
	server := newErrorServer(t, http.StatusNotFound, apiParentNotFoundMessage)
	defer server.Close()

	r, plan, state := subnetCreateFixture(t, "sub_missing")
	r.client = &nxipClient{baseURL: server.URL, apiKey: "key", organization: "org_customer", http: server.Client()}
	diags := runSubnetCreate(t, r, plan, state)

	if !diagsContainDetail(diags, "the organization set by the `organization` attribute (\"org_customer\")") {
		t.Fatalf("expected the hint to name the configured organization, got: %v", diags)
	}
}

func TestSubnetCreate_OrganizationNotFoundIsNeverNoMatchingPool(t *testing.T) {
	// Same status, same route, no parent_subnet_id: everything the
	// no-matching-pool hint keys on, except the message.
	server := newErrorServer(t, http.StatusNotFound, "Organization not found.")
	defer server.Close()

	r, plan, state := subnetCreateFixture(t, "")
	r.client = &nxipClient{baseURL: server.URL, apiKey: "key", organization: "org_missing", http: server.Client()}
	diags := runSubnetCreate(t, r, plan, state)

	for _, forbidden := range []string{"No matching pool", "nxip_pool.<name>", wantNoMatchingPoolLink} {
		if diagsMention(diags, forbidden) {
			t.Fatalf("an organization-not-found 404 was given the pool hint (%q): %v", forbidden, diags)
		}
	}
	if !diagsContainDetail(diags, "the `organization` attribute (\"org_missing\")") {
		t.Fatalf("expected the organization hint, got: %v", diags)
	}
	if !diagsContainDetail(diags, wantOrganizationNotFoundLink) {
		t.Fatalf("expected the %s link, got: %v", wantOrganizationNotFoundLink, diags)
	}
}

func TestPoolRead_OrganizationNotFoundLinksAndKeepsState(t *testing.T) {
	server := newNotFoundServer(t, "Organization not found.")
	defer server.Close()

	r, state := poolReadDeleteFixture(t, "pool_123")
	r.client = &nxipClient{baseURL: server.URL, apiKey: "key", organization: "org_missing", http: server.Client()}

	readResp := &resource.ReadResponse{State: state}
	r.Read(context.Background(), resource.ReadRequest{State: state}, readResp)

	if !readResp.Diagnostics.HasError() {
		t.Fatalf("expected an error diagnostic, got none")
	}
	if !diagsContainDetail(readResp.Diagnostics, wantOrganizationNotFoundLink) {
		t.Fatalf("expected the %s link, got: %v", wantOrganizationNotFoundLink, readResp.Diagnostics)
	}

	// The link is appended to the rewritten message, which Read's
	// isOrganizationNotFound check matches on. If appending it ever broke
	// that match, Read would fall through to its ordinary 404 handling and
	// drop a possibly still-live pool from state.
	if readResp.State.Raw.IsNull() {
		t.Fatalf("expected state to be left untouched, but Read cleared it")
	}
	var out PoolResourceModel
	if diags := readResp.State.Get(context.Background(), &out); diags.HasError() {
		t.Fatalf("unexpected error reading back state: %v", diags)
	}
	if out.ID.ValueString() != "pool_123" {
		t.Fatalf("expected state to still have id %q, got %q", "pool_123", out.ID.ValueString())
	}
}

func TestPoolDelete_HoldsSubnetsGivesReplacementHint(t *testing.T) {
	// The API refuses this with 400, not 409: see the pool-has-subnets
	// branch of DELETE /v1/pools/:id in routes/pools.ts.
	server := newErrorServer(t, http.StatusBadRequest, apiPoolHasSubnetsMessage)
	defer server.Close()

	r, state := poolReadDeleteFixture(t, "pool_123")
	r.client = &nxipClient{baseURL: server.URL, apiKey: "key", http: server.Client()}

	deleteResp := &resource.DeleteResponse{}
	r.Delete(context.Background(), resource.DeleteRequest{State: state}, deleteResp)

	if !deleteResp.Diagnostics.HasError() {
		t.Fatalf("expected an error diagnostic, got none")
	}
	if !diagsContainDetail(deleteResp.Diagnostics, apiPoolHasSubnetsMessage) {
		t.Fatalf("expected the API message verbatim in the detail, got: %v", deleteResp.Diagnostics)
	}
	if !diagsContainDetail(deleteResp.Diagnostics, "If Terraform is replacing this pool because its name, environment, region or family changed") {
		t.Fatalf("expected the replacement hint, got: %v", deleteResp.Diagnostics)
	}
	if !diagsContainDetail(deleteResp.Diagnostics, wantPoolReplacedLink) {
		t.Fatalf("expected the %s link, got: %v", wantPoolReplacedLink, deleteResp.Diagnostics)
	}
}
