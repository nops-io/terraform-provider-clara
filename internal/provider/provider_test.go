package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"regexp"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

func TestMain(m *testing.M) {
	// Use the installed Terraform binary instead of downloading one.
	if os.Getenv("TF_ACC_TERRAFORM_PATH") == "" {
		if path, err := exec.LookPath("terraform"); err == nil {
			_ = os.Setenv("TF_ACC_TERRAFORM_PATH", path)
		}
	}
	os.Exit(m.Run())
}

func factories() map[string]func() (tfprotov6.ProviderServer, error) {
	p := &claraProvider{version: "test", pollInterval: 5 * time.Millisecond, retryBackoff: time.Millisecond}
	return map[string]func() (tfprotov6.ProviderServer, error){
		"clara": providerserver.NewProtocol6WithError(p),
	}
}

func providerBlock(url string) string {
	return fmt.Sprintf(`
provider "clara" {
  api_base_url = %q
  api_key      = %q
}
`, url, testAPIKey)
}

func integrationConfig(url, mode string) string {
	return providerBlock(url) + fmt.Sprintf(`
resource "clara_aws_integration" "test" {
  payer_account_id = "123456789012"
  permission_mode  = %q
  role_arn         = "arn:aws:iam::123456789012:role/nops-integration-role-42"
  external_id      = "11111111-2222-3333-4444-555555555555"
  cur_bucket_name  = "nops-cur-42"
}
`, mode)
}

func TestAwsIntegration_lifecycle(t *testing.T) {
	fake := newFakeClara(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(),
		CheckDestroy: func(_ *terraform.State) error {
			if got := fake.status("int-1"); got != "disconnected" {
				return fmt.Errorf("integration int-1 should be disconnected after destroy, is %q", got)
			}
			return nil
		},
		Steps: []resource.TestStep{
			{
				Config: integrationConfig(fake.url(), "minimal"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("clara_aws_integration.test", "id", "int-1"),
					resource.TestCheckResourceAttr("clara_aws_integration.test", "status", "active"),
					resource.TestCheckResourceAttr("clara_aws_integration.test", "permission_mode", "minimal"),
					resource.TestCheckResourceAttr("clara_aws_integration.test", "name", "AWS Account 123456789012"),
					resource.TestCheckResourceAttr("clara_aws_integration.test", "cur_report_name", "nops_cur_42_hourly"),
					resource.TestCheckResourceAttr("clara_aws_integration.test", "cur_s3_prefix", "nops-cur"),
					resource.TestCheckNoResourceAttr("clara_aws_integration.test", "cur2_export_name"),
				),
			},
			{
				// Changing the permission level updates the same integration and verifies again.
				Config: integrationConfig(fake.url(), "full"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("clara_aws_integration.test", "id", "int-1"),
					resource.TestCheckResourceAttr("clara_aws_integration.test", "permission_mode", "full"),
					resource.TestCheckResourceAttr("clara_aws_integration.test", "status", "active"),
					func(_ *terraform.State) error {
						if !fake.saw("PATCH /api/v1/aws/integrations/int-1") {
							return fmt.Errorf("expected a PATCH for the update")
						}
						return nil
					},
				),
			},
			{
				ResourceName:            "clara_aws_integration.test",
				ImportState:             true,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"external_id"},
			},
		},
	})
}

func TestAwsIntegration_cur2Settings(t *testing.T) {
	fake := newFakeClara(t)

	config := providerBlock(fake.url()) + `
resource "clara_aws_integration" "test" {
  payer_account_id = "123456789012"
  permission_mode  = "full"
  role_arn         = "arn:aws:iam::123456789012:role/nops-integration-role-42"
  external_id      = "11111111-2222-3333-4444-555555555555"
  cur_bucket_name  = "nops-cur-42"
  cur_report_name  = "my_report"
  cur_s3_prefix    = "my-prefix"
  cur2_export_name = "my_cur2"
  cur2_s3_prefix   = "my-cur2"
}
`
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config: config,
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr("clara_aws_integration.test", "cur_report_name", "my_report"),
				resource.TestCheckResourceAttr("clara_aws_integration.test", "cur_s3_prefix", "my-prefix"),
				resource.TestCheckResourceAttr("clara_aws_integration.test", "cur2_export_name", "my_cur2"),
				resource.TestCheckResourceAttr("clara_aws_integration.test", "cur2_s3_prefix", "my-cur2"),
			),
		}},
	})
}

