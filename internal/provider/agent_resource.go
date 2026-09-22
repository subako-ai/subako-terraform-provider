package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/resourcevalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/setvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/setplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"

	"github.com/subako-ai/terraform-provider-subako/internal/client"
)

var (
	_ resource.ResourceWithConfigure        = (*agentResource)(nil)
	_ resource.ResourceWithImportState      = (*agentResource)(nil)
	_ resource.ResourceWithModifyPlan       = (*agentResource)(nil)
	_ resource.ResourceWithConfigValidators = (*agentResource)(nil)
)

func newAgentResource() resource.Resource {
	return &agentResource{}
}

// agentResource is an agent and the config its latest version carries.
// Changing the config publishes a new version; versions are immutable and
// are never removed on their own, so the agent is the unit Terraform owns.
type agentResource struct {
	client *client.Client
}

type agentModel struct {
	ID              types.String `tfsdk:"id"`
	WorkspaceID     types.String `tfsdk:"workspace_id"`
	Name            types.String `tfsdk:"name"`
	ModelProviderID types.String `tfsdk:"model_provider_id"`
	SystemPrompt    types.String `tfsdk:"system_prompt"`
	Model           types.Object `tfsdk:"model"`
	MCPServers      types.List   `tfsdk:"mcp_servers"`
	Skills          types.List   `tfsdk:"skills"`
	AllowedOrigins  types.Set    `tfsdk:"allowed_origins"`
	Version         types.Int64  `tfsdk:"version"`
	VersionID       types.String `tfsdk:"version_id"`
}

// agentModelConfig is `model`: one nested attribute per wire format, of which
// the schema's ExactlyOneOf leaves exactly one set.
type agentModelConfig struct {
	Anthropic       *anthropicModelConfig       `tfsdk:"anthropic"`
	OpenAIResponses *openaiResponsesModelConfig `tfsdk:"openai_responses"`
}

type anthropicModelConfig struct {
	Name                 types.String `tfsdk:"name"`
	MaxTokens            types.Int64  `tfsdk:"max_tokens"`
	ThinkingBudgetTokens types.Int64  `tfsdk:"thinking_budget_tokens"`
}

type openaiResponsesModelConfig struct {
	Name            types.String `tfsdk:"name"`
	MaxTokens       types.Int64  `tfsdk:"max_tokens"`
	ContextWindow   types.Int64  `tfsdk:"context_window"`
	ReasoningEffort types.String `tfsdk:"reasoning_effort"`
}

var (
	anthropicModelTypes = map[string]attr.Type{
		"name":                   types.StringType,
		"max_tokens":             types.Int64Type,
		"thinking_budget_tokens": types.Int64Type,
	}
	openaiResponsesModelTypes = map[string]attr.Type{
		"name":             types.StringType,
		"max_tokens":       types.Int64Type,
		"context_window":   types.Int64Type,
		"reasoning_effort": types.StringType,
	}
	agentModelTypes = map[string]attr.Type{
		client.FormatAnthropic:       types.ObjectType{AttrTypes: anthropicModelTypes},
		client.FormatOpenAIResponses: types.ObjectType{AttrTypes: openaiResponsesModelTypes},
	}
)

type mcpServerModel struct {
	Name          types.String `tfsdk:"name"`
	URL           types.String `tfsdk:"url"`
	DefaultPolicy types.String `tfsdk:"default_policy"`
	Tools         types.List   `tfsdk:"tools"`
}

type toolRuleModel struct {
	Name   types.String `tfsdk:"name"`
	Policy types.String `tfsdk:"policy"`
}

var toolRuleType = types.ObjectType{AttrTypes: map[string]attr.Type{
	"name":   types.StringType,
	"policy": types.StringType,
}}

var mcpServerType = types.ObjectType{AttrTypes: map[string]attr.Type{
	"name":           types.StringType,
	"url":            types.StringType,
	"default_policy": types.StringType,
	"tools":          types.ListType{ElemType: toolRuleType},
}}

type skillGrantModel struct {
	Name    types.String `tfsdk:"name"`
	SkillID types.String `tfsdk:"skill_id"`
	Version types.Int64  `tfsdk:"version"`
}

