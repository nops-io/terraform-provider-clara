module "clara" {
  source  = "nops-io/clara-onboarding/aws"
  version = "~> 0.1"

  providers = {
    aws           = aws
    aws.us_east_1 = aws.us_east_1
    aws.sharesave = aws
  }

  client_id       = "<clara-client-id>"
  cur_bucket_name = "<globally-unique-bucket>"
  permission_mode = "minimal"
}

resource "clara_aws_integration" "this" {
  payer_account_id = "123456789012"
  permission_mode  = "minimal"
  role_arn         = module.clara.role_arn
  external_id      = module.clara.external_id
  cur_bucket_name  = module.clara.cur_bucket_name
}
