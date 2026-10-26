package syncer

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"log/slog"
)

// Directory is the Entra write surface the executor needs.
type Directory interface {
	CreateUser(ctx context.Context, body map[string]any) (string, error)
	UpdateUser(ctx context.Context, id string, patch map[string]any) error
}

// Result reports what happened when a plan was applied.
type Result struct {
	DryRun  bool           `json:"dryRun"`
	Planned map[string]int `json:"planned"`
	Applied map[string]int `json:"applied"`
	Failed  []FailedAction `json:"failed,omitempty"`
	Plan    Plan           `json:"plan"`
}

type FailedAction struct {
	Kind  ActionKind `json:"kind"`
	UPN   string     `json:"upn"`
	Error string     `json:"error"`
}

// Apply executes the plan. With dryRun it only logs. One user's failure does
// not stop the rest.
func Apply(ctx context.Context, log *slog.Logger, dir Directory, plan Plan, dryRun bool) Result {
	res := Result{DryRun: dryRun, Planned: map[string]int{}, Applied: map[string]int{}, Plan: plan}
	for k, n := range plan.Counts() {
		res.Planned[string(k)] = n
	}
	for _, w := range plan.Warnings {
		log.Warn("plan warning", "detail", w)
	}
	for _, a := range plan.Actions {
		l := log.With("action", a.Kind, "upn", a.UPN, "reason", a.Reason, "fields", fieldNames(a.Body))
		if dryRun {
			l.Info("would apply")
			continue
		}
		var err error
		switch a.Kind {
		case Create:
			var id string
			id, err = dir.CreateUser(ctx, withPassword(a.Body, RandomPassword()))
			l = l.With("entraId", id)
		default:
			err = dir.UpdateUser(ctx, a.EntraID, a.Body)
		}
		if err != nil {
			l.Error("failed", "error", err.Error())
			res.Failed = append(res.Failed, FailedAction{Kind: a.Kind, UPN: a.UPN, Error: err.Error()})
			continue
		}
		res.Applied[string(a.Kind)]++
		l.Info("applied")
	}
	return res
}

func fieldNames(body map[string]any) []string {
	var out []string
	for k := range body {
		out = append(out, k)
	}
	return out
}

// withPassword returns a copy of a create body with a passwordProfile added.
// The password is never used: sign-in is federated to Google, but Graph
// requires one to create a user.
func withPassword(body map[string]any, password string) map[string]any {
	out := make(map[string]any, len(body)+1)
	for k, v := range body {
		out[k] = v
	}
	out["passwordProfile"] = map[string]any{
		"password":                      password,
		"forceChangePasswordNextSignIn": false,
	}
	return out
}

// RandomPassword returns a high-entropy password that satisfies Entra's
// complexity rules. It is never used for sign-in on a federated domain.
func RandomPassword() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b) + "aA1!"
}
