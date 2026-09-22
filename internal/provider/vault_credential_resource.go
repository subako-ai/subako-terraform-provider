package provider

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-jsontypes/jsontypes"
	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/resourcevalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/objectplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"

	"github.com/subako-ai/subako-terraform-provider/internal/client"
)

var (
	_ resource.ResourceWithConfigure        = (*vaultCredentialResource)(nil)
	_ resource.ResourceWithImportState      = (*vaultCredentialResource)(nil)
	_ resource.ResourceWithConfigValidators = (*vaultCredentialResource)(nil)
)

func newVaultCredentialResource() resource.Resource {
	return &vaultCredentialResource{}
}

// vaultCredentialResource is one credential in a vault. The API stores a
// credential once and never returns its secret, so every change replaces it
// and the secret attributes are write-only.
type vaultCredentialResource struct {
	client *client.Client
}

type vaultCredentialModel struct {
	ID             types.String `tfsdk:"id"`
	VaultID        types.String `tfsdk:"vault_id"`
	Target         types.String `tfsdk:"target"`
	DisplayName    types.String `tfsdk:"display_name"`
	StaticBearer   types.Object `tfsdk:"static_bearer"`
	OAuth          types.Object `tfsdk:"oauth"`
	SecretsVersion types.Int64  `tfsdk:"secrets_wo_version"`
}

// The auth scheme's settings, one nested attribute per scheme, of which the
// schema's ExactlyOneOf leaves exactly one set.
type (
	staticBearerConfig struct {
		Token types.String `tfsdk:"token_wo"`
	}
	oauthConfig struct {
		AccessToken types.String   `tfsdk:"access_token_wo"`
		ExpiresAt   types.String   `tfsdk:"expires_at"`
		Refresh     *refreshConfig `tfsdk:"refresh"`
	}
	refreshConfig struct {
		TokenEndpoint     types.String         `tfsdk:"token_endpoint"`
		ClientID          types.String         `tfsdk:"client_id"`
		Scope             types.String         `tfsdk:"scope"`
		RefreshToken      types.String         `tfsdk:"refresh_token_wo"`
		TokenEndpointAuth jsontypes.Normalized `tfsdk:"token_endpoint_auth_wo"`
	}
)

var (
	refreshTypes = map[string]attr.Type{
		"token_endpoint":         types.StringType,
		"client_id":              types.StringType,
		"scope":                  types.StringType,
		"refresh_token_wo":       types.StringType,
		"token_endpoint_auth_wo": jsontypes.NormalizedType{},
	}
	staticBearerTypes = map[string]attr.Type{
		"token_wo": types.StringType,
	}
	oauthTypes = map[string]attr.Type{
		"access_token_wo": types.StringType,
		"expires_at":      types.StringType,
		"refresh":         types.ObjectType{AttrTypes: refreshTypes},
	}
)

// How the vault authenticates to the token endpoint when the configuration
// names no method.
const defaultTokenEndpointAuth = `{"type":"none"}`

// The protocol a credential's target speaks, as `ProtocolBody` spells it.
const protocolMCP = "mcp"

// jsonObjectValidator refuses a JSON value that is not an object, which is
// what the token endpoint auth is on the wire. The attribute's own type has
// already refused anything that is not JSON at all.
type jsonObjectValidator struct{}

func (jsonObjectValidator) Description(context.Context) string {
	return "must be a JSON object"
}

func (v jsonObjectValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (v jsonObjectValidator) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	encoded, ok := plannedString(req.ConfigValue).Get()
	if !ok {
		return
	}
	var object map[string]any
	if err := json.Unmarshal([]byte(encoded), &object); err != nil || object == nil {
		resp.Diagnostics.AddAttributeError(req.Path, "Not a JSON object",
			"This setting takes a JSON object, such as "+defaultTokenEndpointAuth+".")
	}
}

func (r *vaultCredentialResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_vault_credential"
}

