package syncer

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/corbaltcode/google-m365-sync/internal/gws"
)

type fakeDirectory struct{ created []map[string]any }

func (f *fakeDirectory) CreateUser(_ context.Context, body map[string]any) (string, error) {
	f.created = append(f.created, body)
	return "new-id", nil
}

func (f *fakeDirectory) UpdateUser(context.Context, string, map[string]any) error { return nil }

func TestCreatePasswordIsSentButNeverInPlan(t *testing.T) {
	g := []gws.User{gUser(t, `{"primaryEmail":"new@corbalt.com","name":{"fullName":"New User"}}`)}
	p, err := BuildPlan(cfg, g, nil)
	if err != nil {
		t.Fatal(err)
	}
	dir := &fakeDirectory{}
	res := Apply(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil)), dir, p, false)
	if len(res.Failed) > 0 || len(dir.created) != 1 {
		t.Fatalf("failed = %v, created = %d", res.Failed, len(dir.created))
	}

	prof, ok := dir.created[0]["passwordProfile"].(map[string]any)
	if !ok {
		t.Fatalf("create body has no passwordProfile: %v", dir.created[0])
	}
	pw, _ := prof["password"].(string)
	if len(pw) < 16 {
		t.Fatalf("password %q too short", pw)
	}

	// The Result (printed in local runs) must not contain the password.
	out, _ := json.Marshal(res)
	if strings.Contains(string(out), pw) || strings.Contains(string(out), "passwordProfile") {
		t.Fatalf("password leaked into result: %s", out)
	}
	if _, has := p.Actions[0].Body["passwordProfile"]; has {
		t.Fatal("Apply mutated the plan's create body")
	}
}
