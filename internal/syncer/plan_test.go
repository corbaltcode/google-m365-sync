package syncer

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/corbaltcode/google-m365-sync/internal/graph"
	"github.com/corbaltcode/google-m365-sync/internal/gws"
)

func gUser(t *testing.T, js string) gws.User {
	t.Helper()
	var u gws.User
	if err := json.Unmarshal([]byte(js), &u); err != nil {
		t.Fatal(err)
	}
	return u
}

var cfg = Config{Domain: "corbalt.com", UsageLocation: "US", Exclude: map[string]bool{}, MaxDisables: 3}

func find(p Plan, kind ActionKind, upn string) *Action {
	for i := range p.Actions {
		if p.Actions[i].Kind == kind && strings.EqualFold(p.Actions[i].UPN, upn) {
			return &p.Actions[i]
		}
	}
	return nil
}

func TestCreateSetsImmutableIDAndDisplayName(t *testing.T) {
	g := []gws.User{gUser(t, `{"primaryEmail":"Fiona.Catcher@corbalt.com",
		"name":{"givenName":"Fiona","familyName":"Catcher","fullName":"Fiona Catcher"},
		"organizations":[{"title":"Engineer","department":"SPOC","primary":true}],
		"phones":[{"value":"206-555-0100","type":"home"},{"value":"206-555-0199","type":"work"}]}`)}
	p, err := BuildPlan(cfg, g, nil)
	if err != nil {
		t.Fatal(err)
	}
	a := find(p, Create, "fiona.catcher@corbalt.com")
	if a == nil {
		t.Fatalf("no create: %+v", p.Actions)
	}
	want := map[string]any{
		"userPrincipalName": "fiona.catcher@corbalt.com", "onPremisesImmutableId": "fiona.catcher@corbalt.com",
		"displayName": "Fiona Catcher", "mailNickname": "fiona.catcher", "givenName": "Fiona",
		"surname": "Catcher", "jobTitle": "Engineer", "department": "SPOC", "usageLocation": "US",
		"accountEnabled": true,
	}
	for k, v := range want {
		if a.Body[k] != v {
			t.Errorf("%s = %v, want %v", k, a.Body[k], v)
		}
	}
	if ph := a.Body["businessPhones"].([]string); len(ph) != 1 || ph[0] != "206-555-0199" {
		t.Errorf("businessPhones = %v (home phone must not map)", ph)
	}
}

func TestMatchesByUPNOrImmutableIDAndSkipsOtherDomains(t *testing.T) {
	g := []gws.User{
		gUser(t, `{"primaryEmail":"dave@corbalt.com","name":{"fullName":"Dave Cuthbert"}}`),
		gUser(t, `{"primaryEmail":"renamed@corbalt.com","name":{"fullName":"R"}}`),
		gUser(t, `{"primaryEmail":"x@superbrilliant.io","name":{"fullName":"X"}}`),
	}
	e := []graph.User{
		{ID: "1", UserPrincipalName: "dave@corbalt.com", DisplayName: "Dave Cuthbert", AccountEnabled: true,
			OnPremisesImmutableID: "dave@corbalt.com", UserType: "Member"},
		{ID: "2", UserPrincipalName: "old@corbalt.com", DisplayName: "R", AccountEnabled: true,
			OnPremisesImmutableID: "renamed@corbalt.com", UserType: "Member"},
	}
	p, err := BuildPlan(cfg, g, e)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Actions) != 0 {
		t.Fatalf("expected no actions, got %+v", p.Actions)
	}
	if p.GoogleUsers != 2 {
		t.Errorf("GoogleUsers = %d, want 2 (other domain excluded)", p.GoogleUsers)
	}
}

func TestUpdateDoesNotClearEntraOnlyValues(t *testing.T) {
	g := []gws.User{gUser(t, `{"primaryEmail":"dave@corbalt.com",
		"name":{"givenName":"Dave","familyName":"Cuthbert","fullName":"Dave Cuthbert"},
		"organizations":[{"title":"VP Engineering"}]}`)}
	e := []graph.User{{ID: "1", UserPrincipalName: "dave@corbalt.com", DisplayName: "Dave Cuthbert",
		GivenName: "Dave", Surname: "Cuthbert", Department: "Engineering", AccountEnabled: true,
		OnPremisesImmutableID: "dave@corbalt.com", UserType: "Member"}}
	p, _ := BuildPlan(cfg, g, e)
	a := find(p, Update, "dave@corbalt.com")
	if a == nil || len(a.Body) != 1 || a.Body["jobTitle"] != "VP Engineering" {
		t.Fatalf("want only jobTitle patch, got %+v", a)
	}
}