func (r *vaultCredentialResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	replaceString := []planmodifier.String{stringplanmodifier.RequiresReplace()}
	replaceObject := []planmodifier.Object{objectplanmodifier.RequiresReplace()}
	resp.Schema = schema.Schema{
		Description: "A credential a vault presents to one MCP server. Credentials cannot be changed " +
			"in place, so any change replaces this one.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"vault_id": schema.StringAttribute{
				Required:      true,
				PlanModifiers: replaceString,
			},
			"target": schema.StringAttribute{
				Required:    true,
				Description: "The exact MCP server URL the credential authenticates against. Unique within the vault.",
				// CODESYNC(target-len)
				Validators:    []validator.String{charsBetween(1, 2048)},
				PlanModifiers: replaceString,
			},
			"display_name": schema.StringAttribute{
				Optional: true,
				Computed: true,
				Default:  stringdefault.StaticString(""),
				// CODESYNC(display-name-len)
				Validators:    []validator.String{charsAtMost(200)},
				PlanModifiers: replaceString,
			},
			"static_bearer": schema.SingleNestedAttribute{
				Optional:      true,
				Description:   "A bearer token supplied once. Exactly one of this and `oauth`.",
				PlanModifiers: replaceObject,
				Attributes: map[string]schema.Attribute{
					"token_wo": schema.StringAttribute{
						Required:    true,
						Sensitive:   true,
						WriteOnly:   true,
						Description: "The bearer token. Write-only.",
					},
				},
			},
			"oauth": schema.SingleNestedAttribute{
				Optional: true,
				Description: "An OAuth access token, and what the vault needs to refresh it. " +
					"Exactly one of this and `static_bearer`.",
				PlanModifiers: replaceObject,
				Attributes: map[string]schema.Attribute{
					"access_token_wo": schema.StringAttribute{
						Required:    true,
						Sensitive:   true,
						WriteOnly:   true,
						Description: "The OAuth access token. Write-only.",
					},
					"expires_at": schema.StringAttribute{
						Optional:    true,
						Description: "When the access token expires, as RFC 3339.",
					},
					"refresh": schema.SingleNestedAttribute{
						Optional:    true,
						Description: "Set this to have the vault refresh the access token itself.",
						Attributes: map[string]schema.Attribute{
							"token_endpoint": schema.StringAttribute{
								Required:    true,
								Description: "Where the vault refreshes the access token.",
							},
							"client_id": schema.StringAttribute{
								Required: true,
							},
							"scope": schema.StringAttribute{
								Optional: true,
							},
							"refresh_token_wo": schema.StringAttribute{
								Required:    true,
								Sensitive:   true,
								WriteOnly:   true,
								Description: "The refresh token. Write-only.",
							},
							"token_endpoint_auth_wo": schema.StringAttribute{
								Optional:   true,
								Sensitive:  true,
								WriteOnly:  true,
								CustomType: jsontypes.NormalizedType{},
								Description: "How the vault authenticates to the token endpoint, as a JSON " +
									"object. Write-only. Defaults to `" + defaultTokenEndpointAuth + "`.",
								Validators: []validator.String{jsonObjectValidator{}},
							},
						},
					},
				},
			},
			"secrets_wo_version": schema.Int64Attribute{
				Required: true,
				Description: "Which write-only secrets this credential holds. Bump it to send new " +
					"ones, which replaces the credential. Required, so a rotation is always " +
					"expressible -- a changed secret alone is invisible to Terraform.",
				Validators:    []validator.Int64{int64validator.AtLeast(0)},
				PlanModifiers: []planmodifier.Int64{int64planmodifier.RequiresReplace()},
			},
		},
	}
}

// ConfigValidators states the one rule the schema cannot: a credential
// authenticates one way, so the two nested attributes are alternatives.
func (r *vaultCredentialResource) ConfigValidators(_ context.Context) []resource.ConfigValidator {
	return []resource.ConfigValidator{
		resourcevalidator.ExactlyOneOf(
			path.MatchRoot("static_bearer"),
			path.MatchRoot("oauth"),
		),
	}
}

func (r *vaultCredentialResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = clientFrom(req.ProviderData, &resp.Diagnostics)
}

// payload reads the secret the configuration carries. Only the config holds
// write-only values, so this never reads the plan or the state.
func (m vaultCredentialModel) payload(ctx context.Context) (client.CredentialPayload, diag.Diagnostics) {
	var diags diag.Diagnostics
	_, bearer := planned(m.StaticBearer).Get()
	_, oauth := planned(m.OAuth).Get()
	switch {
	case bearer && !oauth:
		var config staticBearerConfig
		diags.Append(m.StaticBearer.As(ctx, &config, basetypes.ObjectAsOptions{})...)
		return client.StaticBearerPayload{Token: config.Token.ValueString()}, diags

	case oauth && !bearer:
		var config oauthConfig
		diags.Append(m.OAuth.As(ctx, &config, basetypes.ObjectAsOptions{})...)
		if diags.HasError() {
			return nil, diags
		}
		payload := client.OAuthPayload{
			AccessToken: config.AccessToken.ValueString(),
			ExpiresAt:   stringPointer(config.ExpiresAt),
		}
		if refresh := config.Refresh; refresh != nil {
			auth := json.RawMessage(defaultTokenEndpointAuth)
			if named, ok := plannedString(refresh.TokenEndpointAuth.StringValue).Get(); ok {
				auth = json.RawMessage(named)
			}
			payload.Refresh = &client.RefreshBlock{
				TokenEndpoint:     refresh.TokenEndpoint.ValueString(),
				ClientID:          refresh.ClientID.ValueString(),
				Scope:             stringPointer(refresh.Scope),
				RefreshToken:      refresh.RefreshToken.ValueString(),
				TokenEndpointAuth: auth,
			}
		}
		return payload, diags
	}
	diags.AddError("Ambiguous credential", "Set exactly one of static_bearer and oauth.")
	return nil, diags
}