var skillGrantType = types.ObjectType{AttrTypes: map[string]attr.Type{
	"name":     types.StringType,
	"skill_id": types.StringType,
	"version":  types.Int64Type,
}}

// Reasoning levels, as `ReasoningEffortBody` spells them.
var reasoningEfforts = []string{"none", "minimal", "low", "medium", "high", "xhigh"}

// maxTokenCount is what the server's token counts are: a u32.
const maxTokenCount = 4294967295

func (r *agentResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_agent"
}

func (r *agentResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	policy := []validator.String{stringvalidator.OneOf(policyActions...)}
	resp.Schema = schema.Schema{
		Description: "An agent and the config its sessions run. Changing the config publishes a new " +
			"agent version; renaming or changing `allowed_origins` does not.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"workspace_id": schema.StringAttribute{
				Computed:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				Required:    true,
				Description: "A label. Agents in one workspace may share one.",
				// CODESYNC(agent-name-len)
				Validators: []validator.String{charsBetween(1, 200)},
			},
			"model_provider_id": schema.StringAttribute{
				Required:    true,
				Description: "The model provider the agent's model calls go to.",
				Validators:  uuidValidators(),
			},
			"system_prompt": schema.StringAttribute{
				Optional: true,
			},
			"model": schema.SingleNestedAttribute{
				Required: true,
				Description: "The model the agent runs, as exactly one of `anthropic` and " +
					"`openai_responses`. The one you set must match the model provider's format.",
				Attributes: map[string]schema.Attribute{
					client.FormatAnthropic: schema.SingleNestedAttribute{
						Optional:    true,
						Description: "An Anthropic Messages model.",
						Attributes: map[string]schema.Attribute{
							"name": schema.StringAttribute{
								Required:    true,
								Description: "The model name, as Anthropic spells it.",
							},
							"max_tokens": schema.Int64Attribute{
								Required:   true,
								Validators: []validator.Int64{int64validator.Between(0, maxTokenCount)},
							},
							"thinking_budget_tokens": schema.Int64Attribute{
								Optional:    true,
								Description: "Enables extended thinking with this budget.",
								Validators:  []validator.Int64{int64validator.Between(0, maxTokenCount)},
							},
						},
					},
					client.FormatOpenAIResponses: schema.SingleNestedAttribute{
						Optional:    true,
						Description: "An OpenAI Responses model.",
						Attributes: map[string]schema.Attribute{
							"name": schema.StringAttribute{
								Required:    true,
								Description: "The model name, as the provider spells it.",
							},
							"max_tokens": schema.Int64Attribute{
								Required:   true,
								Validators: []validator.Int64{int64validator.Between(0, maxTokenCount)},
							},
							"context_window": schema.Int64Attribute{
								Required:   true,
								Validators: []validator.Int64{int64validator.Between(0, maxTokenCount)},
							},
							"reasoning_effort": schema.StringAttribute{
								Optional:    true,
								Description: "Absent leaves the model's own default.",
								Validators:  []validator.String{stringvalidator.OneOf(reasoningEfforts...)},
							},
						},
					},
				},
			},
			"mcp_servers": schema.ListNestedAttribute{
				Optional:    true,
				Description: "MCP servers the agent may reach, each under a tool policy.",
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"name": schema.StringAttribute{
							Required:    true,
							Description: "Unique within `mcp_servers`.",
							Validators:  grantNameValidators(),
						},
						"url": schema.StringAttribute{
							Required: true,
						},
						"default_policy": schema.StringAttribute{
							Required:    true,
							Description: "Applies to every tool `tools` does not name.",
							Validators:  policy,
						},
						"tools": schema.ListNestedAttribute{
							Optional: true,
							NestedObject: schema.NestedAttributeObject{
								Attributes: map[string]schema.Attribute{
									"name":   schema.StringAttribute{Required: true},
									"policy": schema.StringAttribute{Required: true, Validators: policy},
								},
							},
						},
					},
				},
			},
			"skills": schema.ListNestedAttribute{
				Optional:    true,
				Description: "Skills the agent may read.",
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"name": schema.StringAttribute{
							Required:    true,
							Description: "Unique within `skills`.",
							Validators:  grantNameValidators(),
						},
						"skill_id": schema.StringAttribute{
							Required:   true,
							Validators: uuidValidators(),
						},
						"version": schema.Int64Attribute{
							Optional: true,
							Description: "Pins one skill version. Absent follows the latest version, " +
								"resolved when each session is created.",
							Validators: []validator.Int64{int64validator.AtLeast(1)},
						},
					},
				},
			},
			"allowed_origins": schema.SetAttribute{
				Optional:    true,
				Computed:    true,
				ElementType: types.StringType,
				Description: "Browser origins the agent's sessions answer. `*` allows every origin. " +
					"Left unset, Terraform does not manage the list.",
				// CODESYNC(agent-origin-count)
				Validators:    []validator.Set{setvalidator.SizeAtMost(32)},
				PlanModifiers: []planmodifier.Set{setplanmodifier.UseStateForUnknown()},
			},
			"version": schema.Int64Attribute{
				Computed:    true,
				Description: "The number of the version carrying the config.",
			},
			"version_id": schema.StringAttribute{
				Computed: true,
			},
		},
	}
}

