package provider

import (
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// These acceptance tests exercise the full CRUD lifecycle against a real
// nxip API instance (not a mock) — by default the local dev stack started
// via `docker-compose up` + `pnpm --filter @nxip/db exec prisma db seed`
// at the repo root, which seeds the "Acme Corp" org, an ADMIN API key, and
// a "10.240.0.0/16" pool in region "us-east-1" / environment "production".
//
// Run with:
//
//	TF_ACC=1 go test ./internal/provider/... -v
//
// Override NXIP_URL / NXIP_API_KEY to point at a different environment
// (e.g. staging) instead of the local dev stack — the same two env vars
// the provider itself reads (see provider.go's Configure), so acceptance
// tests exercise the exact interface documented for real users.

func testAccAPIURL() string {
	if v := os.Getenv("NXIP_URL"); v != "" {
		return v
	}
	return "http://localhost:3000"
}

func testAccAPIKey() string {
	if v := os.Getenv("NXIP_API_KEY"); v != "" {
		return v
	}
	return "nc_live_dev_testkey123"
}

var testAccProtoV6ProviderFactories = map[string]func() (tfprotov6.ProviderServer, error){
	"nxip": providerserver.NewProtocol6WithError(New("test")()),
}

func testAccPreCheck(t *testing.T) {
	if os.Getenv("TF_ACC") == "" {
		t.Skip("Acceptance tests skipped unless TF_ACC=1 is set (requires a running nxip API)")
	}
}

// testAccCheckSubnetDestroyed verifies, via a direct API call bypassing
// Terraform entirely, that every subnet created during the test was
// actually released server-side — not just dropped from state.
func testAccCheckSubnetDestroyed(s *terraform.State) error {
	for _, rs := range s.RootModule().Resources {
		if rs.Type != "nxip_subnet" {
			continue
		}

		req, err := http.NewRequest(http.MethodGet, testAccAPIURL()+"/v1/subnets/"+rs.Primary.ID, nil)
		if err != nil {
			return err
		}
		req.Header.Set("x-api-key", testAccAPIKey())

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusNotFound {
			return fmt.Errorf("subnet %s still exists after destroy (API returned status %d)", rs.Primary.ID, resp.StatusCode)
		}
	}
	return nil
}

func testAccSubnetConfig(prefixLength int) string {
	return fmt.Sprintf(`
provider "nxip" {
  api_key = %q
  url     = %q
}

resource "nxip_subnet" "test" {
  environment   = "production"
  region        = "us-east-1"
  family        = "IPV4"
  prefix_length = %d
}
`, testAccAPIKey(), testAccAPIURL(), prefixLength)
}

func TestAccSubnetResource_lifecycle(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckSubnetDestroyed,
		Steps: []resource.TestStep{
			// 1. Create and verify all attributes are populated.
			{
				Config: testAccSubnetConfig(28),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("nxip_subnet.test", "environment", "production"),
					resource.TestCheckResourceAttr("nxip_subnet.test", "region", "us-east-1"),
					resource.TestCheckResourceAttr("nxip_subnet.test", "family", "IPV4"),
					resource.TestCheckResourceAttr("nxip_subnet.test", "prefix_length", "28"),
					resource.TestCheckResourceAttrSet("nxip_subnet.test", "id"),
					resource.TestCheckResourceAttrSet("nxip_subnet.test", "cidr"),
				),
			},
			// 2. Import by ID and verify the imported state matches exactly
			// (exercises ImportState + Read).
			{
				ResourceName:      "nxip_subnet.test",
				ImportState:       true,
				ImportStateVerify: true,
			},
			// 3. Changing prefix_length must force replacement (new id/cidr),
			// since subnets are immutable server-side (RequiresReplace).
			{
				Config: testAccSubnetConfig(29),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("nxip_subnet.test", "prefix_length", "29"),
					resource.TestCheckResourceAttrSet("nxip_subnet.test", "id"),
					resource.TestCheckResourceAttrSet("nxip_subnet.test", "cidr"),
				),
			},
			// Final step's implicit destroy at test teardown exercises Delete,
			// and CheckDestroy above confirms it actually released server-side.
		},
	})
}

// TestAccSubnetResource_driftDetection verifies that a subnet deleted
// directly via the API (i.e. outside Terraform) is correctly detected as
// gone on the next plan/apply, rather than erroring.
func TestAccSubnetResource_driftDetection(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckSubnetDestroyed,
		Steps: []resource.TestStep{
			{
				Config: testAccSubnetConfig(30),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet("nxip_subnet.test", "id"),
					deleteSubnetOutOfBand("nxip_subnet.test"),
				),
				// The out-of-band delete means Terraform's post-apply refresh
				// will detect the resource is gone; expect a non-empty plan
				// on the next run rather than an error.
				ExpectNonEmptyPlan: true,
			},
		},
	})
}

