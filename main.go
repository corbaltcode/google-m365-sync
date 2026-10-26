// Command google-m365-sync syncs Google Workspace users into Microsoft Entra ID
// for a domain federated to Google. It runs as an AWS Lambda (provided.al2023
// runtime, via aws-lambda-go), or once from a shell for testing.
//
// Configuration is via environment variables; see README.md.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/ssm"

	"github.com/corbaltcode/google-m365-sync/internal/graph"
	"github.com/corbaltcode/google-m365-sync/internal/gws"
	"github.com/corbaltcode/google-m365-sync/internal/syncer"
)

type settings struct {
	Domain        string
	GoogleAdmin   string
	TenantID      paramSource
	ClientID      paramSource
	UsageLocation string
	MaxDisables   int
	DryRun        bool
	Exclude       map[string]bool
	GoogleKeySrc  secretSource
	EntraCertSrc  secretSource
}

// secretSource is either a Secrets Manager ARN (Lambda) or a local file (CLI).
type secretSource struct{ arn, file string }

// paramSource is either an SSM parameter name (Lambda) or a literal value (CLI).
// Lambda gets only the name, so the value never appears in its configuration.
type paramSource struct{ name, value string }

func loadSettings() (settings, error) {
	s := settings{
		Domain:        env("SYNC_DOMAIN", "corbalt.com"),
		GoogleAdmin:   os.Getenv("GOOGLE_ADMIN_SUBJECT"),
		TenantID:      paramSource{os.Getenv("ENTRA_TENANT_ID_PARAM"), os.Getenv("ENTRA_TENANT_ID")},
		ClientID:      paramSource{os.Getenv("ENTRA_CLIENT_ID_PARAM"), os.Getenv("ENTRA_CLIENT_ID")},
		UsageLocation: env("USAGE_LOCATION", "US"),
		Exclude:       map[string]bool{},
		GoogleKeySrc:  secretSource{os.Getenv("GOOGLE_SA_SECRET_ARN"), os.Getenv("GOOGLE_SA_KEY_FILE")},
		EntraCertSrc:  secretSource{os.Getenv("ENTRA_CERT_SECRET_ARN"), os.Getenv("ENTRA_CERT_FILE")},
	}
	// Safe by default: only an explicit DRY_RUN=false writes to Entra.
	s.DryRun = !strings.EqualFold(env("DRY_RUN", "true"), "false")

	n, err := strconv.Atoi(env("MAX_DISABLES", "3"))
	if err != nil || n < 0 {
		return s, fmt.Errorf("MAX_DISABLES must be a non-negative integer")
	}
	s.MaxDisables = n
	for _, u := range strings.Split(os.Getenv("EXCLUDE_UPNS"), ",") {
		if u = strings.ToLower(strings.TrimSpace(u)); u != "" {
			s.Exclude[u] = true
		}
	}
	var missing []string
	if s.TenantID == (paramSource{}) {
		missing = append(missing, "ENTRA_TENANT_ID_PARAM or ENTRA_TENANT_ID")
	}
	if s.ClientID == (paramSource{}) {
		missing = append(missing, "ENTRA_CLIENT_ID_PARAM or ENTRA_CLIENT_ID")
	}
	if s.GoogleKeySrc == (secretSource{}) {
		missing = append(missing, "GOOGLE_SA_SECRET_ARN or GOOGLE_SA_KEY_FILE")
	}
	if s.EntraCertSrc == (secretSource{}) {
		missing = append(missing, "ENTRA_CERT_SECRET_ARN or ENTRA_CERT_FILE")
	}
	if len(missing) > 0 {
		return s, fmt.Errorf("missing configuration: %s", strings.Join(missing, ", "))
	}
	return s, nil
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

type awsClients struct {
	sm  *secretsmanager.Client
	ssm *ssm.Client
}

// awsClientsFunc returns the shared AWS clients, creating them on first use
// so purely local runs (files and literal values) need no AWS credentials.
type awsClientsFunc func() (*awsClients, error)

// newAWSClients uses the SDK's default credential chain: the execution role
// in Lambda; environment variables, profiles or SSO locally.
func newAWSClients() (*awsClients, error) {
	cfg, err := config.LoadDefaultConfig(context.Background())
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}
	return &awsClients{sm: secretsmanager.NewFromConfig(cfg), ssm: ssm.NewFromConfig(cfg)}, nil
}

