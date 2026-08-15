resource "nxip_address" "lb_vip" {
  subnet_id = nxip_subnet.web_subnet.id
  address   = "10.0.0.10"
  status    = "RESERVED"
  hostname  = "lb-01"
}
