# Changelog

## 0.1.0 (Unreleased)

FEATURES:

- **New resource:** `clara_aws_integration` registers an AWS payer account with Clara. It is create-only: changes are refused at plan
  time, destroy leaves the integration in Clara, and there is no import.
