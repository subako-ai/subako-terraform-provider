package client

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
)

// Model wire formats, as `ModelProviderFormatBody` spells them.
const (
	FormatAnthropic       = "anthropic"
	FormatOpenAIResponses = "openai_responses"
)

// AgentConfig is everything one published agent version carries, in the
// provider's own canonical form: nil for every absent optional value and
// every empty list, so two configs compare with ==-style equality.
type AgentConfig struct {
	ModelProviderID string
	SystemPrompt    *string
	Model           ModelConfig
	MCP             []MCPGrant
	Skills          []SkillGrant
}

// ModelConfig is `ModelConfigBody`: one wire format and the settings that
// format carries. The two types below are its only forms -- the unexported
// method seals the interface -- and each writes its own format tag.
type ModelConfig interface {
	json.Marshaler
	// Format is the tag the wire carries, as `ModelProviderFormatBody`
	// spells it.
	Format() string
	sealedModelConfig()
}

// AnthropicModel is `AnthropicModelBody`. A budget enables extended thinking.
type AnthropicModel struct {
	Model                string
	MaxTokens            int64
	ThinkingBudgetTokens *int64
}

// OpenAIResponsesModel is `OpenaiResponsesModelBody`.
type OpenAIResponsesModel struct {
	Model           string
	MaxTokens       int64
	ContextWindow   int64
	ReasoningEffort *string
}

func (AnthropicModel) Format() string       { return FormatAnthropic }
func (OpenAIResponsesModel) Format() string { return FormatOpenAIResponses }

func (AnthropicModel) sealedModelConfig()       {}
func (OpenAIResponsesModel) sealedModelConfig() {}

// anthropicSettings and openaiResponsesSettings are one format's settings,
// spelled the same way in the publish body and in the stored form.
type anthropicSettings struct {
	Model     string             `json:"model"`
	MaxTokens int64              `json:"max_tokens"`
	Thinking  *anthropicThinking `json:"thinking,omitempty"`
}

type anthropicThinking struct {
	BudgetTokens int64 `json:"budget_tokens"`
}

type openaiResponsesSettings struct {
	Model           string  `json:"model"`
	MaxTokens       int64   `json:"max_tokens"`
	ContextWindow   int64   `json:"context_window"`
	ReasoningEffort *string `json:"reasoning_effort,omitempty"`
}

// The settings under the tag `ModelConfigBody` puts beside them.
type (
	anthropicBody struct {
		Format string `json:"format"`
		anthropicSettings
	}
	openaiResponsesBody struct {
		Format string `json:"format"`
		openaiResponsesSettings
	}
)

func (m AnthropicModel) settings() anthropicSettings {
	settings := anthropicSettings{Model: m.Model, MaxTokens: m.MaxTokens}
	if m.ThinkingBudgetTokens != nil {
		settings.Thinking = &anthropicThinking{BudgetTokens: *m.ThinkingBudgetTokens}
	}
	return settings
}

func (m AnthropicModel) MarshalJSON() ([]byte, error) {
	return json.Marshal(anthropicBody{Format: m.Format(), anthropicSettings: m.settings()})
}

func (m OpenAIResponsesModel) settings() openaiResponsesSettings {
	return openaiResponsesSettings{
		Model:           m.Model,
		MaxTokens:       m.MaxTokens,
		ContextWindow:   m.ContextWindow,
		ReasoningEffort: m.ReasoningEffort,
	}
}

func (m OpenAIResponsesModel) MarshalJSON() ([]byte, error) {
	return json.Marshal(openaiResponsesBody{Format: m.Format(), openaiResponsesSettings: m.settings()})
}

// decodeModel reads the settings a version stores under the format naming
// them. A format this provider has no type for is drift between server and
// provider, so it is refused rather than guessed at.
func decodeModel(format string, settings json.RawMessage) (ModelConfig, error) {
	switch format {
	case FormatAnthropic:
		var decoded anthropicSettings
		if err := json.Unmarshal(settings, &decoded); err != nil {
			return nil, err
		}
		model := AnthropicModel{Model: decoded.Model, MaxTokens: decoded.MaxTokens}
		if decoded.Thinking != nil {
			model.ThinkingBudgetTokens = &decoded.Thinking.BudgetTokens
		}
		return model, nil
	case FormatOpenAIResponses:
		var decoded openaiResponsesSettings
		if err := json.Unmarshal(settings, &decoded); err != nil {
			return nil, err
		}
		return OpenAIResponsesModel{
			Model:           decoded.Model,
			MaxTokens:       decoded.MaxTokens,
			ContextWindow:   decoded.ContextWindow,
			ReasoningEffort: decoded.ReasoningEffort,
		}, nil
	default:
		return nil, fmt.Errorf("unknown model format %q", format)
	}
}

