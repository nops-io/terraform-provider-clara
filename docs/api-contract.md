# Clara API contract used by this provider

Status: **proposed**. The provider and its tests (`internal/provider/fake_clara_test.go`) are written against this contract.

All routes live under `/api/v1/aws` on the Clara API host (`https://api.nops.io`).

## Conventions

- **Auth:** `Authorization: Bearer <Clara API key>`. Reads need `aws-onboarding:read`. Writes need `aws-onboarding:write`. The key's owner must
  be a Clara admin, the same rule the wizard enforces. Every route is scoped to the key's organization; another organization's ids return `404`.
- **Success:** `{ "data": ... }`.
- **Error:** `{ "error": { "code": "...", "message": "...", "request_id": "..." } }`. `429` carries `Retry-After`.
- **IDs:** ids may be strings or numbers. The provider reads both.
- **Secrets:** `external_id` is accepted on write and **never returned**. Do not log it.
- **Audit:** every write is recorded with the API key id.

## Integration object

```json
{
  "id": "int_123",
  "status": "active",
  "payer_account_id": "123456789012",
  "name": "AWS Account 123456789012",
  "permission_mode": "minimal",
  "role_arn": "arn:aws:iam::123456789012:role/nops-integration-role-42",
  "cur_bucket_name": "nops-cur-42",
  "cur_report_name": "nops_cur_42_hourly",
  "cur_s3_prefix": "nops-cur",
  "cur2_enabled": true,
  "cur2_export_name": "nops_cur2_42_hourly",
  "cur2_s3_prefix": "nops-cur2"
}
```

`status` is one of: `validating` (created, not yet verified), `active`, `awaiting_customer_setup` (verification failed), `error`, `disconnected`.
These are the values Clara's `verify-sa` already produces, plus `disconnected`.

## Routes

### `GET /api/v1/aws/context`

Scope: read. Returns the key's organization.

```json
{ "data": { "client_id": 42, "organization_id": "org_abc" } }
```

### `GET /api/v1/aws/integrations?payerAccountId=123456789012`

Scope: read. `{ "data": [ <integration> ] }`. Excludes `disconnected` integrations. Empty list when none.

### `POST /api/v1/aws/integrations`

Scope: write. Body (all values are strings unless noted):

```json
{
  "payer_account_id": "123456789012",
  "name": "optional",
  "method": "terraform",
  "permission_mode": "minimal",
  "role_arn": "arn:aws:iam::123456789012:role/nops-integration-role-42",
  "external_id": "11111111-2222-3333-4444-555555555555",
  "cur_bucket_name": "nops-cur-42",
  "cur_report_name": "optional",
  "cur_s3_prefix": "optional",
  "cur2_enabled": true,
  "cur2_export_name": "optional",
  "cur2_s3_prefix": "optional"
}
```

- Omitted optional values take the wizard's suggested defaults (`nops_cur_<client id>_hourly`, `nops-cur`, `nops_cur2_<client id>_hourly`, `nops-cur2`).
- `201` with the integration, status `validating`.
- `409` with code `integration_exists` when the payer account already has a non-disconnected integration. The provider then looks it up and calls `PATCH`.
- `400` for invalid input. `method` must be `terraform`.

### `GET /api/v1/aws/integrations/{id}`

Scope: read. `200` with the integration, or `404`.

### `PATCH /api/v1/aws/integrations/{id}`

Scope: write. Same body fields as `POST`, except `payer_account_id` and `method`, which are fixed. Resets `status` to `validating`. `200` with the integration.

### `POST /api/v1/aws/integrations/{id}/verify-sa`

Scope: write. Runs the same access check as the setup wizard's Verify step and returns `{ "data": { "status": "active" } }`. The status is also stored.
`validating` means the check is still in progress; the provider then polls `GET` until the status settles or its timeout passes.

### `POST /api/v1/aws/integrations/{id}/disconnect`

Scope: write. Marks the integration `disconnected` and stops ingestion. Does not touch AWS. `204` with no body. A second call returns `204` or `404`.

## What the provider does with these

| Provider action | Calls |
|---|---|
| `data.clara_context` read | `GET /context` |
| Create | `POST /integrations`; on `409`: `GET /integrations?payerAccountId=` then `PATCH`. Then `POST .../verify-sa`, polling `GET` while `validating` |
| Read | `GET /integrations/{id}`; `404` or status `disconnected` removes it from state |
| Update | `PATCH`, then verify |
| Destroy | `POST .../disconnect` |
| Import | `GET /integrations/{id}` |

Retries: `429` and `503`, up to three times, honoring `Retry-After` (capped at 30 seconds).
