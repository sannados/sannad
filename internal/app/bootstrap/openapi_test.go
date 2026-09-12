package bootstrap_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestOpenAPISpecDescribesTheGatewaysOwnSurface: an integrator's first stop
// when building against the gateway without reading the source. It must be
// reachable without a token — a spec describing a REST surface is not itself
// the surface — and it must actually name the endpoints this package wires,
// not merely resemble an OpenAPI document.
func TestOpenAPISpecDescribesTheGatewaysOwnSurface(t *testing.T) {
	app := gatewayApp(t)

	resp := doGateway(t, app, httptest.NewRequest(http.MethodGet, "/api/v1/openapi.json", nil))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var doc map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if doc["openapi"] != "3.0.3" {
		t.Fatalf("openapi version = %v, want 3.0.3", doc["openapi"])
	}

	paths, ok := doc["paths"].(map[string]any)
	if !ok {
		t.Fatalf("paths is %T, want an object", doc["paths"])
	}

	for _, want := range []string{
		"/api/v1/auth/login",
		"/api/v1/capabilities/{ref}",
		"/api/v1/webhooks/endpoints",
		"/api/v1/webhooks/endpoints/{id}",
		"/api/v1/webhooks/endpoints/{id}/rotate-secret",
		"/api/v1/webhooks/deliveries/{id}/replay",
	} {
		if _, ok := paths[want]; !ok {
			t.Errorf("spec is missing path %s", want)
		}
	}

	// The one endpoint whose payload the kernel genuinely cannot describe must
	// say so honestly rather than fabricate a schema.
	dispatch, ok := paths["/api/v1/capabilities/{ref}"].(map[string]any)
	if !ok {
		t.Fatalf("dispatch path missing")
	}
	post, ok := dispatch["post"].(map[string]any)
	if !ok {
		t.Fatalf("dispatch has no POST operation")
	}
	desc, _ := post["description"].(string)
	if desc == "" {
		t.Error("dispatch operation has no description explaining the opaque payload")
	}
}

// TestOpenAPISpecRequiresNoAuthentication: documentation a caller needs a
// token to read defeats its own purpose — an integrator has no token until
// they have read the docs.
func TestOpenAPISpecRequiresNoAuthentication(t *testing.T) {
	app := gatewayApp(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/openapi.json", nil)
	// Deliberately no Authorization header.
	resp := doGateway(t, app, req)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 with no bearer token", resp.StatusCode)
	}
}