func TestSuspendedAndUnsuspended(t *testing.T) {
	g := []gws.User{
		gUser(t, `{"primaryEmail":"a@corbalt.com","name":{"fullName":"A"},"suspended":true}`),
		gUser(t, `{"primaryEmail":"b@corbalt.com","name":{"fullName":"B"}}`),
		gUser(t, `{"primaryEmail":"c@corbalt.com","name":{"fullName":"C"},"archived":true}`),
	}
	e := []graph.User{
		{ID: "a", UserPrincipalName: "a@corbalt.com", DisplayName: "A", AccountEnabled: true, OnPremisesImmutableID: "a@corbalt.com", UserType: "Member"},
		{ID: "b", UserPrincipalName: "b@corbalt.com", DisplayName: "B", AccountEnabled: false, OnPremisesImmutableID: "b@corbalt.com", UserType: "Member"},
	}
	p, _ := BuildPlan(cfg, g, e)
	if a := find(p, Update, "a@corbalt.com"); a == nil || a.Body["accountEnabled"] != false {
		t.Errorf("suspended user not disabled: %+v", a)
	}
	if b := find(p, Update, "b@corbalt.com"); b == nil || b.Body["accountEnabled"] != true {
		t.Errorf("active user not re-enabled: %+v", b)
	}
	if find(p, Create, "c@corbalt.com") != nil {
		t.Errorf("archived user should not be created")
	}
}

func TestDisableMissingWithSafetyValveAndExclusions(t *testing.T) {
	g := []gws.User{gUser(t, `{"primaryEmail":"keep@corbalt.com","name":{"fullName":"K"}}`)}
	mk := func(upn string) graph.User {
		return graph.User{ID: upn, UserPrincipalName: upn, DisplayName: upn, AccountEnabled: true,
			OnPremisesImmutableID: upn, UserType: "Member"}
	}
	e := []graph.User{
		mk("keep@corbalt.com"), mk("gone@corbalt.com"), mk("breakglass@corbalt.com"),
		{ID: "g", UserPrincipalName: "guest_x#EXT#@corbalt.com", AccountEnabled: true, UserType: "Guest"},
		mk("other@superbrilliant.io"),
	}
	c := cfg
	c.Exclude = map[string]bool{"breakglass@corbalt.com": true}
	p, _ := BuildPlan(c, g, e)
	if find(p, Disable, "gone@corbalt.com") == nil {
		t.Errorf("missing user not disabled: %+v", p.Actions)
	}
	if n := p.Counts()[Disable]; n != 1 {
		t.Errorf("disables = %d, want 1 (excluded, guest, other-domain untouched)", n)
	}

	// Exceed the valve.
	for i := 0; i < 4; i++ {
		e = append(e, mk(string(rune('p'+i))+"@corbalt.com"))
	}
	p, _ = BuildPlan(c, g, e)
	if p.Counts()[Disable] != 0 || len(p.DisablesBlocked) != 5 {
		t.Errorf("valve: disables=%d blocked=%d", p.Counts()[Disable], len(p.DisablesBlocked))
	}
	if !strings.Contains(p.DisablesBlockedReason, "MAX_DISABLES=3") {
		t.Errorf("DisablesBlockedReason = %q", p.DisablesBlockedReason)
	}

	// A later warning must not displace the reason.
	g = append(g, gUser(t, `{"primaryEmail":"noname@corbalt.com"}`))
	p, _ = BuildPlan(c, g, e)
	if !strings.Contains(p.DisablesBlockedReason, "MAX_DISABLES=3") {
		t.Errorf("DisablesBlockedReason = %q", p.DisablesBlockedReason)
	}
}

