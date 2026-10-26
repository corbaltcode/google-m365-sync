// Package syncer computes and applies the changes needed to make Entra users
// in a federated domain match Google Workspace.
//
// Matching: a Google user matches an Entra user whose userPrincipalName or
// onPremisesImmutableId equals the Google primary email (case-insensitive).
// Google federation sends the primary email as the SAML NameID, and Entra
// matches that against onPremisesImmutableId (the "SourceAnchor"), so every
// user this tool creates gets onPremisesImmutableId = primary email.
package syncer

import (
	"fmt"
	"sort"
	"strings"

	"github.com/corbaltcode/google-m365-sync/internal/graph"
	"github.com/corbaltcode/google-m365-sync/internal/gws"
)

// Config controls planning.
type Config struct {
	Domain        string          // e.g. "corbalt.com"; only users in this domain are considered
	UsageLocation string          // ISO country code set on created users (needed for licensing)
	Exclude       map[string]bool // lowercase UPNs never modified (break-glass, service accounts)
	MaxDisables   int             // safety valve; more planned disables than this blocks all disables
}

type ActionKind string

const (
	Create  ActionKind = "create"
	Update  ActionKind = "update"
	Disable ActionKind = "disable" // Entra user has no Google counterpart
)

// Action is one planned change.
type Action struct {
	Kind    ActionKind     `json:"kind"`
	UPN     string         `json:"upn"`
	EntraID string         `json:"entraId,omitempty"`
	Body    map[string]any `json:"body,omitempty"` // POST body (create, minus password) or PATCH body (update/disable)
	Reason  string         `json:"reason,omitempty"`
}

// Plan is the full result of comparing both directories.
type Plan struct {
	Actions  []Action `json:"actions"`
	Warnings []string `json:"warnings,omitempty"`
	// DisablesBlocked is set when the disable count exceeded MaxDisables; the
	// disables are then dropped from Actions and listed here instead, and
	// DisablesBlockedReason explains why. The run fails with that reason.
	DisablesBlocked       []string `json:"disablesBlocked,omitempty"`
	DisablesBlockedReason string   `json:"disablesBlockedReason,omitempty"`
	GoogleUsers           int      `json:"googleUsers"`
	EntraUsers            int      `json:"entraUsers"`
}

// Counts summarizes the actions by kind.
func (p Plan) Counts() map[ActionKind]int {
	m := map[ActionKind]int{}
	for _, a := range p.Actions {
		m[a.Kind]++
	}
	return m
}

func inDomain(email, domain string) bool {
	return strings.HasSuffix(strings.ToLower(email), "@"+strings.ToLower(domain))
}

