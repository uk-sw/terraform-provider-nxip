package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-log/tflog"
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

	// wait and now are what do() uses to sit out a 429 and to read a
	// Retry-After given as a date. Left nil, which is what
	// newNxipClient and every hand-built client in the tests do, they are
	// a real context-aware sleep and time.Now. Tests set them so a retry
	// can be checked without actually waiting a minute.
	wait func(ctx context.Context, d time.Duration) error
	now  func() time.Time
}

// The 429 retry budget. The API's limiter works on a one-minute window and
// says in Retry-After how much of it is left, so no honest wait is ever
// longer than rateLimitMaxWait; the cap is there so a malformed or hostile
// header cannot hang an apply. rateLimitMaxRetries keeps the worst case
// to a few minutes before the 429 is reported as it was.
const (
	rateLimitMaxRetries = 3
	rateLimitMaxWait    = 60 * time.Second
	// rateLimitDefaultWait is used when a 429 carries no Retry-After that
	// can be read. Both of the API's limiters send one, so this is for
	// something in between (a proxy, say) answering 429 on its own.
	rateLimitDefaultWait = 10 * time.Second
)

// retryAfterDelay reads a Retry-After header, which RFC 9110 allows as
// either a number of seconds or an HTTP date. The API sends seconds
// ("retry-after: 57"); a date is handled because the header allows it and
// anything in front of the API may use it. A date already passed means no
// wait. The result is capped at rateLimitMaxWait, and anything unreadable
// falls back to rateLimitDefaultWait.
func retryAfterDelay(header string, now time.Time) time.Duration {
	header = strings.TrimSpace(header)
	if header == "" {
		return rateLimitDefaultWait
	}
	var delay time.Duration
	if seconds, err := strconv.Atoi(header); err == nil {
		if seconds < 0 {
			return rateLimitDefaultWait
		}
		delay = time.Duration(seconds) * time.Second
	} else if when, err := http.ParseTime(header); err == nil {
		delay = when.Sub(now)
		if delay < 0 {
			delay = 0
		}
	} else {
		return rateLimitDefaultWait
	}
	if delay > rateLimitMaxWait {
		delay = rateLimitMaxWait
	}
	return delay
}

// sleepContext waits for d, or returns early with the context's error if
// the context ends first, so Ctrl+C during a rate-limit wait stops the
// apply at once rather than after the full Retry-After.
func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
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

// troubleshootingPageURL is the docs page that explains the errors people
// hit for a reason that is not a bug (a missing pool, a wrong organization,
// a pool Terraform wants to replace), one section per error. Kept as the one
// place the URL is written, and only ever turned into a link through
// troubleshootingLink below, so no link can be built by hand and drift.
const troubleshootingPageURL = "https://nx-ip.com/docs/troubleshooting"

// The anchors on troubleshootingPageURL that this provider links to. A
// released provider prints these, so the page promises never to rename or
// remove them (see docs/specs/troubleshooting-page.md in net-saas-monorepo).
// Each is written here once and nowhere else: every call site names the
// constant, so a typo cannot hide in one diagnostic out of several.
const (
	troubleshootingNoMatchingPool       = "no-matching-pool"
	troubleshootingPoolReplaced         = "pool-replaced"
	troubleshootingOrganizationNotFound = "organization-not-found"
)

// troubleshootingLink builds the sentence every diagnostic appends to point
// at one section of the troubleshooting page, so the wording is the same
// wherever it appears.
func troubleshootingLink(anchor string) string {
	return "Troubleshooting: " + troubleshootingPageURL + "#" + anchor
}

// organizationNotFoundAPIMessage is the API's own fixed error text (see
// ORGANIZATION_NOT_FOUND_BODY in net-saas-monorepo's
// apps/api/src/middleware/auth.ts) when x-nxip-organization names an
// organization that is not this key's own, and is not one of its
// customers, or the link between them has ended. It is always exactly this
// string, which is what makes it safe to match on: a resource's own
// "not found" message (a missing pool, subnet or address) is worded
// differently, so this match only ever fires for the organization case.
const organizationNotFoundAPIMessage = "Organization not found."

// organizationNotFoundDiagnosticPrefix opens the message do() rewrites
// organizationNotFoundAPIMessage into below. Shared as one constant, rather
// than repeated as a literal in both the rewrite and isOrganizationNotFound,
// so the two can never drift apart: whatever do() actually writes is
// exactly what isOrganizationNotFound checks for.
const organizationNotFoundDiagnosticPrefix = "the `organization` attribute ("

