# google-m365-sync

Syncs Google Workspace users into Microsoft Entra ID for a domain that is
**federated to Google** (corbalt.com). It runs as an AWS Lambda on a schedule.

Microsoft's built-in Google Identity Sync can't create users in a federated
domain. Entra requires `onPremisesImmutableId` (the "SourceAnchor") on every
federated user, and Identity Sync doesn't send it, so each create fails with
`SourceAnchor is a required property for creation of a federated user`. This
tool sets `onPremisesImmutableId` to the Google primary email. That is the
value Google sends as the SAML NameID, and the value the existing Corbalt
users already have.

## Dependencies

Authentication, request signing and the Lambda runtime use the vendors' own
libraries rather than hand-written code:

| Concern | Module | Maintainer |
| --- | --- | --- |
| Lambda runtime loop | `github.com/aws/aws-lambda-go` | AWS |
| Secrets Manager, SSM, credentials | `github.com/aws/aws-sdk-go-v2` | AWS |
| Google service-account tokens | `golang.org/x/oauth2` | Go team |
| Entra certificate login | `github.com/AzureAD/microsoft-authentication-library-for-go` (MSAL) | Microsoft |

The Google Directory and Microsoft Graph calls themselves are plain REST
([internal/httpx](internal/httpx/httpx.go)). The sync uses one Directory
endpoint and three Graph endpoints, and the official SDKs for those APIs are
very large in comparison.

The function holds `User.ReadWrite.All` in Entra, so dependencies are checked
in CI: `govulncheck` runs on every push and pull request, and the deployable
zip is built in CI with the Go version pinned in `go.mod`.

## What it does on each run

| Google state | Entra state | Action |
| --- | --- | --- |
| Active, no Entra user | — | **Create** (enabled, immutable ID = email, usage location set) |
| Suspended or archived, no Entra user | — | Nothing |
| Exists | Name / title / department / work phone differs | **Update** those fields (empty Google values never clear Entra values) |
| Suspended or archived | Enabled | **Disable** (`accountEnabled=false`) |
| Active | Disabled | **Re-enable** |
| Exists | `onPremisesImmutableId` empty | Set it to the email |
| Exists | `onPremisesImmutableId` set to something else | **Warn only**; never rewritten |
| Missing (deleted) | Enabled member in the domain | **Disable**. Nothing is ever deleted. |

Matching is case-insensitive on UPN or `onPremisesImmutableId` = Google
primary email. Only `userType=Member` accounts whose UPN is in `SYNC_DOMAIN`
are considered. Guests, other domains, and anything listed in `EXCLUDE_UPNS`
are never touched.

Safety rails:

- **`DRY_RUN` defaults to true** (unless run in the Corbalt configuration).
  Only `DRY_RUN=false` writes. A manual invoke with `{"dryRun": true}` forces a
  dry run, but an event can't turn writes on. Any other payload (a misspelled
  field, `"true"` as a string, bad JSON) is rejected and the run fails without
  touching Entra.
- If an email matches more than one Entra user (by UPN or immutable ID), none
  of them is modified or disabled, and a warning names them.
- If Google returns zero users, the run aborts.
- If more than `MAX_DISABLES` (default 3) users would be disabled for being
  missing from Google, *no* missing-user disables are applied and the run
  fails. This guards against a bad API response or a wrong domain. Suspended
  users are still disabled.
- `reserved_concurrent_executions = 1`, so runs can't overlap.
- Graph error bodies are decoded and logged, e.g. `graph HTTP 400
  Request_BadRequest: ...`, instead of a bare BadRequest.
- Any failed action fails the invocation. That increments the Lambda `Errors`
  metric and fires the CloudWatch alarm.

## One-time setup

### 1. Google: service account holding a read-only admin role

The service account is assigned a Workspace admin role directly and calls the
Directory API as itself. There's no domain-wide delegation and no user account
to impersonate.

1. In a GCP project, enable the **Admin SDK API**. Then go to **IAM & Admin →
   Service Accounts → Create**. No GCP IAM roles are needed. Create a **JSON
   key**.
2. In admin.google.com, go to **Account → Admin roles → Directory Users
   Read-Only (API)**. That's the custom role with only *Users → Read*. Choose
   **Assign service accounts** and enter the service account's email
   (`…@….iam.gserviceaccount.com`).

The sync requests only the `admin.directory.user.readonly` OAuth scope. The
role limits what the service account can do in Workspace, and the scope limits
what each token can do.

### 2. Entra: app registration with a certificate

```sh
openssl req -x509 -newkey rsa:4096 -sha256 -days 730 -nodes \
  -subj "/CN=google-m365-sync" -keyout key.pem -out cert.pem
cat cert.pem key.pem > entra-bundle.pem   # this goes in Secrets Manager
```

1. **Entra → App registrations → New registration**, named `google-m365-sync`,
   single tenant.
2. **Certificates & secrets → Certificates → Upload** `cert.pem`.
3. **API permissions → Microsoft Graph → Application → `User.ReadWrite.All`**,
   then **Grant admin consent**.
