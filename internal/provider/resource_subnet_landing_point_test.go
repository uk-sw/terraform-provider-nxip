package provider

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"
)

// Tests for landing_point on nxip_subnet (docs/specs/landing-point-flag.md
// in net-saas-monorepo). A kind-tagged top-level subnet used to be a
// landing point by virtue of its kind alone, so a virtual pod range tagged
// "pod-cidr" next to a real region block gave every ordinary request two
// landing points and a 409. landing_point separates the two: it is
// Optional+Computed, defaults server-side to true for a kind-tagged
// top-level subnet, and flips in place through PATCH /v1/subnets/:id.
//
// These run real Terraform plans and applies (resource.UnitTest, so no
// TF_ACC and no running nxip API) against fakeSubnetAPI below, an httptest
// server that keeps subnets in memory and applies the spec's rules for the
// field: the default when it is absent, and the 400 for true without kind
// or with a parent. Every POST and PATCH body is recorded, because the
// thing most likely to go wrong is the body itself: an Optional+Computed
// bool left unset in config is unknown at create, and ValueBool() on an
// unknown quietly returns false, which sent to the API would turn every
// pre-existing region block into a non-landing subnet.

// fakeSubnetAPI is the slice of the nxip API that nxip_subnet talks to:
// POST /v1/subnets, and GET, PATCH and DELETE /v1/subnets/:id. The POST
// body is kept per created id so a test with several subnets in one config
// can check what each one sent.
type fakeSubnetAPI struct {
	mu         sync.Mutex
	server     *httptest.Server
	subnets    map[string]map[string]any
	nextID     int
	createBody map[string]map[string]any
	patches    []map[string]any
	creates    int
	deletes    int
}

func newFakeSubnetAPI(t *testing.T) *fakeSubnetAPI {
	t.Helper()
	f := &fakeSubnetAPI{subnets: map[string]map[string]any{}, createBody: map[string]map[string]any{}}
	f.server = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.server.Close)
	return f
}

// landingPointRejected mirrors the API's validation for the field: true
// needs kind set and no parentSubnetId. Returned as the plain message the
// API would send, or "" when the combination is allowed.
func landingPointRejected(landingPoint bool, kind, parent any) string {
	if !landingPoint {
		return ""
	}
	if kind == nil {
		return "landingPoint requires kind to be set."
	}
	if parent != nil {
		return "landingPoint is only allowed on a top-level subnet (no parentSubnetId)."
	}
	return ""
}

func (f *fakeSubnetAPI) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	var body map[string]any
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}

	if r.Method == http.MethodPost && r.URL.Path == "/v1/subnets" {
		f.nextID++
		f.creates++
		id := fmt.Sprintf("sub_%d", f.nextID)
		f.createBody[id] = body

		kind := body["kind"] // nil, so JSON null, when absent
		parent := body["parentSubnetId"]

		// The spec's default: decided at the API layer only when the field
		// is absent, so a client that never sends it keeps today's
		// behaviour, and a client that sends false is believed.
		landingPoint := kind != nil && parent == nil
		if v, ok := body[subnetLandingPointField]; ok {
			landingPoint, _ = v.(bool)
		}
		if msg := landingPointRejected(landingPoint, kind, parent); msg != "" {
			writeAPIError(w, http.StatusBadRequest, msg)
			return
		}

		prefixLength := float64(24)
		if v, ok := body["prefixLength"].(float64); ok {
			prefixLength = v
		}
		metadata := map[string]any{}
		if m, ok := body["metadata"].(map[string]any); ok {
			metadata = m
		}
		subnet := map[string]any{
			"id":                    id,
			"cidr":                  fmt.Sprintf("10.0.%d.0/%d", f.nextID, int(prefixLength)),
			"prefixLength":          prefixLength,
			"family":                body["family"],
			"environment":           body["environment"],
			"region":                body["region"],
			"parentSubnetId":        parent,
			"kind":                  kind,
			subnetLandingPointField: landingPoint,
			"name":                  body["name"],
			"description":           body["description"],
			"metadata":              metadata,
		}
		f.subnets[id] = subnet
		writeJSON(w, http.StatusCreated, subnet)
		return
	}

	if !strings.HasPrefix(r.URL.Path, "/v1/subnets/") {
		writeAPIError(w, http.StatusNotFound, "Route not found")
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/v1/subnets/")
	subnet, ok := f.subnets[id]
	if !ok {
		writeAPIError(w, http.StatusNotFound, fmt.Sprintf("Subnet with ID '%s' not found.", id))
		return
	}

	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, subnet)
	case http.MethodPatch:
		// The rules the API's patchSubnetSchema enforces once landingPoint
		// joins it: at least one field, nothing but the four patchable
		// ones, and the same true-needs-kind rule as on create.
		if len(body) == 0 {
			writeAPIError(w, http.StatusBadRequest, "At least one of `name`, `description`, `metadata`, or `landingPoint` is required.")
			return
		}
		for key := range body {
			if key != "name" && key != "description" && key != "metadata" && key != subnetLandingPointField {
				writeAPIError(w, http.StatusBadRequest, "Unrecognized key: "+key)
				return
			}
		}
		if v, ok := body[subnetLandingPointField]; ok {
			landingPoint, _ := v.(bool)
			if msg := landingPointRejected(landingPoint, subnet["kind"], subnet["parentSubnetId"]); msg != "" {
				writeAPIError(w, http.StatusBadRequest, msg)
				return
			}
		}
		f.patches = append(f.patches, body)
		for key, value := range body {
			subnet[key] = value
		}
		writeJSON(w, http.StatusOK, subnet)
	case http.MethodDelete:
		f.deletes++
		delete(f.subnets, id)
		writeJSON(w, http.StatusOK, map[string]any{"message": "Subnet released successfully", "id": id, "releasedCidr": subnet["cidr"]})
	default:
		writeAPIError(w, http.StatusMethodNotAllowed, "Method not allowed")
	}
}

