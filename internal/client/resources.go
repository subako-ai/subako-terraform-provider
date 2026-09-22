package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
)

// Workspace is `WorkspaceBody`.
type Workspace struct {
	ID             string `json:"id"`
	OrganizationID string `json:"organization_id"`
	Name           string `json:"name"`
}

func (c *Client) GetWorkspace(ctx context.Context, id string) (*Workspace, error) {
	var out Workspace
	err := c.do(ctx, request{method: http.MethodGet, path: "/v1/workspaces/" + url.PathEscape(id)}, &out)
	return &out, err
}

// Agent is `AgentBody`.
type Agent struct {
	ID          string `json:"id"`
	WorkspaceID string `json:"workspace_id"`
	Name        string `json:"name"`
}

type agentName struct {
	Name string `json:"name"`
}

func (c *Client) CreateAgent(ctx context.Context, name string) (*Agent, error) {
	req, err := jsonRequest(http.MethodPost, "/v1/agents", agentName{Name: name})
	if err != nil {
		return nil, err
	}
	var out Agent
	return &out, c.do(ctx, req, &out)
}

func (c *Client) GetAgent(ctx context.Context, id string) (*Agent, error) {
	var out Agent
	err := c.do(ctx, request{method: http.MethodGet, path: agentPath(id)}, &out)
	return &out, err
}

func (c *Client) RenameAgent(ctx context.Context, id, name string) (*Agent, error) {
	req, err := jsonRequest(http.MethodPatch, agentPath(id), agentName{Name: name})
	if err != nil {
		return nil, err
	}
	var out Agent
	return &out, c.do(ctx, req, &out)
}

func (c *Client) DeleteAgent(ctx context.Context, id string) error {
	return c.do(ctx, request{method: http.MethodDelete, path: agentPath(id)}, nil)
}

func agentPath(id string) string {
	return "/v1/agents/" + url.PathEscape(id)
}

// AgentSecurity is `AgentSecurityBody`, and the `PUT` body it answers.
type AgentSecurity struct {
	AllowedOrigins []string `json:"allowed_origins"`
}

func (c *Client) GetAgentSecurity(ctx context.Context, agentID string) (*AgentSecurity, error) {
	var out AgentSecurity
	err := c.do(ctx, request{method: http.MethodGet, path: agentPath(agentID) + "/security"}, &out)
	return &out, err
}

func (c *Client) PutAgentSecurity(ctx context.Context, agentID string, security AgentSecurity) (*AgentSecurity, error) {
	if security.AllowedOrigins == nil {
		security.AllowedOrigins = []string{}
	}
	req, err := jsonRequest(http.MethodPut, agentPath(agentID)+"/security", security)
	if err != nil {
		return nil, err
	}
	var out AgentSecurity
	return &out, c.do(ctx, req, &out)
}

// AgentVersion is `AgentVersionBody`. Config is the stored form.
type AgentVersion struct {
	ID              string          `json:"id"`
	AgentID         string          `json:"agent_id"`
	Version         int64           `json:"version"`
	ModelProviderID string          `json:"model_provider_id"`
	Config          json.RawMessage `json:"config"`
}

func (c *Client) PublishAgentVersion(ctx context.Context, agentID string, config AgentConfig) (*AgentVersion, error) {
	req, err := jsonRequest(http.MethodPost, agentPath(agentID)+"/versions", config.publishBody())
	if err != nil {
		return nil, err
	}
	var out AgentVersion
	return &out, c.do(ctx, req, &out)
}

// LatestAgentVersion is the newest published version, or nil for an agent
// that has none.
func (c *Client) LatestAgentVersion(ctx context.Context, agentID string) (*AgentVersion, error) {
	return latest[AgentVersion](ctx, c, agentPath(agentID)+"/versions")
}

// Skill is `SkillBody`.
type Skill struct {
	ID            string `json:"id"`
	DisplayName   string `json:"display_name"`
	Name          string `json:"name"`
	LatestVersion int64  `json:"latest_version"`
}

// ManifestEntry is one file a stored skill version holds.
type ManifestEntry struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	Sha256 string `json:"sha256"`
}

// SkillVersion is `SkillVersionBody`.
type SkillVersion struct {
	ID          string          `json:"id"`
	SkillID     string          `json:"skill_id"`
	Version     int64           `json:"version"`
	Manifest    []ManifestEntry `json:"manifest"`
	BundleBytes int64           `json:"bundle_bytes"`
}

func skillPath(id string) string {
	return "/v1/skills/" + url.PathEscape(id)
}

func bundleRequest(path string, bundle []byte) request {
	return request{
		method:      http.MethodPost,
		path:        path,
		body:        bundle,
		contentType: "application/gzip",
	}
}

// CreateSkill uploads a gzipped tar as a new skill's first version.
func (c *Client) CreateSkill(ctx context.Context, bundle []byte) (*SkillVersion, error) {
	var out SkillVersion
	return &out, c.do(ctx, bundleRequest("/v1/skills", bundle), &out)
}

