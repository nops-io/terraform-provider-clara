package provider

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/listplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ resource.Resource               = &awsIntegrationResource{}
	_ resource.ResourceWithConfigure  = &awsIntegrationResource{}
	_ resource.ResourceWithModifyPlan = &awsIntegrationResource{}

	accountIDPattern = regexp.MustCompile(`^[0-9]{12}$`)
	roleArnPattern   = regexp.MustCompile(`^arn:aws:iam::[0-9]{12}:role/.+$`)
	bucketPattern    = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
	namePattern      = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
	prefixPattern    = regexp.MustCompile(`^[0-9A-Za-z._/-]+$`)
)

// NewAwsIntegrationResource returns clara_aws_integration.
func NewAwsIntegrationResource() resource.Resource { return &awsIntegrationResource{} }

type awsIntegrationResource struct{ client *apiClient }

type awsIntegrationModel struct {
	ID                  types.String `tfsdk:"id"`
	PayerAccountID      types.String `tfsdk:"payer_account_id"`
	Name                types.String `tfsdk:"name"`
	PermissionMode      types.String `tfsdk:"permission_mode"`
	RoleArn             types.String `tfsdk:"role_arn"`
	ExternalID          types.String `tfsdk:"external_id"`
	CurBucketName       types.String `tfsdk:"cur_bucket_name"`
	CurReportName       types.String `tfsdk:"cur_report_name"`
	CurS3Prefix         types.String `tfsdk:"cur_s3_prefix"`
	Cur2ExportName      types.String `tfsdk:"cur2_export_name"`
	Cur2S3Prefix        types.String `tfsdk:"cur2_s3_prefix"`
	Status              types.String `tfsdk:"status"`
	VerificationState   types.String `tfsdk:"verification_state"`
	VerificationMessage types.String `tfsdk:"verification_message"`
	FailedChecks        types.List   `tfsdk:"failed_checks"`
}

func (r *awsIntegrationResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_aws_integration"
}

