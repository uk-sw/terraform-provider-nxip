resource "nxip_address" "lb_vip" {
  subnet_id = nxip_subnet.web_subnet.id
  address   = "10.0.0.10"
  status    = "RESERVED"
  hostname  = "lb-01"

  # Editable after creation, unlike every other attribute here - see the
  # metadata attribute's own description. Adding an owner or re-tagging an
  # asset later updates this address in place, it does not release and
  # re-register it at the same IP.
  metadata = {
    owner = "platform-team"
  }
}
