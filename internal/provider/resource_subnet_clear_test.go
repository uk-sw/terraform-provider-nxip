package provider

import (
	"fmt"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// Tests for null versus empty on nxip_subnet's name and description (item 2
// of the Weft Networks feedback, docs/research/weft-feedback-triage-2026-10-07.md
// in net-saas-monorepo). Both attributes are Optional only, and the API
// stores "" as null, which broke two ways:
//
//   - A description set in the dashboard on a subnet whose config has none.
//     The plan wanted null, Update left the field out, the API kept its
//     value and sent it back, and Terraform failed the apply with
//     ".description: was null, but now cty.StringVal(...)", on every apply.
//   - `description = ""` on create. The API stored null, and Terraform
//     failed with "was cty.StringVal(\"\"), but now null".
//
// They run against fakeSubnetAPI (resource_subnet_landing_point_test.go),
// which mirrors the API on the points that matter here: "" stored as null on
// create, an empty name rejected, and `null` on PATCH clearing the value.
// The fake's rejectNullClear stands in for an API that predates that last
// one.

// setSubnetFieldOutOfBand stands in for someone editing a subnet in the
// dashboard, behind Terraform's back.
func (f *fakeSubnetAPI) setSubnetFieldOutOfBand(key string, value any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, subnet := range f.subnets {
		subnet[key] = value
	}
}

// expectStoredSubnetField checks what the fake API holds for key on the
// named resource, so a test can say "the API holds null" independently of
// what Terraform's state says.
func expectStoredSubnetField(f *fakeSubnetAPI, resourceName, key string, want any) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[resourceName]
		if !ok {
			return fmt.Errorf("resource not found in state: %s", resourceName)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		subnet, ok := f.subnets[rs.Primary.ID]
		if !ok {
			return fmt.Errorf("fake API holds no subnet %s", rs.Primary.ID)
		}
		if got := subnet[key]; got != want {
			return fmt.Errorf("want API %s = %#v, got %#v", key, want, got)
		}
		return nil
	}
}

// expectPostBodyLacks checks that the create for the named resource did not
// send key at all.
func expectPostBodyLacks(f *fakeSubnetAPI, resourceName, key string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[resourceName]
		if !ok {
			return fmt.Errorf("resource not found in state: %s", resourceName)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		body, ok := f.createBody[rs.Primary.ID]
		if !ok {
			return fmt.Errorf("no POST body recorded for %s", resourceName)
		}
		if _, present := body[key]; present {
			return fmt.Errorf("want %s absent from POST body, got body %v", key, body)
		}
		return nil
	}
}

// expectLastPatchField checks the last PATCH body for key: wantPresent says
// whether it was sent at all, and want what it was when it was (nil for a
// JSON null).
func expectLastPatchField(f *fakeSubnetAPI, key string, wantPresent bool, want any) resource.TestCheckFunc {
	return func(*terraform.State) error {
		body := f.lastPatch()
		if body == nil {
			return fmt.Errorf("no PATCH was sent")
		}
		got, present := body[key]
		if present != wantPresent {
			return fmt.Errorf("want %s present in PATCH body: %v, got body %v", key, wantPresent, body)
		}
		if present && got != want {
			return fmt.Errorf("want PATCH body %s = %#v, got %#v", key, want, got)
		}
		return nil
	}
}

// clearableSubnetFields is the two attributes this file covers, each with
// the API's JSON key, which for both happens to be the attribute name.
var clearableSubnetFields = []string{"name", "description"}

// TestSubnetResource_fieldSetOutOfBandIsCleared is the reported bug: a
// subnet created with no description, a description then added in the
// dashboard. The next apply must be an in-place update that sends an
// explicit null, leave the API holding null, and be followed by a clean
// plan, rather than failing with an inconsistent result.
func TestSubnetResource_fieldSetOutOfBandIsCleared(t *testing.T) {
	for _, field := range clearableSubnetFields {
		t.Run(field, func(t *testing.T) {
			f := newFakeSubnetAPI(t)
			config := fakeSubnetConfig(f.server.URL, fakeSubnet{"s", nil})

			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
				Steps: []resource.TestStep{
					{
						Config: config,
						Check:  resource.TestCheckNoResourceAttr("nxip_subnet.s", field),
					},
					{
						PreConfig:        func() { f.setSubnetFieldOutOfBand(field, "set in the dashboard") },
						Config:           config,
						ConfigPlanChecks: expectSubnetAction("nxip_subnet.s", plancheck.ResourceActionUpdate),
						Check: resource.ComposeAggregateTestCheckFunc(
							resource.TestCheckNoResourceAttr("nxip_subnet.s", field),
							expectLastPatchField(f, field, true, nil),
							expectStoredSubnetField(f, "nxip_subnet.s", field, nil),
						),
					},
					{
						Config:   config,
						PlanOnly: true,
					},
				},
			})
		})
	}
}

