package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	fwresource "github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// Tests for editing an address in place (docs/specs/address-tree-and-editing.md
// in net-saas-monorepo, part A). hostname and status used to be
// RequiresReplace, so a hostname corrected in the dashboard made the next
// plan want to release the address and register it again. They are now
// updated through PATCH /v1/addresses/:id.
//
// These run real Terraform plans and applies (resource.UnitTest, so no
// TF_ACC and no running nxip API) against fakeAddressAPI below, an
// httptest server that keeps addresses in memory and answers the way the
// API does. The plan checks assert the planned action itself, Update or
// Replace, so a replace that happened to land on the same IP cannot pass
// for an in-place update.

// fakeAddressAPI is the slice of the nxip API that nxip_address talks to:
// POST /v1/subnets/:id/addresses, and GET, PATCH and DELETE
// /v1/addresses/:id. It records every PATCH body and counts registrations
// and releases, which is how a test tells an update from a replace after
// the fact.
type fakeAddressAPI struct {
	mu        sync.Mutex
	server    *httptest.Server
	addresses map[string]map[string]any
	nextID    int
	patches   []map[string]any
	creates   int
	deletes   int
}

func newFakeAddressAPI(t *testing.T) *fakeAddressAPI {
	t.Helper()
	f := &fakeAddressAPI{addresses: map[string]map[string]any{}}
	f.server = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.server.Close)
	return f
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeAPIError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"statusCode": status, "error": http.StatusText(status), "message": message})
}

func (f *fakeAddressAPI) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	var body map[string]any
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}

	if r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/subnets/") && strings.HasSuffix(r.URL.Path, "/addresses") {
		subnetID := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/subnets/"), "/addresses")
		f.nextID++
		f.creates++
		status := "ACTIVE"
		if s, ok := body["status"].(string); ok {
			status = s
		}
		metadata := map[string]any{}
		if m, ok := body["metadata"].(map[string]any); ok {
			metadata = m
		}
		address := map[string]any{
			"id":       fmt.Sprintf("addr_%d", f.nextID),
			"subnetId": subnetID,
			"address":  body["address"],
			"family":   "IPV4",
			"status":   status,
			"hostname": body["hostname"], // nil, so JSON null, when absent
			"metadata": metadata,
		}
		f.addresses[address["id"].(string)] = address
		writeJSON(w, http.StatusCreated, address)
		return
	}

	if !strings.HasPrefix(r.URL.Path, "/v1/addresses/") {
		writeAPIError(w, http.StatusNotFound, "Route not found")
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/v1/addresses/")
	address, ok := f.addresses[id]
	if !ok {
		writeAPIError(w, http.StatusNotFound, fmt.Sprintf("Address with ID '%s' not found in your organization.", id))
		return
	}

	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, address)
	case http.MethodPatch:
		// The same rules the API's patchAddressBody enforces: at least one
		// field, and nothing but hostname, status and metadata.
		if len(body) == 0 {
			writeAPIError(w, http.StatusBadRequest, "At least one of `hostname`, `status`, or `metadata` is required.")
			return
		}
		for key := range body {
			if key != "hostname" && key != "status" && key != "metadata" {
				writeAPIError(w, http.StatusBadRequest, "Unrecognized key: "+key)
				return
			}
		}
		f.patches = append(f.patches, body)
		for key, value := range body {
			address[key] = value
		}
		writeJSON(w, http.StatusOK, address)
	case http.MethodDelete:
		f.deletes++
		delete(f.addresses, id)
		writeJSON(w, http.StatusOK, map[string]any{"message": "Address released successfully", "id": id, "releasedAddress": address["address"]})
	default:
		writeAPIError(w, http.StatusMethodNotAllowed, "Method not allowed")
	}
}

// lastPatch returns the most recent PATCH body, or nil if there was none.
func (f *fakeAddressAPI) lastPatch() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.patches) == 0 {
		return nil
	}
	return f.patches[len(f.patches)-1]
}

func (f *fakeAddressAPI) counts() (patches, creates, deletes int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.patches), f.creates, f.deletes
}

// setHostnameOutOfBand stands in for someone correcting the hostname in the
// dashboard, behind Terraform's back.
func (f *fakeAddressAPI) setHostnameOutOfBand(hostname string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, address := range f.addresses {
		address["hostname"] = hostname
	}
}

// fakeAddressConfig is one nxip_address with the given optional attribute
// lines (for example `hostname = "web-01"`). The subnet id is a literal:
// the fake API has no subnets to check it against, and none are needed.
func fakeAddressConfig(url, address string, lines ...string) string {
	return fmt.Sprintf(`
provider "nxip" {
  api_key = "nc_live_fake"
  url     = %q
}

resource "nxip_address" "test" {
  subnet_id = "sub_fake"
  address   = %q
  %s
}
`, url, address, strings.Join(lines, "\n  "))
}

