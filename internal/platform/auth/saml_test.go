package auth_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"html"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"testing"
	"time"

	ksaml "github.com/getkayan/kayan/kayan-saml"
	"github.com/sannados/sannad/internal/platform/auth"
)

// These tests run a real SAML round trip: a genuine kayan-saml IdentityProvider
// signs a genuine assertion with its own key, and the Service under test
// verifies it with kayan-saml's own XML-DSig verifier. Nothing here is
// mocked — a wrong key, wrong signature, or wrong tenant is caught exactly
// as a real identity provider integration would catch it.

const (
	samlSPEntityID  = "https://sp.example.com"
	samlSPACSUrl    = "https://sp.example.com/acs"
	samlIdPEntityID = "https://idp.example.com"
	samlIdPSSOUrl   = "https://idp.example.com/sso"
)

// samlTestIdP bundles a real kayan-saml IdentityProvider with the key pair
// it signs with, so a test can swap the signing key to prove a wrong
// signature is rejected.
type samlTestIdP struct {
	idp    *ksaml.IdentityProvider
	nameID string
}

// newSAMLTestIdP builds a self-signed IdP that always authenticates as
// nameID and registers this deployment's SP, then returns both the running
// IdP and the PEM certificate the Service must be configured to trust.
func newSAMLTestIdP(t *testing.T, nameID string) (*samlTestIdP, string) {
	t.Helper()
	key, cert := generateTestCertificate(t, "test-idp")

	idp := ksaml.NewIdentityProvider(ksaml.IdPServerConfig{
		EntityID:    samlIdPEntityID,
		SSOUrl:      samlIdPSSOUrl,
		Certificate: cert,
		PrivateKey:  key,
	}, nil, nil)

	idp.RegisterSP(&ksaml.SPRegistration{
		ID:       samlSPEntityID,
		EntityID: samlSPEntityID,
		ACSUrl:   samlSPACSUrl,
	})
	idp.SetHooks(ksaml.IdPHooks{
		AuthenticateUser: func(context.Context, *http.Request) (any, error) {
			return struct{}{}, nil
		},
		GetNameID: func(context.Context, any, *ksaml.SPRegistration) (string, error) {
			return nameID, nil
		},
		GetUserAttributes: func(context.Context, any, *ksaml.SPRegistration) (map[string][]string, error) {
			return map[string][]string{"email": {nameID}}, nil
		},
	})

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
	return &samlTestIdP{idp: idp, nameID: nameID}, string(certPEM)
}

// signIn drives redirectURL (as returned by Service.InitiateSAMLLogin)
// through the test IdP exactly as a browser would, and returns the
// SAMLResponse and RelayState the IdP posts back to the ACS endpoint.
func (t2 *samlTestIdP) signIn(t *testing.T, redirectURL string) (samlResponse, relayState string) {
	t.Helper()
	parsed, err := url.Parse(redirectURL)
	if err != nil {
		t.Fatalf("parse redirect url: %v", err)
	}

	// The SP sends the AuthnRequest DEFLATE-compressed, per the HTTP-Redirect
	// binding it uses (SAML 2.0 Bindings 3.4.4.1). kayan-saml's own
	// IdentityProvider.HandleSSORequest only decodes plain base64, so the
	// query is rewritten here the way an HTTP-Redirect-aware IdP endpoint
	// would: inflate what the SP sent, then hand the IdP the raw XML the way
	// it expects it.
	rawXML, err := ksaml.ParseRedirectBinding(parsed.Query(), "SAMLRequest")
	if err != nil {
		t.Fatalf("inflate SAMLRequest: %v", err)
	}
	values := url.Values{
		"SAMLRequest": {base64.StdEncoding.EncodeToString(rawXML)},
		"RelayState":  {parsed.Query().Get("RelayState")},
	}

	req := httptest.NewRequest(http.MethodGet, "/sso?"+values.Encode(), nil)
	rec := httptest.NewRecorder()
	t2.idp.HandleSSORequest(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("idp HandleSSORequest: got %d, body %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	return extractFormValue(t, body, "SAMLResponse"), extractFormValue(t, body, "RelayState")
}

func extractFormValue(t *testing.T, body, name string) string {
	t.Helper()
	re := regexp.MustCompile(fmt.Sprintf(`name="%s" value="([^"]*)"`, name))
	m := re.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no %s field in IdP response form:\n%s", name, body)
	}
	return html.UnescapeString(m[1])
}

func generateTestCertificate(t *testing.T, commonName string) (*rsa.PrivateKey, *x509.Certificate) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	template := x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}
	return key, cert
}

