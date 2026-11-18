# Clara API contract used by this provider

The provider calls **one** Clara endpoint. The API key it uses can do nothing else.

All requests go to the Clara API host (`https://api.nops.io`).

## Authentication

`Authorization: Bearer <Clara API key>`. The key needs the **`aws-onboarding:create`** permission and nothing else.

That permission is accepted by this endpoint and by no other. A key that holds only it cannot read, change or delete
anything in Clara, including integrations it created itself. Create the key in Clara under **Settings → Security → API Keys** and
select only that permission.

The organization must have access to the Clara public API, the same as for any other API key.

## Conventions

- **Success:** `{ "data": ... }`.
- **Error:** `{ "error": { "code": "...", "message": "...", "request_id": "..." } }`. `429` carries `Retry-After`.
- **Idempotency:** `Idempotency-Key` is required. A retry with the same key returns the first response instead of registering again.
- **Secrets:** `externalId` is accepted and **never returned**.
- **Audit:** every registration is recorded with the API key id.

## `POST /v1/aws/onboarding`

Registers one payer account and runs Clara's Savings Analysis access check.

```json
{
  "payerAccountId": "123456789012",
  "name": "optional",
  "roleArn": "arn:aws:iam::123456789012:role/nops-integration-role-42",
  "externalId": "11111111-2222-3333-4444-555555555555",
  "permissionMode": "minimal",
  "cur": { "bucketName": "nops-cur-42", "reportName": "optional", "s3Prefix": "optional" },
  "cur2": { "enabled": true, "exportName": "optional", "s3Prefix": "optional" }
}
```

- Unknown fields are rejected.
- `roleArn` must be a role in `payerAccountId`.
- Omitted optional values take the setup wizard's defaults (`nops_cur_<client id>_hourly`, `nops-cur`, `nops_cur2_<client id>_hourly`, `nops-cur2`). CUR 2.0 uses the CUR bucket unless `cur2.bucketName` is given.
- `permissionMode` is `minimal` or `full` and defaults to `minimal`.

`201`:

```json
{
  "data": {
    "id": "clx123",
    "status": "active",
    "name": "AWS Account 123456789012",
    "payerAccountId": "123456789012",
    "verification": {
      "state": "verified",
      "roleAssumption": "ok",
      "failedChecks": [],
      "message": null
    }
  }
}
```

`verification.state`:

| State | Meaning | `status` |
|---|---|---|
| `verified` | Role assumed, all checks passed | `active` |
| `partial` | Savings Analysis checks failed, cost and usage ingest works | `active` |
| `failed` | The role could not be assumed, or ingest permissions are missing | `awaiting_customer_setup` |
| `unavailable` | The check could not run | `validating` |

A freshly created role may not be assumable for a few seconds. Clara retries that failure up to three times before reporting `failed`.

Errors:

| Status | Code | When |
|---|---|---|
| `400` | `invalid_request` | The body is invalid. The message names the field. |
| `400` | `missing_idempotency_key` | No `Idempotency-Key` header. |
| `401` | `invalid_credentials` | The key is invalid. |
| `403` | `forbidden_scope` | The key lacks `aws-onboarding:create`. |
| `403` | `forbidden_tenant` | The organization is not set up for API access. |
| `409` | `already_exists` | The payer account already has an integration. The response says nothing else about it. |
| `429` | `quota_exceeded` / `rate_limited` | Too many registrations. Default cap: 50 per key per day. |

A payer account whose earlier integration ended in `error` can be registered again. Any other existing integration returns `409`.

## What the provider does with it

| Provider action | Calls |
|---|---|
| Create | `POST /v1/aws/onboarding`, retrying `429` and `503` up to three times with the same `Idempotency-Key` |
| Read, plan, refresh | none |
| Update | none. A changed argument is refused at plan time. |
| Destroy | none. The integration stays in Clara. |

There is no import: the key cannot read integrations.