// isOrganizationNotFound reports whether a response is the specific 404
// do() produces when x-nxip-organization named an organization that is not
// this key's own, and is not one of its customers, or the link has ended.
//
// This matters to Read and Delete on every resource, which otherwise treat
// any 404 as "this resource is already gone" - correct for an ordinary
// missing pool/subnet/address, but wrong here: an organization-not-found
// 404 means the wrong organization was asked about entirely, not that the
// resource was deleted from the right one. Read must not drop a possibly-
// still-live resource from state on that basis, and Delete must not report
// success when it never reached the resource at all. Both should call this
// before falling back to their ordinary 404 handling.
func isOrganizationNotFound(status int, apiMessage string) bool {
	return status == http.StatusNotFound && strings.HasPrefix(apiMessage, organizationNotFoundDiagnosticPrefix)
}

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
//
// A 429 is the one status do() acts on itself rather than handing back: it
// waits for the response's Retry-After and sends the request again, up to
// rateLimitMaxRetries times, and only then returns the last 429 to the
// caller like any other status. Without this, a plan then apply of a couple
// of dozen resources on the free tier (60 requests a minute) failed partway
// through, although the API had said exactly how long to wait.
//
// Retrying is safe for every method, POST included, only because both of
// the API's rate limiters reject in onRequest/preHandler hooks, before any
// route handler runs (apps/api/src/app.ts in net-saas-monorepo), so a 429
// never follows a write that happened. Nothing else is retried: a 5xx or a
// timeout on a POST may follow a committed allocation, and sending it again
// would allocate a second block.
func (c *nxipClient) do(ctx context.Context, method, path string, body any, out any) (int, string, error) {
	// Encoded once, outside the retry loop, and wrapped in a fresh reader
	// per attempt: a reader is drained by the first send, so reusing it
	// would retry with an empty body.
	var encoded []byte
	if body != nil {
		var err error
		encoded, err = json.Marshal(body)
		if err != nil {
			return 0, "", fmt.Errorf("failed to encode request body: %w", err)
		}
	}

	wait := c.wait
	if wait == nil {
		wait = sleepContext
	}
	now := c.now
	if now == nil {
		now = time.Now
	}

	var httpResp *http.Response
	for attempt := 0; ; attempt++ {
		httpReq, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bytes.NewReader(encoded))
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

		httpResp, err = c.http.Do(httpReq)
		if err != nil {
			return 0, "", fmt.Errorf("failed to reach nxip API: %w", err)
		}
		if httpResp.StatusCode != http.StatusTooManyRequests || attempt >= rateLimitMaxRetries {
			break
		}

		delay := retryAfterDelay(httpResp.Header.Get("Retry-After"), now())
		// Drained before closing so the connection can be reused for the
		// retry. The body is only the API's "retry in N seconds" sentence,
		// which Retry-After already says; the last 429's body is the one
		// that reaches the caller, read below like any other response.
		_, _ = io.Copy(io.Discard, httpResp.Body)
		_ = httpResp.Body.Close()

		// Warn, so it shows at TF_LOG=WARN and above: an apply that goes
		// quiet for up to a minute should say why to anyone who looks.
		tflog.Warn(ctx, "nxip API rate limit reached, waiting before retrying", map[string]interface{}{
			"method":       method,
			"path":         path,
			"retry_after":  httpResp.Header.Get("Retry-After"),
			"wait_seconds": delay.Seconds(),
			"retry":        attempt + 1,
			"max_retries":  rateLimitMaxRetries,
		})
		if err := wait(ctx, delay); err != nil {
			return http.StatusTooManyRequests, "", fmt.Errorf("stopped waiting for the nxip API rate limit to reset: %w", err)
		}
	}
	defer func() { _ = httpResp.Body.Close() }()

	// Read the whole body once, regardless of expected shape — a DELETE with
	// 204 No Content has nothing to read, but everything else (success or
	// error) might, and reading it once lets us both decode `out` on success
	// and peek for a `message` field on failure without a second round trip.
	var respBody []byte
	if httpResp.StatusCode != http.StatusNoContent {
		var err error
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
	//
	// The troubleshooting link goes on here too, rather than in each
	// resource, for the same reason the rewrite does: this is the one place
	// every resource's organization-not-found passes through, so all of them
	// get it. It is appended after the prefix, never before, so
	// isOrganizationNotFound still matches.
	if c.organization != "" && apiMessage == organizationNotFoundAPIMessage {
		apiMessage = fmt.Sprintf(
			organizationNotFoundDiagnosticPrefix+"%q) is not this API key's own organization and is not "+
				"one of its customers, or the link between them has ended. %s",
			c.organization,
			troubleshootingLink(troubleshootingOrganizationNotFound),
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
