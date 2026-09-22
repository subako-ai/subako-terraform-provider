package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/subako-ai/terraform-provider-subako/internal/client"
)

var (
	_ resource.ResourceWithConfigure   = (*modelProviderResource)(nil)
	_ resource.ResourceWithImportState = (*modelProviderResource)(nil)
)

func newModelProviderResource() resource.Resource {
	return &modelProviderResource{}
}

// modelProviderResource is a workspace's own model provider, bound to its
// own API key. The key is write-only: it never reaches the plan or the
// state, and the API never returns it, so `api_key_wo_version` is what names
// which key the provider holds, and bumping it rotates the key in place.
type modelProviderResource struct {
	client *client.Client
}

type modelProviderModel struct {
	ID            types.String `tfsdk:"id"`
	Format        types.String `tfsdk:"format"`
	DisplayName   types.String `tfsdk:"display_name"`
	Description   types.String `tfsdk:"description"`
	BaseURL       types.String `tfsdk:"base_url"`
	APIKey        types.String `tfsdk:"api_key_wo"`
	APIKeyVersion types.Int64  `tfsdk:"api_key_wo_version"`
}

func (r *modelProviderResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_model_provider"
}

func (r *modelProviderResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "A model provider bound to the workspace's own API key, which a bumped " +
			"`api_key_wo_version` rotates in place. Deleting it deletes it: agent versions that " +
			"still name it can no longer run. Use `create_before_destroy` on a change that does " +
			"replace one agents use -- a changed `format` -- so they publish versions naming the " +
			"new one first.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"format": schema.StringAttribute{
				Required:      true,
				Description:   "`anthropic` or `openai_responses`. The API cannot change it, so changing it replaces the provider.",
				Validators:    []validator.String{stringvalidator.OneOf(client.FormatAnthropic, client.FormatOpenAIResponses)},
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"display_name": schema.StringAttribute{
				Required: true,
				// CODESYNC(display-name-len)
				Validators: []validator.String{charsAtMost(200)},
			},
			"description": schema.StringAttribute{
				Optional: true,
				Computed: true,
				Default:  stringdefault.StaticString(""),
				// CODESYNC(description-len)
				Validators: []validator.String{charsAtMost(1024)},
			},
			"base_url": schema.StringAttribute{
				Required: true,
				Description: "The upstream base URL, as the format's own client spells it: " +
					"`https://api.anthropic.com` or `https://api.openai.com/v1`. The API does not " +
					"return it, so a change made outside Terraform goes unnoticed.",
				// CODESYNC(model-provider-base-url-len)
				Validators: []validator.String{charsBetween(1, 2048)},
			},
			"api_key_wo": schema.StringAttribute{
				Required:  true,
				Sensitive: true,
				WriteOnly: true,
				Description: "The upstream API key. Write-only: never stored in the plan or the state. " +
					"It travels when the provider is created and whenever `api_key_wo_version` changes.",
				// CODESYNC(model-provider-api-key-len)
				Validators: []validator.String{charsBetween(1, 16384)},
			},
			"api_key_wo_version": schema.Int64Attribute{
				Required: true,
				Description: "Which `api_key_wo` this provider holds. Bump it to send a new one, " +
					"which rotates the key in place: the provider keeps its id and the agent " +
					"versions naming it keep running. Required, so a rotation is always " +
					"expressible -- a changed `api_key_wo` alone is invisible to Terraform.",
				Validators: []validator.Int64{int64validator.AtLeast(0)},
			},
		},
	}
}

func (r *modelProviderResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = clientFrom(req.ProviderData, &resp.Diagnostics)
}

func setModelProvider(m *modelProviderModel, p *client.ModelProvider) {
	m.Format = types.StringValue(p.Format)
	m.DisplayName = types.StringValue(p.DisplayName)
	m.Description = types.StringValue(p.Description)
}

// The kinds of model provider `ModelProviderOwnerBody` tells apart.
const ownerWorkspace = "byok"

// read refreshes m from the server. found is false when the provider is gone.
// A platform provider is not one this resource manages, so reading one is a
// refusal rather than a value.
func (r *modelProviderResource) read(ctx context.Context, m *modelProviderModel) (found bool, diags diag.Diagnostics) {
	id := m.ID.ValueString()
	current, err := r.client.GetModelProvider(ctx, id)
	if client.IsNotFound(err) {
		return false, diags
	}
	if err != nil {
		apiError(&diags, "read model provider "+id, err)
		return true, diags
	}
	if current.Type != ownerWorkspace {
		diags.AddError("Not a workspace model provider",
			"Model provider "+id+" is a platform provider, which only the operator manages. "+
				"Read it with the subako_model_providers data source instead.")
		return true, diags
	}
	setModelProvider(m, current)
	m.APIKey = types.StringNull()
	return true, diags
}

func (r *modelProviderResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan, config modelProviderModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}
	provider, err := r.client.CreateModelProvider(ctx, client.NewModelProvider{
		Format:      plan.Format.ValueString(),
		DisplayName: plan.DisplayName.ValueString(),
		Description: plan.Description.ValueString(),
		BaseURL:     plan.BaseURL.ValueString(),
		APIKey:      config.APIKey.ValueString(),
	})
	if err != nil {
		apiError(&resp.Diagnostics, "create model provider", err)
		return
	}
	// Recorded before the read-back, so a failure there taints this provider
	// rather than orphaning it.
	created := plan
	created.ID = types.StringValue(provider.ID)
	created.APIKey = types.StringNull()
	next := created
	finishCreate(ctx, resp, subject{"Model provider", provider.ID}, r.read, next, created)
}

func (r *modelProviderResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	refresh(ctx, req, resp, r.read)
}

func (r *modelProviderResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, config, state modelProviderModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	id := state.ID.ValueString()
	update := client.ModelProviderUpdate{
		DisplayName: plan.DisplayName.ValueString(),
		Description: plan.Description.ValueString(),
		BaseURL:     plan.BaseURL.ValueString(),
	}
	// The key is write-only, so the version is the only thing that tells a
	// rotation from an apply leaving the key as it stands. A version the
	// state never held -- an imported provider's -- is a change like any
	// other, and the configuration's key becomes the provider's. The value
	// itself lives in the configuration, never in the plan.
	if !plannedInt64(plan.APIKeyVersion).Equal(plannedInt64(state.APIKeyVersion)) {
		key := config.APIKey.ValueString()
		update.APIKey = &key
	}
	run(&resp.Diagnostics, step{
		action: "update model provider " + id,
		call: func() error {
			_, err := r.client.UpdateModelProvider(ctx, id, update)
			return err
		},
	})

	next := plan
	next.ID = state.ID
	finishUpdate(ctx, resp, subject{"Model provider", id}, r.read, next, state)
}

func (r *modelProviderResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state modelProviderModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	id := state.ID.ValueString()
	remove(resp, "delete model provider "+id, func() error { return r.client.DeleteModelProvider(ctx, id) })
}

func (r *modelProviderResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}
