# When the pool is in this same configuration, take environment, region and
# family from the pool's own attributes. The reference is what tells
# Terraform to create the pool first. Written as plain text instead,
# Terraform sees no link between the two, creates them at the same time, and
# the subnet fails with "No matching IPV4 IP pool found". See
# https://nx-ip.com/docs/troubleshooting#no-matching-pool
resource "nxip_pool" "production_us_east" {
  name        = "prod-us-east-1"
  cidr        = "10.0.0.0/16"
  family      = "IPV4"
  environment = "production"
  region      = "us-east-1"
}

resource "nxip_subnet" "web_subnet" {
  environment   = nxip_pool.production_us_east.environment
  region        = nxip_pool.production_us_east.region
  family        = nxip_pool.production_us_east.family
  prefix_length = 24
  name          = "web-tier"
}

# Pinning the block instead of letting nxip choose. Use this for anything
# whose address other systems depend on: firewall rules, route tables, NSGs,
# DNS records. An auto-resolved subnet gets whichever block is free at the
# time, so destroying and recreating in a different order reassigns it, and
# every reference to the old address silently stops matching. Declaring the
# CIDR is what makes it stable.
resource "nxip_subnet" "database_tier" {
  environment = nxip_pool.production_us_east.environment
  region      = nxip_pool.production_us_east.region
  family      = nxip_pool.production_us_east.family
  cidr        = "10.0.10.0/24"
  name        = "database-tier"
}

# When the pool lives in another configuration (the usual team workflow:
# a platform team owns the pools, and each app team asks for address space
# without needing to know which pool it comes from), plain text is right.
# The pool must already exist, or be imported, before this is applied.
resource "nxip_subnet" "app_team_a" {
  environment   = "staging"
  region        = "eu-west-2"
  family        = "IPV4"
  prefix_length = 26
  name          = "app-team-a"
}
