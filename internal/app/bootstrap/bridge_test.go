package bootstrap_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sannados/sannad/examples/contracts/directory"
	"github.com/sannados/sannad/examples/embedded/people"
	"github.com/sannados/sannad/internal/app/bootstrap"
	"github.com/sannados/sannad/pkg/config"
	"github.com/sannados/sannad/pkg/modulekit"
)

// The bridge is what makes ADR 0001's central claim true for tier 3: the same
// capability, reachable from outside the process, with no proto and no
// hand-written handler for the module that serves it.
//
// These tests go through the real gateway — auth middleware, Fiber routing, the
// net/http adaptor — because the parts most likely to be wrong are the seams,
// not the dispatch logic. In particular the request context must survive the
// adaptor carrying the tenant and caller the middleware put there; if it does
// not, every capability call arrives unauthenticated and the isolation callbacks
// refuse it.

// exposedPeople publishes the directory contract externally. The people module
// itself does not, which is correct: exposure is a deployment decision, and this
// wrapper is how an entrypoint makes one.
type exposedPeople struct{ *people.Module }

func (m exposedPeople) Descriptor() modulekit.Descriptor {
	d := m.Module.Descriptor()
	d.ID = "com.example.people.exposed"
	d.Exposed = []modulekit.CapabilityRef{directory.Reader}
	return d
}

// hiddenProvider registers a capability and deliberately does not expose it.
type hiddenProvider struct{}

var hiddenRef = modulekit.CapabilityRef{Name: "internal.secret.reader", Version: "v1"}

func (hiddenProvider) Descriptor() modulekit.Descriptor {
	return modulekit.Descriptor{
		ID:       "com.example.hidden",
		Kind:     "embedded",
		Provides: []modulekit.CapabilityRef{hiddenRef},
		Storage:  modulekit.StorageNone,
	}
}

func (hiddenProvider) Install(reg modulekit.Registrar) error {
	return modulekit.HandleTyped(reg, hiddenRef,
		func(_ context.Context, _ directory.ListRequest) (directory.ListResponse, error) {
			return directory.ListResponse{People: []directory.Person{{Name: "should not be reachable"}}}, nil
		})
}

func (hiddenProvider) Start(context.Context) error { return nil }
func (hiddenProvider) Stop(context.Context) error  { return nil }

func bridgeApp(t *testing.T) *bootstrap.App {
	t.Helper()
	cfg := config.Config{
		Env:                 "development",
		KernelAddr:          ":0",
		GatewayAddr:         ":0",
		DatabaseDSN:         filepath.Join(t.TempDir(), "bridge-test.db"),
		SessionSecret:       "bridge-test-session-secret-long-enough",
		KayanIssuer:         "http://localhost:8080",
		CORSOrigins:         "*",
		TenantSelfProvision: true,
	}
	app, err := bootstrap.New(cfg)
	if err != nil {
		t.Fatalf("bootstrap.New: %v", err)
	}
	if err := app.RegisterModule(exposedPeople{people.New(
		directory.Person{ID: "1", Name: "Amina", Email: "amina@example.com"},
		directory.Person{ID: "2", Name: "Omar", Email: "omar@example.com"},
	)}); err != nil {
		t.Fatalf("register people: %v", err)
	}
	if err := app.RegisterModule(hiddenProvider{}); err != nil {
		t.Fatalf("register hidden: %v", err)
	}
	if err := app.RegisterModule(leakyProvider{}); err != nil {
		t.Fatalf("register leaky: %v", err)
	}
	if err := app.Start(t.Context()); err != nil {
		t.Fatalf("app.Start: %v", err)
	}
	t.Cleanup(func() { _ = app.Shutdown(t.Context()) })
	return app
}

