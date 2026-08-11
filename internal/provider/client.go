package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
// non-empty), and returns the HTTP status code. Callers compare the status
// code themselves — a non-2xx status is not an error at this layer, since
// what counts as "expected" (e.g. 404 meaning "already gone, that's fine")
// varies by caller.
func (c *nxipClient) do(ctx context.Context, method, path string, body any, out any) (int, error) {
	var reqBody *bytes.Buffer
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return 0, fmt.Errorf("failed to encode request body: %w", err)
		}
		reqBody = bytes.NewBuffer(encoded)
	} else {
		reqBody = bytes.NewBuffer(nil)
	}

	httpReq, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reqBody)
	if err != nil {
		return 0, fmt.Errorf("failed to create request: %w", err)
	}
	httpReq.Header.Set("x-api-key", c.apiKey)
	if body != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}

	httpResp, err := c.http.Do(httpReq)
	if err != nil {
		return 0, fmt.Errorf("failed to reach nxip API: %w", err)
	}
	defer httpResp.Body.Close()

	if out != nil {
		// A DELETE with 204 No Content (pools) or a body-less response has
		// nothing to decode — only decode when there's actually a body.
		if httpResp.ContentLength != 0 && httpResp.StatusCode != http.StatusNoContent {
			if err := json.NewDecoder(httpResp.Body).Decode(out); err != nil {
				return httpResp.StatusCode, fmt.Errorf("failed to parse nxip API response: %w", err)
			}
		}
	}

	return httpResp.StatusCode, nil
}
