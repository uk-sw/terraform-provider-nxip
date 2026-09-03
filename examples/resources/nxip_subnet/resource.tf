resource "nxip_subnet" "web_subnet" {
  environment   = "production"
  region        = "us-east-1"
  family        = "IPV4"
  prefix_length = 24
  name          = "web-tier"

  # Requires a matching nxip_pool to already exist for this
  # environment/region/family - there is no implicit pool creation.
  depends_on = [nxip_pool.production_us_east]
}

# Pinning the block instead of letting nxip choose. Use this for anything
# whose address other systems depend on: firewall rules, route tables, NSGs,
# DNS records. An auto-resolved subnet gets whichever block is free at the
# time, so destroying and recreating in a different order reassigns it, and
# every reference to the old address silently stops matching. Declaring the
# CIDR is what makes it stable.
resource "nxip_subnet" "database_tier" {
  environment = "production"
  region      = "us-east-1"
  family      = "IPV4"
  cidr        = "10.0.10.0/24"
  name        = "database-tier"

  depends_on = [nxip_pool.production_us_east]
}