func (src secretSource) load(ctx context.Context, clients awsClientsFunc) ([]byte, error) {
	if src.file != "" {
		return os.ReadFile(src.file)
	}
	c, err := clients()
	if err != nil {
		return nil, err
	}
	out, err := c.sm.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{SecretId: aws.String(src.arn)})
	if err != nil {
		return nil, err
	}
	if out.SecretString == nil || *out.SecretString == "" {
		return nil, fmt.Errorf("secret %s has no SecretString (binary secrets are not supported)", src.arn)
	}
	return []byte(*out.SecretString), nil
}

func (src paramSource) load(ctx context.Context, clients awsClientsFunc) (string, error) {
	if src.value != "" {
		return src.value, nil
	}
	c, err := clients()
	if err != nil {
		return "", err
	}
	out, err := c.ssm.GetParameter(ctx, &ssm.GetParameterInput{Name: aws.String(src.name), WithDecryption: aws.Bool(true)})
	if err != nil {
		return "", err
	}
	if out.Parameter == nil || aws.ToString(out.Parameter.Value) == "" {
		return "", fmt.Errorf("parameter %s is empty", src.name)
	}
	return *out.Parameter.Value, nil
}

// effectiveDryRun applies an invocation payload to the configured dry-run
// setting. An event may force a dry run but cannot turn one off.
//
// The only accepted payloads are empty, null, {}, and {"dryRun": <bool>}.
// Anything else is rejected so a mistyped dry-run request never runs live.
// The payload is checked token by token because encoding/json would accept
// a repeated or case-variant key (last one wins) and trailing junk.
func effectiveDryRun(configured bool, payload []byte) (bool, error) {
	if len(bytes.TrimSpace(payload)) == 0 {
		return configured, nil
	}
	invalid := func(why string) (bool, error) {
		return true, fmt.Errorf(`invalid event %s: %s (expected {} or {"dryRun": true})`, payload, why)
	}
	dec := json.NewDecoder(bytes.NewReader(payload))
	tok, err := dec.Token()
	if err != nil {
		return invalid(err.Error())
	}
	requested := false
	if tok != nil { // null is accepted as "no override"
		if d, ok := tok.(json.Delim); !ok || d != '{' {
			return invalid("not a JSON object")
		}
		seen := false
		for dec.More() {
			key, err := dec.Token()
			if err != nil {
				return invalid(err.Error())
			}
			if key != "dryRun" {
				return invalid(fmt.Sprintf("unknown field %q", key))
			}
			if seen {
				return invalid("dryRun given more than once")
			}
			seen = true
			v, err := dec.Token()
			if err != nil {
				return invalid(err.Error())
			}
			b, ok := v.(bool)
			if !ok {
				return invalid("dryRun must be true or false")
			}
			requested = b
		}
		if _, err := dec.Token(); err != nil { // closing }
			return invalid(err.Error())
		}
	}
	if _, err := dec.Token(); err != io.EOF {
		return invalid("trailing data after the JSON object")
	}
	return configured || requested, nil
}