// expectPatch checks the last PATCH body was exactly want, and that the
// counts of PATCHes, registrations and releases are what an in-place update
// leaves behind: the given number of PATCHes, one registration, no release.
func expectPatch(f *fakeAddressAPI, want map[string]any, wantPatches int) resource.TestCheckFunc {
	return func(*terraform.State) error {
		patches, creates, deletes := f.counts()
		if patches != wantPatches || creates != 1 || deletes != 0 {
			return fmt.Errorf("want %d PATCHes, 1 registration and 0 releases, got %d, %d and %d", wantPatches, patches, creates, deletes)
		}
		if got := f.lastPatch(); !reflect.DeepEqual(got, want) {
			return fmt.Errorf("want last PATCH body %v, got %v", want, got)
		}
		return nil
	}
}

func expectAction(action plancheck.ResourceActionType) resource.ConfigPlanChecks {
	return resource.ConfigPlanChecks{
		PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction("nxip_address.test", action)},
	}
}

// TestAddressResource_hostnameAndStatusUpdateInPlace walks one address
// through each edit the spec names, and checks both the planned action and
// the exact PATCH body each apply sent.
func TestAddressResource_hostnameAndStatusUpdateInPlace(t *testing.T) {
	f := newFakeAddressAPI(t)
	url := f.server.URL

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: fakeAddressConfig(url, "10.0.0.10", `hostname = "web-01"`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("nxip_address.test", "hostname", "web-01"),
					resource.TestCheckResourceAttr("nxip_address.test", "status", "ACTIVE"),
				),
			},
			// A hostname change is an Update, and sends only hostname.
			{
				Config:           fakeAddressConfig(url, "10.0.0.10", `hostname = "web-02"`),
				ConfigPlanChecks: expectAction(plancheck.ResourceActionUpdate),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("nxip_address.test", "hostname", "web-02"),
					resource.TestCheckResourceAttr("nxip_address.test", "id", "addr_1"),
					expectPatch(f, map[string]any{"hostname": "web-02"}, 1),
				),
			},
			// A status change is an Update, and sends only status.
			{
				Config:           fakeAddressConfig(url, "10.0.0.10", `hostname = "web-02"`, `status = "RESERVED"`),
				ConfigPlanChecks: expectAction(plancheck.ResourceActionUpdate),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("nxip_address.test", "status", "RESERVED"),
					expectPatch(f, map[string]any{"status": "RESERVED"}, 2),
				),
			},
			// Removing hostname from config clears it: an Update sending
			// an explicit null, not an absent field.
			{
				Config:           fakeAddressConfig(url, "10.0.0.10", `status = "RESERVED"`),
				ConfigPlanChecks: expectAction(plancheck.ResourceActionUpdate),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckNoResourceAttr("nxip_address.test", "hostname"),
					expectPatch(f, map[string]any{"hostname": nil}, 3),
				),
			},
			// And once cleared, the same config plans clean.
			{
				Config:   fakeAddressConfig(url, "10.0.0.10", `status = "RESERVED"`),
				PlanOnly: true,
			},
			// A metadata change still sends metadata alone, as before.
			{
				Config:           fakeAddressConfig(url, "10.0.0.10", `status = "RESERVED"`, `metadata = { owner = "net" }`),
				ConfigPlanChecks: expectAction(plancheck.ResourceActionUpdate),
				Check:            expectPatch(f, map[string]any{"metadata": map[string]any{"owner": "net"}}, 4),
			},
		},
	})
}

// TestAddressResource_addressChangeStillReplaces is the other side: the
// address itself is not editable, so changing it must still plan a
// replacement (a release and a fresh registration), never a PATCH.
func TestAddressResource_addressChangeStillReplaces(t *testing.T) {
	f := newFakeAddressAPI(t)
	url := f.server.URL

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: fakeAddressConfig(url, "10.0.0.10", `hostname = "web-01"`),
			},
			{
				Config:           fakeAddressConfig(url, "10.0.0.11", `hostname = "web-01"`),
				ConfigPlanChecks: expectAction(plancheck.ResourceActionReplace),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("nxip_address.test", "address", "10.0.0.11"),
					func(*terraform.State) error {
						patches, creates, deletes := f.counts()
						if patches != 0 || creates != 2 || deletes != 1 {
							return fmt.Errorf("want 0 PATCHes, 2 registrations and 1 release, got %d, %d and %d", patches, creates, deletes)
						}
						return nil
					},
				),
			},
		},
	})
}

// TestAddressResource_dashboardHostnameEditIsNotAReplace is the case the
// spec gives as the reason the provider must change with the API: a
// hostname corrected in the dashboard. The next plan must want an in-place
// update back to what config says, not a destroy and recreate.
func TestAddressResource_dashboardHostnameEditIsNotAReplace(t *testing.T) {
	f := newFakeAddressAPI(t)
	url := f.server.URL
	config := fakeAddressConfig(url, "10.0.0.10", `hostname = "web-01"`)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: config,
			},
			{
				PreConfig:        func() { f.setHostnameOutOfBand("web-01-typo-fixed") },
				Config:           config,
				ConfigPlanChecks: expectAction(plancheck.ResourceActionUpdate),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("nxip_address.test", "hostname", "web-01"),
					expectPatch(f, map[string]any{"hostname": "web-01"}, 1),
				),
			},
		},
	})
}

