# terraform-provider-clara

Terraform provider that registers AWS accounts with [nOps Clara](https://nops.io) through the Clara public API.

It is the headless counterpart of Clara's setup wizard. The AWS resources themselves are created by the
[`nops-io/clara-onboarding/aws`](https://github.com/nops-io/terraform-aws-clara-onboarding) module. This provider only tells Clara
about them and waits for Clara's verification.

## Using it

```hcl
terraform {
  required_providers {
    clara = {
      source  = "nops-io/clara"
      version = "~> 0.1"
    }
  }
}

# The API key comes from CLARA_API_KEY. Do not put it in configuration files.
provider "clara" {}

data "clara_context" "this" {}
```

Most users do not write `clara_aws_integration` directly. They use the module's `modules/registration`, which wraps it.

### Authentication

| Setting | Environment variable | Default |
|---|---|---|
| `api_key` | `CLARA_API_KEY` | none, required |
| `api_base_url` | `CLARA_API_URL` | `https://api.nops.io` |

The API key needs the `aws-onboarding:read` and `aws-onboarding:write` permissions, and its owner must be a Clara admin. The provider
refuses `http://` URLs except for localhost, so the key is never sent in clear text. The key is not stored in Terraform state.

## Resources

- `clara_aws_integration`: registers a payer account. Destroying it disconnects the integration in Clara and does **not** delete anything in AWS.
- `data.clara_context`: the Clara client id and organization for the API key.

See `docs/`.

## Behavior to know

- **One integration per payer account.** If Clara already has one, the provider adopts it and updates it to match the configuration, with a warning. You do not need to import first.
- **Verification.** After create and update the provider asks Clara to verify access, then waits for the status to become `active`. It fails on `awaiting_customer_setup` and on timeout. Set `wait_for_verification = false` to skip.
- **Failed verification keeps the resource.** The integration exists in Clara, so Terraform marks the resource tainted. Fix the cause and apply again; the adopt path makes that safe.
- **Import.** `terraform import clara_aws_integration.this <integration id>`. `external_id` is write-only, so the next plan sends it.

## API contract

The provider talks to the Clara routes described in [`docs/api-contract.md`](docs/api-contract.md). That file is also the specification for the Clara side.

## Development

Needs Go 1.25 or newer and a `terraform` binary on `PATH` (the tests run the provider through it).

```bash
make test
```

The tests use an in-memory fake of the Clara API (`internal/provider/fake_clara_test.go`). No network or credentials are needed.

To try a local build in a configuration, add a development override to your CLI config:

```hcl
provider_installation {
  dev_overrides {
    "nops-io/clara" = "/path/to/directory/containing/the/binary"
  }
  direct {}
}
```

## License

Apache-2.0
