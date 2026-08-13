package provider

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// testAccCheckAddressDestroyed verifies, via a direct API call bypassing
// Terraform entirely, that every address created during the test was
// actually released server-side — not just dropped from state. Unlike
// pool/subnet, an address's URL is nested under its parent subnet, so the
// subnet_id has to come along with the address ID to check it.
func testAccCheckAddressDestroyed(s *terraform.State) error {
	for _, rs := range s.RootModule().Resources {
		if rs.Type != "nxip_address" {
			continue
		}

		subnetID := rs.Primary.Attributes["subnet_id"]
		req, err := http.NewRequest(http.MethodGet, testAccAPIURL()+"/v1/subnets/"+subnetID+"/addresses/"+rs.Primary.ID, nil)
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
			// 2. Import by the composite subnet_id/address_id identifier and
			// verify the imported state matches exactly (exercises the
			// custom ImportState + Read).
			{
				ResourceName:      "nxip_address.test",
				ImportState:       true,
				ImportStateVerify: true,
				ImportStateIdFunc: func(s *terraform.State) (string, error) {
					rs, ok := s.RootModule().Resources["nxip_address.test"]
					if !ok {
						return "", fmt.Errorf("resource not found in state: nxip_address.test")
					}
					return rs.Primary.Attributes["subnet_id"] + "/" + rs.Primary.ID, nil
				},
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

		subnetID := rs.Primary.Attributes["subnet_id"]
		req, err := http.NewRequest(http.MethodDelete, testAccAPIURL()+"/v1/subnets/"+subnetID+"/addresses/"+rs.Primary.ID, nil)
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