func (f *fakeSubnetAPI) lastPatch() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.patches) == 0 {
		return nil
	}
	return f.patches[len(f.patches)-1]
}

func (f *fakeSubnetAPI) counts() (patches, creates, deletes int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.patches), f.creates, f.deletes
}

// setLandingPointOutOfBand stands in for someone flipping the flag in the
// dashboard, behind Terraform's back.
func (f *fakeSubnetAPI) setLandingPointOutOfBand(landingPoint bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, subnet := range f.subnets {
		subnet[subnetLandingPointField] = landingPoint
	}
}

// fakeSubnetConfig is one or more nxip_subnet resources, each given as its
// Terraform name and the optional attribute lines to add beneath the fixed
// environment/region/family/prefix_length (for example `kind = "region"`).
type fakeSubnet struct {
	name  string
	lines []string
}

func fakeSubnetConfig(url string, subnets ...fakeSubnet) string {
	var b strings.Builder
	fmt.Fprintf(&b, `
provider "nxip" {
  api_key = "nc_live_fake"
  url     = %q
}
`, url)
	for _, s := range subnets {
		fmt.Fprintf(&b, `
resource "nxip_subnet" %q {
  environment   = "production"
  region        = "us-east-1"
  family        = "IPV4"
  prefix_length = 24
  %s
}
`, s.name, strings.Join(s.lines, "\n  "))
	}
	return b.String()
}

// expectSubnetPostBody checks what the create for the named resource sent:
// wantKey says whether landingPoint was in the body at all, and wantValue
// what it was when it was. An unset landing_point must reach the API as an
// absent field, never as false.
func expectSubnetPostBody(f *fakeSubnetAPI, resourceName string, wantKey bool, wantValue bool) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[resourceName]
		if !ok {
			return fmt.Errorf("resource not found in state: %s", resourceName)
		}
		f.mu.Lock()
		body, ok := f.createBody[rs.Primary.ID]
		f.mu.Unlock()
		if !ok {
			return fmt.Errorf("no POST body recorded for %s (id %s)", resourceName, rs.Primary.ID)
		}
		got, present := body[subnetLandingPointField]
		if present != wantKey {
			return fmt.Errorf("want %s present in POST body: %v, got body %v", subnetLandingPointField, wantKey, body)
		}
		if present && got != wantValue {
			return fmt.Errorf("want POST body %s = %v, got %v", subnetLandingPointField, wantValue, got)
		}
		return nil
	}
}

