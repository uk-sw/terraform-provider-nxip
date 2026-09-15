package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// Unit tests for the `organization` provider attribute
// (docs/specs/msp-tenancy-phase2.md Part B): header injection, attribute
// versus environment precedence, the 404 diagnostic naming the attribute,
// and the advisory "organization not set" warning. These run without
// TF_ACC and never contact a live API - every server here is a local
// httptest.Server standing in for nxip.

// -----------------------------------------------------------------------
// Precedence (Done means 11: env var used when the attribute is unset,
// attribute wins over env)
// -----------------------------------------------------------------------

func TestResolveConfigValue(t *testing.T) {
	const envVar = "NXIP_TEST_RESOLVE_VAR"

	t.Run("attribute set, no env: attribute wins", func(t *testing.T) {
		t.Setenv(envVar, "")
		got := resolveConfigValue(types.StringValue("attr-value"), envVar)
		if got != "attr-value" {
			t.Fatalf("expected %q, got %q", "attr-value", got)
		}
	})

	t.Run("attribute unset, env set: env is used", func(t *testing.T) {
		t.Setenv(envVar, "env-value")
		got := resolveConfigValue(types.StringNull(), envVar)
		if got != "env-value" {
			t.Fatalf("expected %q, got %q", "env-value", got)
		}
	})

	t.Run("attribute set, env also set: attribute wins", func(t *testing.T) {
		t.Setenv(envVar, "env-value")
		got := resolveConfigValue(types.StringValue("attr-value"), envVar)
		if got != "attr-value" {
			t.Fatalf("expected the attribute to win, got %q", got)
		}
	})

	t.Run("attribute explicitly empty string, env set: env is used", func(t *testing.T) {
		// An empty string in the config is treated the same as unset - a
		// blank organization = "" is never distinct from "not configured".
		t.Setenv(envVar, "env-value")
		got := resolveConfigValue(types.StringValue(""), envVar)
		if got != "env-value" {
			t.Fatalf("expected %q, got %q", "env-value", got)
		}
	})

	t.Run("neither set: empty", func(t *testing.T) {
		t.Setenv(envVar, "")
		got := resolveConfigValue(types.StringNull(), envVar)
		if got != "" {
			t.Fatalf("expected empty string, got %q", got)
		}
	})
}

// -----------------------------------------------------------------------
// Header injection (Done means 11: organization set sends the header on
// every request, unset sends none)
// -----------------------------------------------------------------------

func TestClientDo_OrganizationHeader(t *testing.T) {
	var received http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	t.Run("organization set: header sent with that exact value", func(t *testing.T) {
		client := &nxipClient{baseURL: server.URL, apiKey: "key", organization: "org_123", http: server.Client()}
		if _, _, err := client.do(context.Background(), http.MethodGet, "/v1/pools/x", nil, nil); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		values := received.Values(organizationHeader)
		if len(values) != 1 || values[0] != "org_123" {
			t.Fatalf("expected %s = [org_123], got %v", organizationHeader, values)
		}
	})

	t.Run("organization unset: header sent on no request at all", func(t *testing.T) {
		client := &nxipClient{baseURL: server.URL, apiKey: "key", organization: "", http: server.Client()}
		if _, _, err := client.do(context.Background(), http.MethodGet, "/v1/pools/x", nil, nil); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if values := received.Values(organizationHeader); len(values) != 0 {
			t.Fatalf("expected no %s header, got %v", organizationHeader, values)
		}
	})
}

// -----------------------------------------------------------------------
// 404 diagnostic (Done means 12: a 404 for the organization yields a
// diagnostic naming `organization`)
// -----------------------------------------------------------------------

func TestClientDo_OrganizationNotFoundDiagnostic(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"statusCode":404,"error":"Not Found","message":"Organization not found."}`))
	}))
	defer server.Close()

	client := &nxipClient{baseURL: server.URL, apiKey: "key", organization: "org_missing", http: server.Client()}
	status, apiMessage, err := client.do(context.Background(), http.MethodPost, "/v1/pools", map[string]any{"name": "x"}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", status)
	}
	if !strings.Contains(apiMessage, "organization") {
		t.Fatalf("expected the diagnostic to name the organization attribute, got %q", apiMessage)
	}
	if !strings.Contains(apiMessage, "org_missing") {
		t.Fatalf("expected the diagnostic to name the configured value, got %q", apiMessage)
	}

	// This is the same message every resource's Create/Update passes
	// straight to apiErrorSummary, so confirm the rewrite survives that
	// too - it is what a real terraform apply actually shows.
	summary := apiErrorSummary("failed to create pool", status, apiMessage)
	if !strings.Contains(summary, "organization") {
		t.Fatalf("expected the API error summary to name organization, got %q", summary)
	}
}

// A plain "that resource doesn't exist" 404 must pass through unchanged.
// Without this guard, a naive "any 404 while organization is set" rewrite
// would misreport an ordinary missing pool/subnet/address as an
// organization problem.
func TestClientDo_ResourceNotFoundIsNotRewritten(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"statusCode":404,"error":"Not Found","message":"Pool not found."}`))
	}))
	defer server.Close()

	client := &nxipClient{baseURL: server.URL, apiKey: "key", organization: "org_123", http: server.Client()}
	_, apiMessage, err := client.do(context.Background(), http.MethodGet, "/v1/pools/abc", nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if apiMessage != "Pool not found." {
		t.Fatalf("expected the message to pass through unchanged, got %q", apiMessage)
	}
}