// ConfigValidators states the one rule the schema cannot: `model` carries one
// format's settings, so the two nested attributes are alternatives.
func (r *agentResource) ConfigValidators(_ context.Context) []resource.ConfigValidator {
	return []resource.ConfigValidator{
		resourcevalidator.ExactlyOneOf(
			path.MatchRoot("model").AtName(client.FormatAnthropic),
			path.MatchRoot("model").AtName(client.FormatOpenAIResponses),
		),
	}
}

func (r *agentResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = clientFrom(req.ProviderData, &resp.Diagnostics)
}

// configChanged reports whether an apply would publish a new version: the
// config the plan sends differs from the one the agent's latest version
// carries. The two are compared as the configs they become, so respelling an
// absent grant list as an empty one publishes nothing. A plan value known
// only after apply could be anything, so it counts as a change.
func configChanged(ctx context.Context, plan, state agentModel) (bool, diag.Diagnostics) {
	known, diags := fullyKnown(ctx, plan.ModelProviderID, plan.SystemPrompt, plan.Model, plan.MCPServers, plan.Skills)
	if diags.HasError() || !known {
		return true, diags
	}
	// An agent holding no version carries no config to compare against.
	if _, ok := planned(state.Model).Get(); !ok {
		return true, diags
	}
	wanted, d := agentConfig(ctx, plan)
	diags.Append(d...)
	held, d := agentConfig(ctx, state)
	diags.Append(d...)
	if diags.HasError() {
		return true, diags
	}
	return !wanted.Equal(held), diags
}

// ModifyPlan keeps the version a rename-only change leaves in place, so the
// plan says "known after apply" only when a new version will be published.
func (r *agentResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() || req.State.Raw.IsNull() {
		return
	}
	var plan, state agentModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	changed, diags := configChanged(ctx, plan, state)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() || changed {
		return
	}
	plan.Version = state.Version
	plan.VersionID = state.VersionID
	resp.Diagnostics.Append(resp.Plan.Set(ctx, &plan)...)
}

// modelConfig reads the variant `model` carries into the wire format it
// names.
func (m agentModelConfig) modelConfig() (client.ModelConfig, diag.Diagnostics) {
	var diags diag.Diagnostics
	switch {
	case m.Anthropic != nil && m.OpenAIResponses == nil:
		return client.AnthropicModel{
			Model:                m.Anthropic.Name.ValueString(),
			MaxTokens:            m.Anthropic.MaxTokens.ValueInt64(),
			ThinkingBudgetTokens: int64Pointer(m.Anthropic.ThinkingBudgetTokens),
		}, diags
	case m.OpenAIResponses != nil && m.Anthropic == nil:
		return client.OpenAIResponsesModel{
			Model:           m.OpenAIResponses.Name.ValueString(),
			MaxTokens:       m.OpenAIResponses.MaxTokens.ValueInt64(),
			ContextWindow:   m.OpenAIResponses.ContextWindow.ValueInt64(),
			ReasoningEffort: stringPointer(m.OpenAIResponses.ReasoningEffort),
		}, diags
	}
	diags.AddAttributeError(path.Root("model"), "Ambiguous model",
		"Set exactly one of model."+client.FormatAnthropic+" and model."+client.FormatOpenAIResponses+".")
	return nil, diags
}

