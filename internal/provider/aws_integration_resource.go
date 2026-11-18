package provider

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64default"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ resource.Resource                = &awsIntegrationResource{}
	_ resource.ResourceWithConfigure   = &awsIntegrationResource{}
	_ resource.ResourceWithImportState = &awsIntegrationResource{}

	accountIDPattern = regexp.MustCompile(`^[0-9]{12}$`)
	roleArnPattern   = regexp.MustCompile(`^arn:aws:iam::[0-9]{12}:role/.+$`)
	bucketPattern    = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
)

// NewAwsIntegrationResource returns clara_aws_integration.
func NewAwsIntegrationResource() resource.Resource { return &awsIntegrationResource{} }

type awsIntegrationResource struct{ client *apiClient }

type awsIntegrationModel struct {
	ID                         types.String `tfsdk:"id"`
	PayerAccountID             types.String `tfsdk:"payer_account_id"`
	Name                       types.String `tfsdk:"name"`
	PermissionMode             types.String `tfsdk:"permission_mode"`
	RoleArn                    types.String `tfsdk:"role_arn"`
	ExternalID                 types.String `tfsdk:"external_id"`
	CurBucketName              types.String `tfsdk:"cur_bucket_name"`
	CurReportName              types.String `tfsdk:"cur_report_name"`
	CurS3Prefix                types.String `tfsdk:"cur_s3_prefix"`
	Cur2ExportName             types.String `tfsdk:"cur2_export_name"`
	Cur2S3Prefix               types.String `tfsdk:"cur2_s3_prefix"`
	WaitForVerification        types.Bool   `tfsdk:"wait_for_verification"`
	VerificationTimeoutSeconds types.Int64  `tfsdk:"verification_timeout_seconds"`
	Status                     types.String `tfsdk:"status"`
}

func (r *awsIntegrationResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_aws_integration"
}

func (r *awsIntegrationResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	str := func(m ...planmodifier.String) []planmodifier.String { return m }

	resp.Schema = schema.Schema{
		Description: "Registers an AWS payer account with Clara after the AWS resources exist (see the nops-io/clara-onboarding/aws module). " +
			"Destroying this resource disconnects the integration in Clara. It does not delete anything in AWS.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:      true,
				Description:   "Clara integration id.",
				PlanModifiers: str(stringplanmodifier.UseStateForUnknown()),
			},
			"payer_account_id": schema.StringAttribute{
				Required:      true,
				Description:   "12-digit payer (management) account id. Changing it creates a new integration.",
				PlanModifiers: str(stringplanmodifier.RequiresReplace()),
				Validators: []validator.String{
					stringvalidator.RegexMatches(accountIDPattern, "must be a 12-digit AWS account id"),
				},
			},
			"name": schema.StringAttribute{
				Optional:      true,
				Computed:      true,
				Description:   "Name shown in Clara. Defaults to \"AWS Account <id>\".",
				PlanModifiers: str(stringplanmodifier.UseStateForUnknown()),
			},
			"permission_mode": schema.StringAttribute{
				Required:    true,
				Description: "minimal (Savings Analysis) or full (Full Platform, Inform & Operate). Must match the role's attached policies.",
				Validators:  []validator.String{stringvalidator.OneOf("minimal", "full")},
			},
			"role_arn": schema.StringAttribute{
				Required:    true,
				Description: "ARN of the integration role created by the onboarding module.",
				Validators: []validator.String{
					stringvalidator.RegexMatches(roleArnPattern, "must be an IAM role ARN"),
				},
			},
			"external_id": schema.StringAttribute{
				Required:    true,
				Sensitive:   true,
				Description: "ExternalId from the role's trust policy. Sent to Clara, never returned by it.",
				Validators:  []validator.String{stringvalidator.LengthAtLeast(8)},
			},
			"cur_bucket_name": schema.StringAttribute{
				Required:    true,
				Description: "S3 bucket that receives the Legacy CUR (and CUR 2.0).",
				Validators: []validator.String{
					stringvalidator.RegexMatches(bucketPattern, "must be a valid S3 bucket name"),
				},
			},
			"cur_report_name": schema.StringAttribute{
				Optional:      true,
				Computed:      true,
				Description:   "Legacy CUR report name. Defaults to nops_cur_<client id>_hourly.",
				PlanModifiers: str(stringplanmodifier.UseStateForUnknown()),
			},
			"cur_s3_prefix": schema.StringAttribute{
				Optional:      true,
				Computed:      true,
				Description:   "Legacy CUR S3 prefix. Defaults to nops-cur.",
				PlanModifiers: str(stringplanmodifier.UseStateForUnknown()),
			},
			"cur2_export_name": schema.StringAttribute{
				Optional:    true,
				Description: "CUR 2.0 export name. Setting it turns CUR 2.0 on for the integration.",
			},
			"cur2_s3_prefix": schema.StringAttribute{
				Optional:      true,
				Computed:      true,
				Description:   "CUR 2.0 S3 prefix. Defaults to nops-cur2.",
				PlanModifiers: str(stringplanmodifier.UseStateForUnknown()),
			},
			"wait_for_verification": schema.BoolAttribute{
				Optional:    true,
				Computed:    true,
				Default:     booldefault.StaticBool(true),
				Description: "Run Clara's access verification after create and update, and fail unless the integration becomes active. Defaults to true.",
			},
			"verification_timeout_seconds": schema.Int64Attribute{
				Optional:    true,
				Computed:    true,
				Default:     int64default.StaticInt64(300),
				Description: "How long to wait for verification. Defaults to 300.",
				Validators:  []validator.Int64{int64validator.Between(30, 3600)},
			},
			"status": schema.StringAttribute{
				Computed:    true,
				Description: "Integration status in Clara, for example active, validating or awaiting_customer_setup.",
			},
		},
	}
}

