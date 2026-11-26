package provider

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

const defaultAPIBaseURL = "https://api.nops.io"

var _ provider.Provider = &claraProvider{}

// claraProvider registers AWS accounts with nOps Clara through the public API.
type claraProvider struct {
	version string

	// Test hook. Zero means the production default.
	retryBackoff time.Duration
}

type claraProviderModel struct {
	APIBaseURL types.String `tfsdk:"api_base_url"`
	APIKey     types.String `tfsdk:"api_key"`
}

// New returns the provider factory used by main.
func New(version string) func() provider.Provider {
	return func() provider.Provider {
		return &claraProvider{version: version}
	}
}

func (p *claraProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "clara"
	resp.Version = p.version
}

func (p *claraProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Registers AWS accounts with nOps Clara. Authenticate with a Clara API key that has only the aws-onboarding:create permission. That key can register new accounts and cannot read, change or delete anything.",
		Attributes: map[string]schema.Attribute{
			"api_base_url": schema.StringAttribute{
				Optional:    true,
				Description: "Clara API base URL. Defaults to " + defaultAPIBaseURL + ". Can also be set with CLARA_API_URL. Must be https. Plain http is accepted only for localhost, for local testing.",
			},
			"api_key": schema.StringAttribute{
				Optional:    true,
				Sensitive:   true,
				Description: "Clara API key. Prefer the CLARA_API_KEY environment variable so the key never appears in configuration files.",
			},
		},
	}
}

func (p *claraProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var cfg claraProviderModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if cfg.APIKey.IsUnknown() || cfg.APIBaseURL.IsUnknown() {
		resp.Diagnostics.AddError(
			"Unknown provider configuration",
			"api_key and api_base_url must be known when the provider is configured. Use the CLARA_API_KEY and CLARA_API_URL environment variables, or static values.",
		)
		return
	}

	baseURL := firstNonEmpty(cfg.APIBaseURL.ValueString(), os.Getenv("CLARA_API_URL"), defaultAPIBaseURL)
	apiKey := firstNonEmpty(cfg.APIKey.ValueString(), os.Getenv("CLARA_API_KEY"))

	if apiKey == "" {
		resp.Diagnostics.AddAttributeError(
			path.Root("api_key"),
			"Missing Clara API key",
			"Set the CLARA_API_KEY environment variable, or the api_key provider argument.",
		)
	}
	if err := validateBaseURL(baseURL); err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("api_base_url"), "Invalid Clara API URL", err.Error())
	}
	if resp.Diagnostics.HasError() {
		return
	}

	client := &apiClient{
		baseURL:      strings.TrimRight(baseURL, "/"),
		apiKey:       apiKey,
		userAgent:    fmt.Sprintf("%s/%s", userAgentName, p.version),
		http:         &http.Client{Timeout: 60 * time.Second},
		retryBackoff: orDuration(p.retryBackoff, time.Second),
	}
	resp.ResourceData = client
}

func (p *claraProvider) DataSources(_ context.Context) []func() datasource.DataSource {
	return nil
}

func (p *claraProvider) Resources(_ context.Context) []func() resource.Resource {
	return []func() resource.Resource{NewAwsIntegrationResource}
}

const userAgentName = "terraform-provider-clara"

// validateBaseURL refuses plain http except on loopback, so an API key is never sent in clear text.
func validateBaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("%q is not a valid URL", raw)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		host := u.Hostname()
		if host == "localhost" {
			return nil
		}
		if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
			return nil
		}
		return fmt.Errorf("%q uses http. The API key would be sent in clear text; use https (http is allowed only for localhost)", raw)
	default:
		return fmt.Errorf("%q must start with https://", raw)
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func orDuration(v, fallback time.Duration) time.Duration {
	if v > 0 {
		return v
	}
	return fallback
}