// TestAccSubnetResource_hierarchy exercises subnet nesting end to end:
// a region-tier subnet tagged with `kind`, a second subnet using
// the exact same environment/region/family that should auto-resolve under
// it (no parent_subnet_id in its config at all), and a third nested
// explicitly via parent_subnet_id under the second. Proves kind/name/
// description round-trip, and that environment/region come back populated
// (Optional+Computed) even when omitted from config entirely.
func TestAccSubnetResource_hierarchy(t *testing.T) {
	region := fmt.Sprintf("acc-test-hierarchy-%d", time.Now().UnixNano())
	config := fmt.Sprintf(`
provider "nxip" {
  api_key = %q
  url     = %q
}

resource "nxip_pool" "test" {
  name        = "Hierarchy Test Pool"
  cidr        = "10.93.0.0/16"
  family      = "IPV4"
  environment = "production"
  region      = %q
}

resource "nxip_subnet" "region" {
  environment   = nxip_pool.test.environment
  region        = nxip_pool.test.region
  family        = nxip_pool.test.family
  prefix_length = 20
  kind          = "region"
  name          = "EMEA region"
  description   = "Top-level region block for acceptance testing"
}

resource "nxip_subnet" "app_team_a" {
  # No parent_subnet_id here at all — this is exactly the config a team
  # would write whether or not nxip_subnet.region exists. It must
  # depend on it implicitly via the shared environment/region/family
  # values so Terraform creates the region first.
  environment   = nxip_subnet.region.environment
  region        = nxip_subnet.region.region
  family        = nxip_subnet.region.family
  prefix_length = 24
  name          = "App Team A"
}

resource "nxip_subnet" "az_subnet" {
  parent_subnet_id = nxip_subnet.app_team_a.id
  family            = "IPV4"
  prefix_length     = 28
  kind              = "az-subnet"
}
`, testAccAPIKey(), testAccAPIURL(), region)

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			testAccCheckPoolDestroyed,
			testAccCheckSubnetDestroyed,
		),
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("nxip_subnet.region", "kind", "region"),
					resource.TestCheckResourceAttr("nxip_subnet.region", "name", "EMEA region"),
					resource.TestCheckNoResourceAttr("nxip_subnet.region", "parent_subnet_id"),

					// Auto-resolution: app_team_a's config never set
					// parent_subnet_id, yet it should have nested under
					// the region subnet, not the pool.
					resource.TestCheckResourceAttr("nxip_subnet.app_team_a", "name", "App Team A"),
					resource.TestCheckResourceAttrPair("nxip_subnet.app_team_a", "parent_subnet_id", "nxip_subnet.region", "id"),
					resource.TestCheckResourceAttrPair("nxip_subnet.app_team_a", "environment", "nxip_subnet.region", "environment"),
					resource.TestCheckResourceAttrPair("nxip_subnet.app_team_a", "region", "nxip_subnet.region", "region"),

					// Explicit nesting: environment/region were never set
					// in az_subnet's config, but should be populated
					// (Optional+Computed), inherited from app_team_a.
					resource.TestCheckResourceAttrPair("nxip_subnet.az_subnet", "parent_subnet_id", "nxip_subnet.app_team_a", "id"),
					resource.TestCheckResourceAttrPair("nxip_subnet.az_subnet", "environment", "nxip_subnet.app_team_a", "environment"),
					resource.TestCheckResourceAttrPair("nxip_subnet.az_subnet", "region", "nxip_subnet.app_team_a", "region"),
					resource.TestCheckResourceAttr("nxip_subnet.az_subnet", "kind", "az-subnet"),
				),
			},
		},
	})
}

// deleteSubnetOutOfBand releases the subnet directly via the API
// (bypassing Terraform) to simulate drift for the driftDetection test above.
func deleteSubnetOutOfBand(resourceName string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[resourceName]
		if !ok {
			return fmt.Errorf("resource not found in state: %s", resourceName)
		}

		req, err := http.NewRequest(http.MethodDelete, testAccAPIURL()+"/v1/subnets/"+rs.Primary.ID, nil)
		if err != nil {
			return err
		}
		req.Header.Set("x-api-key", testAccAPIKey())

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("out-of-band delete failed with status %d", resp.StatusCode)
		}
		return nil
	}
}