// expectSubnetPatch checks the last PATCH body for landingPoint: wantKey
// says whether it was sent at all, wantValue what it was when it was. The
// counts of PATCHes, creates and deletes must be what an in-place update
// leaves behind: the given number of PATCHes, one create, no delete, so a
// replace that landed on a fresh id would fail even where the plan check
// was somehow bypassed. Only the landingPoint key is pinned, not the whole
// body: Update already resent name, description and metadata whenever they
// are set, before landing_point existed, and that is not this test's to
// change.
func expectSubnetPatch(f *fakeSubnetAPI, wantKey bool, wantValue bool, wantPatches int) resource.TestCheckFunc {
	return func(*terraform.State) error {
		patches, creates, deletes := f.counts()
		if patches != wantPatches || creates != 1 || deletes != 0 {
			return fmt.Errorf("want %d PATCHes, 1 create and 0 deletes, got %d, %d and %d", wantPatches, patches, creates, deletes)
		}
		body := f.lastPatch()
		got, present := body[subnetLandingPointField]
		if present != wantKey {
			return fmt.Errorf("want %s present in PATCH body: %v, got body %v", subnetLandingPointField, wantKey, body)
		}
		if present && got != wantValue {
			return fmt.Errorf("want PATCH body %s = %v, got %v", subnetLandingPointField, wantValue, got)
		}
		return nil
	}
}

func expectSubnetAction(resourceName string, action plancheck.ResourceActionType) resource.ConfigPlanChecks {
	return resource.ConfigPlanChecks{
		PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(resourceName, action)},
	}
}

// TestSubnetResource_landingPointFalseIsSent is the modules' case: a
// kind-tagged virtual range that must sit in the pool without becoming a
// landing point. landing_point = false has to reach the API as an explicit
// false, and a second plan of the same config must be clean.
func TestSubnetResource_landingPointFalseIsSent(t *testing.T) {
	f := newFakeSubnetAPI(t)
	config := fakeSubnetConfig(f.server.URL, fakeSubnet{"pods", []string{`kind = "pod-cidr"`, `landing_point = false`}})

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("nxip_subnet.pods", "kind", "pod-cidr"),
					resource.TestCheckResourceAttr("nxip_subnet.pods", "landing_point", "false"),
					expectSubnetPostBody(f, "nxip_subnet.pods", true, false),
				),
			},
			{
				Config:   config,
				PlanOnly: true,
			},
		},
	})
}

// TestSubnetResource_landingPointUnsetIsNotSent covers every configuration
// written before the attribute existed. Neither subnet mentions
// landing_point, so neither POST body may carry it (the API's default only
// applies to an absent field), and state reads back what the API chose:
// true for the kind-tagged region block, false for the plain subnet. The
// re-plan must be clean, and a later rename must plan landing_point as its
// known state value rather than "(known after apply)": the framework turns
// a computed null into unknown whenever anything else about the resource
// changes, and UseStateForUnknown is what stops that.
func TestSubnetResource_landingPointUnsetIsNotSent(t *testing.T) {
	f := newFakeSubnetAPI(t)
	config := func(lines ...string) string {
		return fakeSubnetConfig(f.server.URL,
			fakeSubnet{"region", append([]string{`kind = "region"`}, lines...)},
			fakeSubnet{"plain", lines},
		)
	}

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: config(),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("nxip_subnet.region", "landing_point", "true"),
					expectSubnetPostBody(f, "nxip_subnet.region", false, false),
					resource.TestCheckResourceAttr("nxip_subnet.plain", "landing_point", "false"),
					expectSubnetPostBody(f, "nxip_subnet.plain", false, false),
				),
			},
			{
				Config:   config(),
				PlanOnly: true,
			},
			{
				Config: config(`name = "renamed"`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction("nxip_subnet.region", plancheck.ResourceActionUpdate),
						plancheck.ExpectKnownValue("nxip_subnet.region", tfjsonpath.New("landing_point"), knownvalue.Bool(true)),
						plancheck.ExpectKnownValue("nxip_subnet.plain", tfjsonpath.New("landing_point"), knownvalue.Bool(false)),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("nxip_subnet.region", "landing_point", "true"),
					resource.TestCheckResourceAttr("nxip_subnet.plain", "landing_point", "false"),
					// Two subnets renamed, so two PATCHes, neither carrying
					// landingPoint: nothing changed it.
					func(*terraform.State) error {
						patches, creates, deletes := f.counts()
						if patches != 2 || creates != 2 || deletes != 0 {
							return fmt.Errorf("want 2 PATCHes, 2 creates and 0 deletes, got %d, %d and %d", patches, creates, deletes)
						}
						f.mu.Lock()
						defer f.mu.Unlock()
						for _, body := range f.patches {
							if _, present := body[subnetLandingPointField]; present {
								return fmt.Errorf("rename PATCH must not carry %s, got body %v", subnetLandingPointField, body)
							}
						}
						return nil
					},
				),
			},
		},
	})
}