func TestAwsIntegration_adoptsExistingIntegration(t *testing.T) {
	fake := newFakeClara(t)
	existing := fake.seed("123456789012")

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config: integrationConfig(fake.url(), "minimal"),
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr("clara_aws_integration.test", "id", existing),
				resource.TestCheckResourceAttr("clara_aws_integration.test", "status", "active"),
				resource.TestCheckResourceAttr("clara_aws_integration.test", "role_arn", "arn:aws:iam::123456789012:role/nops-integration-role-42"),
				func(_ *terraform.State) error {
					if !fake.saw("PATCH /api/v1/aws/integrations/" + existing) {
						return fmt.Errorf("the existing integration should have been updated")
					}
					return nil
				},
			),
		}},
	})
}

func TestAwsIntegration_failedVerificationIsAnError(t *testing.T) {
	fake := newFakeClara(t)
	fake.verifyStatus = "awaiting_customer_setup"

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config:      integrationConfig(fake.url(), "minimal"),
			ExpectError: regexp.MustCompile(`awaiting_customer_setup`),
		}},
	})
}

func TestAwsIntegration_waitsWhileValidating(t *testing.T) {
	fake := newFakeClara(t)
	fake.verifyStatus = "validating"
	fake.validatingPolls = 3

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config: integrationConfig(fake.url(), "minimal"),
			Check:  resource.TestCheckResourceAttr("clara_aws_integration.test", "status", "active"),
		}},
	})
}

func TestAwsIntegration_skipsVerificationWhenAsked(t *testing.T) {
	fake := newFakeClara(t)
	fake.verifyStatus = "awaiting_customer_setup"

	config := providerBlock(fake.url()) + `
resource "clara_aws_integration" "test" {
  payer_account_id      = "123456789012"
  permission_mode       = "minimal"
  role_arn              = "arn:aws:iam::123456789012:role/nops-integration-role-42"
  external_id           = "11111111-2222-3333-4444-555555555555"
  cur_bucket_name       = "nops-cur-42"
  wait_for_verification = false
}
`
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config: config,
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr("clara_aws_integration.test", "status", "validating"),
				func(_ *terraform.State) error {
					if fake.saw("POST /api/v1/aws/integrations/int-1/verify-sa") {
						return fmt.Errorf("verification must not run when wait_for_verification is false")
					}
					return nil
				},
			),
		}},
	})
}

func TestAwsIntegration_rejectsInvalidInput(t *testing.T) {
	fake := newFakeClara(t)
	config := providerBlock(fake.url()) + `
resource "clara_aws_integration" "test" {
  payer_account_id = "12345"
  permission_mode  = "everything"
  role_arn         = "not-an-arn"
  external_id      = "short"
  cur_bucket_name  = "Bad_Bucket"
}
`
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config:      config,
			ExpectError: regexp.MustCompile(`12-digit AWS account id`),
		}},
	})
}

func TestContextDataSource(t *testing.T) {
	fake := newFakeClara(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config: providerBlock(fake.url()) + `data "clara_context" "this" {}`,
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr("data.clara_context.this", "client_id", "42"),
				resource.TestCheckResourceAttr("data.clara_context.this", "organization_id", "org_abc"),
			),
		}},
	})
}