func (r *awsIntegrationResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	client, ok := req.ProviderData.(*apiClient)
	if !ok {
		resp.Diagnostics.AddError("Unexpected provider data", fmt.Sprintf("Expected *apiClient, got %T.", req.ProviderData))
		return
	}
	r.client = client
}

func (m *awsIntegrationModel) toRequest() awsIntegrationRequest {
	cur2 := strings.TrimSpace(m.Cur2ExportName.ValueString())
	return awsIntegrationRequest{
		PayerAccountID: m.PayerAccountID.ValueString(),
		Name:           m.Name.ValueString(),
		PermissionMode: m.PermissionMode.ValueString(),
		RoleArn:        m.RoleArn.ValueString(),
		ExternalID:     m.ExternalID.ValueString(),
		CurBucketName:  m.CurBucketName.ValueString(),
		CurReportName:  m.CurReportName.ValueString(),
		CurS3Prefix:    m.CurS3Prefix.ValueString(),
		Cur2Enabled:    cur2 != "",
		Cur2ExportName: cur2,
		Cur2S3Prefix:   m.Cur2S3Prefix.ValueString(),
	}
}

// valueIfUnknown keeps a configured value and fills a computed one from the server.
func valueIfUnknown(current types.String, server string) types.String {
	if current.IsUnknown() || current.IsNull() {
		return types.StringValue(server)
	}
	return current
}

// applyServerValues records what Clara returned after create or update without
// overriding anything the user configured.
func (m *awsIntegrationModel) applyServerValues(in *awsIntegration) {
	m.ID = types.StringValue(string(in.ID))
	m.Status = types.StringValue(in.Status)
	m.Name = valueIfUnknown(m.Name, in.Name)
	m.CurReportName = valueIfUnknown(m.CurReportName, in.CurReportName)
	m.CurS3Prefix = valueIfUnknown(m.CurS3Prefix, in.CurS3Prefix)
	m.Cur2S3Prefix = valueIfUnknown(m.Cur2S3Prefix, in.Cur2S3Prefix)
}

// refreshFromServer overwrites state with Clara's view so drift shows up in plans.
func (m *awsIntegrationModel) refreshFromServer(in *awsIntegration) {
	m.ID = types.StringValue(string(in.ID))
	m.Status = types.StringValue(in.Status)
	m.PayerAccountID = types.StringValue(in.PayerAccountID)
	m.Name = types.StringValue(in.Name)
	m.PermissionMode = types.StringValue(in.PermissionMode)
	m.RoleArn = types.StringValue(in.RoleArn)
	m.CurBucketName = types.StringValue(in.CurBucketName)
	m.CurReportName = types.StringValue(in.CurReportName)
	m.CurS3Prefix = types.StringValue(in.CurS3Prefix)
	m.Cur2S3Prefix = types.StringValue(in.Cur2S3Prefix)
	if in.Cur2Enabled {
		m.Cur2ExportName = types.StringValue(in.Cur2ExportName)
	} else {
		m.Cur2ExportName = types.StringNull()
	}
}