func newSAMLTestService(t *testing.T, nameID string) (*auth.Service, *samlTestIdP) {
	t.Helper()
	testIdP, certPEM := newSAMLTestIdP(t, nameID)

	svc := newTestService(t)
	err := svc.InitSAML(context.Background(),
		auth.SAMLSPConfig{EntityID: samlSPEntityID, ACSUrl: samlSPACSUrl},
		map[string]auth.SAMLProviderConfig{
			"test-idp": {
				TenantID:    "acme",
				EntityID:    samlIdPEntityID,
				SSOUrl:      samlIdPSSOUrl,
				Certificate: certPEM,
			},
		},
	)
	if err != nil {
		t.Fatalf("InitSAML: %v", err)
	}
	return svc, testIdP
}

// TestSAML_EndToEndLoginProvisionsAccountAndIssuesSession: a first-time
// SAML sign-on creates an account in the IdP's configured tenant and issues
// a working session — no tenant parameter passed anywhere in the flow.
func TestSAML_EndToEndLoginProvisionsAccountAndIssuesSession(t *testing.T) {
	svc, idp := newSAMLTestService(t, "erin@example.com")
	ctx := context.Background()

	redirectURL, err := svc.InitiateSAMLLogin(ctx, "test-idp", "/dashboard")
	if err != nil {
		t.Fatalf("InitiateSAMLLogin: %v", err)
	}

	samlResponse, relayState := idp.signIn(t, redirectURL)

	result, err := svc.HandleSAMLResponse(ctx, samlResponse, relayState)
	if err != nil {
		t.Fatalf("HandleSAMLResponse: %v", err)
	}
	if result.AccessToken == "" {
		t.Fatal("no access token issued")
	}
	if result.TenantID != "acme" {
		t.Fatalf("tenant = %q, want acme", result.TenantID)
	}

	subject, tenantID, err := svc.ValidateForCaller(result.AccessToken)
	if err != nil {
		t.Fatalf("ValidateForCaller: %v", err)
	}
	if subject != result.Subject || tenantID != "acme" {
		t.Fatalf("validated (subject=%q, tenant=%q), want (%q, acme)", subject, tenantID, result.Subject)
	}
}

// TestSAML_SecondLoginFindsExistingAccount: the same NameID signing in
// again resolves to the same account, not a second one.
func TestSAML_SecondLoginFindsExistingAccount(t *testing.T) {
	svc, idp := newSAMLTestService(t, "erin@example.com")
	ctx := context.Background()

	redirectURL1, err := svc.InitiateSAMLLogin(ctx, "test-idp", "")
	if err != nil {
		t.Fatalf("InitiateSAMLLogin (1st): %v", err)
	}
	resp1, relay1 := idp.signIn(t, redirectURL1)
	first, err := svc.HandleSAMLResponse(ctx, resp1, relay1)
	if err != nil {
		t.Fatalf("HandleSAMLResponse (1st): %v", err)
	}

	redirectURL2, err := svc.InitiateSAMLLogin(ctx, "test-idp", "")
	if err != nil {
		t.Fatalf("InitiateSAMLLogin (2nd): %v", err)
	}
	resp2, relay2 := idp.signIn(t, redirectURL2)
	second, err := svc.HandleSAMLResponse(ctx, resp2, relay2)
	if err != nil {
		t.Fatalf("HandleSAMLResponse (2nd): %v", err)
	}

	if first.Subject != second.Subject {
		t.Fatalf("two logins by the same NameID produced different accounts: %q vs %q", first.Subject, second.Subject)
	}
}

