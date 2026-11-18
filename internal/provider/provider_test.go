package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
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
	p := &claraProvider{version: "test", retryBackoff: time.Millisecond}
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

func integrationConfig(url, extra string) string {
	return providerBlock(url) + fmt.Sprintf(`
resource "clara_aws_integration" "test" {
  payer_account_id = "123456789012"
  permission_mode  = "minimal"
  role_arn         = "arn:aws:iam::123456789012:role/nops-integration-role-42"
  external_id      = "11111111-2222-3333-4444-555555555555"
  cur_bucket_name  = "nops-cur-42"
%s}
`, extra)
}

const onlyCreate = "POST /api/v1/aws/onboarding"

func TestAwsIntegration_createsOnceAndNeverReadsChangesOrDeletes(t *testing.T) {
	fake := newFakeClara(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(),
		CheckDestroy: func(_ *terraform.State) error {
			// The provider forgot the resource and, with a create-only key, could not have done more.
			if got := fake.requestLog(); len(got) != 1 || got[0] != onlyCreate {
				return fmt.Errorf("expected exactly one create request over the whole lifecycle, got %v", got)
			}
			return nil
		},
		Steps: []resource.TestStep{
			{
				Config: integrationConfig(fake.url(), ""),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("clara_aws_integration.test", "id", "int-1"),
					resource.TestCheckResourceAttr("clara_aws_integration.test", "status", "active"),
					resource.TestCheckResourceAttr("clara_aws_integration.test", "name", "AWS Account 123456789012"),
					resource.TestCheckResourceAttr("clara_aws_integration.test", "verification_state", "verified"),
					resource.TestCheckResourceAttr("clara_aws_integration.test", "failed_checks.#", "0"),
					resource.TestCheckNoResourceAttr("clara_aws_integration.test", "cur2_export_name"),
				),
			},
			{
				// The same configuration again: a refresh and a plan, no requests, no diff.
				Config:   integrationConfig(fake.url(), ""),
				PlanOnly: true,
			},
		},
	})
}

func TestAwsIntegration_sendsTheConfiguration(t *testing.T) {
	fake := newFakeClara(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config: integrationConfig(fake.url(), `  name             = "Production payer"
  cur_report_name  = "my_report"
  cur_s3_prefix    = "my-prefix"
  cur2_export_name = "my_cur2"
  cur2_s3_prefix   = "my-cur2-prefix"
`),
			Check: func(_ *terraform.State) error {
				got := fake.lastBody()
				switch {
				case got.PayerAccountID != "123456789012", got.Name != "Production payer",
					got.PermissionMode != "minimal", got.ExternalID != "11111111-2222-3333-4444-555555555555",
					got.RoleArn != "arn:aws:iam::123456789012:role/nops-integration-role-42",
					got.Cur.BucketName != "nops-cur-42", got.Cur.ReportName != "my_report", got.Cur.S3Prefix != "my-prefix":
					return fmt.Errorf("unexpected request body: %+v", got)
				case got.Cur2 == nil, !got.Cur2.Enabled, got.Cur2.ExportName != "my_cur2", got.Cur2.S3Prefix != "my-cur2-prefix":
					return fmt.Errorf("unexpected CUR 2.0 settings: %+v", got.Cur2)
				}
				return nil
			},
		}},
	})
}

func TestAwsIntegration_leavesCur2OffByDefault(t *testing.T) {
	fake := newFakeClara(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config: integrationConfig(fake.url(), ""),
			Check: func(_ *terraform.State) error {
				if got := fake.lastBody(); got.Cur2 != nil || got.Cur.ReportName != "" || got.Cur.S3Prefix != "" {
					return fmt.Errorf("optional settings must be omitted, got %+v", got)
				}
				return nil
			},
		}},
	})
}

func TestAwsIntegration_keepsTheResourceWhenVerificationFails(t *testing.T) {
	fake := newFakeClara(t)
	fake.status = "awaiting_customer_setup"
	msg := "AccessDenied"
	fake.verification = onboardVerification{State: "failed", RoleAssumption: "failed", FailedChecks: []string{}, Message: &msg}

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			// A warning, not an error: the integration exists, and tainting the resource would
			// make the next apply a refused duplicate.
			Config: integrationConfig(fake.url(), ""),
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr("clara_aws_integration.test", "status", "awaiting_customer_setup"),
				resource.TestCheckResourceAttr("clara_aws_integration.test", "verification_state", "failed"),
				resource.TestCheckResourceAttr("clara_aws_integration.test", "verification_message", "AccessDenied"),
			),
		}},
	})
}

func TestAwsIntegration_recordsFailedChecksOfAPartialVerification(t *testing.T) {
	fake := newFakeClara(t)
	fake.verification = onboardVerification{State: "partial", RoleAssumption: "ok", FailedChecks: []string{"ec2", "rds"}}

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config: integrationConfig(fake.url(), ""),
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr("clara_aws_integration.test", "verification_state", "partial"),
				resource.TestCheckResourceAttr("clara_aws_integration.test", "failed_checks.#", "2"),
				resource.TestCheckResourceAttr("clara_aws_integration.test", "failed_checks.0", "ec2"),
			),
		}},
	})
}