func TestMatchingIsOrderIndependent(t *testing.T) {
	g := []gws.User{gUser(t, `{"primaryEmail":"b@corbalt.com","name":{"fullName":"New Name"}}`)}
	// a's immutable ID is b's UPN: the email identifies two different users.
	a := graph.User{ID: "a", UserPrincipalName: "a@corbalt.com", DisplayName: "A", AccountEnabled: true,
		OnPremisesImmutableID: "b@corbalt.com", UserType: "Member"}
	b := graph.User{ID: "b", UserPrincipalName: "b@corbalt.com", DisplayName: "B", AccountEnabled: true,
		OnPremisesImmutableID: "b@corbalt.com", UserType: "Member"}
	c := graph.User{ID: "c", UserPrincipalName: "c@corbalt.com", DisplayName: "C", AccountEnabled: true,
		UserType: "Member"}
	bNoID := b
	bNoID.OnPremisesImmutableID = ""

	for _, tc := range []struct {
		name  string
		users []graph.User
	}{
		{"immutable ID vs UPN, a first", []graph.User{a, bNoID}},
		{"immutable ID vs UPN, b first", []graph.User{bNoID, a}},
		{"shared immutable ID, a first", []graph.User{a, b}},
		{"shared immutable ID, b first", []graph.User{b, a}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := BuildPlan(cfg, g, append(tc.users, c))
			if err != nil {
				t.Fatal(err)
			}
			// Neither a nor b is updated or disabled; c (unmatched) is disabled.
			if len(p.Actions) != 1 || find(p, Disable, "c@corbalt.com") == nil {
				t.Fatalf("actions = %+v", p.Actions)
			}
			found := false
			for _, w := range p.Warnings {
				if strings.Contains(w, "b@corbalt.com matches more than one Entra user (a@corbalt.com, b@corbalt.com)") {
					found = true
				}
			}
			if !found {
				t.Errorf("warnings = %v", p.Warnings)
			}
		})
	}
}

func TestImmutableIDPreferredOverUPN(t *testing.T) {
	// Only one user is identified: by immutable ID (renamed UPN). No conflict.
	g := []gws.User{gUser(t, `{"primaryEmail":"new@corbalt.com","name":{"fullName":"N"}}`)}
	e := []graph.User{{ID: "1", UserPrincipalName: "old@corbalt.com", DisplayName: "Old", AccountEnabled: true,
		OnPremisesImmutableID: "new@corbalt.com", UserType: "Member"}}
	p, _ := BuildPlan(cfg, g, e)
	if a := find(p, Update, "old@corbalt.com"); a == nil || a.Body["displayName"] != "N" {
		t.Fatalf("actions = %+v", p.Actions)
	}
}

func TestEmptyGoogleRefuses(t *testing.T) {
	e := []graph.User{{ID: "1", UserPrincipalName: "dave@corbalt.com", AccountEnabled: true, UserType: "Member"}}
	if _, err := BuildPlan(cfg, nil, e); err == nil {
		t.Fatal("expected refusal when Google returns nothing")
	}
}

func TestImmutableIDBackfillAndMismatchWarning(t *testing.T) {
	g := []gws.User{
		gUser(t, `{"primaryEmail":"a@corbalt.com","name":{"fullName":"A"}}`),
		gUser(t, `{"primaryEmail":"b@corbalt.com","name":{"fullName":"B"}}`),
	}
	e := []graph.User{
		{ID: "a", UserPrincipalName: "a@corbalt.com", DisplayName: "A", AccountEnabled: true, UserType: "Member"},
		{ID: "b", UserPrincipalName: "b@corbalt.com", DisplayName: "B", AccountEnabled: true,
			OnPremisesImmutableID: "AbCdEf==", UserType: "Member"},
	}
	p, _ := BuildPlan(cfg, g, e)
	if a := find(p, Update, "a@corbalt.com"); a == nil || a.Body["onPremisesImmutableId"] != "a@corbalt.com" {
		t.Errorf("empty immutable ID not backfilled: %+v", a)
	}
	if find(p, Update, "b@corbalt.com") != nil {
		t.Errorf("mismatched immutable ID must not be rewritten")
	}
	if len(p.Warnings) != 1 || !strings.Contains(p.Warnings[0], "b@corbalt.com") {
		t.Errorf("warnings = %v", p.Warnings)
	}
}

func TestMailNickname(t *testing.T) {
	for in, want := range map[string]string{
		"fiona.catcher@corbalt.com": "fiona.catcher",
		"o'brien@corbalt.com":       "o'brien",
		"a(b)c@corbalt.com":         "abc",
	} {
		if got := MailNickname(in); got != want {
			t.Errorf("MailNickname(%q) = %q, want %q", in, got, want)
		}
	}
}

