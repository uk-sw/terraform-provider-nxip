resource "nxip_allocation" "web_subnet" {
  environment   = "production"
  region        = "us-east-1"
  family        = "IPV4"
  prefix_length = 24
  name          = "web-tier"

  # Requires a matching nxip_pool to already exist for this
  # environment/region/family - there is no implicit pool creation.
  depends_on = [nxip_pool.production_us_east]
}
