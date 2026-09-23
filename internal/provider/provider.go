// Package provider is the Subako Terraform provider: the resources a
// workspace's agents are built from, managed through the public API.
package provider

import (
	"context"
	"fmt"
	"os"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/subako-ai/terraform-provider-subako/internal/client"
)

// Environment variables a provider block may leave its settings to.
const (
	envServer    = "SUBAKO_SERVER"
	envToken     = "SUBAKO_TOKEN"
	envWorkspace = "SUBAKO_WORKSPACE"
	// envCLI names the Subako CLI the provider asks for the signed-in user's
	// token when no token is set; `subako` on the PATH when it is empty.
	envCLI = "SUBAKO_CLI"
)

// DefaultServer is the API a provider block reaches when nothing names one:
// the one the Subako CLI signs in to by default.
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
				Optional: true,
				Description: "The Subako API base URL. Defaults to `$SUBAKO_SERVER`, then the server the " +
					"Subako CLI is signed in to when the token comes from it, then " + DefaultServer + ".",
			},
			"token": schema.StringAttribute{
				Optional:  true,
				Sensitive: true,
				Description: "An API key (`sbk_ak_...`) or a user access token (`sbk_at_...`). " +
					"Defaults to `$SUBAKO_TOKEN`, then the signed-in user's, which the Subako CLI's " +
					"`subako token` answers and renews as it nears expiry.",
			},
			"workspace_id": schema.StringAttribute{
				Optional: true,
				Description: "The workspace resources are managed in. Required with a user access token; " +
					"an API key implies its own. Defaults to `$SUBAKO_WORKSPACE`, then the workspace " +
					"`subako workspace use` selected when the token comes from the Subako CLI. Set it in " +
					"a configuration others apply, so it cannot follow whatever workspace they last selected.",
			},
		},
	}
}

// origin is where a provider argument's value came from.
type origin int

const (
	unset origin = iota
	fromConfig
	fromEnv
)

// environment is where the provider reads what its block leaves unset: the
// process environment in a run, a map in a test.
type environment func(string) string

// askCLI is how the provider asks the Subako CLI for the signed-in user's
// token: client.AskCLI in a run, a stand-in in a test.
type askCLI func(ctx context.Context, program string) (client.Issued, client.Auth, error)

// resolve answers what one provider argument is set to, and where that came
// from: the value the block names, else the environment variable. An
// argument set to the empty string is one the block names, and is left to
// the client to refuse -- falling back would reach a server, or use a
// credential, the operator did not name.
func resolve(value types.String, env string, getenv environment) (string, origin) {
	if set, ok := plannedString(value).Get(); ok {
		return set, fromConfig
	}
	if v := getenv(env); v != "" {
		return v, fromEnv
	}
	return "", unset
}

// settle resolves the provider block into what a client needs, each argument
// looked for in turn: in the block, then in the environment, then -- when
// the token comes from the signed-in user -- from the Subako CLI.
func settle(ctx context.Context, config providerModel, getenv environment, ask askCLI) (client.Config, diag.Diagnostics) {
	var diags diag.Diagnostics
	server, serverFrom := resolve(config.Server, envServer, getenv)
	if serverFrom == unset {
		server = DefaultServer
	}
	token, tokenFrom := resolve(config.Token, envToken, getenv)
	workspaceID, workspaceFrom := resolve(config.WorkspaceID, envWorkspace, getenv)

	var auth client.Auth
	switch tokenFrom {
	case fromConfig, fromEnv:
		static, err := client.Static(token)
		if err != nil {
			diags.AddError("Invalid provider configuration", err.Error())
			return client.Config{}, diags
		}
		auth = static
	case unset:
		program := getenv(envCLI)
		if program == "" {
			program = "subako"
		}
		issued, cliAuth, err := ask(ctx, program)
		if err != nil {
			diags.AddError("No Subako credential",
				"No token is set and $SUBAKO_TOKEN is empty, so the provider asked the Subako CLI "+
					"for the signed-in user's: "+err.Error())
			return client.Config{}, diags
		}
		auth = cliAuth
		if serverFrom == unset {
			server = issued.Server
		}
		if workspaceFrom == unset && issued.WorkspaceID != "" {
			workspaceID = issued.WorkspaceID
			diags.AddAttributeWarning(path.Root("workspace_id"), "Workspace taken from the Subako CLI",
				fmt.Sprintf("Neither workspace_id nor $SUBAKO_WORKSPACE is set, so resources are managed in "+
					"%q (%s), the workspace `subako workspace use` selected. Set workspace_id to pin this "+
					"configuration to one workspace.", issued.WorkspaceName, issued.WorkspaceID))
		}
	default:
		panic(fmt.Sprintf("unhandled token origin %d", tokenFrom))
	}
	return client.Config{Server: server, Auth: auth, WorkspaceID: workspaceID}, diags
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

	settings, diags := settle(ctx, config, os.Getenv, client.AskCLI)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	settings.UserAgent = "terraform-provider-subako/" + p.version
	c, err := client.New(settings)
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