// modelObject is the `model` attribute holding one wire format's settings.
func modelObject(ctx context.Context, config client.ModelConfig) (types.Object, diag.Diagnostics) {
	var diags diag.Diagnostics
	variants := map[string]attr.Value{
		client.FormatAnthropic:       types.ObjectNull(anthropicModelTypes),
		client.FormatOpenAIResponses: types.ObjectNull(openaiResponsesModelTypes),
	}
	var variant basetypes.ObjectValue
	var d diag.Diagnostics
	switch model := config.(type) {
	case client.AnthropicModel:
		variant, d = types.ObjectValueFrom(ctx, anthropicModelTypes, anthropicModelConfig{
			Name:                 types.StringValue(model.Model),
			MaxTokens:            types.Int64Value(model.MaxTokens),
			ThinkingBudgetTokens: optionalInt64(model.ThinkingBudgetTokens),
		})
	case client.OpenAIResponsesModel:
		variant, d = types.ObjectValueFrom(ctx, openaiResponsesModelTypes, openaiResponsesModelConfig{
			Name:            types.StringValue(model.Model),
			MaxTokens:       types.Int64Value(model.MaxTokens),
			ContextWindow:   types.Int64Value(model.ContextWindow),
			ReasoningEffort: types.StringPointerValue(model.ReasoningEffort),
		})
	}
	diags.Append(d...)
	variants[config.Format()] = variant

	object, d := types.ObjectValue(agentModelTypes, variants)
	diags.Append(d...)
	return object, diags
}

// agentConfig reads the config a version should carry out of a model whose
// values are all known.
func agentConfig(ctx context.Context, m agentModel) (client.AgentConfig, diag.Diagnostics) {
	var diags diag.Diagnostics
	var model agentModelConfig
	diags.Append(m.Model.As(ctx, &model, basetypes.ObjectAsOptions{})...)
	var servers []mcpServerModel
	if _, ok := planned(m.MCPServers).Get(); ok {
		diags.Append(m.MCPServers.ElementsAs(ctx, &servers, false)...)
	}
	var skills []skillGrantModel
	if _, ok := planned(m.Skills).Get(); ok {
		diags.Append(m.Skills.ElementsAs(ctx, &skills, false)...)
	}
	if diags.HasError() {
		return client.AgentConfig{}, diags
	}
	wire, d := model.modelConfig()
	diags.Append(d...)
	if diags.HasError() {
		return client.AgentConfig{}, diags
	}

	config := client.AgentConfig{
		ModelProviderID: m.ModelProviderID.ValueString(),
		SystemPrompt:    stringPointer(m.SystemPrompt),
		Model:           wire,
	}
	for _, server := range servers {
		var tools []toolRuleModel
		if _, ok := planned(server.Tools).Get(); ok {
			diags.Append(server.Tools.ElementsAs(ctx, &tools, false)...)
		}
		grant := client.MCPGrant{
			Name:          server.Name.ValueString(),
			URL:           server.URL.ValueString(),
			DefaultPolicy: server.DefaultPolicy.ValueString(),
		}
		for _, tool := range tools {
			grant.Tools = append(grant.Tools, client.ToolRule{
				Name:   tool.Name.ValueString(),
				Policy: tool.Policy.ValueString(),
			})
		}
		config.MCP = append(config.MCP, grant)
	}
	for _, skill := range skills {
		config.Skills = append(config.Skills, client.SkillGrant{
			Name:    skill.Name.ValueString(),
			SkillID: skill.SkillID.ValueString(),
			Version: int64Pointer(skill.Version),
		})
	}
	return config.Canonical(), diags
}

func optionalInt64(n *int64) types.Int64 {
	if n == nil {
		return types.Int64Null()
	}
	return types.Int64Value(*n)
}