func run(ctx context.Context, log *slog.Logger, s settings, payload []byte) (syncer.Result, error) {
	dryRun, err := effectiveDryRun(s.DryRun, payload)
	if err != nil {
		return syncer.Result{}, err
	}

	clients := awsClientsFunc(sync.OnceValues(newAWSClients))
	keyJSON, err := s.GoogleKeySrc.load(ctx, clients)
	if err != nil {
		return syncer.Result{}, fmt.Errorf("load google key: %w", err)
	}
	certPEM, err := s.EntraCertSrc.load(ctx, clients)
	if err != nil {
		return syncer.Result{}, fmt.Errorf("load entra cert: %w", err)
	}
	tenantID, err := s.TenantID.load(ctx, clients)
	if err != nil {
		return syncer.Result{}, fmt.Errorf("load entra tenant id: %w", err)
	}
	clientID, err := s.ClientID.load(ctx, clients)
	if err != nil {
		return syncer.Result{}, fmt.Errorf("load entra client id: %w", err)
	}
	gc, err := gws.New(keyJSON, s.GoogleAdmin)
	if err != nil {
		return syncer.Result{}, err
	}
	ec, err := graph.New(tenantID, clientID, certPEM)
	if err != nil {
		return syncer.Result{}, err
	}

	gUsers, err := gc.ListUsers(ctx, s.Domain)
	if err != nil {
		return syncer.Result{}, err
	}
	eUsers, err := ec.ListUsers(ctx)
	if err != nil {
		return syncer.Result{}, err
	}
	plan, err := syncer.BuildPlan(syncer.Config{
		Domain: s.Domain, UsageLocation: s.UsageLocation, Exclude: s.Exclude, MaxDisables: s.MaxDisables,
	}, gUsers, eUsers)
	if err != nil {
		return syncer.Result{Plan: plan}, err
	}
	log.Info("plan built", "googleUsers", plan.GoogleUsers, "entraUsers", plan.EntraUsers,
		"counts", plan.Counts(), "dryRun", dryRun)

	res := syncer.Apply(ctx, log, ec, plan, dryRun)
	log.Info("sync finished", "dryRun", res.DryRun, "planned", res.Planned, "applied", res.Applied,
		"failed", len(res.Failed), "disablesBlocked", len(plan.DisablesBlocked))

	// Failing the invocation makes the CloudWatch Errors metric (and alarm) fire.
	if len(res.Failed) > 0 {
		return res, fmt.Errorf("%d of %d actions failed; first: %s %s: %s",
			len(res.Failed), len(plan.Actions), res.Failed[0].Kind, res.Failed[0].UPN, res.Failed[0].Error)
	}
	if plan.DisablesBlockedReason != "" {
		return res, errors.New(plan.DisablesBlockedReason)
	}
	return res, nil
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	s, err := loadSettings()
	inLambda := os.Getenv("AWS_LAMBDA_RUNTIME_API") != ""
	if err != nil {
		log.Error("config", "error", err.Error())
		if !inLambda {
			os.Exit(1)
		}
		// In Lambda, fail each invocation with the config error rather than
		// exiting, so the invoke result says what's wrong instead of
		// Runtime.ExitError. The Errors metric and alarm fire either way.
		lambda.Start(func(context.Context, json.RawMessage) (any, error) {
			return nil, fmt.Errorf("config: %w", err)
		})
	}

	if !inLambda {
		// Local one-shot run, e.g. DRY_RUN=true GOOGLE_SA_KEY_FILE=... ./bootstrap
		res, err := run(context.Background(), log, s, nil)
		out, _ := json.MarshalIndent(res, "", "  ")
		fmt.Fprintln(os.Stderr, string(out))
		if err != nil {
			log.Error("sync failed", "error", err.Error())
			os.Exit(1)
		}
		return
	}

	lambda.Start(func(ctx context.Context, payload json.RawMessage) (any, error) {
		res, err := run(ctx, log, s, payload)
		if err != nil {
			log.Error("sync failed", "error", err.Error())
			return nil, err
		}
		// Return the counts only; each action (kind, UPN, changed field names)
		// is already in the logs.
		return map[string]any{"dryRun": res.DryRun, "planned": res.Planned, "applied": res.Applied}, nil
	})
}