// TestAddressUpdatePayload covers addressUpdatePayload directly, including
// the empty case a real plan never reaches but Update still guards.
func TestAddressUpdatePayload(t *testing.T) {
	ctx := context.Background()
	metadata := func(values map[string]string) types.Map {
		m, diags := types.MapValueFrom(ctx, types.StringType, values)
		if diags.HasError() {
			t.Fatalf("building metadata: %v", diags)
		}
		return m
	}
	base := AddressResourceModel{
		Hostname: types.StringValue("web-01"),
		Status:   types.StringValue("ACTIVE"),
		Metadata: metadata(map[string]string{"owner": "net"}),
	}

	cases := []struct {
		name string
		edit func(m *AddressResourceModel)
		want map[string]any
	}{
		{"nothing changed", func(m *AddressResourceModel) {}, map[string]any{}},
		{"hostname changed", func(m *AddressResourceModel) { m.Hostname = types.StringValue("web-02") }, map[string]any{"hostname": "web-02"}},
		{"hostname removed", func(m *AddressResourceModel) { m.Hostname = types.StringNull() }, map[string]any{"hostname": nil}},
		{"status changed", func(m *AddressResourceModel) { m.Status = types.StringValue("RESERVED") }, map[string]any{"status": "RESERVED"}},
		{"metadata changed", func(m *AddressResourceModel) { m.Metadata = metadata(map[string]string{"owner": "app"}) },
			map[string]any{"metadata": map[string]string{"owner": "app"}}},
		{"all three changed", func(m *AddressResourceModel) {
			m.Hostname = types.StringValue("db-01")
			m.Status = types.StringValue("RESERVED")
			m.Metadata = metadata(map[string]string{})
		}, map[string]any{"hostname": "db-01", "status": "RESERVED", "metadata": map[string]string{}}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan := base
			tc.edit(&plan)
			got, diags := addressUpdatePayload(ctx, plan, base)
			if diags.HasError() {
				t.Fatalf("unexpected diagnostics: %v", diags)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("want %#v, got %#v", tc.want, got)
			}
		})
	}
}

// TestAddressUpdate_noChangeSendsNothing drives Update itself with a plan
// identical to state. The API answers an empty PATCH with a 400, so Update
// must not send one: it must make no request at all and keep the state.
func TestAddressUpdate_noChangeSendsNothing(t *testing.T) {
	ctx := context.Background()
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		writeAPIError(w, http.StatusBadRequest, "At least one of `hostname`, `status`, or `metadata` is required.")
	}))
	t.Cleanup(server.Close)

	r := &AddressResource{client: newNxipClient(&NxipProviderModel{
		APIKey: types.StringValue("nc_live_fake"),
		URL:    types.StringValue(server.URL),
	})}
	schemaResp := &fwresource.SchemaResponse{}
	r.Schema(ctx, fwresource.SchemaRequest{}, schemaResp)
	objectType := schemaResp.Schema.Type().TerraformType(ctx)
	raw := tftypes.NewValue(objectType, map[string]tftypes.Value{
		"id":        tftypes.NewValue(tftypes.String, "addr_1"),
		"subnet_id": tftypes.NewValue(tftypes.String, "sub_fake"),
		"address":   tftypes.NewValue(tftypes.String, "10.0.0.10"),
		"family":    tftypes.NewValue(tftypes.String, "IPV4"),
		"status":    tftypes.NewValue(tftypes.String, "ACTIVE"),
		"hostname":  tftypes.NewValue(tftypes.String, "web-01"),
		"metadata":  tftypes.NewValue(tftypes.Map{ElementType: tftypes.String}, map[string]tftypes.Value{}),
	})

	updateResp := &fwresource.UpdateResponse{State: tfsdk.State{Raw: raw, Schema: schemaResp.Schema}}
	r.Update(ctx, fwresource.UpdateRequest{
		Plan:  tfsdk.Plan{Raw: raw, Schema: schemaResp.Schema},
		State: tfsdk.State{Raw: raw, Schema: schemaResp.Schema},
	}, updateResp)

	if updateResp.Diagnostics.HasError() {
		t.Fatalf("unexpected diagnostics: %v", updateResp.Diagnostics)
	}
	if requests != 0 {
		t.Fatalf("want no request to the API, got %d", requests)
	}
	var state AddressResourceModel
	updateResp.Diagnostics.Append(updateResp.State.Get(ctx, &state)...)
	if state.Hostname.ValueString() != "web-01" || state.ID.ValueString() != "addr_1" {
		t.Fatalf("state changed: %+v", state)
	}
}
