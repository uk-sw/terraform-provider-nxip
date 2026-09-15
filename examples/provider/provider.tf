terraform {
  required_providers {
    nxip = {
      source = "uk-sw/nxip"
    }
  }
}

# Recommended: leave api_key unset here and provide it via the
# NXIP_API_KEY environment variable instead, so a real credential never
# ends up written into a .tf file. Get a key by signing up at
# https://nx-ip.com.
provider "nxip" {}

# A pool is the top-level CIDR block that nxip_subnet resources carve
# non-overlapping subnets from - scoped to exactly one address family per
# environment/region.
resource "nxip_pool" "production_us_east" {
  name        = "prod-us-east-1"
  cidr        = "10.0.0.0/16"
  family      = "IPV4"
  environment = "production"
  region      = "us-east-1"
}

# A subnet auto-resolves onto a matching pool by environment/region/family -
# this config never has to change whether or not that pool already has
# other subnets under it.
resource "nxip_subnet" "web_subnet" {
  environment   = "production"
  region        = "us-east-1"
  family        = "IPV4"
  prefix_length = 24
  name          = "web-tier"

  depends_on = [nxip_pool.production_us_east]
}

# Addresses are always explicit, never auto-picked - which address to use
# is normally chosen by whoever's deploying the host, or by DHCP.
resource "nxip_address" "lb_vip" {
  subnet_id = nxip_subnet.web_subnet.id
  address   = "10.0.0.10"
  status    = "RESERVED"
  hostname  = "lb-01"
}

# A provider (an MSP, an ISP, or an acquirer after a merger) whose API key
# belongs to an Enterprise organization with linked customers manages each
# customer through its own aliased provider block. organization is
# provider-level only, never a resource argument, so one configuration
# always targets exactly one organization - use an alias per customer to
# manage several side by side. If you do not set organization, requests go
# to the organization your API key belongs to; this is the default for
# everyone, and nothing changes for an organization with no customers.
provider "nxip" {
  alias        = "customer_a"
  organization = "org_acme_ltd"
}

provider "nxip" {
  alias        = "customer_b"
  organization = "org_widgets_co"
}

# Each customer is a hard boundary, so the same CIDR can be used in both
# without conflict - unlike two pools inside one organization, which must
# not overlap.
resource "nxip_pool" "customer_a_production" {
  provider    = nxip.customer_a
  name        = "customer-a-prod"
  cidr        = "10.0.0.0/16"
  family      = "IPV4"
  environment = "production"
  region      = "us-east-1"
}

resource "nxip_pool" "customer_b_production" {
  provider    = nxip.customer_b
  name        = "customer-b-prod"
  cidr        = "10.0.0.0/16"
  family      = "IPV4"
  environment = "production"
  region      = "us-east-1"
}