// bridgeToken registers and logs in, returning a bearer token.
func bridgeToken(t *testing.T, app *bootstrap.App) string {
	t.Helper()
	creds := map[string]string{
		"email":    "bridge@example.com",
		"password": "correct-horse-battery-staple",
		"tenant":   "bridge-tenant",
	}

	resp := doGateway(t, app, jsonRequest(t, http.MethodPost, "/api/v1/auth/register", creds))
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("register: got %d, want 201", resp.StatusCode)
	}

	resp = doGateway(t, app, jsonRequest(t, http.MethodPost, "/api/v1/auth/login", creds))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login: got %d, want 200", resp.StatusCode)
	}

	var body struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode login: %v", err)
	}
	if body.AccessToken == "" {
		t.Fatal("login returned no access token")
	}
	return body.AccessToken
}

func bridgeCall(t *testing.T, app *bootstrap.App, token, ref string, payload any) *http.Response {
	t.Helper()
	req := jsonRequest(t, http.MethodPost, "/api/v1/capabilities/"+ref, payload)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return doGateway(t, app, req)
}

// TestBridgeCallsAnExposedCapability is the claim: a capability written as an
// ordinary typed Go handler, reached over HTTP, with nothing written for the
// transport.
func TestBridgeCallsAnExposedCapability(t *testing.T) {
	app := bridgeApp(t)
	token := bridgeToken(t, app)

	resp := bridgeCall(t, app, token, directory.Reader.Key(), directory.ListRequest{})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("bridge call: got %d, want 200", resp.StatusCode)
	}

	var out directory.ListResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.People) != 2 || out.People[0].Name != "Amina" {
		t.Fatalf("unexpected response: %+v", out)
	}
}

// TestBridgeRespectsRequestFields proves the body actually reaches the handler
// decoded. A bridge that ignored the payload would pass the test above, since
// the default returns everything.
func TestBridgeRespectsRequestFields(t *testing.T) {
	app := bridgeApp(t)
	token := bridgeToken(t, app)

	resp := bridgeCall(t, app, token, directory.Reader.Key(), directory.ListRequest{Limit: 1})
	defer resp.Body.Close()

	var out directory.ListResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.People) != 1 {
		t.Fatalf("limit was ignored: got %d people, want 1", len(out.People))
	}
}

// TestUnexposedCapabilityIsNotReachable: registration is not publication. The
// capability resolves internally and must still be refused here.
func TestUnexposedCapabilityIsNotReachable(t *testing.T) {
	app := bridgeApp(t)
	token := bridgeToken(t, app)

	resp := bridgeCall(t, app, token, hiddenRef.Key(), directory.ListRequest{})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("an unexposed capability returned %d, want 404", resp.StatusCode)
	}
}

// TestUnknownAndUnexposedAreIndistinguishable: if the two differed, the endpoint
// would be an oracle for enumerating what a deployment runs internally.
func TestUnknownAndUnexposedAreIndistinguishable(t *testing.T) {
	app := bridgeApp(t)
	token := bridgeToken(t, app)

	hidden := bridgeCall(t, app, token, hiddenRef.Key(), directory.ListRequest{})
	defer hidden.Body.Close()
	missing := bridgeCall(t, app, token, "nothing.at.all@v1", directory.ListRequest{})
	defer missing.Body.Close()

	if hidden.StatusCode != missing.StatusCode {
		t.Fatalf("unexposed returned %d and unknown returned %d; the difference leaks what exists",
			hidden.StatusCode, missing.StatusCode)
	}

	var hiddenBody, missingBody map[string]string
	_ = json.NewDecoder(hidden.Body).Decode(&hiddenBody)
	_ = json.NewDecoder(missing.Body).Decode(&missingBody)
	if hiddenBody["error"] != missingBody["error"] {
		t.Fatalf("bodies differ: %q vs %q", hiddenBody["error"], missingBody["error"])
	}
}

// TestBridgeRequiresAuthentication. Without this the bridge is an unauthenticated
// path to every exposed capability in the deployment.
func TestBridgeRequiresAuthentication(t *testing.T) {
	app := bridgeApp(t)

	resp := bridgeCall(t, app, "", directory.Reader.Key(), directory.ListRequest{})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated bridge call returned %d, want 401", resp.StatusCode)
	}
}