// PushSkillVersion uploads a gzipped tar as a skill's next version.
func (c *Client) PushSkillVersion(ctx context.Context, skillID string, bundle []byte) (*SkillVersion, error) {
	var out SkillVersion
	return &out, c.do(ctx, bundleRequest(skillPath(skillID)+"/versions", bundle), &out)
}

func (c *Client) GetSkill(ctx context.Context, id string) (*Skill, error) {
	var out Skill
	err := c.do(ctx, request{method: http.MethodGet, path: skillPath(id)}, &out)
	return &out, err
}

func (c *Client) RenameSkill(ctx context.Context, id, displayName string) (*Skill, error) {
	req, err := jsonRequest(http.MethodPatch, skillPath(id), map[string]string{"display_name": displayName})
	if err != nil {
		return nil, err
	}
	var out Skill
	return &out, c.do(ctx, req, &out)
}

func (c *Client) DeleteSkill(ctx context.Context, id string) error {
	return c.do(ctx, request{method: http.MethodDelete, path: skillPath(id)}, nil)
}

func (c *Client) LatestSkillVersion(ctx context.Context, skillID string) (*SkillVersion, error) {
	return latest[SkillVersion](ctx, c, skillPath(skillID)+"/versions")
}

// Vault is `VaultBody`. Metadata is whatever JSON is stored under it: the
// create and update bodies take an unconstrained value, so a vault another
// client made may carry anything at all there.
type Vault struct {
	ID          string          `json:"id"`
	WorkspaceID string          `json:"workspace_id"`
	DisplayName string          `json:"display_name"`
	Metadata    json.RawMessage `json:"metadata"`
}

type vaultCreated struct {
	ID string `json:"id"`
}

func vaultPath(id string) string {
	return "/v1/vaults/" + url.PathEscape(id)
}

// CreateVault answers the new vault's id; the server returns nothing more.
func (c *Client) CreateVault(ctx context.Context, displayName string, metadata map[string]string) (string, error) {
	req, err := jsonRequest(http.MethodPost, "/v1/vaults", map[string]any{
		"display_name": displayName,
		"metadata":     metadataObject(metadata),
	})
	if err != nil {
		return "", err
	}
	var out vaultCreated
	if err := c.do(ctx, req, &out); err != nil {
		return "", err
	}
	return out.ID, nil
}

func (c *Client) GetVault(ctx context.Context, id string) (*Vault, error) {
	var out Vault
	err := c.do(ctx, request{method: http.MethodGet, path: vaultPath(id)}, &out)
	return &out, err
}

// UpdateVault replaces both fields: the provider always knows the whole
// desired vault.
func (c *Client) UpdateVault(ctx context.Context, id, displayName string, metadata map[string]string) (*Vault, error) {
	req, err := jsonRequest(http.MethodPatch, vaultPath(id), map[string]any{
		"display_name": displayName,
		"metadata":     metadataObject(metadata),
	})
	if err != nil {
		return nil, err
	}
	var out Vault
	return &out, c.do(ctx, req, &out)
}

func (c *Client) DeleteVault(ctx context.Context, id string) error {
	return c.do(ctx, request{method: http.MethodDelete, path: vaultPath(id)}, nil)
}

func metadataObject(metadata map[string]string) map[string]string {
	if metadata == nil {
		return map[string]string{}
	}
	return metadata
}

// Credential is `CredentialBody`: metadata only.
type Credential struct {
	ID          string `json:"id"`
	VaultID     string `json:"vault_id"`
	Protocol    string `json:"protocol"`
	AuthScheme  string `json:"auth_scheme"`
	Target      string `json:"target"`
	DisplayName string `json:"display_name"`
}

// NewCredential is `AddCredentialBody`. Payload carries the secret.
type NewCredential struct {
	Protocol    string            `json:"protocol"`
	Target      string            `json:"target"`
	DisplayName string            `json:"display_name"`
	Payload     CredentialPayload `json:"payload"`
}

// Auth schemes, as `AuthSchemeBody` spells them.
const (
	AuthStaticBearer = "static_bearer"
	AuthOAuth        = "oauth"
)

// CredentialPayload is `CredentialPayloadBody`: the secret a credential
// seals, and how it authenticates with it. The two types below are its only
// forms -- the unexported method seals the interface -- and each writes its
// own auth scheme tag.
type CredentialPayload interface {
	json.Marshaler
	// AuthScheme is the tag the wire carries, as `AuthSchemeBody` spells it.
	AuthScheme() string
	sealedCredentialPayload()
}

// StaticBearerPayload is a bearer token supplied once.
type StaticBearerPayload struct {
	Token string
}

// OAuthPayload is an access token, with what the vault needs to refresh it
// once it expires.
type OAuthPayload struct {
	AccessToken string
	ExpiresAt   *string
	Refresh     *RefreshBlock
}

func (StaticBearerPayload) AuthScheme() string { return AuthStaticBearer }
func (OAuthPayload) AuthScheme() string        { return AuthOAuth }

