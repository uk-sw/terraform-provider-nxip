# terraform-provider-nxip-ipam

Terraform provider for [nxip](https://nxip.dev): IPAM (IP Address Management) built API-first for infrastructure-as-code. Manages dynamic, conflict-free CIDR subnets across multi-cloud and on-prem environments.

> **Registry address changed 2026-08-26**: this provider now publishes as `uk-sw/nxip-ipam`, not `uk-sw/nxip`. The old address is still live and works (frozen at `0.2.0`), but gets no further releases - repoint `source` as shown below. Nothing else changes: resource types are still `nxip_pool`/`nxip_subnet`/`nxip_address`, and the local block name can stay `nxip`.

## Usage

```hcl
terraform {
  required_providers {
    nxip = {
      source  = "uk-sw/nxip-ipam"
      version = "~> 0.3" # optional, but recommended - see the Registry page for the latest 0.3.x
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
