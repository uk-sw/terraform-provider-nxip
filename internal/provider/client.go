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
	baseURL string
	apiKey  string
	http    *http.Client
}

func newNxipClient(data *NxipProviderModel) *nxipClient {
	baseURL := "https://nxip.dev"
	if data != nil && !data.URL.IsNull() && data.URL.ValueString() != "" {
		baseURL = data.URL.ValueString()
	}
	apiKey := ""
	if data != nil {
		apiKey = data.APIKey.ValueString()
	}
	return &nxipClient{
		baseURL: baseURL,
		apiKey:  apiKey,
		http:    &http.Client{Timeout: 10 * time.Second},
	}
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

	if out != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, out); err != nil {
			return httpResp.StatusCode, "", fmt.Errorf("failed to parse nxip API response: %w", err)
		}
	}

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

	return httpResp.StatusCode, apiMessage, nil
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