// -----------------------------------------------------------------------
// hasAnyCustomers (backs the advisory warning, Done means 12a)
// -----------------------------------------------------------------------

func TestClientHasAnyCustomers(t *testing.T) {
	t.Run("customers present: true, ok", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v1/organizations/children" {
				t.Errorf("unexpected path %q", r.URL.Path)
			}
			if got := r.URL.Query().Get("limit"); got != "1" {
				t.Errorf("expected limit=1, got %q", got)
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":[{"id":"org_1","name":"Acme"}],"meta":{"limit":1,"nextCursor":null}}`))
		}))
		defer server.Close()

		client := &nxipClient{baseURL: server.URL, apiKey: "key", http: server.Client()}
		hasCustomers, ok := client.hasAnyCustomers(context.Background())
		if !ok || !hasCustomers {
			t.Fatalf("expected (true, true), got (%v, %v)", hasCustomers, ok)
		}
	})

	t.Run("no customers: false, ok", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":[],"meta":{"limit":1,"nextCursor":null}}`))
		}))
		defer server.Close()

		client := &nxipClient{baseURL: server.URL, apiKey: "key", http: server.Client()}
		hasCustomers, ok := client.hasAnyCustomers(context.Background())
		if !ok || hasCustomers {
			t.Fatalf("expected (false, true), got (%v, %v)", hasCustomers, ok)
		}
	})

	t.Run("call fails with a non-200 (e.g. a role that cannot list): false, not ok", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"statusCode":403,"error":"Forbidden","message":"API key lacks the required role."}`))
		}))
		defer server.Close()

		client := &nxipClient{baseURL: server.URL, apiKey: "key", http: server.Client()}
		hasCustomers, ok := client.hasAnyCustomers(context.Background())
		if ok || hasCustomers {
			t.Fatalf("expected (false, false), got (%v, %v)", hasCustomers, ok)
		}
	})

	t.Run("network error: false, not ok", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		unreachableURL := server.URL
		server.Close() // closed before use, so the request cannot possibly reach it

		client := &nxipClient{baseURL: unreachableURL, apiKey: "key", http: &http.Client{Timeout: time.Second}}
		hasCustomers, ok := client.hasAnyCustomers(context.Background())
		if ok || hasCustomers {
			t.Fatalf("expected (false, false), got (%v, %v)", hasCustomers, ok)
		}
	})
}

// -----------------------------------------------------------------------
// Provider Configure(): the advisory warning end to end, and the wiring
// from config/env into the resolved provider model (Done means 11, 12a)
// -----------------------------------------------------------------------

// configureTestProvider drives NxipProvider.Configure the same way
// Terraform itself would, building a real tfsdk.Config from the provider's
// own schema rather than calling Configure's internals directly, so these
// tests exercise the actual req.Config.Get(...) wiring, not a shortcut
// around it. An empty string for any argument means "not set in config"
// (a null value), matching an omitted attribute in a .tf file.
func configureTestProvider(t *testing.T, apiKey, url, organization string) (*NxipProviderModel, diag.Diagnostics) {
	t.Helper()
	ctx := context.Background()
	p := &NxipProvider{version: "test"}

	schemaResp := &provider.SchemaResponse{}
	p.Schema(ctx, provider.SchemaRequest{}, schemaResp)

	objectType := schemaResp.Schema.Type().TerraformType(ctx)

	stringOrNull := func(s string) tftypes.Value {
		if s == "" {
			return tftypes.NewValue(tftypes.String, nil)
		}
		return tftypes.NewValue(tftypes.String, s)
	}

	raw := tftypes.NewValue(objectType, map[string]tftypes.Value{
		"api_key":      stringOrNull(apiKey),
		"url":          stringOrNull(url),
		"organization": stringOrNull(organization),
	})

	configureResp := &provider.ConfigureResponse{}
	p.Configure(ctx, provider.ConfigureRequest{
		Config: tfsdk.Config{Raw: raw, Schema: schemaResp.Schema},
	}, configureResp)

	data, _ := configureResp.ResourceData.(*NxipProviderModel)
	return data, configureResp.Diagnostics
}

func diagsContainWarning(diags diag.Diagnostics, summarySubstring string) bool {
	for _, d := range diags {
		if d.Severity() == diag.SeverityWarning && strings.Contains(d.Summary(), summarySubstring) {
			return true
		}
	}
	return false
}

func newChildrenTestServer(t *testing.T, hasCustomers bool) *httptest.Server {
	t.Helper()
	data := "[]"
	if hasCustomers {
		data = `[{"id":"org_1","name":"Acme","roleCeiling":"MEMBER","linkedAt":"2026-01-01T00:00:00.000Z"}]`
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/organizations/children" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, `{"data":%s,"meta":{"limit":1,"nextCursor":null}}`, data)
	}))
}

func TestProviderConfigure_OrganizationWarning(t *testing.T) {
	t.Run("unset, key's organization has customers: warning added, apply still proceeds", func(t *testing.T) {
		t.Setenv("NXIP_ORGANIZATION", "")
		server := newChildrenTestServer(t, true)
		defer server.Close()

		data, diags := configureTestProvider(t, "key123", server.URL, "")
		if diags.HasError() {
			t.Fatalf("Configure must not error: %v", diags)
		}
		if data == nil {
			t.Fatalf("expected ResourceData to be set so plans/applies can proceed")
		}
		if !diagsContainWarning(diags, "organization not set") {
			t.Fatalf("expected an 'organization not set' warning, got: %v", diags)
		}
	})

	t.Run("unset, key's organization has no customers: no diagnostic", func(t *testing.T) {
		t.Setenv("NXIP_ORGANIZATION", "")
		server := newChildrenTestServer(t, false)
		defer server.Close()

		_, diags := configureTestProvider(t, "key123", server.URL, "")
		if len(diags) != 0 {
			t.Fatalf("expected no diagnostics at all, got: %v", diags)
		}
	})

	t.Run("unset, the check call fails: no diagnostic, never blocks", func(t *testing.T) {
		t.Setenv("NXIP_ORGANIZATION", "")
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer server.Close()

		data, diags := configureTestProvider(t, "key123", server.URL, "")
		if diags.HasError() {
			t.Fatalf("an advisory check failing must never become an error: %v", diags)
		}
		if len(diags) != 0 {
			t.Fatalf("expected no diagnostics when the check fails, got: %v", diags)
		}
		if data == nil || data.APIKey.ValueString() != "key123" {
			t.Fatalf("expected Configure to still succeed and populate ResourceData")
		}
	})

	t.Run("organization set: the children endpoint is never called, no warning", func(t *testing.T) {
		called := false
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			called = true
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":[{"id":"org_1"}],"meta":{"limit":1,"nextCursor":null}}`))
		}))
		defer server.Close()

		_, diags := configureTestProvider(t, "key123", server.URL, "org_customerA")
		if called {
			t.Fatalf("Configure must not call GET /v1/organizations/children when organization is already set")
		}
		if len(diags) != 0 {
			t.Fatalf("expected no diagnostics, got: %v", diags)
		}
	})
}

