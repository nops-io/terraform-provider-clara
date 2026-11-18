# clara_aws_integration

Registers an AWS payer account with Clara once its AWS resources exist. Destroying the resource disconnects the integration in Clara.
It does not delete anything in AWS.

```hcl
resource "clara_aws_integration" "this" {
  payer_account_id = "123456789012"
  permission_mode  = "minimal"
  role_arn         = module.clara.role_arn
  external_id      = module.clara.external_id
  cur_bucket_name  = module.clara.cur_bucket_name
}
```

## Arguments

- `payer_account_id` (Required): 12-digit payer account id. Changing it creates a new integration.
- `permission_mode` (Required): `minimal` or `full`. Must match the role's attached policies.
- `role_arn` (Required): the integration role ARN.
- `external_id` (Required, Sensitive): the ExternalId from the role's trust policy. Write-only.
- `cur_bucket_name` (Required): bucket that receives the Legacy CUR and CUR 2.0.
- `name` (Optional): name shown in Clara. Defaults to `AWS Account <id>`.
- `cur_report_name`, `cur_s3_prefix` (Optional): Legacy CUR settings. Default to Clara's wizard suggestions.
- `cur2_export_name` (Optional): setting it turns CUR 2.0 on.
- `cur2_s3_prefix` (Optional): CUR 2.0 prefix.
- `wait_for_verification` (Optional): verify access and require status `active`. Default `true`.
- `verification_timeout_seconds` (Optional): default `300`, between 30 and 3600.

## Attributes

- `id`: Clara integration id.
- `status`: for example `active`, `validating`, `awaiting_customer_setup`.

## Import

```bash
terraform import clara_aws_integration.this <integration id>
```
