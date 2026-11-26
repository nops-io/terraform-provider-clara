# Changelog

## 0.1.0 (Unreleased)

FEATURES:

- **New resource:** `clara_aws_integration` registers an AWS payer account with Clara. It is create-only: changes are refused at plan
  time, destroy leaves the integration in Clara, and there is no import.
- Creates are retried with the same idempotency key when the response is lost (network error, 502, 504) or Clara is busy (429,
  503), and wait while an earlier attempt with that key is still running. Clara replays the first result, so a retry never
  registers the account twice. A daily quota error fails at once.
