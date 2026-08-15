terraform {
  required_providers {
    nxip = {
      source = "uk-sw/nxip"
    }
  }
}

provider "nxip" {
  # api_key can also be left unset here and provided via the NXIP_API_KEY
  # environment variable instead - the recommended approach for CI/CD, so
  # a real credential never ends up written into a .tf file.
  api_key = var.nxip_api_key
}

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
