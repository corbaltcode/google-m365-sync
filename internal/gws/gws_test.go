package gws

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestServiceAccountAsItselfOmitsSub(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	pk8, _ := x509.MarshalPKCS8PrivateKey(key)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			r.ParseForm()
			cb, _ := base64.RawURLEncoding.DecodeString(strings.Split(r.Form.Get("assertion"), ".")[1])
			var claims map[string]any
			json.Unmarshal(cb, &claims)
			if _, has := claims["sub"]; has {
				t.Errorf("sub must be omitted when acting as the service account: %v", claims)
			}
			w.Write([]byte(`{"access_token":"gtok","expires_in":3600}`))
			return
		}
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"error":{"code":403,"message":"Not Authorized to access this resource/api"}}`))
	}))
	defer srv.Close()
	keyJSON, _ := json.Marshal(map[string]string{
		"type":         "service_account",
		"client_email": "sync@proj.iam.gserviceaccount.com",
		"private_key":  string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pk8})),
		"token_uri":    srv.URL + "/token",
	})
	c, err := New(keyJSON, "")
	if err != nil {
		t.Fatal(err)
	}
	c.baseURL = srv.URL
	_, err = c.ListUsers(context.Background(), "corbalt.com")
	if err == nil || !strings.Contains(err.Error(), "admin role assigned to sync@proj.iam.gserviceaccount.com") {
		t.Fatalf("want role-assignment hint, got %v", err)
	}
}

func TestListUsersPaginatesWithDelegatedToken(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	pk8, _ := x509.MarshalPKCS8PrivateKey(key)
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			r.ParseForm()
			parts := strings.Split(r.Form.Get("assertion"), ".")
			cb, _ := base64.RawURLEncoding.DecodeString(parts[1])
			var claims map[string]any
			json.Unmarshal(cb, &claims)
			if claims["sub"] != "admin@corbalt.com" || claims["scope"] != DirectoryUserReadonlyScope ||
				claims["iss"] != "sync@proj.iam.gserviceaccount.com" {
				t.Errorf("claims = %v", claims)
			}
			w.Write([]byte(`{"access_token":"gtok","expires_in":3600}`))
		case "/admin/directory/v1/users":
			if r.Header.Get("Authorization") != "Bearer gtok" || r.URL.Query().Get("domain") != "corbalt.com" {
				t.Errorf("bad request %s %v", r.URL, r.Header)
			}
			if r.URL.Query().Get("pageToken") == "" {
				w.Write([]byte(`{"users":[{"primaryEmail":"a@corbalt.com","name":{"fullName":"A"}}],"nextPageToken":"p2"}`))
			} else {
				w.Write([]byte(`{"users":[{"primaryEmail":"b@corbalt.com","suspended":true,
					"phones":[{"value":"1","type":"work"},{"value":"2","type":"work","primary":true}]}]}`))
			}
		default:
			t.Errorf("unexpected %s", r.URL)
		}
	}))
	defer srv.Close()

	keyJSON, _ := json.Marshal(map[string]string{
		"type":         "service_account",
		"client_email": "sync@proj.iam.gserviceaccount.com",
		"private_key":  string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pk8})),
		"token_uri":    srv.URL + "/token",
	})
	c, err := New(keyJSON, "admin@corbalt.com")
	if err != nil {
		t.Fatal(err)
	}
	c.baseURL = srv.URL
	users, err := c.ListUsers(context.Background(), "corbalt.com")
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 2 || !users[1].Suspended || users[1].WorkPhone() != "2" || users[0].DisplayName() != "A" {
		t.Fatalf("users = %+v", users)
	}
}
