package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// Unit tests proving Read and Delete treat an organization-not-found 404
// (isOrganizationNotFound in client.go) differently from an ordinary "this
// resource doesn't exist" 404 (docs/specs/msp-tenancy-phase2.md Part B): the
// former must surface as an error, never as "already gone" - Read must
// leave state untouched, Delete must not report a silent success. Only
// nxip_pool is exercised directly; nxip_subnet and nxip_address gained the
// identical two checks in the same two methods.

// poolReadDeleteFixture builds a PoolResource plus a tfsdk.State already
// populated with one pool, matching what Terraform would pass into Read or
// Delete for a resource already in state - built from the resource's own
// Schema(), the same way configureTestProvider in organization_test.go
// builds a provider Config from the provider's own Schema().
func poolReadDeleteFixture(t *testing.T, id string) (*PoolResource, tfsdk.State) {
	t.Helper()
	ctx := context.Background()
	r := &PoolResource{}

	schemaResp := &resource.SchemaResponse{}
	r.Schema(ctx, resource.SchemaRequest{}, schemaResp)

	objectType := schemaResp.Schema.Type().TerraformType(ctx)
	raw := tftypes.NewValue(objectType, map[string]tftypes.Value{
		"id":          tftypes.NewValue(tftypes.String, id),
		"name":        tftypes.NewValue(tftypes.String, "prod-us-east"),
		"cidr":        tftypes.NewValue(tftypes.String, "10.0.0.0/16"),
		"family":      tftypes.NewValue(tftypes.String, "IPV4"),
		"environment": tftypes.NewValue(tftypes.String, "production"),
		"region":      tftypes.NewValue(tftypes.String, "us-east-1"),
		"metadata":    tftypes.NewValue(tftypes.Map{ElementType: tftypes.String}, map[string]tftypes.Value{}),
	})

	return r, tfsdk.State{Raw: raw, Schema: schemaResp.Schema}
}

func diagsContainDetail(diags diag.Diagnostics, substr string) bool {
	for _, d := range diags {
		if strings.Contains(d.Detail(), substr) {
			return true
		}
	}
	return false
}

func newNotFoundServer(t *testing.T, message string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"statusCode":404,"error":"Not Found","message":"` + message + `"}`))
	}))
}

func TestPoolResourceRead_OrganizationNotFoundLeavesStateUntouched(t *testing.T) {
	server := newNotFoundServer(t, "Organization not found.")
	defer server.Close()

	r, state := poolReadDeleteFixture(t, "pool_123")
	r.client = &nxipClient{baseURL: server.URL, apiKey: "key", organization: "org_missing", http: server.Client()}

	readResp := &resource.ReadResponse{State: state}
	r.Read(context.Background(), resource.ReadRequest{State: state}, readResp)

	if !readResp.Diagnostics.HasError() {
		t.Fatalf("expected an error diagnostic, got none: %v", readResp.Diagnostics)
	}
	if !diagsContainDetail(readResp.Diagnostics, "organization") {
		t.Fatalf("expected a diagnostic naming the organization attribute, got: %v", readResp.Diagnostics)
	}

	// The whole point: state must not be cleared as if the pool were gone -
	// it might still be perfectly live in the customer organization the
	// request never actually reached.
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

func TestPoolResourceRead_OrdinaryNotFoundStillRemovesFromState(t *testing.T) {
	// Same status code, but the API's ordinary "this pool doesn't exist"
	// message, not the organization one - existing drift-detection
	// behaviour must be unchanged.
	server := newNotFoundServer(t, "Pool not found.")
	defer server.Close()

	r, state := poolReadDeleteFixture(t, "pool_123")
	r.client = &nxipClient{baseURL: server.URL, apiKey: "key", organization: "org_customerA", http: server.Client()}

	readResp := &resource.ReadResponse{State: state}
	r.Read(context.Background(), resource.ReadRequest{State: state}, readResp)

	if readResp.Diagnostics.HasError() {
		t.Fatalf("expected no error diagnostic for an ordinary not-found, got: %v", readResp.Diagnostics)
	}
	if !readResp.State.Raw.IsNull() {
		t.Fatalf("expected state to be removed (drift: deleted outside Terraform), but it was kept")
	}
}

func TestPoolResourceDelete_OrganizationNotFoundIsAnError(t *testing.T) {
	server := newNotFoundServer(t, "Organization not found.")
	defer server.Close()

	r, state := poolReadDeleteFixture(t, "pool_123")
	r.client = &nxipClient{baseURL: server.URL, apiKey: "key", organization: "org_missing", http: server.Client()}

	deleteResp := &resource.DeleteResponse{}
	r.Delete(context.Background(), resource.DeleteRequest{State: state}, deleteResp)

	if !deleteResp.Diagnostics.HasError() {
		t.Fatalf("expected delete to report an error rather than silently succeed, got: %v", deleteResp.Diagnostics)
	}
	if !diagsContainDetail(deleteResp.Diagnostics, "organization") {
		t.Fatalf("expected a diagnostic naming the organization attribute, got: %v", deleteResp.Diagnostics)
	}
}

func TestPoolResourceDelete_OrdinaryNotFoundStillIdempotent(t *testing.T) {
	server := newNotFoundServer(t, "Pool not found.")
	defer server.Close()

	r, state := poolReadDeleteFixture(t, "pool_123")
	r.client = &nxipClient{baseURL: server.URL, apiKey: "key", organization: "org_customerA", http: server.Client()}

	deleteResp := &resource.DeleteResponse{}
	r.Delete(context.Background(), resource.DeleteRequest{State: state}, deleteResp)

	if deleteResp.Diagnostics.HasError() {
		t.Fatalf("expected an ordinary not-found delete to still be treated as idempotent success, got: %v", deleteResp.Diagnostics)
	}
}
