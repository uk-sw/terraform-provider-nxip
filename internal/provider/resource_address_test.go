package provider

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// testAccCheckAddressDestroyed verifies, via a direct API call bypassing
// Terraform entirely, that every address created during the test was
// actually released server-side, not just dropped from state. Uses the
// flat GET /v1/addresses/:id now that it exists, matching how the provider
// itself reads an address.
func testAccCheckAddressDestroyed(s *terraform.State) error {
	for _, rs := range s.RootModule().Resources {
		if rs.Type != "nxip_address" {
			continue
		}

		req, err := http.NewRequest(http.MethodGet, testAccAPIURL()+"/v1/addresses/"+rs.Primary.ID, nil)
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
			return fmt.Errorf("address %s still exists after destroy (API returned status %d)", rs.Primary.ID, resp.StatusCode)
		}
	}
	return nil
}

// TestAccAddressResource_lifecycle exercises the full CRUD loop plus
// import — a pool and subnet as prerequisites, then an address registered
// within that subnet with status/hostname/metadata all set, verifying each
// round-trips, then importing by the composite <subnet_id>/<address_id>
// identifier and confirming the imported state matches exactly.
func TestAccAddressResource_lifecycle(t *testing.T) {
	region := fmt.Sprintf("acc-test-address-%d", time.Now().UnixNano())
	config := fmt.Sprintf(`
provider "nxip" {
  api_key = %q
  url     = %q
}

resource "nxip_pool" "test" {
  name        = "Address Test Pool"
  cidr        = "10.94.0.0/16"
  family      = "IPV4"
  environment = "production"
  region      = %q
}

resource "nxip_subnet" "test" {
  environment   = nxip_pool.test.environment
  region        = nxip_pool.test.region
  family        = nxip_pool.test.family
  prefix_length = 24
}

resource "nxip_address" "test" {
  subnet_id = nxip_subnet.test.id
  address   = "${cidrhost(nxip_subnet.test.cidr, 10)}"
  status    = "RESERVED"
  hostname  = "acc-test-host"

  metadata = {
    owner = "acc-test"
  }
}
`, testAccAPIKey(), testAccAPIURL(), region)

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			testAccCheckPoolDestroyed,
			testAccCheckSubnetDestroyed,
			testAccCheckAddressDestroyed,
		),
		Steps: []resource.TestStep{
			// 1. Create and verify every attribute is populated correctly.
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet("nxip_address.test", "id"),
					resource.TestCheckResourceAttrPair("nxip_address.test", "subnet_id", "nxip_subnet.test", "id"),
					resource.TestCheckResourceAttr("nxip_address.test", "status", "RESERVED"),
					resource.TestCheckResourceAttr("nxip_address.test", "hostname", "acc-test-host"),
					resource.TestCheckResourceAttr("nxip_address.test", "family", "IPV4"),
					resource.TestCheckResourceAttr("nxip_address.test", "metadata.owner", "acc-test"),
				),
			},
			// 2. Import by the address's own ID alone and verify the
			// imported state matches exactly (exercises ImportState + Read).
			{
				ResourceName:      "nxip_address.test",
				ImportState:       true,
				ImportStateVerify: true,
			},
			// Final step's implicit destroy at test teardown exercises
			// Delete, and CheckDestroy above confirms it actually released
			// server-side.
		},
	})
}

