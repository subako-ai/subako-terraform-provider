package provider

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/subako-ai/terraform-provider-subako/internal/client"
)

// env is a stand-in environment holding exactly the variables it names.
func env(vars map[string]string) environment {
	return func(name string) string { return vars[name] }
}

// signedIn is a stand-in Subako CLI signed in to `server`, with `workspace`
// selected when it is not empty. It counts the times it is asked.
type signedIn struct {
	server, workspace string
	asked             int
	program           string
}

func (s *signedIn) ask(_ context.Context, program string) (client.Issued, client.Auth, error) {
	s.asked++
	s.program = program
	auth, err := client.Static("sbk_at_from_cli")
	if err != nil {
		return client.Issued{}, nil, err
	}
	return client.Issued{
		Version:       1,
		Server:        s.server,
		AccessToken:   "sbk_at_from_cli",
		ExpiresAt:     time.Now().Add(time.Hour),
		WorkspaceID:   s.workspace,
		WorkspaceName: "infra",
	}, auth, nil
}

func notAsked(t *testing.T) askCLI {
	return func(context.Context, string) (client.Issued, client.Auth, error) {
		t.Fatal("the Subako CLI was asked although a token was set")
		return client.Issued{}, nil, nil
	}
}

func block(server, token, workspace *string) providerModel {
	value := func(v *string) types.String {
		if v == nil {
			return types.StringNull()
		}
		return types.StringValue(*v)
	}
	return providerModel{Server: value(server), Token: value(token), WorkspaceID: value(workspace)}
}

func ptr(s string) *string { return &s }

func warnings(diags diag.Diagnostics) []string {
	var out []string
	for _, d := range diags.Warnings() {
		out = append(out, d.Summary())
	}
	return out
}

func TestASetTokenIsUsedAndTheCLIIsNeverAsked(t *testing.T) {
	for name, tc := range map[string]struct {
		config providerModel
		vars   map[string]string
	}{
		"in the block":       {block(nil, ptr("sbk_ak_block"), nil), nil},
		"in the environment": {block(nil, nil, nil), map[string]string{envToken: "sbk_ak_env"}},
	} {
		t.Run(name, func(t *testing.T) {
			settings, diags := settle(context.Background(), tc.config, env(tc.vars), notAsked(t))
			if diags.HasError() || len(diags) != 0 {
				t.Fatalf("diags = %v", diags)
			}
			if settings.Server != DefaultServer {
				t.Fatalf("server = %q", settings.Server)
			}
		})
	}
}

// With no token set, the signed-in user's comes from the CLI, and so do the
// server and the workspace -- the workspace with a warning, since it follows
// whatever `subako workspace use` last chose.
func TestWithNoTokenTheCLISuppliesTheRest(t *testing.T) {
	cli := &signedIn{server: "https://api.example.test", workspace: "ws-cli"}
	settings, diags := settle(context.Background(), block(nil, nil, nil), env(nil), cli.ask)
	if diags.HasError() {
		t.Fatalf("diags = %v", diags)
	}
	if cli.asked != 1 || cli.program != "subako" {
		t.Fatalf("asked %d times, of %q", cli.asked, cli.program)
	}
	if settings.Server != "https://api.example.test" || settings.WorkspaceID != "ws-cli" || settings.Auth == nil {
		t.Fatalf("settings = %+v", settings)
	}
	if got := warnings(diags); len(got) != 1 || got[0] != "Workspace taken from the Subako CLI" {
		t.Fatalf("warnings = %v", got)
	}
	if detail := diags.Warnings()[0].Detail(); !strings.Contains(detail, `"infra" (ws-cli)`) {
		t.Fatalf("the warning names the workspace: %s", detail)
	}
}

