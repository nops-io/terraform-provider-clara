# clara_context

The Clara client and organization that the API key belongs to.

```hcl
data "clara_context" "this" {}

output "client_id" {
  value = data.clara_context.this.client_id
}
```

## Attributes

- `client_id`: Clara client id. Use it as `client_id` in the onboarding module.
- `organization_id`: Clara organization id.