// TestAccAddressResource_driftDetection verifies that an address released
// directly via the API (i.e. outside Terraform) is correctly detected as
// gone on the next plan/apply, rather than erroring.
func TestAccAddressResource_driftDetection(t *testing.T) {
	region := fmt.Sprintf("acc-test-address-drift-%d", time.Now().UnixNano())
	config := fmt.Sprintf(`
provider "nxip" {
  api_key = %q
  url     = %q
}

resource "nxip_pool" "test" {
  name        = "Address Drift Test Pool"
  cidr        = "10.95.0.0/16"
  family      = "IPV4"
  environment = "production"
  region      = %q
}

resource "nxip_subnet" "test" {
  environment   = nxip_pool.test.environment
  region        = nxip_pool.test.region
  family        = nxip_pool.test.family
  prefix_length = 24
}

resource "nxip_address" "test" {
  subnet_id = nxip_subnet.test.id
  address   = "${cidrhost(nxip_subnet.test.cidr, 10)}"
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
					resource.TestCheckResourceAttrSet("nxip_address.test", "id"),
					deleteAddressOutOfBand("nxip_address.test"),
				),
				// The out-of-band release means Terraform's post-apply
				// refresh will detect the resource is gone; expect a
				// non-empty plan on the next run rather than an error.
				ExpectNonEmptyPlan: true,
			},
		},
	})
}

// deleteAddressOutOfBand releases the address directly via the API
// (bypassing Terraform) to simulate drift for the driftDetection test above.
func deleteAddressOutOfBand(resourceName string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[resourceName]
		if !ok {
			return fmt.Errorf("resource not found in state: %s", resourceName)
		}

		req, err := http.NewRequest(http.MethodDelete, testAccAPIURL()+"/v1/addresses/"+rs.Primary.ID, nil)
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
			return fmt.Errorf("out-of-band delete of address %s failed with status %d", rs.Primary.ID, resp.StatusCode)
		}
		return nil
	}
}

func testAccAddressConfigWithMetadata(region, owner string) string {
	return fmt.Sprintf(`
provider "nxip" {
  api_key = %q
  url     = %q
}

resource "nxip_pool" "test" {
  name        = "Address Metadata Test Pool"
  cidr        = "10.96.0.0/16"
  family      = "IPV4"
  environment = "production"
  region      = %q
}

resource "nxip_subnet" "test" {
  environment   = nxip_pool.test.environment
  region        = nxip_pool.test.region
  family        = nxip_pool.test.family
  prefix_length = 24
}

resource "nxip_address" "test" {
  subnet_id = nxip_subnet.test.id
  address   = "${cidrhost(nxip_subnet.test.cidr, 10)}"

  metadata = {
    owner = %q
  }
}
`, testAccAPIKey(), testAccAPIURL(), region, owner)
}

// TestAccAddressResource_metadataUpdateInPlace is the regression test for
// the one address attribute that is not RequiresReplace. Changing metadata
// must PATCH the address, not release it and register a new one at the
// same IP - a recreate would briefly free the address for something else to
// claim, which is exactly the kind of gap this fix closes.
//
// plancheck.ExpectResourceAction asserts the plan's actual action rather
// than inferring it from the id surviving: a replace that happened to land
// on the same address would still pass a naive id check, but not this one.
func TestAccAddressResource_metadataUpdateInPlace(t *testing.T) {
	region := fmt.Sprintf("acc-test-address-metadata-%d", time.Now().UnixNano())

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			testAccCheckPoolDestroyed,
			testAccCheckSubnetDestroyed,
			testAccCheckAddressDestroyed,
		),
		Steps: []resource.TestStep{
			{
				Config: testAccAddressConfigWithMetadata(region, "platform-team"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("nxip_address.test", "metadata.owner", "platform-team"),
					resource.TestCheckResourceAttrSet("nxip_address.test", "id"),
				),
			},
			{
				Config: testAccAddressConfigWithMetadata(region, "data-team"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction("nxip_address.test", plancheck.ResourceActionUpdate),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("nxip_address.test", "metadata.owner", "data-team"),
					resource.TestCheckResourceAttrSet("nxip_address.test", "id"),
				),
			},
		},
	})
}

// TestAccAddressResource_noMetadataIsStable covers the other half of the
// UseStateForUnknown fix: a config that never mentions metadata at all must
// still plan clean on a second apply. Without the plan modifier the
// attribute re-plans as "(known after apply)" every time, and because every
// other attribute on this resource is still RequiresReplace, that alone
// would force the address to be released and re-registered on any
// unrelated change.
func TestAccAddressResource_noMetadataIsStable(t *testing.T) {
	region := fmt.Sprintf("acc-test-address-nometadata-%d", time.Now().UnixNano())
	config := fmt.Sprintf(`
provider "nxip" {
  api_key = %q
  url     = %q
}

resource "nxip_pool" "test" {
  name        = "Address No-Metadata Test Pool"
  cidr        = "10.97.0.0/16"
  family      = "IPV4"
  environment = "production"
  region      = %q
}

resource "nxip_subnet" "test" {
  environment   = nxip_pool.test.environment
  region        = nxip_pool.test.region
  family        = nxip_pool.test.family
  prefix_length = 24
}

resource "nxip_address" "test" {
  subnet_id = nxip_subnet.test.id
  address   = "${cidrhost(nxip_subnet.test.cidr, 10)}"
}
`, testAccAPIKey(), testAccAPIURL(), region)

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			testAccCheckPoolDestroyed,
			testAccCheckSubnetDestroyed,
			testAccCheckAddressDestroyed,
		),
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("nxip_address.test", "metadata.%", "0"),
					resource.TestCheckResourceAttrSet("nxip_address.test", "id"),
				),
			},
			{
				Config:   config,
				PlanOnly: true,
			},
		},
	})
}