// emptyList is an empty list where the prior value spelled one, and null
// where it did not, so reading back an empty grant list is no change.
func emptyList(prior types.List, elem attr.Type) types.List {
	if _, ok := planned(prior).Get(); ok {
		return types.ListValueMust(elem, []attr.Value{})
	}
	return types.ListNull(elem)
}

// setAgentConfig writes a version's config into m. prior decides how an
// empty list reads.
func setAgentConfig(ctx context.Context, m *agentModel, config client.AgentConfig, prior agentModel) diag.Diagnostics {
	var diags diag.Diagnostics
	m.ModelProviderID = types.StringValue(config.ModelProviderID)
	m.SystemPrompt = types.StringPointerValue(config.SystemPrompt)

	model, d := modelObject(ctx, config.Model)
	diags.Append(d...)
	m.Model = model

	priorTools := map[string]types.List{}
	if _, ok := planned(prior.MCPServers).Get(); ok {
		var servers []mcpServerModel
		diags.Append(prior.MCPServers.ElementsAs(ctx, &servers, false)...)
		for _, server := range servers {
			priorTools[server.Name.ValueString()] = server.Tools
		}
	}

	if len(config.MCP) == 0 {
		m.MCPServers = emptyList(prior.MCPServers, mcpServerType)
	} else {
		servers := make([]mcpServerModel, 0, len(config.MCP))
		for _, grant := range config.MCP {
			tools := emptyList(priorTools[grant.Name], toolRuleType)
			if len(grant.Tools) > 0 {
				rules := make([]toolRuleModel, 0, len(grant.Tools))
				for _, tool := range grant.Tools {
					rules = append(rules, toolRuleModel{
						Name:   types.StringValue(tool.Name),
						Policy: types.StringValue(tool.Policy),
					})
				}
				tools, d = types.ListValueFrom(ctx, toolRuleType, rules)
				diags.Append(d...)
			}
			servers = append(servers, mcpServerModel{
				Name:          types.StringValue(grant.Name),
				URL:           types.StringValue(grant.URL),
				DefaultPolicy: types.StringValue(grant.DefaultPolicy),
				Tools:         tools,
			})
		}
		m.MCPServers, d = types.ListValueFrom(ctx, mcpServerType, servers)
		diags.Append(d...)
	}

	if len(config.Skills) == 0 {
		m.Skills = emptyList(prior.Skills, skillGrantType)
	} else {
		grants := make([]skillGrantModel, 0, len(config.Skills))
		for _, grant := range config.Skills {
			grants = append(grants, skillGrantModel{
				Name:    types.StringValue(grant.Name),
				SkillID: types.StringValue(grant.SkillID),
				Version: optionalInt64(grant.Version),
			})
		}
		m.Skills, d = types.ListValueFrom(ctx, skillGrantType, grants)
		diags.Append(d...)
	}
	return diags
}

// read refreshes m from the server. found is false when the agent is gone.
func (r *agentResource) read(ctx context.Context, m *agentModel) (found bool, diags diag.Diagnostics) {
	id := m.ID.ValueString()
	agent, err := r.client.GetAgent(ctx, id)
	if client.IsNotFound(err) {
		return false, diags
	}
	if err != nil {
		apiError(&diags, "read agent "+id, err)
		return true, diags
	}
	prior := *m
	m.WorkspaceID = types.StringValue(agent.WorkspaceID)
	m.Name = types.StringValue(agent.Name)

	version, err := r.client.LatestAgentVersion(ctx, id)
	if err != nil {
		apiError(&diags, "read agent "+id+" versions", err)
		return true, diags
	}
	if version == nil {
		m.ModelProviderID = types.StringNull()
		m.SystemPrompt = types.StringNull()
		m.Model = types.ObjectNull(agentModelTypes)
		m.MCPServers = types.ListNull(mcpServerType)
		m.Skills = types.ListNull(skillGrantType)
		m.Version = types.Int64Null()
		m.VersionID = types.StringNull()
	} else {
		config, err := version.AgentConfig()
		if err != nil {
			diags.AddError("Unreadable agent version", err.Error())
			return true, diags
		}
		diags.Append(setAgentConfig(ctx, m, config, prior)...)
		m.Version = types.Int64Value(version.Version)
		m.VersionID = types.StringValue(version.ID)
	}

	security, err := r.client.GetAgentSecurity(ctx, id)
	if err != nil {
		apiError(&diags, "read agent "+id+" security", err)
		return true, diags
	}
	origins, d := types.SetValueFrom(ctx, types.StringType, security.AllowedOrigins)
	diags.Append(d...)
	m.AllowedOrigins = origins
	return true, diags
}

