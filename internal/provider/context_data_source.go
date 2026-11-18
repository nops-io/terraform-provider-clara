package provider

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ datasource.DataSource              = &contextDataSource{}
	_ datasource.DataSourceWithConfigure = &contextDataSource{}
)

// NewContextDataSource returns clara_context.
func NewContextDataSource() datasource.DataSource { return &contextDataSource{} }

type contextDataSource struct{ client *apiClient }

type contextModel struct {
	ClientID       types.String `tfsdk:"client_id"`
	OrganizationID types.String `tfsdk:"organization_id"`
}

func (d *contextDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_context"
}

func (d *contextDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "The Clara client and organization the API key belongs to. Use client_id as the client_id input of the onboarding module.",
		Attributes: map[string]schema.Attribute{
			"client_id":       schema.StringAttribute{Computed: true, Description: "Clara client id."},
			"organization_id": schema.StringAttribute{Computed: true, Description: "Clara organization id."},
		},
	}
}

func (d *contextDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	client, ok := req.ProviderData.(*apiClient)
	if !ok {
		resp.Diagnostics.AddError("Unexpected provider data", fmt.Sprintf("Expected *apiClient, got %T.", req.ProviderData))
		return
	}
	d.client = client
}

func (d *contextDataSource) Read(ctx context.Context, _ datasource.ReadRequest, resp *datasource.ReadResponse) {
	out, err := d.client.getContext(ctx)
	if err != nil {
		resp.Diagnostics.AddError("Could not read the Clara context", err.Error())
		return
	}
	state := contextModel{
		ClientID:       types.StringValue(string(out.ClientID)),
		OrganizationID: types.StringValue(string(out.OrganizationID)),
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}