// BuildPlan is pure: no I/O, so it is fully unit-testable.
func BuildPlan(cfg Config, gUsers []gws.User, eUsers []graph.User) (Plan, error) {
	var plan Plan

	idx := indexEntra(cfg, eUsers)
	candidates := idx.candidates
	plan.EntraUsers = len(candidates)
	plan.Warnings = append(plan.Warnings, idx.duplicateWarnings()...)

	// Pass 1: every Entra user each Google email could refer to, and how many
	// Google emails claim each Entra user. Only a one-to-one match is ever
	// written; anything else is reported and left alone.
	type googleEntry struct {
		g     gws.User
		email string
		cands []*graph.User
	}
	var entries []googleEntry
	claimedBy := map[*graph.User][]string{}
	for _, g := range gUsers {
		email := strings.ToLower(strings.TrimSpace(g.PrimaryEmail))
		if !inDomain(email, cfg.Domain) {
			continue
		}
		plan.GoogleUsers++
		cands := idx.candidatesFor(email)
		for _, c := range cands {
			claimedBy[c] = append(claimedBy[c], email)
		}
		entries = append(entries, googleEntry{g, email, cands})
	}
	var conflicts []string
	for e, emails := range claimedBy {
		if len(emails) > 1 {
			sort.Strings(emails)
			conflicts = append(conflicts, fmt.Sprintf(
				"Entra user %s is matched by more than one Google user (%s) by UPN or immutable ID; not modifying it",
				e.UserPrincipalName, strings.Join(emails, ", ")))
		}
	}
	sort.Strings(conflicts)
	plan.Warnings = append(plan.Warnings, conflicts...)

	// Pass 2: plan changes for unambiguous matches. Every Entra user that any
	// Google email refers to counts as matched, so it is never disabled as
	// missing from Google.
	matched := map[*graph.User]bool{}
	for _, entry := range entries {
		g, email := entry.g, entry.email
		for _, c := range entry.cands {
			matched[c] = true
		}
		if len(entry.cands) > 1 {
			var upns []string
			for _, c := range entry.cands {
				upns = append(upns, c.UserPrincipalName)
			}
			plan.Warnings = append(plan.Warnings, fmt.Sprintf(
				"%s matches more than one Entra user (%s) by UPN or immutable ID; not modifying any of them",
				email, strings.Join(upns, ", ")))
			continue
		}
		if cfg.Exclude[email] {
			continue
		}
		var e *graph.User
		if len(entry.cands) == 1 {
			e = entry.cands[0]
			if len(claimedBy[e]) > 1 {
				continue // reported above
			}
		}
		if e == nil {
			if g.Suspended || g.Archived {
				continue // nothing to create for an inactive account
			}
			if g.DisplayName() == "" {
				plan.Warnings = append(plan.Warnings, fmt.Sprintf("%s has no name in Google; skipped", email))
				continue
			}
			plan.Actions = append(plan.Actions, Action{
				Kind: Create, UPN: email, Body: createBody(cfg, g, email),
			})
			continue
		}
		if cfg.Exclude[strings.ToLower(e.UserPrincipalName)] {
			continue
		}
		if e.OnPremisesSyncEnabled != nil && *e.OnPremisesSyncEnabled {
			plan.Warnings = append(plan.Warnings, fmt.Sprintf(
				"%s is synced from on-prem AD (onPremisesSyncEnabled); not modifying", e.UserPrincipalName))
			continue
		}
		patch, reasons := diff(g, e, email)
		if imm := e.OnPremisesImmutableID; imm != "" && !strings.EqualFold(imm, email) {
			plan.Warnings = append(plan.Warnings, fmt.Sprintf(
				"%s has immutable ID %q, not %q; federated sign-in may fail. Not changing it automatically.",
				e.UserPrincipalName, imm, email))
		}
		if len(patch) > 0 {
			plan.Actions = append(plan.Actions, Action{
				Kind: Update, UPN: e.UserPrincipalName, EntraID: e.ID, Body: patch,
				Reason: strings.Join(reasons, ", "),
			})
		}
	}

	if plan.GoogleUsers == 0 {
		return plan, fmt.Errorf("google returned no users in %s; refusing to plan (would disable everyone)", cfg.Domain)
	}

	var disables []Action
	for _, e := range candidates {
		if matched[e] || !e.AccountEnabled || cfg.Exclude[strings.ToLower(e.UserPrincipalName)] {
			continue
		}
		if e.OnPremisesSyncEnabled != nil && *e.OnPremisesSyncEnabled {
			continue
		}
		disables = append(disables, Action{
			Kind: Disable, UPN: e.UserPrincipalName, EntraID: e.ID,
			Body: map[string]any{"accountEnabled": false}, Reason: "not found in Google Workspace",
		})
	}
	if len(disables) > cfg.MaxDisables {
		for _, d := range disables {
			plan.DisablesBlocked = append(plan.DisablesBlocked, d.UPN)
		}
		plan.DisablesBlockedReason = fmt.Sprintf(
			"%d Entra users have no Google counterpart, above MAX_DISABLES=%d; no disables applied. "+
				"Add intentional ones to EXCLUDE_UPNS or raise MAX_DISABLES.", len(disables), cfg.MaxDisables)
		plan.Warnings = append(plan.Warnings, plan.DisablesBlockedReason)
	} else {
		plan.Actions = append(plan.Actions, disables...)
	}

	sort.SliceStable(plan.Actions, func(i, j int) bool {
		if plan.Actions[i].Kind != plan.Actions[j].Kind {
			return plan.Actions[i].Kind < plan.Actions[j].Kind
		}
		return plan.Actions[i].UPN < plan.Actions[j].UPN
	})
	return plan, nil
}

// entraIndex finds the Entra users a Google email could refer to. Immutable
// IDs and UPNs are kept in separate maps so the result doesn't depend on the
// order Graph returns users.
type entraIndex struct {
	candidates  []*graph.User
	byUPN       map[string]*graph.User
	byImmutable map[string][]*graph.User
}

func indexEntra(cfg Config, eUsers []graph.User) entraIndex {
	idx := entraIndex{byUPN: map[string]*graph.User{}, byImmutable: map[string][]*graph.User{}}
	for i := range eUsers {
		u := &eUsers[i]
		if !strings.EqualFold(u.UserType, "Member") || !inDomain(u.UserPrincipalName, cfg.Domain) {
			continue
		}
		idx.candidates = append(idx.candidates, u)
		idx.byUPN[strings.ToLower(u.UserPrincipalName)] = u
		if id := strings.ToLower(u.OnPremisesImmutableID); id != "" {
			idx.byImmutable[id] = append(idx.byImmutable[id], u)
		}
	}
	return idx
}