4. Modifying or disabling users who hold admin roles needs more than
   `User.ReadWrite.All`. Either assign the app's service principal the **User
   Administrator** role, or put admins in `EXCLUDE_UPNS` and manage them by
   hand. Recommended: exclude them, especially Global Admins and any
   break-glass account. Otherwise those updates fail with
   `Authorization_RequestDenied`.
5. Note the **tenant ID** and **application (client) ID**.

### 3. Deploy

Build the zip, or download the `google-m365-sync-<commit>` artifact from the
CI run for the commit you're deploying and put `google-m365-sync.zip` in
`dist/`. The CI-built zip is preferred: it's built with the Go version pinned in
`go.mod` and has passed `govulncheck`.

```sh
make test zip   # skip if using the CI artifact
cd terraform
cp terraform.tfvars.example terraform.tfvars   # dry_run = true until reviewed
aws ssm put-parameter --name /google-m365-sync/entra-tenant-id --type String --value <tenant-id>
aws ssm put-parameter --name /google-m365-sync/entra-client-id --type String --value <client-id>
terraform init && terraform apply   # state: s3://terraform-state-795973919855-us-west-2-an/google-m365-sync.tfstate

aws secretsmanager put-secret-value \
  --secret-id "$(terraform output -raw google_sa_secret_arn)" \
  --secret-string file://path/to/service-account.json
aws secretsmanager put-secret-value \
  --secret-id "$(terraform output -raw entra_cert_secret_arn)" \
  --secret-string file://entra-bundle.pem
```

The secrets are created empty by Terraform, so key material never enters
Terraform state.

Delete `key.pem` and `entra-bundle.pem` from disk after loading the secret.

### 4. Roll out

```sh
aws lambda invoke --function-name google-m365-sync --payload '{}' \
  --cli-binary-format raw-in-base64-out /dev/stdout
aws logs tail /aws/lambda/google-m365-sync --since 10m
```

Review the `would apply` lines and the `plan warning` lines. Expected on the
first run: about 8 creates (fiona.catcher@, ido@, sohrab@, adam.emrich@, …), a
handful of updates, and possibly `disable` entries for Entra-only accounts such
as `x-clockify-test@`. Add any account that should stay enabled to
`exclude_upns`. Then set `dry_run = false` and `terraform apply`.

After the first real run, re-run Microsoft's Identity Sync if you still want
it for groups. It will report the new users as Skipped (already exist), which
is expected.

## Running locally

```sh
go build -o bootstrap . && \
DRY_RUN=true SYNC_DOMAIN=corbalt.com \
ENTRA_TENANT_ID=... ENTRA_CLIENT_ID=... \
GOOGLE_SA_KEY_FILE=./sa.json ENTRA_CERT_FILE=./entra-bundle.pem ./bootstrap
```

Outside Lambda it runs once, logs JSON to stdout, and prints the full plan to
stderr.

To read the Entra IDs from Parameter Store instead, set
`ENTRA_TENANT_ID_PARAM=/google-m365-sync/entra-tenant-id` and
`ENTRA_CLIENT_ID_PARAM=/google-m365-sync/entra-client-id`. AWS credentials come
from the SDK's usual sources: environment variables, `AWS_PROFILE`, or an SSO
session (`aws sso login`). Set `AWS_REGION` if your profile doesn't.

## Configuration

| Variable | Default | |
| --- | --- | --- |
| `SYNC_DOMAIN` | `corbalt.com` | Only this domain is synced |
| `GOOGLE_ADMIN_SUBJECT` | — | Leave unset: the service account calls Google as itself. Set only to impersonate an admin via domain-wide delegation (legacy) |
| `GOOGLE_SA_SECRET_ARN` / `GOOGLE_SA_KEY_FILE` | — | Service account JSON key |
| `ENTRA_TENANT_ID_PARAM` / `ENTRA_TENANT_ID` | — | Tenant ID: SSM parameter name (Lambda) or literal value (local) |
| `ENTRA_CLIENT_ID_PARAM` / `ENTRA_CLIENT_ID` | — | App registration client ID: SSM parameter name or literal value |
| `ENTRA_CERT_SECRET_ARN` / `ENTRA_CERT_FILE` | — | PEM bundle: certificate + RSA key |
| `USAGE_LOCATION` | `US` | Set on created users so licenses can be assigned |
| `MAX_DISABLES` | `3` | Safety valve for missing-from-Google disables |
| `EXCLUDE_UPNS` | — | Comma-separated UPNs never modified |
| `DRY_RUN` | `true` | Must be exactly `false` to write |

## Layout

```text
main.go                     config, Lambda/CLI entrypoint
internal/syncer/plan.go     pure diff planner (most of the logic; unit-tested)
internal/syncer/apply.go    executor
internal/gws                Directory API client (x/oauth2 service-account tokens)
internal/graph              Graph client (MSAL certificate login)
internal/httpx              JSON HTTP with 429/5xx retry + Retry-After
terraform/                  Lambda, IAM, secrets, scheduler, alarm
```

## Limits

- Users only. Groups aren't synced.
- Licenses aren't assigned. Use group-based licensing in Entra if you want that
  to be automatic.
- Renaming a user's primary email in Google creates a new Entra user and
  disables the old one, because the immutable ID is the email. Do renames by
  hand: update the UPN, mail and immutable ID together.
