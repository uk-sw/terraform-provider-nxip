resource "nxip_pool" "production_us_east" {
  name        = "prod-us-east-1"
  cidr        = "10.0.0.0/16"
  family      = "IPV4"
  environment = "production"
  region      = "us-east-1"
}

# An on-prem site. "region" is a free-form string, so a pool outside a cloud
# region needs latitude/longitude before the nxip GUI can place it on the
# world map - a recognized cloud region (like us-east-1 above) is placed
# automatically, with no tags at all.
resource "nxip_pool" "manchester_dc" {
  name        = "dc-manchester"
  cidr        = "10.40.0.0/16"
  family      = "IPV4"
  environment = "production"
  region      = "dc-manchester"

  metadata = {
    latitude  = "53.4808"
    longitude = "-2.2426"
    owner     = "platform-team"
  }
}