// duplicateWarnings reports immutable IDs held by more than one user, in a
// stable order.
func (idx entraIndex) duplicateWarnings() []string {
	var out []string
	for id, users := range idx.byImmutable {
		if len(users) < 2 {
			continue
		}
		var upns []string
		for _, u := range users {
			upns = append(upns, u.UserPrincipalName)
		}
		sort.Strings(upns)
		out = append(out, fmt.Sprintf("immutable ID %q is on more than one user: %s", id, strings.Join(upns, ", ")))
	}
	sort.Strings(out)
	return out
}

// candidatesFor returns every Entra user the email could refer to: those
// whose immutable ID is the email, plus the one whose UPN is the email,
// deduplicated and sorted by UPN. More than one means the email is ambiguous.
func (idx entraIndex) candidatesFor(email string) []*graph.User {
	var out []*graph.User
	seen := map[*graph.User]bool{}
	add := func(u *graph.User) {
		if u != nil && !seen[u] {
			seen[u] = true
			out = append(out, u)
		}
	}
	for _, u := range idx.byImmutable[email] {
		add(u)
	}
	add(idx.byUPN[email])
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i].UserPrincipalName) < strings.ToLower(out[j].UserPrincipalName)
	})
	return out
}

// createBody omits passwordProfile; Apply adds it just before sending so the
// plan, which is logged and printed, never holds a password.
func createBody(cfg Config, g gws.User, email string) map[string]any {
	title, dept := g.PrimaryOrg()
	b := map[string]any{
		"accountEnabled":        true,
		"userPrincipalName":     email,
		"mailNickname":          MailNickname(email),
		"displayName":           g.DisplayName(),
		"onPremisesImmutableId": email,
	}
	setIf(b, "givenName", strings.TrimSpace(g.Name.GivenName))
	setIf(b, "surname", strings.TrimSpace(g.Name.FamilyName))
	setIf(b, "jobTitle", title)
	setIf(b, "department", dept)
	setIf(b, "usageLocation", cfg.UsageLocation)
	if p := g.WorkPhone(); p != "" {
		b["businessPhones"] = []string{p}
	}
	return b
}

// diff returns the PATCH body that brings e in line with g. Attributes that
// are empty in Google are left alone rather than cleared, so values set
// directly in Entra (e.g. department) survive.
func diff(g gws.User, e *graph.User, email string) (map[string]any, []string) {
	patch := map[string]any{}
	var reasons []string
	set := func(field, want, have string) {
		if want != "" && want != have {
			patch[field] = want
			reasons = append(reasons, field)
		}
	}
	title, dept := g.PrimaryOrg()
	set("displayName", g.DisplayName(), e.DisplayName)
	set("givenName", strings.TrimSpace(g.Name.GivenName), e.GivenName)
	set("surname", strings.TrimSpace(g.Name.FamilyName), e.Surname)
	set("jobTitle", title, e.JobTitle)
	set("department", dept, e.Department)
	if p := g.WorkPhone(); p != "" && (len(e.BusinessPhones) != 1 || e.BusinessPhones[0] != p) {
		patch["businessPhones"] = []string{p}
		reasons = append(reasons, "businessPhones")
	}
	if want := !(g.Suspended || g.Archived); want != e.AccountEnabled {
		patch["accountEnabled"] = want
		if want {
			reasons = append(reasons, "accountEnabled (unsuspended in Google)")
		} else {
			reasons = append(reasons, "accountEnabled (suspended/archived in Google)")
		}
	}
	if e.OnPremisesImmutableID == "" {
		patch["onPremisesImmutableId"] = email
		reasons = append(reasons, "onPremisesImmutableId (was empty)")
	}
	return patch, reasons
}

func setIf(m map[string]any, k, v string) {
	if v != "" {
		m[k] = v
	}
}

// MailNickname derives a Graph-valid mailNickname from the email local part.
func MailNickname(email string) string {
	local := email
	if i := strings.IndexByte(email, '@'); i >= 0 {
		local = email[:i]
	}
	var b strings.Builder
	for _, r := range local {
		if r > 127 || strings.ContainsRune(`@()\[]";:<>, `, r) {
			continue
		}
		b.WriteRune(r)
	}
	if b.Len() == 0 {
		return "user"
	}
	return b.String()
}
