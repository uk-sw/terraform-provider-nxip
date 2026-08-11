resource "nxip_pool" "production_us_east" {
  name        = "prod-us-east-1"
  cidr        = "10.0.0.0/16"
  family      = "IPV4"
  environment = "production"
  region      = "us-east-1"
}
