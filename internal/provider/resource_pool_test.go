package provider

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
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
// "us-east-1", which the subnet tests and the seeded dev pool already
// occupy) so this test's pool doesn't collide with either the pre-seeded
// pool or a pool a subnet test might implicitly depend on — a pool is
// unique per (organization, environment, region, family) server-side.
//
// The CIDRs here matter just as much, and for a second reason: a pool is
// ALSO unique per (organization, cidrBlock) alone, regardless of
// environment or region. These tests run against a real organization, so
// a hardcoded CIDR that happens to match a pool someone actually created
// fails with a 409 that has nothing to do with the code under test. That
// is exactly what happened to 10.90.0.0/16, which collided with a real
// "Staging US-West" pool in the dogfood org. Acceptance-test pools
// therefore live in 10.230.0.0/16 upward, deliberately far from the
// 10.90-10.110 range real pools use.
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
				Config: testAccPoolConfig("10.230.0.0/16"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("nxip_pool.test", "name", "Acceptance Test Pool"),
					resource.TestCheckResourceAttr("nxip_pool.test", "cidr", "10.230.0.0/16"),
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
				Config: testAccPoolConfig("10.231.0.0/16"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("nxip_pool.test", "cidr", "10.231.0.0/16"),
					resource.TestCheckResourceAttrSet("nxip_pool.test", "id"),
				),
			},
			// Final step's implicit destroy at test teardown exercises Delete,
			// and CheckDestroy above confirms it actually deleted server-side.
		},
	})
}

func testAccPoolConfigWithMetadata(latitude string) string {
	return fmt.Sprintf(`
provider "nxip" {
  api_key = %q
  url     = %q
}

resource "nxip_pool" "test" {
  name        = "Metadata Test Pool"
  cidr        = "10.232.0.0/16"
  family      = "IPV4"
  environment = "production"
  region      = "acc-test-pool-metadata"

  metadata = {
    latitude  = %q
    longitude = "-0.1278"
  }
}
`, testAccAPIKey(), testAccAPIURL(), latitude)
}

// TestAccPoolResource_metadataUpdateInPlace is the regression test for the
// one pool attribute that is not RequiresReplace. Changing metadata must
// PATCH the pool, not destroy and recreate it - a recreate would take every
// subnet in the pool with it, which is a catastrophic outcome for what is
// meant to be "record where this site is".
//
// plancheck.ExpectResourceAction asserts the plan's actual action rather than
// inferring it from the id surviving: a replace that happened to land on the
// same values would still pass a naive id check, but not this one.
func TestAccPoolResource_metadataUpdateInPlace(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckPoolDestroyed,
		Steps: []resource.TestStep{
			{
				Config: testAccPoolConfigWithMetadata("51.5074"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("nxip_pool.test", "metadata.latitude", "51.5074"),
					resource.TestCheckResourceAttr("nxip_pool.test", "metadata.longitude", "-0.1278"),
					resource.TestCheckResourceAttrSet("nxip_pool.test", "id"),
				),
			},
			{
				Config: testAccPoolConfigWithMetadata("53.4808"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction("nxip_pool.test", plancheck.ResourceActionUpdate),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("nxip_pool.test", "metadata.latitude", "53.4808"),
					resource.TestCheckResourceAttr("nxip_pool.test", "cidr", "10.232.0.0/16"),
					resource.TestCheckResourceAttrSet("nxip_pool.test", "id"),
				),
			},
		},
	})
}

// TestAccPoolResource_noMetadataIsStable covers the other half of the
// UseStateForUnknown fix: a config that never mentions metadata at all must
// still plan clean on a second apply. Without the plan modifier the attribute
// re-plans as "(known after apply)" every time, and because every other
// attribute on this resource is RequiresReplace, that alone would force a
// pool replacement on any unrelated change.
func TestAccPoolResource_noMetadataIsStable(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckPoolDestroyed,
		Steps: []resource.TestStep{
			{
				Config: testAccPoolConfig("10.233.0.0/16"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("nxip_pool.test", "metadata.%", "0"),
					resource.TestCheckResourceAttrSet("nxip_pool.test", "id"),
				),
			},
			{
				Config:   testAccPoolConfig("10.233.0.0/16"),
				PlanOnly: true,
			},
		},
	})
}

// TestAccPoolAndSubnet_composition proves the two resources actually
// compose the way a real user would rely on: a subnet referencing a
// pool that's *also* managed by this same Terraform run, not a pool that
// happens to pre-exist via seed data. Destroy order matters here too — the
// subnet must be destroyed before the pool (Terraform infers this from
// the implicit dependency via the shared environment/region/family values;
// the pool API itself also refuses to delete a non-empty pool as a
// server-side backstop, see PoolResource.Delete).
func TestAccPoolAndSubnet_composition(t *testing.T) {
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

resource "nxip_subnet" "test" {
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
			testAccCheckSubnetDestroyed,
		),
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet("nxip_pool.test", "id"),
					resource.TestCheckResourceAttrSet("nxip_subnet.test", "id"),
					resource.TestCheckResourceAttrSet("nxip_subnet.test", "cidr"),
					resource.TestCheckResourceAttrPair("nxip_subnet.test", "region", "nxip_pool.test", "region"),
				),
			},
		},
	})
}