// verify runs Clara's check and waits for a final status. It updates m.Status.
func (r *awsIntegrationResource) verify(ctx context.Context, m *awsIntegrationModel) error {
	if !m.WaitForVerification.ValueBool() {
		return nil
	}
	timeout := time.Duration(m.VerificationTimeoutSeconds.ValueInt64()) * time.Second
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	id := m.ID.ValueString()
	status, err := r.client.verifyIntegration(ctx, id)
	if err != nil {
		return fmt.Errorf("running verification: %w", err)
	}

	for {
		m.Status = types.StringValue(status)
		switch status {
		case "active":
			return nil
		case "validating", "pending", "":
			// Still being checked: keep polling.
		default:
			return fmt.Errorf("verification finished with status %q. Clara could not confirm access to the account. Open the integration in Clara to see which checks failed, fix them, and apply again", status)
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("the integration is still %q after %s. Apply again, or set wait_for_verification = false", status, timeout)
		case <-time.After(r.client.pollInterval):
		}

		current, err := r.client.getIntegration(ctx, id)
		if err != nil {
			if ctx.Err() != nil {
				return fmt.Errorf("the integration is still %q after %s. Apply again, or set wait_for_verification = false", status, timeout)
			}
			return fmt.Errorf("checking the integration status: %w", err)
		}
		status = current.Status
	}
}

func (r *awsIntegrationResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan awsIntegrationModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	body := plan.toRequest()
	body.Method = "terraform"

	integration, err := r.client.createIntegration(ctx, body)
	if err != nil {
		if statusOf(err) != http.StatusConflict {
			resp.Diagnostics.AddError("Could not create the Clara integration", err.Error())
			return
		}
		// One integration per payer account. Take over the existing one.
		existing, findErr := r.client.findIntegration(ctx, plan.PayerAccountID.ValueString())
		if findErr != nil || existing == nil {
			detail := "Clara says an integration already exists for this payer account, but it could not be looked up."
			if findErr != nil {
				detail += " " + findErr.Error()
			}
			resp.Diagnostics.AddError("Could not adopt the existing Clara integration", detail)
			return
		}
		integration, err = r.client.updateIntegration(ctx, string(existing.ID), body)
		if err != nil {
			resp.Diagnostics.AddError("Could not update the existing Clara integration", err.Error())
			return
		}
		resp.Diagnostics.AddWarning(
			"Adopted an existing Clara integration",
			fmt.Sprintf("Payer account %s already had Clara integration %s. It now follows this configuration.", plan.PayerAccountID.ValueString(), existing.ID),
		)
	}

	plan.applyServerValues(integration)
	// Save before verifying so a failed verification taints the resource instead of losing it.
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if err := r.verify(ctx, &plan); err != nil {
		resp.Diagnostics.AddError("Clara could not verify the integration", err.Error())
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

func (r *awsIntegrationResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state awsIntegrationModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	integration, err := r.client.getIntegration(ctx, state.ID.ValueString())
	if err != nil {
		if statusOf(err) == http.StatusNotFound {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("Could not read the Clara integration", err.Error())
		return
	}
	if integration.Status == "disconnected" {
		resp.State.RemoveResource(ctx)
		return
	}

	state.refreshFromServer(integration)
	resp.Diagnostics.Append(resp.State.Set(ctx, state)...)
}

func (r *awsIntegrationResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state awsIntegrationModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	plan.ID = state.ID

	integration, err := r.client.updateIntegration(ctx, state.ID.ValueString(), plan.toRequest())
	if err != nil {
		resp.Diagnostics.AddError("Could not update the Clara integration", err.Error())
		return
	}

	plan.applyServerValues(integration)
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if err := r.verify(ctx, &plan); err != nil {
		resp.Diagnostics.AddError("Clara could not verify the integration", err.Error())
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

func (r *awsIntegrationResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state awsIntegrationModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if err := r.client.disconnectIntegration(ctx, state.ID.ValueString()); err != nil && statusOf(err) != http.StatusNotFound {
		resp.Diagnostics.AddError("Could not disconnect the Clara integration", err.Error())
	}
}

func (r *awsIntegrationResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("wait_for_verification"), true)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("verification_timeout_seconds"), int64(300))...)
}
