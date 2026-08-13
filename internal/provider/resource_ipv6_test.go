package provider

import (
	"fmt"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// TestAccIPv6_lifecycle proves the whole resource set (nxip_pool,
// nxip_subnet — both auto-resolved and explicitly nested — and
// nxip_address) works correctly for IPV6, not just IPV4. Every other
// acceptance test in this package uses IPV4 exclusively; family-specific
// bugs (e.g. in CIDR math, containment checks, or address normalization)
// wouldn't be caught by any of them. Uses 2001:db8::/32, the IANA-reserved
// documentation prefix (RFC 3849) — never a real, routable range.
func TestAccIPv6_lifecycle(t *testing.T) {
	region := fmt.Sprintf("acc-test-ipv6-%d", time.Now().UnixNano())
	config := fmt.Sprintf(`
provider "nxip" {
  api_key = %q
  url     = %q
}

resource "nxip_pool" "test" {
  name        = "IPv6 Test Pool"
  cidr        = "2001:db8:acc0::/48"
  family      = "IPV6"
  environment = "production"
  region      = %q
}

resource "nxip_subnet" "region" {
  environment   = nxip_pool.test.environment
  region        = nxip_pool.test.region
  family        = nxip_pool.test.family
  prefix_length = 56
  kind          = "region"
  name          = "IPv6 region block"
}

# No parent_subnet_id here at all, same as the IPv4 hierarchy test — proves
# auto-resolution (routing to the kind-tagged subnet above) works
# identically for IPv6, not just IPv4.
resource "nxip_subnet" "app" {
  environment   = nxip_subnet.region.environment
  region        = nxip_subnet.region.region
  family        = nxip_subnet.region.family
  prefix_length = 64
  name          = "IPv6 app team"
}

# Explicit nesting under app, the other path nxip_subnet supports.
resource "nxip_subnet" "az" {
  parent_subnet_id = nxip_subnet.app.id
  family            = "IPV6"
  prefix_length     = 65
  kind              = "az-subnet"
}

# A specific address within the /64 — proves nxip_address's CIDR
# containment and normalization logic also holds for IPv6, where addresses
# are commonly written in shorthand (::1, not 0000:0000:...:0001).
resource "nxip_address" "test" {
  subnet_id = nxip_subnet.app.id
  address   = "${cidrhost(nxip_subnet.app.cidr, 1)}"
  status    = "RESERVED"
  hostname  = "ipv6-acc-test-host"
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
					resource.TestCheckResourceAttr("nxip_pool.test", "cidr", "2001:db8:acc0::/48"),
					resource.TestCheckResourceAttr("nxip_pool.test", "family", "IPV6"),

					resource.TestCheckResourceAttr("nxip_subnet.region", "family", "IPV6"),
					resource.TestCheckResourceAttrSet("nxip_subnet.region", "cidr"),

					// Auto-resolution: app's config never set parent_subnet_id,
					// yet it should have nested under the region subnet.
					resource.TestCheckResourceAttrPair("nxip_subnet.app", "parent_subnet_id", "nxip_subnet.region", "id"),
					resource.TestCheckResourceAttrPair("nxip_subnet.app", "environment", "nxip_subnet.region", "environment"),
					resource.TestCheckResourceAttrPair("nxip_subnet.app", "region", "nxip_subnet.region", "region"),
					resource.TestCheckResourceAttrSet("nxip_subnet.app", "cidr"),

					// Explicit nesting: environment/region were never set in
					// az's config, but should be populated (Optional+Computed),
					// inherited from app — same as the IPv4 hierarchy test.
					resource.TestCheckResourceAttrPair("nxip_subnet.az", "parent_subnet_id", "nxip_subnet.app", "id"),
					resource.TestCheckResourceAttrPair("nxip_subnet.az", "environment", "nxip_subnet.app", "environment"),
					resource.TestCheckResourceAttrPair("nxip_subnet.az", "region", "nxip_subnet.app", "region"),

					resource.TestCheckResourceAttr("nxip_address.test", "status", "RESERVED"),
					resource.TestCheckResourceAttr("nxip_address.test", "family", "IPV6"),
					resource.TestCheckResourceAttrSet("nxip_address.test", "address"),
				),
			},
			// Import round-trip for the IPv6 address specifically — the
			// composite <subnet_id>/<address_id> identifier and the
			// shorthand-vs-expanded-form normalization are both things
			// that could plausibly behave differently for IPv6 than IPv4.
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
		},
	})
}