// read refreshes m from the server. The server returns a credential's
// metadata only, so the scheme it names decides which variant the state
// holds; a variant the state already spells keeps the settings it carries,
// none of which the server would report.
func (r *vaultCredentialResource) read(ctx context.Context, m *vaultCredentialModel) (found bool, diags diag.Diagnostics) {
	vaultID, id := m.VaultID.ValueString(), m.ID.ValueString()
	credential, err := r.client.FindCredential(ctx, vaultID, id)
	if client.IsNotFound(err) {
		return false, diags
	}
	if err != nil {
		apiError(&diags, "read credential "+id, err)
		return true, diags
	}
	m.Target = types.StringValue(credential.Target)
	m.DisplayName = types.StringValue(credential.DisplayName)

	switch credential.AuthScheme {
	case client.AuthStaticBearer:
		if _, ok := planned(m.StaticBearer).Get(); !ok {
			m.StaticBearer = types.ObjectValueMust(staticBearerTypes, map[string]attr.Value{
				"token_wo": types.StringNull(),
			})
		}
		m.OAuth = types.ObjectNull(oauthTypes)
	case client.AuthOAuth:
		if _, ok := planned(m.OAuth).Get(); !ok {
			m.OAuth = types.ObjectValueMust(oauthTypes, map[string]attr.Value{
				"access_token_wo": types.StringNull(),
				"expires_at":      types.StringNull(),
				"refresh":         types.ObjectNull(refreshTypes),
			})
		}
		m.StaticBearer = types.ObjectNull(staticBearerTypes)
	default:
		diags.AddError("Unknown auth scheme",
			"Credential "+id+" authenticates as "+credential.AuthScheme+", which this provider does not manage.")
	}
	return true, diags
}

func (r *vaultCredentialResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan, config vaultCredentialModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}
	payload, diags := config.payload(ctx)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	vaultID := plan.VaultID.ValueString()
	id, err := r.client.AddCredential(ctx, vaultID, client.NewCredential{
		Protocol:    protocolMCP,
		Target:      plan.Target.ValueString(),
		DisplayName: plan.DisplayName.ValueString(),
		Payload:     payload,
	})
	if err != nil {
		apiError(&resp.Diagnostics, "add a credential to vault "+vaultID, err)
		return
	}
	// Recorded before the read-back, so a failure there taints this
	// credential rather than orphaning it.
	created := plan
	created.ID = types.StringValue(id)
	next := created
	finishCreate(ctx, resp, subject{"Credential", id}, r.read, next, created)
}

func (r *vaultCredentialResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	refresh(ctx, req, resp, r.read)
}

// Update exists because the framework asks for it, and no plan reaches it:
// every stored attribute replaces the credential, and a write-only value is
// null in plan and state alike, so none can differ. It sends nothing, and
// records what the server holds.
func (r *vaultCredentialResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state vaultCredentialModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	finishUpdate(ctx, resp, subject{"Credential", plan.ID.ValueString()}, r.read, plan, state)
}

func (r *vaultCredentialResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state vaultCredentialModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	vaultID, id := state.VaultID.ValueString(), state.ID.ValueString()
	remove(resp, "delete credential "+id, func() error {
		return r.client.DeleteCredential(ctx, vaultID, id)
	})
}

// ImportState takes `<vault_id>/<credential_id>`.
func (r *vaultCredentialResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	vaultID, id, ok := strings.Cut(req.ID, "/")
	if !ok || vaultID == "" || id == "" {
		resp.Diagnostics.AddError("Invalid import id", "Expected <vault_id>/<credential_id>, got "+req.ID)
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("vault_id"), vaultID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), id)...)
}
