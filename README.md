# terraform-provider-nxip

Terraform provider for [nxip](https://nxip.dev): IPAM (IP Address Management) built API-first for infrastructure-as-code. Manages dynamic, conflict-free CIDR subnets across multi-cloud and on-prem environments.

**Status: pre-release / early access.** Published under a pre-release version (e.g. `v0.2.0-alpha.1`), which `terraform init` will not resolve to unless pinned to that exact version. This is deliberate, not an oversight. Resource schemas may still change, and the nxip API and this provider are both still being validated with real users. **Do not use for production infrastructure yet.** The Free tier backing the API carries no data-durability guarantee; see [nxip's Terms of Service](https://nx-ip.com/terms).

## Usage

```hcl
terraform {
  required_providers {
    nxip = {
      source  = "uk-sw/nxip"
      version = "0.2.0-alpha.1" # pin the exact pre-release version while this is still early access
    }
  }
}

provider "nxip" {
  api_key = var.nxip_api_key # or NXIP_API_KEY env var
}

resource "nxip_pool" "production_us_east" {
  name        = "prod-us-east-1"
  cidr        = "10.0.0.0/16"
  family      = "IPV4"
  environment = "production"
  region      = "us-east-1"
}

resource "nxip_subnet" "web_subnet" {
  environment   = "production"
  region        = "us-east-1"
  family        = "IPV4"
  prefix_length = 24
  name          = "web-tier"

  depends_on = [nxip_pool.production_us_east]
}

resource "nxip_address" "lb_vip" {
  subnet_id = nxip_subnet.web_subnet.id
  address   = "10.0.0.10"
  status    = "RESERVED"
  hostname  = "lb-01"
}
```

## Resources

- **`nxip_pool`**: registers a top-level IP pool, the parent CIDR block that `nxip_subnet` resources carve non-overlapping subnets from. Scoped to exactly one address family per environment/region.
- **`nxip_subnet`**: a dynamic, non-overlapping CIDR subnet. Auto-resolves onto a matching pool by environment/region/family, or nests directly under an existing subnet via `parent_subnet_id`.
- **`nxip_address`**: registers or reserves a specific IP address within an already-allocated `nxip_subnet`. Manual only, by design; no auto-pick.

All three resources support `terraform import` (`nxip_address` via a composite `<subnet_id>/<address_id>` identifier, since its own ID alone isn't enough to fetch it).

## Local development

```bash
go build ./...
go vet ./...
go test ./...                    # unit tests only
TF_ACC=1 NXIP_URL=http://localhost:3000 NXIP_API_KEY=<a-real-key> go test ./... -v  # acceptance tests, needs a live nxip API
```