// TestSAML_WrongSignatureIsRejected: an assertion signed by a key this
// deployment never configured must not authenticate anyone.
func TestSAML_WrongSignatureIsRejected(t *testing.T) {
	svc, idp := newSAMLTestService(t, "erin@example.com")
	ctx := context.Background()

	// Replace the running IdP's signing key with one the SP never trusted,
	// without changing anything else about the exchange.
	forgedKey, forgedCert := generateTestCertificate(t, "forged-idp")
	forgedIdP := ksaml.NewIdentityProvider(ksaml.IdPServerConfig{
		EntityID:    samlIdPEntityID,
		SSOUrl:      samlIdPSSOUrl,
		Certificate: forgedCert,
		PrivateKey:  forgedKey,
	}, nil, nil)
	forgedIdP.RegisterSP(&ksaml.SPRegistration{ID: samlSPEntityID, EntityID: samlSPEntityID, ACSUrl: samlSPACSUrl})
	forgedIdP.SetHooks(ksaml.IdPHooks{
		AuthenticateUser: func(context.Context, *http.Request) (any, error) { return struct{}{}, nil },
		GetNameID:        func(context.Context, any, *ksaml.SPRegistration) (string, error) { return "erin@example.com", nil },
	})
	forged := &samlTestIdP{idp: forgedIdP}

	redirectURL, err := svc.InitiateSAMLLogin(ctx, "test-idp", "")
	if err != nil {
		t.Fatalf("InitiateSAMLLogin: %v", err)
	}
	samlResponse, relayState := forged.signIn(t, redirectURL)

	if _, err := svc.HandleSAMLResponse(ctx, samlResponse, relayState); err == nil {
		t.Fatal("a response signed by an untrusted key was accepted")
	}
	_ = idp // the legitimate IdP is unused on this path; kept for symmetry with other tests
}

// TestSAML_UnconfiguredProviderIsRejected: naming an IdP nobody configured
// is refused before any redirect is produced.
func TestSAML_UnconfiguredProviderIsRejected(t *testing.T) {
	svc, _ := newSAMLTestService(t, "erin@example.com")
	if _, err := svc.InitiateSAMLLogin(context.Background(), "no-such-idp", ""); err == nil {
		t.Fatal("expected an error for an unconfigured provider")
	}
}

// TestSAML_LockedAccountCannotLogin: the account lock checked by password
// and OIDC login is enforced for SAML too — SSO is a second door into the
// same account, not a separate one.
func TestSAML_LockedAccountCannotLogin(t *testing.T) {
	svc, idp := newSAMLTestService(t, "erin@example.com")
	ctx := context.Background()

	redirectURL, err := svc.InitiateSAMLLogin(ctx, "test-idp", "")
	if err != nil {
		t.Fatalf("InitiateSAMLLogin: %v", err)
	}
	samlResponse, relayState := idp.signIn(t, redirectURL)
	first, err := svc.HandleSAMLResponse(ctx, samlResponse, relayState)
	if err != nil {
		t.Fatalf("HandleSAMLResponse: %v", err)
	}

	if err := svc.LockAccount(tenantCtx("acme"), first.Subject); err != nil {
		t.Fatalf("lock: %v", err)
	}

	redirectURL2, err := svc.InitiateSAMLLogin(ctx, "test-idp", "")
	if err != nil {
		t.Fatalf("InitiateSAMLLogin (2nd): %v", err)
	}
	samlResponse2, relayState2 := idp.signIn(t, redirectURL2)
	if _, err := svc.HandleSAMLResponse(ctx, samlResponse2, relayState2); !errors.Is(err, auth.ErrAccountLocked) {
		t.Fatalf("expected ErrAccountLocked, got %v", err)
	}
}