func (StaticBearerPayload) sealedCredentialPayload() {}
func (OAuthPayload) sealedCredentialPayload()        {}

func (p StaticBearerPayload) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		AuthScheme string `json:"auth_scheme"`
		Token      string `json:"token"`
	}{p.AuthScheme(), p.Token})
}

func (p OAuthPayload) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		AuthScheme  string        `json:"auth_scheme"`
		AccessToken string        `json:"access_token"`
		ExpiresAt   *string       `json:"expires_at,omitempty"`
		Refresh     *RefreshBlock `json:"refresh,omitempty"`
	}{p.AuthScheme(), p.AccessToken, p.ExpiresAt, p.Refresh})
}

// RefreshBlock is `RefreshBlockBody`.
type RefreshBlock struct {
	TokenEndpoint     string          `json:"token_endpoint"`
	ClientID          string          `json:"client_id"`
	Scope             *string         `json:"scope,omitempty"`
	RefreshToken      string          `json:"refresh_token"`
	TokenEndpointAuth json.RawMessage `json:"token_endpoint_auth"`
}

func (c *Client) AddCredential(ctx context.Context, vaultID string, credential NewCredential) (string, error) {
	req, err := jsonRequest(http.MethodPost, vaultPath(vaultID)+"/credentials", credential)
	if err != nil {
		return "", err
	}
	var out vaultCreated
	if err := c.do(ctx, req, &out); err != nil {
		return "", err
	}
	return out.ID, nil
}

// FindCredential walks a vault's credentials for one id. A missing
// credential is a not-found error, like a missing resource elsewhere.
func (c *Client) FindCredential(ctx context.Context, vaultID, credentialID string) (*Credential, error) {
	all, err := listAll[Credential](ctx, c, vaultPath(vaultID)+"/credentials")
	if err != nil {
		return nil, err
	}
	for i := range all {
		if all[i].ID == credentialID {
			return &all[i], nil
		}
	}
	return nil, &Error{Status: http.StatusNotFound, Code: "not_found", Message: "credential not found"}
}

func (c *Client) DeleteCredential(ctx context.Context, vaultID, credentialID string) error {
	path := vaultPath(vaultID) + "/credentials/" + url.PathEscape(credentialID)
	return c.do(ctx, request{method: http.MethodDelete, path: path}, nil)
}

// ModelProvider is `ModelProviderBody`: a platform provider or a workspace's
// own, told apart by Type.
type ModelProvider struct {
	Type        string          `json:"type"`
	ID          string          `json:"id"`
	Format      string          `json:"format"`
	DisplayName string          `json:"display_name"`
	Description string          `json:"description"`
	WorkspaceID *string         `json:"workspace_id"`
	Models      []PlatformModel `json:"models"`
}

// PlatformModel is `PlatformModelBody`.
type PlatformModel struct {
	Model       string `json:"model"`
	DisplayName string `json:"display_name"`
	Description string `json:"description"`
}

// NewModelProvider is `CreateWorkspaceModelProviderBody`.
type NewModelProvider struct {
	Format      string `json:"format"`
	DisplayName string `json:"display_name"`
	Description string `json:"description"`
	BaseURL     string `json:"base_url"`
	APIKey      string `json:"api_key"`
}

// ModelProviderUpdate is `UpdateModelProviderBody`. An absent APIKey leaves
// the stored key as it stands; one sent replaces it.
type ModelProviderUpdate struct {
	DisplayName string  `json:"display_name"`
	Description string  `json:"description"`
	BaseURL     string  `json:"base_url"`
	APIKey      *string `json:"api_key,omitempty"`
}

func modelProviderPath(id string) string {
	return "/v1/model-providers/" + url.PathEscape(id)
}

func (c *Client) CreateModelProvider(ctx context.Context, provider NewModelProvider) (*ModelProvider, error) {
	req, err := jsonRequest(http.MethodPost, "/v1/model-providers", provider)
	if err != nil {
		return nil, err
	}
	var out ModelProvider
	return &out, c.do(ctx, req, &out)
}

func (c *Client) GetModelProvider(ctx context.Context, id string) (*ModelProvider, error) {
	var out ModelProvider
	err := c.do(ctx, request{method: http.MethodGet, path: modelProviderPath(id)}, &out)
	return &out, err
}

func (c *Client) UpdateModelProvider(ctx context.Context, id string, update ModelProviderUpdate) (*ModelProvider, error) {
	req, err := jsonRequest(http.MethodPatch, modelProviderPath(id), update)
	if err != nil {
		return nil, err
	}
	var out ModelProvider
	return &out, c.do(ctx, req, &out)
}

func (c *Client) DeleteModelProvider(ctx context.Context, id string) error {
	return c.do(ctx, request{method: http.MethodDelete, path: modelProviderPath(id)}, nil)
}

func (c *Client) ListModelProviders(ctx context.Context) ([]ModelProvider, error) {
	return listAll[ModelProvider](ctx, c, "/v1/model-providers")
}
