package graph

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/AzureAD/microsoft-authentication-library-for-go/apps/confidential"
)

func testCertPEM(t *testing.T) ([]byte, *rsa.PrivateKey) {
	t.Helper()
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	pk8, _ := x509.MarshalPKCS8PrivateKey(key)
	out := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	out = append(out, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pk8})...)
	return out, key
}

func TestTokenAssertionAndGraphErrorDecoding(t *testing.T) {
	certPEM, key := testCertPEM(t)
	var creates, tokens int
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/tenant-id/v2.0/.well-known/openid-configuration":
			json.NewEncoder(w).Encode(map[string]any{
				"token_endpoint":         srv.URL + "/tenant-id/oauth2/v2.0/token",
				"authorization_endpoint": srv.URL + "/tenant-id/oauth2/v2.0/authorize",
				"issuer":                 srv.URL + "/tenant-id/v2.0",
			})
		case r.URL.Path == "/tenant-id/oauth2/v2.0/token":
			tokens++
			r.ParseForm()
			verifyAssertion(t, r.Form, key, srv.URL+r.URL.Path)
			json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600, "token_type": "Bearer"})
		case r.URL.Path == "/v1.0/users" && r.Method == http.MethodPost:
			creates++
			if r.Header.Get("Authorization") != "Bearer tok" {
				t.Errorf("auth header = %q", r.Header.Get("Authorization"))
			}
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":{"code":"Request_BadRequest","message":"SourceAnchor is a required property for creation of a federated user."}}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
		}
	}))
	defer srv.Close()

	c, err := newClient(srv.URL+"/tenant-id", "client-id", certPEM, srv.Client(),
		confidential.WithInstanceDiscovery(false))
	if err != nil {
		t.Fatal(err)
	}
	c.baseURL = srv.URL + "/v1.0"

	for range 2 {
		_, err = c.CreateUser(context.Background(), map[string]any{"userPrincipalName": "x@corbalt.com"})
		var ge *Error
		if !errors.As(err, &ge) || ge.Status != 400 || !strings.Contains(ge.Message, "SourceAnchor") {
			t.Fatalf("want decoded Graph error, got %v", err)
		}
	}
	if creates != 2 {
		t.Errorf("400 must not be retried; creates = %d, want 2", creates)
	}
	if tokens != 1 {
		t.Errorf("token not cached; token requests = %d, want 1", tokens)
	}
}

func verifyAssertion(t *testing.T, form url.Values, key *rsa.PrivateKey, aud string) {
	t.Helper()
	if form.Get("grant_type") != "client_credentials" || form.Get("client_id") != "client-id" {
		t.Errorf("bad form: %v", form)
	}
	parts := strings.Split(form.Get("client_assertion"), ".")
	if len(parts) != 3 {
		t.Fatalf("assertion not a JWT")
	}
	var hdr, claims map[string]any
	hb, _ := base64.RawURLEncoding.DecodeString(parts[0])
	cb, _ := base64.RawURLEncoding.DecodeString(parts[1])
	json.Unmarshal(hb, &hdr)
	json.Unmarshal(cb, &claims)
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	var err error
	switch hdr["alg"] {
	case "RS256":
		err = rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, sum[:], sig)
	case "PS256":
		err = rsa.VerifyPSS(&key.PublicKey, crypto.SHA256, sum[:], sig, nil)
	default:
		t.Fatalf("unexpected alg in header %v", hdr)
	}
	if err != nil {
		t.Errorf("assertion signature invalid: %v", err)
	}
	if hdr["x5t"] == nil && hdr["x5t#S256"] == nil {
		t.Errorf("no certificate thumbprint in header %v", hdr)
	}
	if claims["aud"] != aud || claims["iss"] != "client-id" || claims["sub"] != "client-id" {
		t.Errorf("claims = %v (aud want %s)", claims, aud)
	}
}

func TestMismatchedKeyRejected(t *testing.T) {
	certPEM, _ := testCertPEM(t)
	other, _ := testCertPEM(t)
	// cert from one pair + key from another
	certOnly := certPEM[:strings.Index(string(certPEM), "-----BEGIN PRIVATE KEY")]
	keyOnly := other[strings.Index(string(other), "-----BEGIN PRIVATE KEY"):]
	if _, err := New("t", "c", append([]byte(certOnly), keyOnly...)); err == nil {
		t.Fatal("expected mismatch error")
	}
}
