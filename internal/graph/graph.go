// Package graph is a minimal Microsoft Graph client for user objects,
// authenticating as an app registration with a certificate credential.
//
// Authentication uses Microsoft's MSAL library. The three Graph calls are
// plain REST rather than msgraph-sdk-go, which is very large for three
// endpoints.
package graph

import (
	"context"
	"crypto"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/AzureAD/microsoft-authentication-library-for-go/apps/confidential"

	"github.com/corbaltcode/google-m365-sync/internal/httpx"
)

const userSelect = "id,userPrincipalName,displayName,givenName,surname,jobTitle,department," +
	"businessPhones,accountEnabled,onPremisesImmutableId,onPremisesSyncEnabled,userType"

var graphScopes = []string{"https://graph.microsoft.com/.default"}

// User is the subset of the Graph user resource we read.
type User struct {
	ID                    string   `json:"id"`
	UserPrincipalName     string   `json:"userPrincipalName"`
	DisplayName           string   `json:"displayName"`
	GivenName             string   `json:"givenName"`
	Surname               string   `json:"surname"`
	JobTitle              string   `json:"jobTitle"`
	Department            string   `json:"department"`
	BusinessPhones        []string `json:"businessPhones"`
	AccountEnabled        bool     `json:"accountEnabled"`
	OnPremisesImmutableID string   `json:"onPremisesImmutableId"`
	OnPremisesSyncEnabled *bool    `json:"onPremisesSyncEnabled"`
	UserType              string   `json:"userType"`
}

// Client talks to Graph v1.0.
type Client struct {
	app     confidential.Client
	http    *http.Client
	baseURL string
}

// New builds a client from a PEM bundle holding the certificate and its
// private key (the format `openssl req -x509 ... -keyout k -out c; cat c k` produces).
func New(tenantID, clientID string, certAndKeyPEM []byte) (*Client, error) {
	return newClient("https://login.microsoftonline.com/"+url.PathEscape(tenantID), clientID, certAndKeyPEM,
		&http.Client{Timeout: 60 * time.Second})
}

func newClient(authority, clientID string, certAndKeyPEM []byte, hc *http.Client, opts ...confidential.Option) (*Client, error) {
	certs, key, err := confidential.CertFromPEM(certAndKeyPEM, "")
	if err != nil {
		return nil, fmt.Errorf("parse entra certificate bundle: %w", err)
	}
	if !publicKeyMatches(certs[0], key) {
		return nil, errors.New("entra certificate and private key do not match")
	}
	cred, err := confidential.NewCredFromCert(certs, key)
	if err != nil {
		return nil, err
	}
	opts = append([]confidential.Option{confidential.WithHTTPClient(hc)}, opts...)
	app, err := confidential.New(authority, clientID, cred, opts...)
	if err != nil {
		return nil, fmt.Errorf("entra client: %w", err)
	}
	return &Client{app: app, http: hc, baseURL: "https://graph.microsoft.com/v1.0"}, nil
}

func publicKeyMatches(cert *x509.Certificate, key crypto.PrivateKey) bool {
	k, ok := key.(interface{ Public() crypto.PublicKey })
	if !ok {
		return false
	}
	pub, ok := cert.PublicKey.(interface{ Equal(crypto.PublicKey) bool })
	return ok && pub.Equal(k.Public())
}

// accessToken returns a cached app token, fetching a new one when needed.
func (c *Client) accessToken(ctx context.Context) (string, error) {
	r, err := c.app.AcquireTokenSilent(ctx, graphScopes)
	if err != nil {
		if r, err = c.app.AcquireTokenByCredential(ctx, graphScopes); err != nil {
			return "", fmt.Errorf("entra token: %w", err)
		}
	}
	return r.AccessToken, nil
}

func (c *Client) api() *httpx.Client {
	return &httpx.Client{HTTP: c.http, Token: c.accessToken}
}

// ListUsers returns all users in the tenant (members and guests).
func (c *Client) ListUsers(ctx context.Context) ([]User, error) {
	next := c.baseURL + "/users?$top=999&$select=" + userSelect
	var all []User
	for next != "" {
		var page struct {
			Value    []User `json:"value"`
			NextLink string `json:"@odata.nextLink"`
		}
		if err := c.api().DoJSON(ctx, http.MethodGet, next, nil, &page); err != nil {
			return nil, fmt.Errorf("list entra users: %w", WrapError(err))
		}
		all = append(all, page.Value...)
		next = page.NextLink
	}
	return all, nil
}

// CreateUser POSTs a new user and returns its object ID.
func (c *Client) CreateUser(ctx context.Context, body map[string]any) (string, error) {
	var out struct {
		ID string `json:"id"`
	}
	if err := c.api().DoJSON(ctx, http.MethodPost, c.baseURL+"/users", body, &out); err != nil {
		return "", WrapError(err)
	}
	return out.ID, nil
}

// UpdateUser PATCHes the given properties.
func (c *Client) UpdateUser(ctx context.Context, id string, patch map[string]any) error {
	return WrapError(c.api().DoJSON(ctx, http.MethodPatch, c.baseURL+"/users/"+url.PathEscape(id), patch, nil))
}

// Error is a decoded Graph error response.
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("graph HTTP %d %s: %s", e.Status, e.Code, e.Message)
}

// WrapError turns an httpx.StatusError carrying a Graph error body into
// *Error so logs show e.g. "SourceAnchor is a required property..." instead
// of a bare BadRequest.
func WrapError(err error) error {
	var se *httpx.StatusError
	if !errors.As(err, &se) {
		return err
	}
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(se.Body), &body) != nil || body.Error.Code == "" {
		return err
	}
	return &Error{Status: se.Status, Code: body.Error.Code, Message: body.Error.Message}
}