// MCPGrant is `McpGrantBody`; the stored form spells it the same way.
type MCPGrant struct {
	Name          string     `json:"name"`
	URL           string     `json:"url"`
	DefaultPolicy string     `json:"default_policy"`
	Tools         []ToolRule `json:"tools,omitempty"`
}

// ToolRule is `ToolRuleBody`.
type ToolRule struct {
	Name   string `json:"name"`
	Policy string `json:"policy"`
}

// SkillGrant is `SkillGrantBody`. A nil Version is `latest`.
type SkillGrant struct {
	Name    string
	SkillID string
	Version *int64
}

type versionPin struct {
	Type   string `json:"type"`
	Number *int64 `json:"number,omitempty"`
}

type skillGrantJSON struct {
	Name    string     `json:"name"`
	SkillID string     `json:"skill_id"`
	Version versionPin `json:"version"`
}

func (g SkillGrant) MarshalJSON() ([]byte, error) {
	pin := versionPin{Type: "latest"}
	if g.Version != nil {
		pin = versionPin{Type: "pinned", Number: g.Version}
	}
	return json.Marshal(skillGrantJSON{Name: g.Name, SkillID: g.SkillID, Version: pin})
}

func (g *SkillGrant) UnmarshalJSON(data []byte) error {
	var raw skillGrantJSON
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*g = SkillGrant{Name: raw.Name, SkillID: raw.SkillID}
	switch raw.Version.Type {
	case "latest":
	case "pinned":
		if raw.Version.Number == nil {
			return fmt.Errorf("skill grant %q pins no version number", raw.Name)
		}
		g.Version = raw.Version.Number
	default:
		return fmt.Errorf("skill grant %q has an unknown version pin %q", raw.Name, raw.Version.Type)
	}
	return nil
}

// publishBody is `PublishAgentVersionBody`.
type publishBody struct {
	ModelProviderID string       `json:"model_provider_id"`
	SystemPrompt    *string      `json:"system_prompt,omitempty"`
	Model           ModelConfig  `json:"model"`
	MCP             []MCPGrant   `json:"mcp,omitempty"`
	Skills          []SkillGrant `json:"skills,omitempty"`
}

func (c AgentConfig) publishBody() publishBody {
	return publishBody{
		ModelProviderID: c.ModelProviderID,
		SystemPrompt:    c.SystemPrompt,
		Model:           c.Model,
		MCP:             c.MCP,
		Skills:          c.Skills,
	}
}

// storedConfig is the config as a version stores it: the model's settings
// sit under `config`, beside the format tag.
type storedConfig struct {
	SystemPrompt *string `json:"system_prompt"`
	Model        struct {
		Format string          `json:"format"`
		Config json.RawMessage `json:"config"`
	} `json:"model"`
	MCP       []MCPGrant        `json:"mcp"`
	Skills    []SkillGrant      `json:"skills"`
	Sandboxes []json.RawMessage `json:"sandboxes"`
}

// AgentConfig decodes the version's stored config into the canonical form.
func (v *AgentVersion) AgentConfig() (AgentConfig, error) {
	var stored storedConfig
	if err := json.Unmarshal(v.Config, &stored); err != nil {
		return AgentConfig{}, fmt.Errorf("decode agent version %d config: %w", v.Version, err)
	}
	if len(stored.Sandboxes) > 0 {
		return AgentConfig{}, fmt.Errorf("agent version %d grants sandboxes, which this provider does not manage", v.Version)
	}
	model, err := decodeModel(stored.Model.Format, stored.Model.Config)
	if err != nil {
		return AgentConfig{}, fmt.Errorf("decode agent version %d model: %w", v.Version, err)
	}
	return AgentConfig{
		ModelProviderID: v.ModelProviderID,
		SystemPrompt:    stored.SystemPrompt,
		Model:           model,
		MCP:             stored.MCP,
		Skills:          stored.Skills,
	}.Canonical(), nil
}

// Equal reports whether two configs would publish the same version. Both
// sides are Canonical, so an absent optional value and an empty list are nil
// on each and compare equal.
func (c AgentConfig) Equal(other AgentConfig) bool {
	return reflect.DeepEqual(c, other)
}

// Canonical folds every empty list to nil, so equal configs compare equal.
func (c AgentConfig) Canonical() AgentConfig {
	out := c
	out.MCP = nil
	for _, grant := range c.MCP {
		if len(grant.Tools) == 0 {
			grant.Tools = nil
		}
		out.MCP = append(out.MCP, grant)
	}
	out.Skills = nil
	if len(c.Skills) > 0 {
		out.Skills = slices.Clone(c.Skills)
	}
	return out
}
