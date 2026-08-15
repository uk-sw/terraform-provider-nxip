# Changelog

All notable changes to this provider are documented here.

## 0.2.0-alpha.1 (2026-08-15)

- **Version scheme fix, no code changes.** Every prior pre-release tag used an unpadded, dot-less suffix (`alpha9`, `alpha10`, ...), which semver's precedence rules compare as plain text, not as numbers, once a pre-release identifier isn't purely digits. Under that rule, `"alpha9"` lexically sorts *above* `"alpha10"` through `"alpha14"` (`9` > `1` as the first differing character) - so every tool that follows the spec, including the Terraform Registry, correctly treated `0.1.0-alpha9` as the highest-precedence version ever published, regardless of publish order. That's why newer releases never became "latest".
- Bumping the minor version (rather than only reformatting the suffix) is the actual fix: a higher `0.2.0` always outranks any `0.1.0-*` prerelease, regardless of the string-comparison quirk above. It's also an honest reflection that `0.1.0-alpha14` already added a genuinely new resource (`nxip_address`), which is what a minor bump is for.
- Going forward, pre-release suffixes use a dot before the number (`alpha.N`, not `alphaN`), so the number is its own field and compares numerically.

## 0.1.0-alpha14 (2026-08-15)

- **Added `nxip_address`**: registers or reserves a specific IP address within an already-allocated `nxip_subnet`, the individual-address layer beneath the subnet itself. Deliberately manual-only (no auto-pick), matching the API: which address to use is normally chosen by whoever's deploying the host, or by DHCP, not requested blind from IPAM. Supports `status` (ACTIVE/RESERVED), `hostname`, and free-form `metadata`, and `terraform import` via a composite `<subnet_id>/<address_id>` identifier; unlike `nxip_pool`/`nxip_subnet`, an address's own ID alone isn't enough to fetch it, since its API route is nested under its parent subnet. Every attribute is immutable (`RequiresReplace`), matching `nxip_subnet`; there's no PATCH endpoint for addresses server-side.
- Added acceptance test coverage for IPv6 resources across the pool/subnet/address hierarchy.
- The provider description now leads with "IPAM (IP Address Management)" instead of just "IP CIDR subnets", so the Registry listing's title, meta description, and search indexing actually surface for that term.
- Internal fix: bumped the Go toolchain to 1.26.6, resolving four stdlib CVEs (`net/url`, `crypto/tls`, `encoding/asn1`, and an `idna` issue reached via `net/http`) that were failing CI's vulnerability scan. No user-facing behavior change.

## 0.1.0-alpha12 (2026-08-13)

- Internal fix: the API's error `message` is now extracted before attempting to decode a resource's own response shape, not after, so a malformed or unexpected response body no longer discards a `message` that was actually present and worth surfacing. No user-facing behavior change from `0.1.0-alpha11` in the normal case; only affects an edge case that hadn't been observed in practice.

## 0.1.0-alpha11 (2026-08-13)

- **Error messages now include the nxip API's own explanation, not just an HTTP status code.** Every resource (`nxip_pool`, `nxip_subnet`) previously discarded the API's response body on failure: a `409` on pool creation showed only `nxip API failed pool creation request with status 409`, even though the API had already sent back a specific reason (which CIDR conflicted, or which environment/region/family combination). Fixed at the shared HTTP client level so it applies uniformly: a conflict now reads like `failed to create pool: An IP Pool with CIDR 10.0.0.0/16 already exists in production / us-east-1. (HTTP 409)`. Pool deletion's "still has subnets attached" error now also includes the API's own subnet count and pool name instead of a generic hardcoded message.

## 0.1.0-alpha10 (2026-08-12)

- Added a `metadata` attribute to `nxip_subnet`: a free-form map of string key/value tags (e.g. `vpc_id`, `cost_center`), stored and returned as-is. Optional and Computed; a config that never sets it reads back as an empty map. Immutable, like every other `nxip_subnet` attribute: changing it forces a new resource.

## 0.1.0-alpha9 (2026-08-12)

- Internal fix: `nxip_pool`'s `cidr` attribute now parses from the API's `cidr` response field (it had been reading a since-renamed `cidrBlock` field). No config or attribute-name changes for users; this is a wire-format fix matching an API-side rename.

## 0.1.0-alpha7 (2026-08-12)

- **Breaking:** renamed `nxip_allocation` to `nxip_subnet` to match the terminology already used everywhere else in the system (the API's REST endpoints, response fields, and the underlying data model). Fixed now, before any real external adoption exists; this is the cheapest this rename will ever be. The `cidr` attribute is unchanged.

## 0.1.0-alpha6 (2026-08-12)

- First published release on the Terraform Registry. `nxip_pool` and `nxip_allocation` resources, both supporting `terraform import`.
- Tagged as a pre-release deliberately, excluded from default `terraform init` version resolution; the API and provider are still being validated with real users and are not yet recommended for production infrastructure.