func TestProvider_requiresAnAPIKey(t *testing.T) {
	t.Setenv("CLARA_API_KEY", "")

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config:      `provider "clara" { api_base_url = "https://api.nops.io" }` + "\n" + `data "clara_context" "this" {}`,
			ExpectError: regexp.MustCompile(`Missing Clara API key`),
		}},
	})
}

func TestProvider_rejectsAWrongAPIKey(t *testing.T) {
	fake := newFakeClara(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config: fmt.Sprintf(`
provider "clara" {
  api_base_url = %q
  api_key      = "wrong"
}
data "clara_context" "this" {}
`, fake.url()),
			ExpectError: regexp.MustCompile(`invalid_credentials`),
		}},
	})
}

func TestProvider_readsTheKeyFromTheEnvironment(t *testing.T) {
	fake := newFakeClara(t)
	t.Setenv("CLARA_API_KEY", testAPIKey)
	t.Setenv("CLARA_API_URL", fake.url())

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config: `data "clara_context" "this" {}`,
			Check:  resource.TestCheckResourceAttr("data.clara_context.this", "client_id", "42"),
		}},
	})
}

func TestValidateBaseURL(t *testing.T) {
	cases := map[string]bool{
		"https://api.nops.io":      true,
		"https://api.example.com/": true,
		"http://localhost:3000":    true,
		"http://127.0.0.1:8080":    true,
		"http://api.nops.io":       false,
		"ftp://api.nops.io":        false,
		"api.nops.io":              false,
		"":                         false,
	}
	for raw, ok := range cases {
		if err := validateBaseURL(raw); (err == nil) != ok {
			t.Errorf("validateBaseURL(%q): ok=%v, err=%v", raw, ok, err)
		}
	}
}

func TestClient_retriesOnRateLimit(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) <= 2 {
			writeError(w, http.StatusTooManyRequests, "rate_limited", "slow down")
			return
		}
		writeJSON(w, 200, map[string]any{"data": map[string]any{"client_id": "7", "organization_id": "o"}})
	}))
	defer srv.Close()

	c := &apiClient{baseURL: srv.URL, apiKey: "k", userAgent: "t", http: srv.Client(), retryBackoff: time.Millisecond}
	got, err := c.getContext(context.Background())
	if err != nil {
		t.Fatalf("expected success after retries, got %v", err)
	}
	if got.ClientID != "7" || atomic.LoadInt32(&calls) != 3 {
		t.Fatalf("client=%q calls=%d", got.ClientID, calls)
	}
}

func TestClient_givesUpAfterRetries(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusTooManyRequests, "rate_limited", "slow down")
	}))
	defer srv.Close()

	c := &apiClient{baseURL: srv.URL, apiKey: "k", userAgent: "t", http: srv.Client(), retryBackoff: time.Millisecond}
	_, err := c.getContext(context.Background())
	if statusOf(err) != http.StatusTooManyRequests {
		t.Fatalf("expected a 429 apiError, got %v", err)
	}
}

func TestClient_parsesErrorEnvelope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusForbidden, "forbidden_scope", "key lacks aws-onboarding:write")
	}))
	defer srv.Close()

	c := &apiClient{baseURL: srv.URL, apiKey: "k", userAgent: "t", http: srv.Client(), retryBackoff: time.Millisecond}
	_, err := c.getContext(context.Background())
	apiErr, ok := err.(*apiError)
	if !ok || apiErr.Code != "forbidden_scope" || apiErr.RequestID != "req-test" {
		t.Fatalf("unexpected error: %#v", err)
	}
}

func TestFlexString(t *testing.T) {
	var got struct {
		A flexString `json:"a"`
		B flexString `json:"b"`
		C flexString `json:"c"`
	}
	if err := jsonUnmarshal(`{"a": 42, "b": "abc", "c": null}`, &got); err != nil {
		t.Fatal(err)
	}
	if got.A != "42" || got.B != "abc" || got.C != "" {
		t.Fatalf("got %#v", got)
	}
}