// planBothOrders builds the plan for every combination of forward and
// reversed Google and Entra input, and fails unless all agree.
func planBothOrders(t *testing.T, g []gws.User, e []graph.User) Plan {
	t.Helper()
	rev := func(n int, swap func(i, j int)) {
		for i, j := 0, n-1; i < j; i, j = i+1, j-1 {
			swap(i, j)
		}
	}
	var first Plan
	for i := range 4 {
		gg := append([]gws.User(nil), g...)
		ee := append([]graph.User(nil), e...)
		if i&1 != 0 {
			rev(len(gg), func(a, b int) { gg[a], gg[b] = gg[b], gg[a] })
		}
		if i&2 != 0 {
			rev(len(ee), func(a, b int) { ee[a], ee[b] = ee[b], ee[a] })
		}
		p, err := BuildPlan(cfg, gg, ee)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = p
			continue
		}
		a, _ := json.Marshal(first.Actions)
		b, _ := json.Marshal(p.Actions)
		if string(a) != string(b) {
			t.Fatalf("plan depends on input order:\n%s\nvs\n%s", a, b)
		}
	}
	return first
}

func hasWarning(p Plan, substr string) bool {
	for _, w := range p.Warnings {
		if strings.Contains(w, substr) {
			return true
		}
	}
	return false
}

func TestEntraUserClaimedByTwoGoogleUsersIsNotModified(t *testing.T) {
	// x has UPN a@ and immutable ID b@: both Google users point at it.
	g := []gws.User{
		gUser(t, `{"primaryEmail":"a@corbalt.com","name":{"fullName":"Alice"}}`),
		gUser(t, `{"primaryEmail":"b@corbalt.com","name":{"fullName":"Bob"}}`),
	}
	e := []graph.User{{ID: "x", UserPrincipalName: "a@corbalt.com", DisplayName: "X", AccountEnabled: true,
		OnPremisesImmutableID: "b@corbalt.com", UserType: "Member"}}
	p := planBothOrders(t, g, e)
	if len(p.Actions) != 0 {
		t.Fatalf("contradictory updates planned: %+v", p.Actions)
	}
	if !hasWarning(p, "Entra user a@corbalt.com is matched by more than one Google user (a@corbalt.com, b@corbalt.com)") {
		t.Errorf("warnings = %v", p.Warnings)
	}
}

func TestSharedImmutableIDDoesNotDisableUPNMatch(t *testing.T) {
	// a and c share immutable ID b@; b has UPN b@ and no immutable ID. b is
	// active in Google and must not be disabled.
	g := []gws.User{gUser(t, `{"primaryEmail":"b@corbalt.com","name":{"fullName":"B"}}`)}
	mk := func(upn, imm string) graph.User {
		return graph.User{ID: upn, UserPrincipalName: upn, DisplayName: "B", AccountEnabled: true,
			OnPremisesImmutableID: imm, UserType: "Member"}
	}
	e := []graph.User{mk("a@corbalt.com", "b@corbalt.com"), mk("c@corbalt.com", "b@corbalt.com"), mk("b@corbalt.com", "")}
	p := planBothOrders(t, g, e)
	if len(p.Actions) != 0 {
		t.Fatalf("actions = %+v", p.Actions)
	}
	if !hasWarning(p, "b@corbalt.com matches more than one Entra user (a@corbalt.com, b@corbalt.com, c@corbalt.com)") {
		t.Errorf("warnings = %v", p.Warnings)
	}
}

func TestAmbiguousUserNotModifiedThroughAnotherEmail(t *testing.T) {
	// c@ is ambiguous (x by immutable ID, y by UPN). d@ uniquely names x by
	// UPN, but x is part of an ambiguous match, so d@ must not update it.
	g := []gws.User{
		gUser(t, `{"primaryEmail":"c@corbalt.com","name":{"fullName":"C"}}`),
		gUser(t, `{"primaryEmail":"d@corbalt.com","name":{"fullName":"New D"}}`),
	}
	e := []graph.User{
		{ID: "x", UserPrincipalName: "d@corbalt.com", DisplayName: "D", AccountEnabled: true,
			OnPremisesImmutableID: "c@corbalt.com", UserType: "Member"},
		{ID: "y", UserPrincipalName: "c@corbalt.com", DisplayName: "C", AccountEnabled: true, UserType: "Member"},
	}
	p := planBothOrders(t, g, e)
	if len(p.Actions) != 0 {
		t.Fatalf("actions = %+v", p.Actions)
	}
}
