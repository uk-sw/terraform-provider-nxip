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
