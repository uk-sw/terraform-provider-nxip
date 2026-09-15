package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// nxipClient centralizes the HTTP plumbing every resource needs: base URL
// resolution, auth header, JSON encoding/decoding, and a shared timeout.
//
// This exists because that plumbing used to be duplicated inline across
// every Create/Read/Delete method (and was about to be re-duplicated a
// third time for a new pool resource) — which is exactly how the API
// authenticated with the wrong header (`Authorization: Bearer` instead of
// the API's actual `x-api-key`) for as long as it did: fixing it meant
// finding and fixing the same mistake in six separate places. One client,
// fixed once, used everywhere, can't drift out of sync with itself again.
type nxipClient struct {
	baseURL      string
	apiKey       string
	organization string
	http         *http.Client
}

func newNxipClient(data *NxipProviderModel) *nxipClient {
	baseURL := "https://nxip.dev"
	if data != nil && !data.URL.IsNull() && data.URL.ValueString() != "" {
		baseURL = data.URL.ValueString()
	}
	apiKey := ""
	organization := ""
	if data != nil {
		apiKey = data.APIKey.ValueString()
		organization = data.Organization.ValueString()
	}
	return &nxipClient{
		baseURL:      baseURL,
		apiKey:       apiKey,
		organization: organization,
		http:         &http.Client{Timeout: 10 * time.Second},
	}
}

// organizationHeader is the header a provider's key uses to name the one
// customer organization a request acts on. Set by the `organization`
// provider attribute (or NXIP_ORGANIZATION); see
// docs/specs/msp-tenancy-phase2.md Part B and
// apps/api/src/middleware/auth.ts (ACTING_ON_BEHALF_HEADER) in
// net-saas-monorepo.
const organizationHeader = "x-nxip-organization"

// organizationNotFoundAPIMessage is the API's own fixed error text (see
// ORGANIZATION_NOT_FOUND_BODY in net-saas-monorepo's
// apps/api/src/middleware/auth.ts) when x-nxip-organization names an
// organization that is not this key's own, and is not one of its
// customers, or the link between them has ended. It is always exactly this
// string, which is what makes it safe to match on: a resource's own
// "not found" message (a missing pool, subnet or address) is worded
// differently, so this match only ever fires for the organization case.
const organizationNotFoundAPIMessage = "Organization not found."

