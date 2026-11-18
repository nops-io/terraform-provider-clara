# clara_aws_integration

Registers an AWS payer account with Clara once its AWS resources exist.

**Create-only.** The API key this provider uses can register an account and nothing else. So:

- **Plan and refresh** make no requests to Clara. The state is not compared with Clara, and drift is not detected.
- **A changed argument is refused** at plan time. Applying it could not update the integration, and replacing it would be refused as a duplicate.
- **Destroy only forgets the resource.** The integration stays in Clara. Disconnect it there if it should go. Nothing is deleted in AWS.
- **There is no import.**

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

- `payer_account_id` (Required): 12-digit payer account id.
- `permission_mode` (Required): `minimal` or `full`. Must match the role's attached policies.
- `role_arn` (Required): the integration role ARN. It must be a role in the payer account.
- `external_id` (Required, Sensitive): the ExternalId from the role's trust policy. Clara never returns it.
- `cur_bucket_name` (Required): bucket that receives the Legacy CUR and CUR 2.0.
- `name` (Optional): name shown in Clara. Defaults to `AWS Account <id>`.
- `cur_report_name`, `cur_s3_prefix` (Optional): Legacy CUR settings. Clara uses the setup wizard's suggestions when unset.
- `cur2_export_name` (Optional): setting it turns CUR 2.0 on.
- `cur2_s3_prefix` (Optional): CUR 2.0 prefix.

## Attributes

These are recorded when the integration is created and are not refreshed.

- `id`: Clara integration id.
- `status`: `active`, `validating` or `awaiting_customer_setup`.
- `verification_state`: `verified`, `partial`, `failed` or `unavailable`.
- `verification_message`: why the role could not be assumed, when that was the failure.
- `failed_checks`: permission checks that failed. Empty when everything passed.

## When verification does not pass

The integration exists in Clara once the call succeeds, so a failed check is a **warning**, not an error. Failing would taint the resource, and the
next apply would be refused because the account is already connected. Check `verification_state`, then open the integration in Clara to see
which checks failed and verify again.

## Already onboarded

If the payer account already has an integration, create fails with a message saying so. The key cannot adopt or change it. If the account was
onboarded on purpose, remove the resource from the configuration. If it is a leftover, disconnect it in Clara first.
