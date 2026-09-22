// Package provider is the Subako Terraform provider: the resources a
// workspace's agents are built from, managed through the public API.
package provider

import (
	"context"
	"os"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/subako-ai/terraform-provider-subako/internal/client"
)

// Environment variables a provider block may leave its settings to. The
// `subako terraform` command sets all three.
const (
	envServer    = "SUBAKO_SERVER"
	envToken     = "SUBAKO_TOKEN"
	envWorkspace = "SUBAKO_WORKSPACE"
)

// DefaultServer is the API a provider block reaches when it names none.
// CODESYNC(default-server)
const DefaultServer = "https://api.us.cloud.subako.ai"

var _ provider.Provider = (*subakoProvider)(nil)

type subakoProvider struct {
	version string
}

// New builds the provider for one release.
func New(version string) func() provider.Provider {
	return func() provider.Provider {
		return &subakoProvider{version: version}
	}
}

type providerModel struct {
	Server      types.String `tfsdk:"server"`
	Token       types.String `tfsdk:"token"`
	WorkspaceID types.String `tfsdk:"workspace_id"`
}

func (p *subakoProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "subako"
	resp.Version = p.version
}

func (p *subakoProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Manages the agents, skills, model providers, and vaults of one Subako workspace.",
		Attributes: map[string]schema.Attribute{
			"server": schema.StringAttribute{
				Optional:    true,
				Description: "The Subako API base URL. Defaults to `$SUBAKO_SERVER`, then " + DefaultServer + ".",
			},
			"token": schema.StringAttribute{
				Optional:  true,
				Sensitive: true,
				Description: "An API key (`sbk_ak_...`) or a user access token (`sbk_at_...`). " +
					"Defaults to `$SUBAKO_TOKEN`.",
			},
			"workspace_id": schema.StringAttribute{
				Optional: true,
				Description: "The workspace resources are created in. Required with a user access token; " +
					"an API key implies its own. Defaults to `$SUBAKO_WORKSPACE`.",
			},
		},
	}
}

// valueOrEnv answers what one provider argument is set to: the value the
// block names, else the environment variable, else the fallback. An argument
// set to the empty string is one the block names, and is left to `client.New`
// to refuse -- falling back would reach a server, or use a credential, the
// operator did not name.
func valueOrEnv(value types.String, env, fallback string) string {
	if set, ok := plannedString(value).Get(); ok {
		return set
	}
	if v := os.Getenv(env); v != "" {
		return v
	}
	return fallback
}

// providerSetting is one provider argument under the name the schema spells
// it. They travel as a slice, so the diagnostics below come out in the same
// order every run.
type providerSetting struct {
	name  string
	value types.String
}

func (p *subakoProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var config providerModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}
	for _, arg := range []providerSetting{
		{"server", config.Server},
		{"token", config.Token},
		{"workspace_id", config.WorkspaceID},
	} {
		if arg.value.IsUnknown() {
			resp.Diagnostics.AddAttributeError(path.Root(arg.name), "Unknown provider setting",
				"The provider cannot be configured from a value known only after apply.")
		}
	}
	if resp.Diagnostics.HasError() {
		return
	}

	c, err := client.New(client.Config{
		Server:      valueOrEnv(config.Server, envServer, DefaultServer),
		Token:       valueOrEnv(config.Token, envToken, ""),
		WorkspaceID: valueOrEnv(config.WorkspaceID, envWorkspace, ""),
		UserAgent:   "terraform-provider-subako/" + p.version,
	})
	if err != nil {
		resp.Diagnostics.AddError("Invalid provider configuration", err.Error())
		return
	}
	resp.ResourceData = c
	resp.DataSourceData = c
}

func (p *subakoProvider) Resources(_ context.Context) []func() resource.Resource {
	return []func() resource.Resource{
		newAgentResource,
		newSkillResource,
		newModelProviderResource,
		newVaultResource,
		newVaultCredentialResource,
	}
}

func (p *subakoProvider) DataSources(_ context.Context) []func() datasource.DataSource {
	return []func() datasource.DataSource{
		newModelProvidersDataSource,
		newWorkspaceDataSource,
	}
}
