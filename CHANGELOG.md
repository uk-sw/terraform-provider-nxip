# Changelog

All notable changes to this provider are documented here.

## Unreleased

- Internal fix: the API's error `message` is now extracted before attempting to decode a resource's own response shape, not after — so a malformed or unexpected response body no longer discards a `message` that was actually present and worth surfacing. No user-facing behavior change from `0.1.0-alpha11` in the normal case; only affects an edge case that hadn't been observed in practice.

## 0.1.0-alpha11 — 2026-08-13

- **Error messages now include the nxip API's own explanation, not just an HTTP status code.** Every resource (`nxip_pool`, `nxip_subnet`) previously discarded the API's response body on failure — a `409` on pool creation showed only `nxip API failed pool creation request with status 409`, even though the API had already sent back a specific reason (which CIDR conflicted, or which environment/region/family combination). Fixed at the shared HTTP client level so it applies uniformly: a conflict now reads like `failed to create pool: An IP Pool with CIDR 10.0.0.0/16 already exists in production / us-east-1. (HTTP 409)`. Pool deletion's "still has subnets attached" error now also includes the API's own subnet count and pool name instead of a generic hardcoded message.

## 0.1.0-alpha10 — 2026-08-12

- Added a `metadata` attribute to `nxip_subnet`: a free-form map of string key/value tags (e.g. `vpc_id`, `cost_center`), stored and returned as-is. Optional and Computed — a config that never sets it reads back as an empty map. Immutable, like every other `nxip_subnet` attribute: changing it forces a new resource.

## 0.1.0-alpha9 — 2026-08-12

- Internal fix: `nxip_pool`'s `cidr` attribute now parses from the API's `cidr` response field (it had been reading a since-renamed `cidrBlock` field). No config or attribute-name changes for users — this is a wire-format fix matching an API-side rename.

## 0.1.0-alpha7 — 2026-08-12

- **Breaking:** renamed `nxip_allocation` to `nxip_subnet` to match the terminology already used everywhere else in the system (the API's REST endpoints, response fields, and the underlying data model). Fixed now, before any real external adoption exists — this is the cheapest this rename will ever be. The `cidr` attribute is unchanged.

## 0.1.0-alpha6 — 2026-08-12

- First published release on the Terraform Registry. `nxip_pool` and `nxip_allocation` resources, both supporting `terraform import`.
- Tagged as a pre-release deliberately, excluded from default `terraform init` version resolution — the API and provider are still being validated with real users and are not yet recommended for production infrastructure.