func (r *awsIntegrationResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	str := func(m ...planmodifier.String) []planmodifier.String { return m }
	keep := str(stringplanmodifier.UseStateForUnknown())

	resp.Schema = schema.Schema{
		Description: "Registers an AWS payer account with Clara after the AWS resources exist (see the nops-io/clara-onboarding/aws module). " +
			"This resource only creates. The API key it uses cannot read, change or delete anything in Clara, so a plan does not " +
			"compare against Clara, a changed argument is refused, and destroying the resource only forgets it: the integration stays " +
			"in Clara until you disconnect it there.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:      true,
				Description:   "Clara integration id.",
				PlanModifiers: keep,
			},
			"payer_account_id": schema.StringAttribute{
				Required:    true,
				Description: "12-digit payer (management) account id.",
				Validators: []validator.String{
					stringvalidator.RegexMatches(accountIDPattern, "must be a 12-digit AWS account id"),
				},
			},
			"name": schema.StringAttribute{
				Optional:      true,
				Computed:      true,
				Description:   "Name shown in Clara. Defaults to \"AWS Account <id>\".",
				PlanModifiers: keep,
			},
			"permission_mode": schema.StringAttribute{
				Required:    true,
				Description: "minimal (Savings Analysis) or full (Full Platform, Inform & Operate). Must match the role's attached policies.",
				Validators:  []validator.String{stringvalidator.OneOf("minimal", "full")},
			},
			"role_arn": schema.StringAttribute{
				Required:    true,
				Description: "ARN of the integration role created by the onboarding module. It must be a role in the payer account.",
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
				Optional:    true,
				Description: "Legacy CUR report name. When unset, Clara uses nops_cur_<client id>_hourly.",
				Validators: []validator.String{
					stringvalidator.RegexMatches(namePattern, "may contain letters, digits, underscores and hyphens"),
				},
			},
			"cur_s3_prefix": schema.StringAttribute{
				Optional:    true,
				Description: "Legacy CUR S3 prefix. When unset, Clara uses nops-cur.",
				Validators: []validator.String{
					stringvalidator.RegexMatches(prefixPattern, "may contain letters, digits, dots, underscores, slashes and hyphens"),
				},
			},
			"cur2_export_name": schema.StringAttribute{
				Optional:    true,
				Description: "CUR 2.0 export name. Setting it turns CUR 2.0 on for the integration.",
				Validators: []validator.String{
					stringvalidator.RegexMatches(namePattern, "may contain letters, digits, underscores and hyphens"),
				},
			},
			"cur2_s3_prefix": schema.StringAttribute{
				Optional:    true,
				Description: "CUR 2.0 S3 prefix. When unset, Clara uses nops-cur2.",
				Validators: []validator.String{
					stringvalidator.RegexMatches(prefixPattern, "may contain letters, digits, dots, underscores, slashes and hyphens"),
				},
			},
			"status": schema.StringAttribute{
				Computed:      true,
				Description:   "Integration status when it was created: active, validating or awaiting_customer_setup. It is not refreshed.",
				PlanModifiers: keep,
			},
			"verification_state": schema.StringAttribute{
				Computed: true,
				Description: "Result of Clara's access check at creation: verified, partial (Savings Analysis checks failed but cost and usage " +
					"ingest works), failed, or unavailable (the check could not run). It is not refreshed.",
				PlanModifiers: keep,
			},
			"verification_message": schema.StringAttribute{
				Computed:      true,
				Description:   "Why the role could not be assumed, when that was the failure.",
				PlanModifiers: keep,
			},
			"failed_checks": schema.ListAttribute{
				Computed:      true,
				ElementType:   types.StringType,
				Description:   "Permission checks that failed at creation. Empty when everything passed.",
				PlanModifiers: []planmodifier.List{listplanmodifier.UseStateForUnknown()},
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

// ModifyPlan refuses a change to an existing integration at plan time. The API key cannot update,
// and replacing would create a second integration for the same payer account, which Clara refuses.
func (r *awsIntegrationResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.State.Raw.IsNull() || req.Plan.Raw.IsNull() {
		return
	}

	var plan, state awsIntegrationModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var changed []string
	check := func(name string, planned, current types.String) {
		if planned.IsUnknown() {
			return
		}
		if planned.ValueString() != current.ValueString() || planned.IsNull() != current.IsNull() {
			changed = append(changed, name)
		}
	}
	check("payer_account_id", plan.PayerAccountID, state.PayerAccountID)
	check("permission_mode", plan.PermissionMode, state.PermissionMode)
	check("role_arn", plan.RoleArn, state.RoleArn)
	check("external_id", plan.ExternalID, state.ExternalID)
	check("cur_bucket_name", plan.CurBucketName, state.CurBucketName)
	check("cur_report_name", plan.CurReportName, state.CurReportName)
	check("cur_s3_prefix", plan.CurS3Prefix, state.CurS3Prefix)
	check("cur2_export_name", plan.Cur2ExportName, state.Cur2ExportName)
	check("cur2_s3_prefix", plan.Cur2S3Prefix, state.Cur2S3Prefix)
	// A name left unset keeps the one Clara chose.
	if !plan.Name.IsNull() {
		check("name", plan.Name, state.Name)
	}

	if len(changed) > 0 {
		resp.Diagnostics.AddError(
			"Clara integrations cannot be changed from Terraform",
			fmt.Sprintf(
				"%s changed for payer account %s. The API key used by this provider can only create integrations, so it cannot update "+
					"this one, and creating it again would be refused because the account is already connected. Change the integration "+
					"in Clara, then put the original values back here.",
				strings.Join(changed, ", "), state.PayerAccountID.ValueString(),
			),
		)
	}
}

func (m *awsIntegrationModel) toRequest() onboardRequest {
	req := onboardRequest{
		PayerAccountID: m.PayerAccountID.ValueString(),
		Name:           m.Name.ValueString(),
		RoleArn:        m.RoleArn.ValueString(),
		ExternalID:     m.ExternalID.ValueString(),
		PermissionMode: m.PermissionMode.ValueString(),
		Cur: onboardCur{
			BucketName: m.CurBucketName.ValueString(),
			ReportName: m.CurReportName.ValueString(),
			S3Prefix:   m.CurS3Prefix.ValueString(),
		},
	}
	if export := strings.TrimSpace(m.Cur2ExportName.ValueString()); export != "" {
		req.Cur2 = &onboardCur2{Enabled: true, ExportName: export, S3Prefix: m.Cur2S3Prefix.ValueString()}
	}
	return req
}

func newIdempotencyKey() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func (r *awsIntegrationResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan awsIntegrationModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// One key per create. The client reuses it across retries, so a lost response cannot register twice.
	key, err := newIdempotencyKey()
	if err != nil {
		resp.Diagnostics.AddError("Could not create an idempotency key", err.Error())
		return
	}

	result, err := r.client.onboardAccount(ctx, plan.toRequest(), key)
	if err != nil {
		switch statusOf(err) {
		case http.StatusConflict:
			resp.Diagnostics.AddError(
				"Clara already has an integration for this account",
				fmt.Sprintf(
					"Payer account %s is already connected. This provider's API key can only create integrations, so it cannot adopt or "+
						"change the existing one. If it was onboarded on purpose, remove this resource from the configuration. If it is a "+
						"leftover, disconnect it in Clara first. (%s)",
					plan.PayerAccountID.ValueString(), err,
				),
			)
		case http.StatusForbidden:
			resp.Diagnostics.AddError(
				"The Clara API key is not allowed to onboard accounts",
				"The key needs the aws-onboarding:create permission, and the organization needs API access. "+err.Error(),
			)
		default:
			resp.Diagnostics.AddError("Could not create the Clara integration", err.Error())
		}
		return
	}

	checks := result.Verification.FailedChecks
	if checks == nil {
		checks = []string{}
	}
	failed, diags := types.ListValueFrom(ctx, types.StringType, checks)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	plan.ID = types.StringValue(result.ID)
	plan.Status = types.StringValue(result.Status)
	plan.Name = types.StringValue(result.Name)
	plan.VerificationState = types.StringValue(result.Verification.State)
	plan.FailedChecks = failed
	plan.VerificationMessage = types.StringValue("")
	if result.Verification.Message != nil {
		plan.VerificationMessage = types.StringValue(*result.Verification.Message)
	}

	// The integration exists now, so keep it in state whatever the verification said. Failing here
	// would taint the resource, and applying again would be refused as a duplicate.
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)

	switch result.Verification.State {
	case "verified":
	case "partial":
		resp.Diagnostics.AddWarning(
			"Clara could only partly verify the integration",
			fmt.Sprintf("Cost and usage ingest works, but these Savings Analysis checks failed: %s. Open the integration in Clara for details.", strings.Join(result.Verification.FailedChecks, ", ")),
		)
	case "failed":
		detail := "Clara could not confirm access to the account. The integration was created and is waiting for setup. Open it in Clara to see which checks failed and to verify again."
		if result.Verification.Message != nil && *result.Verification.Message != "" {
			detail += " Clara reported: " + *result.Verification.Message
		}
		resp.Diagnostics.AddWarning("Clara could not verify the integration", detail)
	default:
		resp.Diagnostics.AddWarning(
			"Clara could not run its access check",
			"The integration was created. Open it in Clara and verify it from there.",
		)
	}
}

// Read keeps the state as it is. The API key cannot read integrations, so there is nothing to compare against.
func (r *awsIntegrationResource) Read(_ context.Context, _ resource.ReadRequest, _ *resource.ReadResponse) {
}

// Update is unreachable: ModifyPlan refuses every change.
func (r *awsIntegrationResource) Update(_ context.Context, _ resource.UpdateRequest, resp *resource.UpdateResponse) {
	resp.Diagnostics.AddError("Clara integrations cannot be changed from Terraform", "This provider's API key can only create integrations.")
}

// Delete forgets the resource. The API key cannot delete or disconnect anything.
func (r *awsIntegrationResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state awsIntegrationModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)

	resp.Diagnostics.AddWarning(
		"The integration stays in Clara",
		fmt.Sprintf(
			"Terraform stopped tracking payer account %s. The API key used by this provider cannot disconnect it, so it is still connected in Clara. "+
				"Disconnect it in Clara if it should go. Nothing was deleted in AWS.",
			state.PayerAccountID.ValueString(),
		),
	)
}