func TestAwsIntegration_explainsAnExistingIntegration(t *testing.T) {
	fake := newFakeClara(t)
	fake.seed("123456789012")

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config:      integrationConfig(fake.url(), ""),
			ExpectError: regexp.MustCompile(`already has an integration`),
		}},
	})
}

func TestAwsIntegration_refusesAChangeAtPlanTime(t *testing.T) {
	fake := newFakeClara(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{Config: integrationConfig(fake.url(), "")},
			{
				// Not an in-place update and not a replacement: either would fail or duplicate.
				Config:      integrationConfig(fake.url(), `  cur_s3_prefix    = "changed"`+"\n"),
				ExpectError: regexp.MustCompile(`cannot be changed from Terraform`),
			},
		},
	})
}

func TestAwsIntegration_explainsAKeyWithoutThePermission(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusForbidden, "forbidden_scope", "Caller is missing required permissions")
	}))
	defer srv.Close()

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config:      integrationConfig(srv.URL, ""),
			ExpectError: regexp.MustCompile(`aws-onboarding:create`),
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
	if got := fake.requestLog(); len(got) != 0 {
		t.Fatalf("invalid input must not reach Clara, got %v", got)
	}
}

func TestProvider_requiresAnAPIKey(t *testing.T) {
	t.Setenv("CLARA_API_KEY", "")

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config:      `provider "clara" { api_base_url = "https://api.nops.io" }` + "\n" + integrationBody,
			ExpectError: regexp.MustCompile(`Missing Clara API key`),
		}},
	})
}

const integrationBody = `
resource "clara_aws_integration" "test" {
  payer_account_id = "123456789012"
  permission_mode  = "minimal"
  role_arn         = "arn:aws:iam::123456789012:role/nops-integration-role-42"
  external_id      = "11111111-2222-3333-4444-555555555555"
  cur_bucket_name  = "nops-cur-42"
}
`

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
`, fake.url()) + integrationBody,
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
			Config: integrationBody,
			Check:  resource.TestCheckResourceAttr("clara_aws_integration.test", "id", "int-1"),
		}},
	})
}

func TestProvider_neverWritesTheAPIKeyToState(t *testing.T) {
	fake := newFakeClara(t)
	t.Setenv("CLARA_API_KEY", testAPIKey)
	t.Setenv("CLARA_API_URL", fake.url())

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config: integrationBody,
			Check: func(s *terraform.State) error {
				raw, _ := json.Marshal(s)
				if strings.Contains(string(raw), testAPIKey) {
					return fmt.Errorf("the API key must never be written to state")
				}
				return nil
			},
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

func testClient(url string) *apiClient {
	return &apiClient{baseURL: url, apiKey: "k", userAgent: "t", http: http.DefaultClient, retryBackoff: time.Millisecond}
}

func TestClient_retriesOnRateLimitWithTheSameIdempotencyKey(t *testing.T) {
	var calls int32
	var mu sync.Mutex
	var keys []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		mu.Unlock()
		if atomic.AddInt32(&calls, 1) <= 2 {
			writeError(w, http.StatusTooManyRequests, "rate_limited", "slow down")
			return
		}
		writeJSON(w, 201, map[string]any{"data": map[string]any{"id": "int-9", "status": "active"}})
	}))
	defer srv.Close()

	got, err := testClient(srv.URL).onboardAccount(context.Background(), onboardRequest{}, "key-1")
	if err != nil {
		t.Fatalf("expected success after retries, got %v", err)
	}
	if got.ID != "int-9" || atomic.LoadInt32(&calls) != 3 {
		t.Fatalf("id=%q calls=%d", got.ID, calls)
	}
	for _, k := range keys {
		if k != "key-1" {
			t.Fatalf("every attempt must reuse the idempotency key, got %v", keys)
		}
	}
}

func TestClient_givesUpAfterRetries(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusTooManyRequests, "rate_limited", "slow down")
	}))
	defer srv.Close()

	_, err := testClient(srv.URL).onboardAccount(context.Background(), onboardRequest{}, "k")
	if statusOf(err) != http.StatusTooManyRequests {
		t.Fatalf("expected a 429 apiError, got %v", err)
	}
}

func TestClient_parsesErrorEnvelope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusForbidden, "forbidden_scope", "Caller is missing required permissions")
	}))
	defer srv.Close()

	_, err := testClient(srv.URL).onboardAccount(context.Background(), onboardRequest{}, "k")
	apiErr, ok := err.(*apiError)
	if !ok || apiErr.Code != "forbidden_scope" || apiErr.RequestID != "req-test" {
		t.Fatalf("unexpected error: %#v", err)
	}
}

func TestClient_sendsEveryRequestToTheOnboardingEndpointOnly(t *testing.T) {
	fake := newFakeClara(t)
	client := testClient(fake.url())
	client.apiKey = testAPIKey
	if _, err := client.onboardAccount(context.Background(), onboardRequest{PayerAccountID: "123456789012"}, "k"); err != nil {
		t.Fatal(err)
	}
	if got := fake.requestLog(); len(got) != 1 || got[0] != onlyCreate {
		t.Fatalf("got %v", got)
	}
}
