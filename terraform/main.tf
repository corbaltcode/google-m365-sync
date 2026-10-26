terraform {
  required_version = ">= 1.10"
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = ">= 6.0"
    }
  }

  backend "s3" {
    bucket              = "terraform-state-795973919855-us-west-2-an"
    key                 = "google-m365-sync.tfstate"
    region              = "us-west-2"
    allowed_account_ids = ["795973919855"]
    encrypt             = true
    use_lockfile        = true
  }
}

# Refuse to plan or apply against any account other than Corbalt's.
provider "aws" {
  allowed_account_ids = ["795973919855"]
  region              = "us-west-2"
}

locals {
  name = var.name
}

# --- Entra app identifiers (populated out of band in Parameter Store) ---
#
# The Lambda reads these at run time, so the values never appear in its
# environment or in Terraform state.

data "aws_caller_identity" "current" {}
data "aws_partition" "current" {}
data "aws_region" "current" {}

locals {
  entra_tenant_id_param = "/${local.name}/entra-tenant-id"
  entra_client_id_param = "/${local.name}/entra-client-id"
  ssm_parameter_arn_prefix = join(":", [
    "arn", data.aws_partition.current.partition, "ssm", data.aws_region.current.region,
    data.aws_caller_identity.current.account_id, "parameter",
  ])
}

# --- Alerting: any failed action, blocked disables, or crash fails the invocation ---

resource "aws_cloudwatch_metric_alarm" "errors" {
  alarm_name          = "${local.name}-errors"
  alarm_description   = "google-m365-sync failed; check /aws/lambda/${local.name} for the Graph error detail"
  namespace           = "AWS/Lambda"
  metric_name         = "Errors"
  dimensions          = { FunctionName = aws_lambda_function.sync.function_name }
  statistic           = "Sum"
  period              = 3600
  evaluation_periods  = 1
  threshold           = 1
  comparison_operator = "GreaterThanOrEqualToThreshold"
  treat_missing_data  = "notBreaching"
  alarm_actions       = var.alarm_sns_topic_arn == null ? [] : [var.alarm_sns_topic_arn]
}