func TestProviderConfigure_OrganizationPrecedence(t *testing.T) {
	t.Run("attribute unset, env set: organization comes from NXIP_ORGANIZATION", func(t *testing.T) {
		server := newChildrenTestServer(t, false)
		defer server.Close()
		t.Setenv("NXIP_ORGANIZATION", "org_from_env")

		data, _ := configureTestProvider(t, "key123", server.URL, "")
		if data.Organization.ValueString() != "org_from_env" {
			t.Fatalf("expected organization from NXIP_ORGANIZATION, got %q", data.Organization.ValueString())
		}
	})

	t.Run("attribute and env both set: the attribute wins", func(t *testing.T) {
		server := newChildrenTestServer(t, false)
		defer server.Close()
		t.Setenv("NXIP_ORGANIZATION", "org_from_env")

		data, _ := configureTestProvider(t, "key123", server.URL, "org_from_attr")
		if data.Organization.ValueString() != "org_from_attr" {
			t.Fatalf("expected the attribute to win over NXIP_ORGANIZATION, got %q", data.Organization.ValueString())
		}
	})

	t.Run("neither set: organization resolves to empty, meaning the key's own organization", func(t *testing.T) {
		server := newChildrenTestServer(t, false)
		defer server.Close()
		t.Setenv("NXIP_ORGANIZATION", "")

		data, _ := configureTestProvider(t, "key123", server.URL, "")
		if data.Organization.ValueString() != "" {
			t.Fatalf("expected organization to be empty, got %q", data.Organization.ValueString())
		}
	})
}
