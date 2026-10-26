// Package gws reads users from the Google Workspace Admin SDK Directory API
// using a service account. Tokens come from golang.org/x/oauth2; the one
// Directory call is plain REST.
//
// Preferred: the service account itself is assigned a Workspace admin role
// (Account > Admin roles > Assign service accounts) and calls the API as
// itself — no domain-wide delegation, no impersonated user. Legacy: set a
// subject to impersonate that admin via domain-wide delegation.
package gws

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"golang.org/x/oauth2/jwt"

	"github.com/corbaltcode/google-m365-sync/internal/httpx"
)

// DirectoryUserReadonlyScope is the only OAuth scope the sync requests.
const DirectoryUserReadonlyScope = "https://www.googleapis.com/auth/admin.directory.user.readonly"

// User is the subset of the Directory API user resource we sync.
type User struct {
	ID           string `json:"id"`
	PrimaryEmail string `json:"primaryEmail"`
	Name         struct {
		GivenName   string `json:"givenName"`
		FamilyName  string `json:"familyName"`
		FullName    string `json:"fullName"`
		DisplayName string `json:"displayName"`
	} `json:"name"`
	Suspended     bool `json:"suspended"`
	Archived      bool `json:"archived"`
	Organizations []struct {
		Title      string `json:"title"`
		Department string `json:"department"`
		Primary    bool   `json:"primary"`
	} `json:"organizations"`
	Phones []struct {
		Value   string `json:"value"`
		Type    string `json:"type"`
		Primary bool   `json:"primary"`
	} `json:"phones"`
}

// Client lists Directory users.
type Client struct {
	conf    *jwt.Config
	http    *http.Client
	baseURL string
}

// New parses a service-account JSON key. Leave subject empty when the
// service account holds an admin role directly (recommended); otherwise it
// is the Workspace admin impersonated through domain-wide delegation.
func New(keyJSON []byte, subject string) (*Client, error) {
	conf, err := google.JWTConfigFromJSON(keyJSON, DirectoryUserReadonlyScope)
	if err != nil {
		return nil, fmt.Errorf("parse service account key: %w", err)
	}
	conf.Subject = subject
	return &Client{
		conf:    conf,
		http:    &http.Client{Timeout: 60 * time.Second},
		baseURL: "https://admin.googleapis.com",
	}, nil
}

// tokenFunc adapts an oauth2.TokenSource to httpx.Client.Token.
func (c *Client) tokenFunc(ts oauth2.TokenSource) func(context.Context) (string, error) {
	return func(context.Context) (string, error) {
		tok, err := ts.Token()
		if err != nil {
			return "", fmt.Errorf("google token for %s: %w", c.conf.Email, err)
		}
		return tok.AccessToken, nil
	}
}

// ListUsers returns every user (active, suspended and archived) in domain.
func (c *Client) ListUsers(ctx context.Context, domain string) ([]User, error) {
	ctx = context.WithValue(ctx, oauth2.HTTPClient, c.http)
	hc := &httpx.Client{HTTP: c.http, Token: c.tokenFunc(c.conf.TokenSource(ctx))}
	var all []User
	pageToken := ""
	for {
		q := url.Values{
			"domain":     {domain},
			"maxResults": {"500"},
			"projection": {"basic"},
			"orderBy":    {"email"},
			"fields":     {"nextPageToken,users(id,primaryEmail,name,suspended,archived,organizations,phones)"},
		}
		if pageToken != "" {
			q.Set("pageToken", pageToken)
		}
		var page struct {
			Users         []User `json:"users"`
			NextPageToken string `json:"nextPageToken"`
		}
		if err := hc.DoJSON(ctx, http.MethodGet, c.baseURL+"/admin/directory/v1/users?"+q.Encode(), nil, &page); err != nil {
			var se *httpx.StatusError
			if errors.As(err, &se) && se.Status == http.StatusForbidden && c.conf.Subject == "" {
				return nil, fmt.Errorf("list google users: %w (is the admin role assigned to %s?)", err, c.conf.Email)
			}
			return nil, fmt.Errorf("list google users: %w", err)
		}
		all = append(all, page.Users...)
		if page.NextPageToken == "" {
			return all, nil
		}
		pageToken = page.NextPageToken
	}
}

// DisplayName prefers the Workspace display name, then full name, then
// given + family.
func (u User) DisplayName() string {
	if s := strings.TrimSpace(u.Name.DisplayName); s != "" {
		return s
	}
	if s := strings.TrimSpace(u.Name.FullName); s != "" {
		return s
	}
	return strings.TrimSpace(u.Name.GivenName + " " + u.Name.FamilyName)
}

// PrimaryOrg returns title and department from the primary (else first)
// organization entry.
func (u User) PrimaryOrg() (title, dept string) {
	for _, o := range u.Organizations {
		if o.Primary {
			return strings.TrimSpace(o.Title), strings.TrimSpace(o.Department)
		}
	}
	if len(u.Organizations) > 0 {
		return strings.TrimSpace(u.Organizations[0].Title), strings.TrimSpace(u.Organizations[0].Department)
	}
	return "", ""
}

// WorkPhone returns the primary work phone, else the first work phone.
// Entra's businessPhones accepts only one value.
func (u User) WorkPhone() string {
	first := ""
	for _, p := range u.Phones {
		if p.Type != "work" || strings.TrimSpace(p.Value) == "" {
			continue
		}
		if p.Primary {
			return strings.TrimSpace(p.Value)
		}
		if first == "" {
			first = strings.TrimSpace(p.Value)
		}
	}
	return first
}