// TestSubnetResource_landingPointFlipsInPlace is Done means 8: a change to
// landing_point plans as an Update, never a replace, and applies as a PATCH
// carrying that field. A later rename must not resend it, so the audit row
// for the rename does not claim the landing point was edited.
func TestSubnetResource_landingPointFlipsInPlace(t *testing.T) {
	f := newFakeSubnetAPI(t)
	url := f.server.URL
	region := func(lines ...string) string {
		return fakeSubnetConfig(url, fakeSubnet{"region", append([]string{`kind = "region"`}, lines...)})
	}

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: region(`landing_point = false`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("nxip_subnet.region", "landing_point", "false"),
					resource.TestCheckResourceAttr("nxip_subnet.region", "id", "sub_1"),
				),
			},
			// false to true is an Update, sending landingPoint = true.
			{
				Config:           region(`landing_point = true`),
				ConfigPlanChecks: expectSubnetAction("nxip_subnet.region", plancheck.ResourceActionUpdate),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("nxip_subnet.region", "landing_point", "true"),
					resource.TestCheckResourceAttr("nxip_subnet.region", "id", "sub_1"),
					expectSubnetPatch(f, true, true, 1),
				),
			},
			// true back to false, likewise.
			{
				Config:           region(`landing_point = false`),
				ConfigPlanChecks: expectSubnetAction("nxip_subnet.region", plancheck.ResourceActionUpdate),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("nxip_subnet.region", "landing_point", "false"),
					resource.TestCheckResourceAttr("nxip_subnet.region", "id", "sub_1"),
					expectSubnetPatch(f, true, false, 2),
				),
			},
			// A rename with landing_point unchanged does not resend it.
			{
				Config:           region(`landing_point = false`, `name = "EMEA region"`),
				ConfigPlanChecks: expectSubnetAction("nxip_subnet.region", plancheck.ResourceActionUpdate),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("nxip_subnet.region", "name", "EMEA region"),
					resource.TestCheckResourceAttr("nxip_subnet.region", "landing_point", "false"),
					expectSubnetPatch(f, false, false, 3),
				),
			},
			{
				Config:   region(`landing_point = false`, `name = "EMEA region"`),
				PlanOnly: true,
			},
		},
	})
}

// TestSubnetResource_landingPointDashboardEditIsNotAReplace: the flag
// flipped in the dashboard behind Terraform's back. The next plan must
// want an in-place update back to what config says, never a destroy and
// recreate of a subnet that may have children.
func TestSubnetResource_landingPointDashboardEditIsNotAReplace(t *testing.T) {
	f := newFakeSubnetAPI(t)
	config := fakeSubnetConfig(f.server.URL, fakeSubnet{"region", []string{`kind = "region"`, `landing_point = false`}})

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: config,
			},
			{
				PreConfig:        func() { f.setLandingPointOutOfBand(true) },
				Config:           config,
				ConfigPlanChecks: expectSubnetAction("nxip_subnet.region", plancheck.ResourceActionUpdate),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("nxip_subnet.region", "landing_point", "false"),
					expectSubnetPatch(f, true, false, 1),
				),
			},
		},
	})
}

// TestSubnetResource_landingPointImport: terraform import goes through
// ImportState and then Read, so this is the proof that Read populates the
// attribute from GET /v1/subnets/:id. ImportStateVerify compares the whole
// imported state with the created one, and the explicit check pins the
// value so a Read that left it null could not slip past on a null-to-null
// comparison of some other attribute.
func TestSubnetResource_landingPointImport(t *testing.T) {
	f := newFakeSubnetAPI(t)
	config := fakeSubnetConfig(f.server.URL,
		fakeSubnet{"pods", []string{`kind = "pod-cidr"`, `landing_point = false`}},
		fakeSubnet{"region", []string{`kind = "region"`}},
	)

	importCheck := func(want string) resource.ImportStateCheckFunc {
		return func(states []*terraform.InstanceState) error {
			if len(states) != 1 {
				return fmt.Errorf("want 1 imported state, got %d", len(states))
			}
			if got := states[0].Attributes["landing_point"]; got != want {
				return fmt.Errorf("want imported landing_point %q, got %q (attributes %v)", want, got, states[0].Attributes)
			}
			return nil
		}
	}

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: config,
			},
			{
				ResourceName:      "nxip_subnet.pods",
				ImportState:       true,
				ImportStateVerify: true,
				ImportStateCheck:  importCheck("false"),
			},
			{
				ResourceName:      "nxip_subnet.region",
				ImportState:       true,
				ImportStateVerify: true,
				ImportStateCheck:  importCheck("true"),
			},
		},
	})
}
