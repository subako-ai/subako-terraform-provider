package provider

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/subako-ai/terraform-provider-subako/internal/client"
)

// anAgent is an agent holding one version's config, granting nothing.
func anAgent(ctx context.Context, t *testing.T) agentModel {
	t.Helper()
	model, diags := modelObject(ctx, client.AnthropicModel{Model: "claude-sonnet-5", MaxTokens: 8192})
	if diags.HasError() {
		t.Fatalf("building the model: %v", diags)
	}
	return agentModel{
		ModelProviderID: types.StringValue(platformProviderID),
		SystemPrompt:    types.StringValue("be brief"),
		Model:           model,
		MCPServers:      types.ListNull(mcpServerType),
		Skills:          types.ListNull(skillGrantType),
	}
}

func TestWhatCountsAsAConfigChange(t *testing.T) {
	ctx := context.Background()
	state := anAgent(ctx, t)

	same := anAgent(ctx, t)

	respelled := anAgent(ctx, t)
	respelled.MCPServers = types.ListValueMust(mcpServerType, []attr.Value{})
	respelled.Skills = types.ListValueMust(skillGrantType, []attr.Value{})

	unknown := anAgent(ctx, t)
	unknown.ModelProviderID = types.StringUnknown()

	another := anAgent(ctx, t)
	another.SystemPrompt = types.StringValue("be thorough")

	// An agent whose versions were all published elsewhere, or none at all.
	versionless := agentModel{
		ModelProviderID: types.StringNull(),
		SystemPrompt:    types.StringNull(),
		Model:           types.ObjectNull(agentModelTypes),
		MCPServers:      types.ListNull(mcpServerType),
		Skills:          types.ListNull(skillGrantType),
	}

	for name, tc := range map[string]struct {
		plan, state agentModel
		changed     bool
	}{
		"the same config":                {same, state, false},
		"a grant list spelled empty":     {respelled, state, false},
		"another system prompt":          {another, state, true},
		"a value known only after apply": {unknown, state, true},
		"an agent holding no version":    {state, versionless, true},
	} {
		t.Run(name, func(t *testing.T) {
			changed, diags := configChanged(ctx, tc.plan, tc.state)
			if diags.HasError() {
				t.Fatalf("comparing: %v", diags)
			}
			if changed != tc.changed {
				t.Fatalf("changed = %v, want %v", changed, tc.changed)
			}
		})
	}
}