// TestMalformedBodyIsABadRequest: the caller sent something the contract does
// not accept. Returning 500 would send an integrator hunting a bug on the wrong
// side of the network.
func TestMalformedBodyIsABadRequest(t *testing.T) {
	app := bridgeApp(t)
	token := bridgeToken(t, app)

	req := rawRequest(t, http.MethodPost, "/api/v1/capabilities/"+directory.Reader.Key(),
		[]byte(`{"limit": "not a number"}`))
	req.Header.Set("Authorization", "Bearer "+token)
	resp := doGateway(t, app, req)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed body returned %d, want 400", resp.StatusCode)
	}
}

// TestMissingVersionIsRefused. A reference without a version asks for "whatever
// you have", which is how an integration silently starts talking to v2.
func TestMissingVersionIsRefused(t *testing.T) {
	app := bridgeApp(t)
	token := bridgeToken(t, app)

	resp := bridgeCall(t, app, token, "directory.people.reader", directory.ListRequest{})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("an unversioned reference returned %d, want 400", resp.StatusCode)
	}
}

// TestListExposedHidesInternalCapabilities: the discovery endpoint must not
// enumerate what a deployment runs internally.
func TestListExposedHidesInternalCapabilities(t *testing.T) {
	app := bridgeApp(t)
	token := bridgeToken(t, app)

	req, _ := http.NewRequest(http.MethodGet, "/api/v1/capabilities", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp := doGateway(t, app, req)
	defer resp.Body.Close()

	var refs []modulekit.CapabilityRef
	if err := json.NewDecoder(resp.Body).Decode(&refs); err != nil {
		t.Fatalf("decode: %v", err)
	}

	var sawExposed, sawHidden bool
	for _, ref := range refs {
		switch ref.Key() {
		case directory.Reader.Key():
			sawExposed = true
		case hiddenRef.Key():
			sawHidden = true
		}
	}
	if !sawExposed {
		t.Error("an exposed capability is missing from the listing")
	}
	if sawHidden {
		t.Error("an unexposed capability appears in the public listing")
	}
}

// rawRequest posts an exact body, for cases where a well-formed value would not
// reproduce the condition under test.
func rawRequest(t *testing.T, method, path string, body []byte) *http.Request {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

// leakyRef is served by a capability that fails with an error carrying internal
// detail — a query fragment, another tenant's identifier — which is what a real
// storage or permission error looks like.
var leakyRef = modulekit.CapabilityRef{Name: "leaky.reader", Version: "v1"}

// secretDetail is what must never reach a caller.
const secretDetail = "SELECT * FROM accounts WHERE tenant_id = 'other-tenant'"

type leakyProvider struct{}

func (leakyProvider) Descriptor() modulekit.Descriptor {
	return modulekit.Descriptor{
		ID:       "com.example.leaky",
		Kind:     "embedded",
		Provides: []modulekit.CapabilityRef{leakyRef},
		Exposed:  []modulekit.CapabilityRef{leakyRef},
		Storage:  modulekit.StorageNone,
	}
}

func (leakyProvider) Install(reg modulekit.Registrar) error {
	return modulekit.HandleTyped(reg, leakyRef,
		func(_ context.Context, _ directory.ListRequest) (directory.ListResponse, error) {
			return directory.ListResponse{}, errors.New(secretDetail)
		})
}

func (leakyProvider) Start(context.Context) error { return nil }
func (leakyProvider) Stop(context.Context) error  { return nil }

// TestCapabilityErrorDetailDoesNotReachTheCaller. A capability error can carry
// internal state — a query, a path, another tenant's identifier — and both
// bridges are reachable from outside the process. The detail belongs in the log
// and nowhere else.
func TestCapabilityErrorDetailDoesNotReachTheCaller(t *testing.T) {
	app := bridgeApp(t)
	token := bridgeToken(t, app)

	resp := bridgeCall(t, app, token, leakyRef.Key(), directory.ListRequest{})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("got %d, want 500", resp.StatusCode)
	}

	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if strings.Contains(body["error"], "SELECT") || strings.Contains(body["error"], "other-tenant") {
		t.Fatalf("the capability's internal error detail reached the caller: %q", body["error"])
	}
}
