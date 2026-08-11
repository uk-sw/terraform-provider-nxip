# terraform-provider-nxip

Terraform provider for [nxip](https://nxip.dev) — IP address management built API-first for infrastructure-as-code. Manages dynamic, conflict-free CIDR allocations across multi-cloud and on-prem environments.

**Status: not yet published to the Terraform Registry.** Source-build only for now — see "Local development" below. This README will get real `source = "uk-sw/nxip"` install instructions once that's live.

## Usage (once published)

```hcl
terraform {
  required_providers {
    nxip = {
      source = "uk-sw/nxip"
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

resource "nxip_allocation" "web_subnet" {
  environment   = "production"
  region        = "us-east-1"
  family        = "IPV4"
  prefix_length = 24
  name          = "web-tier"

  depends_on = [nxip_pool.production_us_east]
}
```

## Resources

- **`nxip_pool`** — registers a top-level IP pool, the parent CIDR block that `nxip_allocation` resources carve non-overlapping subnets from. Scoped to exactly one address family per environment/region.
- **`nxip_allocation`** — allocates a dynamic, non-overlapping CIDR subnet. Auto-resolves onto a matching pool by environment/region/family, or nests directly under an existing subnet via `parent_subnet_id`.

Both resources support `terraform import`.

## Local development

```bash
go build ./...
go vet ./...
go test ./...                    # unit tests only
TF_ACC=1 NXIP_API_URL=http://localhost:3000 NXIP_API_KEY=<a-real-key> go test ./... -v  # acceptance tests, needs a live nxip API
```
