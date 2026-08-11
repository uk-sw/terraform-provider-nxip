package provider

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// testAccCheckPoolDestroyed verifies, via a direct API call bypassing
// Terraform entirely, that every pool created during the test was actually
// deleted server-side — not just dropped from state.
func testAccCheckPoolDestroyed(s *terraform.State) error {
	for _, rs := range s.RootModule().Resources {
		if rs.Type != "nxip_pool" {
			continue
		}

		req, err := http.NewRequest(http.MethodGet, testAccAPIURL()+"/v1/pools/"+rs.Primary.ID, nil)
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
			return fmt.Errorf("pool %s still exists after destroy (API returned status %d)", rs.Primary.ID, resp.StatusCode)
		}
	}
	return nil
}

// testAccPoolConfig uses region "acc-test-pool" (distinct from
// "us-east-1", which the allocation tests and the seeded dev pool already
// occupy) so this test's pool doesn't collide with either the pre-seeded
// pool or a pool an allocation test might implicitly depend on — a pool is
// unique per (organization, environment, region, family) server-side.
func testAccPoolConfig(cidr string) string {
	return fmt.Sprintf(`
provider "nxip" {
  api_key = %q
  url     = %q
}

resource "nxip_pool" "test" {
  name        = "Acceptance Test Pool"
  cidr        = %q
  family      = "IPV4"
  environment = "production"
  region      = "acc-test-pool"
}
`, testAccAPIKey(), testAccAPIURL(), cidr)
}

func TestAccPoolResource_lifecycle(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckPoolDestroyed,
		Steps: []resource.TestStep{
			// 1. Create and verify all attributes are populated.
			{
				Config: testAccPoolConfig("10.90.0.0/16"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("nxip_pool.test", "name", "Acceptance Test Pool"),
					resource.TestCheckResourceAttr("nxip_pool.test", "cidr", "10.90.0.0/16"),
					resource.TestCheckResourceAttr("nxip_pool.test", "family", "IPV4"),
					resource.TestCheckResourceAttr("nxip_pool.test", "environment", "production"),
					resource.TestCheckResourceAttr("nxip_pool.test", "region", "acc-test-pool"),
					resource.TestCheckResourceAttrSet("nxip_pool.test", "id"),
				),
			},
			// 2. Import by ID and verify the imported state matches exactly
			// (exercises ImportState + Read).
			{
				ResourceName:      "nxip_pool.test",
				ImportState:       true,
				ImportStateVerify: true,
			},
			// 3. Changing cidr must force replacement (new id), since pools
			// are immutable server-side (RequiresReplace).
			{
				Config: testAccPoolConfig("10.91.0.0/16"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("nxip_pool.test", "cidr", "10.91.0.0/16"),
					resource.TestCheckResourceAttrSet("nxip_pool.test", "id"),
				),
			},
			// Final step's implicit destroy at test teardown exercises Delete,
			// and CheckDestroy above confirms it actually deleted server-side.
		},
	})
}

// TestAccPoolAndAllocation_composition proves the two resources actually
// compose the way a real user would rely on: an allocation referencing a
// pool that's *also* managed by this same Terraform run, not a pool that
// happens to pre-exist via seed data. Destroy order matters here too — the
// allocation must be destroyed before the pool (Terraform infers this from
// the implicit dependency via the shared environment/region/family values;
// the pool API itself also refuses to delete a non-empty pool as a
// server-side backstop, see PoolResource.Delete).
func TestAccPoolAndAllocation_composition(t *testing.T) {
	config := fmt.Sprintf(`
provider "nxip" {
  api_key = %q
  url     = %q
}

resource "nxip_pool" "test" {
  name        = "Composition Test Pool"
  cidr        = "10.92.0.0/16"
  family      = "IPV4"
  environment = "production"
  region      = "acc-test-composition"
}

resource "nxip_allocation" "test" {
  environment   = nxip_pool.test.environment
  region        = nxip_pool.test.region
  family        = nxip_pool.test.family
  prefix_length = 24
}
`, testAccAPIKey(), testAccAPIURL())

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			testAccCheckPoolDestroyed,
			testAccCheckAllocationDestroyed,
		),
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet("nxip_pool.test", "id"),
					resource.TestCheckResourceAttrSet("nxip_allocation.test", "id"),
					resource.TestCheckResourceAttrSet("nxip_allocation.test", "cidr"),
					resource.TestCheckResourceAttrPair("nxip_allocation.test", "region", "nxip_pool.test", "region"),
				),
			},
		},
	})
}