// do sends a request to `path` (e.g. "/v1/pools/abc123") with an optional
// JSON body, decodes a JSON response into `out` (if non-nil and the body is
// non-empty), and returns the HTTP status code plus the API's own error
// message when the response body included one. Callers compare the status
// code themselves — a non-2xx status is not an error at this layer, since
// what counts as "expected" (e.g. 404 meaning "already gone, that's fine")
// varies by caller.
//
// The returned message comes from the nxip API's ErrorResponse shape
// (`{"statusCode", "error", "message"}` — see apps/api/src/routes/*.ts in
// net-saas-monorepo), which is already specific ("An IP Pool with CIDR
// 10.0.0.0/16 already exists in production / us-east-1.", not just "409").
// Extracting it here, once, means every resource's error branches can
// surface it instead of a bare status code, without each one re-implementing
// the same best-effort JSON peek.
func (c *nxipClient) do(ctx context.Context, method, path string, body any, out any) (int, string, error) {
	var reqBody *bytes.Buffer
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return 0, "", fmt.Errorf("failed to encode request body: %w", err)
		}
		reqBody = bytes.NewBuffer(encoded)
	} else {
		reqBody = bytes.NewBuffer(nil)
	}

	httpReq, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reqBody)
	if err != nil {
		return 0, "", fmt.Errorf("failed to create request: %w", err)
	}
	httpReq.Header.Set("x-api-key", c.apiKey)
	// Sent only when organization is actually set: an empty header would
	// still be a present header, and the API only treats the header as
	// absent when the client sends none at all (see resolveActingOnBehalf
	// in net-saas-monorepo's apps/api/src/middleware/auth.ts).
	if c.organization != "" {
		httpReq.Header.Set(organizationHeader, c.organization)
	}
	if body != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}

	httpResp, err := c.http.Do(httpReq)
	if err != nil {
		return 0, "", fmt.Errorf("failed to reach nxip API: %w", err)
	}
	defer httpResp.Body.Close()

	// Read the whole body once, regardless of expected shape — a DELETE with
	// 204 No Content has nothing to read, but everything else (success or
	// error) might, and reading it once lets us both decode `out` on success
	// and peek for a `message` field on failure without a second round trip.
	var respBody []byte
	if httpResp.StatusCode != http.StatusNoContent {
		respBody, err = io.ReadAll(httpResp.Body)
		if err != nil {
			return httpResp.StatusCode, "", fmt.Errorf("failed to read nxip API response: %w", err)
		}
	}

	// Extracted before the `out` decode below, and independently of whether
	// that decode succeeds: a malformed body (e.g. an infra-layer proxy's
	// own HTML error page, not the nxip API's JSON) would otherwise cause
	// the `out` decode to fail and return early, discarding a `message`
	// that — had it been present — would still have been worth surfacing.
	var apiMessage string
	if len(respBody) > 0 {
		var errBody struct {
			Message string `json:"message"`
		}
		// Best-effort: a success response won't have a "message" field
		// (poolResponse/subnetResponse don't define one), so this silently
		// leaves apiMessage empty rather than erroring — only error
		// responses populate it.
		if json.Unmarshal(respBody, &errBody) == nil {
			apiMessage = errBody.Message
		}
	}

	// Rewrite the API's generic "Organization not found." into a message
	// that names the `organization` attribute, so whichever resource is
	// about to build a diagnostic out of apiMessage doesn't have to know
	// anything about organizations to produce a clear one. Only rewritten
	// when this client actually sent the header - without it, the API
	// cannot produce this exact message for this reason, so there is
	// nothing to translate.
	if c.organization != "" && apiMessage == organizationNotFoundAPIMessage {
		apiMessage = fmt.Sprintf(
			"the `organization` attribute (%q) is not this API key's own organization and is not "+
				"one of its customers, or the link between them has ended",
			c.organization,
		)
	}

	if out != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, out); err != nil {
			return httpResp.StatusCode, apiMessage, fmt.Errorf("failed to parse nxip API response: %w", err)
		}
	}

	return httpResp.StatusCode, apiMessage, nil
}

// childrenListResponse is the minimal shape this client needs from
// GET /v1/organizations/children (see childOrganizationResponse in
// net-saas-monorepo's apps/api/src/routes/organizationLinks.ts) - just
// enough to tell whether the list is empty, not the full per-child usage
// payload.
type childrenListResponse struct {
	Data []json.RawMessage `json:"data"`
}

// hasAnyCustomers reports whether the API key's own organization currently
// has any customer organizations linked to it, by calling
// GET /v1/organizations/children?limit=1 - the smallest page that still
// answers the question. Used only to decide whether Configure should warn
// that `organization` is unset (docs/specs/msp-tenancy-phase2.md Part B),
// so this is advisory only: ok is false whenever the check could not be
// completed (a key whose role cannot list customers, a network error, or
// any non-200 response), and the caller adds no diagnostic either way in
// that case. This client must not itself have `organization` set when this
// is called - the check runs against the key's own organization on purpose.
func (c *nxipClient) hasAnyCustomers(ctx context.Context) (hasCustomers bool, ok bool) {
	var result childrenListResponse
	status, _, err := c.do(ctx, http.MethodGet, "/v1/organizations/children?limit=1", nil, &result)
	if err != nil || status != http.StatusOK {
		return false, false
	}
	return len(result.Data) > 0, true
}

// apiErrorSummary formats an actionable diagnostic for an unexpected API
// status: the API's own message when the response body had one, falling
// back to just the status code when it didn't (e.g. an intermediary proxy's
// own error page, not the nxip API itself).
func apiErrorSummary(action string, status int, apiMessage string) string {
	if apiMessage != "" {
		return fmt.Sprintf("%s: %s (HTTP %d)", action, apiMessage, status)
	}
	return fmt.Sprintf("%s: nxip API returned unexpected status %d", action, status)
}
