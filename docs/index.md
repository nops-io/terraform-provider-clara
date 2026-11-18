# Clara provider

Registers AWS accounts with nOps Clara.

```hcl
provider "clara" {
  # api_key and api_base_url are usually set with CLARA_API_KEY and CLARA_API_URL.
}
```

## Arguments

- `api_key` (Optional, Sensitive): Clara API key. Environment variable `CLARA_API_KEY`. Required. It needs only the `aws-onboarding:create` permission.
- `api_base_url` (Optional): Clara API base URL. Environment variable `CLARA_API_URL`. Default `https://api.nops.io`. Must be `https`, except for localhost.

## Resources

- [`clara_aws_integration`](resources/aws_integration.md)