// The block, then the environment, then the CLI: a workspace named in either
// of the first two is the one used, and draws no warning.
func TestAWorkspaceNamedBeforeTheCLIWins(t *testing.T) {
	for name, tc := range map[string]struct {
		config providerModel
		vars   map[string]string
		want   string
	}{
		"in the block":       {block(nil, nil, ptr("ws-block")), map[string]string{envWorkspace: "ws-env"}, "ws-block"},
		"in the environment": {block(nil, nil, nil), map[string]string{envWorkspace: "ws-env"}, "ws-env"},
	} {
		t.Run(name, func(t *testing.T) {
			cli := &signedIn{server: "https://api.example.test", workspace: "ws-cli"}
			settings, diags := settle(context.Background(), tc.config, env(tc.vars), cli.ask)
			if diags.HasError() || len(warnings(diags)) != 0 {
				t.Fatalf("diags = %v", diags)
			}
			if settings.WorkspaceID != tc.want {
				t.Fatalf("workspace = %q, want %q", settings.WorkspaceID, tc.want)
			}
		})
	}
}

// A token the operator set goes to whichever server they named, from the
// block or the environment. The CLI's token is for the one server it signed
// in to, and a server named for it that is another one is refused rather
// than sent the token.
func TestOnlyTheCLIsTokenIsHeldToItsServer(t *testing.T) {
	for name, tc := range map[string]struct {
		config providerModel
		vars   map[string]string
		server string // "" when settle refuses
	}{
		"the environment's token, the block's server": {
			block(ptr("https://staging.example.test"), nil, nil),
			map[string]string{envToken: "sbk_ak_env"}, "https://staging.example.test"},
		"the block's token, the environment's server": {
			block(nil, ptr("sbk_ak_block"), nil),
			map[string]string{envServer: "https://staging.example.test"}, "https://staging.example.test"},
		"the block's server over the environment's": {
			block(ptr("https://block.example.test"), nil, nil),
			map[string]string{envToken: "sbk_ak_env", envServer: "https://env.example.test"}, "https://block.example.test"},
		"the CLI's token, another server in the environment": {
			block(nil, nil, nil), map[string]string{envServer: "https://staging.example.test"}, ""},
		"the CLI's token, another server in the block": {
			block(ptr("https://staging.example.test"), nil, nil), nil, ""},
		"the CLI's token, its own server named again": {
			block(nil, nil, nil), map[string]string{envServer: "https://api.example.test/"}, "https://api.example.test"},
	} {
		t.Run(name, func(t *testing.T) {
			cli := &signedIn{server: "https://api.example.test"}
			settings, diags := settle(context.Background(), tc.config, env(tc.vars), cli.ask)
			switch {
			case tc.server == "" && (!diags.HasError() || diags.Errors()[0].Summary() != "Server does not match the Subako CLI's login"):
				t.Fatalf("want a refusal, got %v", diags)
			case tc.server != "" && (diags.HasError() || settings.Server != tc.server):
				t.Fatalf("server = %q, diags = %v, want %q", settings.Server, diags, tc.server)
			}
		})
	}
}

func TestSubakoCLINamesTheProgramAsked(t *testing.T) {
	cli := &signedIn{server: "https://api.example.test"}
	settings, _ := settle(context.Background(), block(nil, nil, nil),
		env(map[string]string{envCLI: "/opt/subako/bin/subako"}), cli.ask)
	if cli.program != "/opt/subako/bin/subako" || settings.Server != "https://api.example.test" {
		t.Fatalf("program = %q, server = %q", cli.program, settings.Server)
	}
}

// Nothing selected in the CLI is no warning: there is no workspace to warn
// about, and the client refuses a user's token without one.
func TestACLIWithNothingSelectedLeavesTheWorkspaceUnset(t *testing.T) {
	cli := &signedIn{server: "https://api.example.test"}
	settings, diags := settle(context.Background(), block(nil, nil, nil), env(nil), cli.ask)
	if len(warnings(diags)) != 0 || settings.WorkspaceID != "" {
		t.Fatalf("settings = %+v, diags = %v", settings, diags)
	}
	if _, err := client.New(settings); err == nil || !strings.Contains(err.Error(), "needs a workspace id") {
		t.Fatalf("err = %v", err)
	}
}

func TestACLIThatCannotAnswerIsAnError(t *testing.T) {
	failing := func(context.Context, string) (client.Issued, client.Auth, error) {
		return client.Issued{}, nil, errors.New("subako was not found")
	}
	_, diags := settle(context.Background(), block(nil, nil, nil), env(nil), failing)
	if !diags.HasError() || !strings.Contains(diags.Errors()[0].Detail(), "subako was not found") {
		t.Fatalf("diags = %v", diags)
	}
}