// putOrigins replaces an agent's browser-origin allowlist.
func (r *agentResource) putOrigins(ctx context.Context, id string, origins []string) step {
	return step{
		action: "set agent " + id + " allowed origins",
		call: func() error {
			_, err := r.client.PutAgentSecurity(ctx, id, client.AgentSecurity{AllowedOrigins: origins})
			return err
		},
	}
}

func (r *agentResource) publish(ctx context.Context, id string, config client.AgentConfig) step {
	return step{
		action: "publish agent " + id + " version",
		call: func() error {
			_, err := r.client.PublishAgentVersion(ctx, id, config)
			return err
		},
	}
}

func (r *agentResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan agentModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	config, diags := agentConfig(ctx, plan)
	resp.Diagnostics.Append(diags...)
	allowed, diags := plannedStrings(ctx, plan.AllowedOrigins)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	agent, err := r.client.CreateAgent(ctx, plan.Name.ValueString())
	if err != nil {
		apiError(&resp.Diagnostics, "create agent", err)
		return
	}
	// Recorded before the steps below, so a failure there taints this agent
	// rather than orphaning it.
	created := agentModel{
		ID:              types.StringValue(agent.ID),
		WorkspaceID:     types.StringValue(agent.WorkspaceID),
		Name:            types.StringValue(agent.Name),
		ModelProviderID: plan.ModelProviderID,
		SystemPrompt:    plan.SystemPrompt,
		Model:           plan.Model,
		MCPServers:      plan.MCPServers,
		Skills:          plan.Skills,
		AllowedOrigins:  types.SetNull(types.StringType),
		Version:         types.Int64Null(),
		VersionID:       types.StringNull(),
	}

	steps := []step{r.publish(ctx, agent.ID, config)}
	if origins, ok := allowed.Get(); ok {
		steps = append(steps, r.putOrigins(ctx, agent.ID, origins))
	}
	run(&resp.Diagnostics, steps...)

	next := plan
	next.ID = created.ID
	finishCreate(ctx, resp, subject{"Agent", agent.ID}, r.read, next, created)
}

func (r *agentResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	refresh(ctx, req, resp, r.read)
}

func (r *agentResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state agentModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	id := state.ID.ValueString()

	var steps []step
	if !plan.Name.Equal(state.Name) {
		steps = append(steps, step{
			action: "rename agent " + id,
			call: func() error {
				_, err := r.client.RenameAgent(ctx, id, plan.Name.ValueString())
				return err
			},
		})
	}
	changed, diags := configChanged(ctx, plan, state)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	if changed {
		config, d := agentConfig(ctx, plan)
		resp.Diagnostics.Append(d...)
		if resp.Diagnostics.HasError() {
			return
		}
		steps = append(steps, r.publish(ctx, id, config))
	}
	wanted, d := plannedStrings(ctx, plan.AllowedOrigins)
	resp.Diagnostics.Append(d...)
	held, d := plannedStrings(ctx, state.AllowedOrigins)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}
	if origins, ok := wanted.Get(); ok && !wanted.Equal(held) {
		steps = append(steps, r.putOrigins(ctx, id, origins))
	}
	run(&resp.Diagnostics, steps...)

	next := plan
	next.ID = state.ID
	finishUpdate(ctx, resp, subject{"Agent", id}, r.read, next, state)
}

func (r *agentResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state agentModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	id := state.ID.ValueString()
	remove(resp, "delete agent "+id, func() error { return r.client.DeleteAgent(ctx, id) })
}

func (r *agentResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}
