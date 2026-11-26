# terraform-provider-clara

Terraform provider that registers AWS accounts with [nOps Clara](https://nops.io) through the Clara public API.

It is the headless counterpart of Clara's setup wizard. The AWS resources themselves are created by the
[`nops-io/clara-onboarding/aws`](https://github.com/nops-io/terraform-aws-clara-onboarding) module. This provider only tells Clara
about them and reports Clara's verification.

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

```

Most users do not write `clara_aws_integration` directly. They use the module's `modules/registration`, which wraps it.

### Authentication

| Setting | Environment variable | Default |
|---|---|---|
| `api_key` | `CLARA_API_KEY` | none, required |
| `api_base_url` | `CLARA_API_URL` | `https://api.nops.io` |

Create the key in Clara under **Settings → Security → API Keys** and give it **only** the `aws-onboarding:create` permission. That
permission registers a new account and nothing else: the key cannot read, change or delete anything in Clara. The provider refuses
`http://` URLs except for localhost, so the key is never sent in clear text, and the key is not stored in Terraform state.

## Resources

- `clara_aws_integration`: registers a payer account.

See `docs/`.

## Behavior to know

This provider is **create-only**, because its API key is. That has consequences:

- **No drift detection.** Plan and refresh make no requests to Clara. The state is what you applied, not what Clara has now.
- **A changed argument is refused** at plan time, with a message. Change the integration in Clara instead.
- **Destroy only forgets the resource.** The integration stays in Clara and nothing in AWS is deleted. Disconnect it in Clara if it should go.
- **No import.**
- **One integration per payer account.** If Clara already has one, create fails with a clear message. The provider cannot adopt it.
- **Verification is a warning.** Clara checks access when it creates the integration and reports `verification_state`. A failed check does not fail the apply,
  because the integration already exists and a tainted resource could not be recreated. Check the attribute, then verify from Clara.

## Development

Needs Go 1.25 or newer and a `terraform` binary on `PATH` (the tests run the provider through it).

```bash
make test
```

The Registry docs in `docs/` are generated from the schema, `templates/` and `examples/`. Regenerate them after a schema change:

```bash
go generate ./...
```

Releases are built by GoReleaser when a `v*` tag is pushed, and signed with the GPG key whose public half is registered in the
Terraform Registry namespace.

The tests use an in-memory fake of the Clara endpoint (`internal/provider/fake_clara_test.go`) that also records every request, so they can prove
the provider sends one create and nothing else. No network or credentials are needed.

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