// TestSubnetResource_emptyStringOnCreate: `description = ""` (or
// `name = ""`) on a new subnet. "" must not reach the API (it would be
// stored as null for description, and rejected for name), the API ends up
// holding null, and state keeps the "" config asked for, so neither the
// apply nor the next plan sees a difference.
func TestSubnetResource_emptyStringOnCreate(t *testing.T) {
	for _, field := range clearableSubnetFields {
		t.Run(field, func(t *testing.T) {
			f := newFakeSubnetAPI(t)
			config := fakeSubnetConfig(f.server.URL, fakeSubnet{"s", []string{field + ` = ""`}})

			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
				Steps: []resource.TestStep{
					{
						Config: config,
						Check: resource.ComposeAggregateTestCheckFunc(
							resource.TestCheckResourceAttr("nxip_subnet.s", field, ""),
							expectPostBodyLacks(f, "nxip_subnet.s", field),
							expectStoredSubnetField(f, "nxip_subnet.s", field, nil),
						),
					},
					{
						Config:   config,
						PlanOnly: true,
					},
				},
			})
		})
	}
}

// TestSubnetResource_clearSendsNull: a value set from config and then
// removed from it. Removing it sends null and the API clears it; setting it
// to "" instead does the same. Going from "" to absent afterwards changes
// nothing the API holds, so it must not send the field at all.
func TestSubnetResource_clearSendsNull(t *testing.T) {
	for _, field := range clearableSubnetFields {
		t.Run(field, func(t *testing.T) {
			f := newFakeSubnetAPI(t)
			withValue := fakeSubnetConfig(f.server.URL, fakeSubnet{"s", []string{field + ` = "from config"`}})
			withEmpty := fakeSubnetConfig(f.server.URL, fakeSubnet{"s", []string{field + ` = ""`}})
			without := fakeSubnetConfig(f.server.URL, fakeSubnet{"s", nil})

			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
				Steps: []resource.TestStep{
					{
						Config: withValue,
						Check:  expectStoredSubnetField(f, "nxip_subnet.s", field, "from config"),
					},
					{
						Config:           without,
						ConfigPlanChecks: expectSubnetAction("nxip_subnet.s", plancheck.ResourceActionUpdate),
						Check: resource.ComposeAggregateTestCheckFunc(
							resource.TestCheckNoResourceAttr("nxip_subnet.s", field),
							expectLastPatchField(f, field, true, nil),
							expectStoredSubnetField(f, "nxip_subnet.s", field, nil),
						),
					},
					{
						Config: withValue,
						Check:  expectStoredSubnetField(f, "nxip_subnet.s", field, "from config"),
					},
					{
						Config:           withEmpty,
						ConfigPlanChecks: expectSubnetAction("nxip_subnet.s", plancheck.ResourceActionUpdate),
						Check: resource.ComposeAggregateTestCheckFunc(
							resource.TestCheckResourceAttr("nxip_subnet.s", field, ""),
							expectLastPatchField(f, field, true, nil),
							expectStoredSubnetField(f, "nxip_subnet.s", field, nil),
						),
					},
					{
						Config: without,
						Check: resource.ComposeAggregateTestCheckFunc(
							resource.TestCheckNoResourceAttr("nxip_subnet.s", field),
							expectLastPatchField(f, field, false, nil),
						),
					},
				},
			})
		})
	}
}

// TestSubnetResource_olderAPIWithoutNullClear covers the window before the
// API accepts null on PATCH /v1/subnets/:id. A subnet that never had a
// description must still rename cleanly (null is only sent when there is
// something to clear, so the older API never sees it), and clearing one
// fails with the API's own 400 rather than an inconsistent-result error.
func TestSubnetResource_olderAPIWithoutNullClear(t *testing.T) {
	f := newFakeSubnetAPI(t)
	f.rejectNullClear = true

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: fakeSubnetConfig(f.server.URL, fakeSubnet{"s", []string{`name = "before"`}}),
			},
			{
				Config: fakeSubnetConfig(f.server.URL, fakeSubnet{"s", []string{`name = "after"`}}),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("nxip_subnet.s", "name", "after"),
					expectLastPatchField(f, "description", false, nil),
				),
			},
			{
				Config: fakeSubnetConfig(f.server.URL, fakeSubnet{"s", []string{`name = "after"`, `description = "notes"`}}),
			},
			{
				Config:      fakeSubnetConfig(f.server.URL, fakeSubnet{"s", []string{`name = "after"`}}),
				ExpectError: regexp.MustCompile(`failed to update subnet: body/description Expected string, received\s+null\s+\(HTTP 400\)`),
			},
		},
	})
}
