# google_admin_subject is unset: the service account holds the Directory
# Users Read-Only (API) role itself, so no user is impersonated.

# Accounts the sync must never touch, e.g. a cloud-only break-glass admin.
exclude_upns = []

# Flip to false after reviewing a dry-run log.
dry_run = false

alarm_sns_topic_arn = "arn:aws:sns:us-west-2:795973919855:google-m365-sync-issues"
