package provider

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/statecheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"
	"github.com/hashicorp/terraform-plugin-testing/tfversion"
)

var factories = map[string]func() (tfprotov6.ProviderServer, error){
	"subako": providerserver.NewProtocol6WithError(New("test")()),
}

// Write-only attributes need Terraform 1.11.
var versionChecks = []tfversion.TerraformVersionCheck{
	tfversion.SkipBelow(tfversion.Version1_11_0),
}

func providerBlock(url, workspaceID string) string {
	return fmt.Sprintf(`
provider "subako" {
  server       = %q
  token        = "sbk_ak_test"
  workspace_id = %q
}
`, url, workspaceID)
}

func writeSkill(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"SKILL.md":       "---\nname: docs\ndescription: d\n---\n" + body,
		"scripts/run.sh": "echo hi\n",
	}
	for rel, content := range files {
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func agentConfigHCL(dir, name, prompt string) string {
	return fmt.Sprintf(`
resource "subako_skill" "s" {
  source_dir = %q
}

resource "subako_agent" "a" {
  name              = %q
  model_provider_id = %q
  system_prompt     = %q

  model = {
    anthropic = {
      name                   = "claude-sonnet-5"
      max_tokens             = 8192
      thinking_budget_tokens = 1024
    }
  }

  mcp_servers = [{
    name           = "github"
    url            = "https://mcp.example.test/"
    default_policy = "require_approval"
    tools          = [{ name = "delete_repo", policy = "deny" }]
  }]

  skills = [{
    name     = "docs"
    skill_id = subako_skill.s.id
  }]

  allowed_origins = ["https://app.example.test"]
}
`, dir, name, platformProviderID, prompt)
}

func checkVersionCount(api *fakeAPI, want int) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs := s.RootModule().Resources["subako_agent.a"]
		api.mu.Lock()
		defer api.mu.Unlock()
		agent, ok := api.agents[rs.Primary.ID]
		if !ok {
			return fmt.Errorf("agent %s is not on the server", rs.Primary.ID)
		}
		if got := len(agent.Versions); got != want {
			return fmt.Errorf("the server holds %d versions, want %d", got, want)
		}
		return nil
	}
}

// checkStoredModel reads back the model the newest version carries: the
// format naming the settings, and only that format's settings.
func checkStoredModel(api *fakeAPI, want map[string]any) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs := s.RootModule().Resources["subako_agent.a"]
		api.mu.Lock()
		defer api.mu.Unlock()
		agent, ok := api.agents[rs.Primary.ID]
		if !ok {
			return fmt.Errorf("agent %s is not on the server", rs.Primary.ID)
		}
		latest := agent.Versions[len(agent.Versions)-1]
		got := latest["config"].(map[string]any)["model"]
		gotJSON, _ := json.Marshal(got)
		wantJSON, _ := json.Marshal(want)
		if string(gotJSON) != string(wantJSON) {
			return fmt.Errorf("the version holds model %s, want %s", gotJSON, wantJSON)
		}
		return nil
	}
}

func checkEmpty(api *fakeAPI) resource.TestCheckFunc {
	return func(*terraform.State) error {
		api.mu.Lock()
		defer api.mu.Unlock()
		if len(api.agents) > 0 || len(api.skills) > 0 || len(api.vaults) > 0 {
			return fmt.Errorf("left behind: %d agents, %d skills, %d vaults",
				len(api.agents), len(api.skills), len(api.vaults))
		}
		for id, p := range api.providers {
			if p.Body["type"] == "byok" {
				return fmt.Errorf("model provider %s is left behind", id)
			}
		}
		return nil
	}
}

func TestAgentPublishesAVersionOnlyWhenItsConfigChanges(t *testing.T) {
	api, server := newFakeAPI(t)
	dir := t.TempDir()
	writeSkill(t, dir, "v1\n")
	head := providerBlock(server.URL, api.workspaceID)

	resource.UnitTest(t, resource.TestCase{
		TerraformVersionChecks:   versionChecks,
		ProtoV6ProviderFactories: factories,
		CheckDestroy:             checkEmpty(api),
		Steps: []resource.TestStep{
			{
				Config: head + agentConfigHCL(dir, "support", "be brief"),
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue("subako_agent.a", tfjsonpath.New("version"), knownvalue.Int64Exact(1)),
					statecheck.ExpectKnownValue("subako_agent.a", tfjsonpath.New("workspace_id"), knownvalue.StringExact(api.workspaceID)),
					statecheck.ExpectKnownValue("subako_agent.a",
						tfjsonpath.New("mcp_servers").AtSliceIndex(0).AtMapKey("tools").AtSliceIndex(0).AtMapKey("policy"),
						knownvalue.StringExact("deny")),
					statecheck.ExpectKnownValue("subako_agent.a",
						tfjsonpath.New("skills").AtSliceIndex(0).AtMapKey("version"), knownvalue.Null()),
					statecheck.ExpectKnownValue("subako_agent.a", tfjsonpath.New("allowed_origins"),
						knownvalue.SetExact([]knownvalue.Check{knownvalue.StringExact("https://app.example.test")})),
					statecheck.ExpectKnownValue("subako_skill.s", tfjsonpath.New("name"), knownvalue.StringExact("docs")),
					statecheck.ExpectKnownValue("subako_skill.s", tfjsonpath.New("latest_version"), knownvalue.Int64Exact(1)),
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPair("subako_agent.a", "skills.0.skill_id", "subako_skill.s", "id"),
					checkVersionCount(api, 1),
					checkStoredModel(api, map[string]any{
						"format": "anthropic",
						"config": map[string]any{
							"model":      "claude-sonnet-5",
							"max_tokens": 8192,
							"thinking":   map[string]any{"budget_tokens": 1024},
						},
					}),
				),
			},
			{
				// A rename leaves the version as it is.
				Config: head + agentConfigHCL(dir, "renamed", "be brief"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction("subako_agent.a", plancheck.ResourceActionUpdate),
						plancheck.ExpectKnownValue("subako_agent.a", tfjsonpath.New("version"), knownvalue.Int64Exact(1)),
						plancheck.ExpectResourceAction("subako_skill.s", plancheck.ResourceActionNoop),
					},
				},
				Check: checkVersionCount(api, 1),
			},
			{
				Config: head + agentConfigHCL(dir, "renamed", "be thorough"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectUnknownValue("subako_agent.a", tfjsonpath.New("version")),
					},
				},
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue("subako_agent.a", tfjsonpath.New("version"), knownvalue.Int64Exact(2)),
				},
				Check: checkVersionCount(api, 2),
			},
			{
				ResourceName:      "subako_agent.a",
				ImportState:       true,
				ImportStateVerify: true,
			},
		},
	})
}

// unmanagedOriginsHCL is an agent whose configuration names no
// allowed_origins, so Terraform does not own the allowlist.
func unmanagedOriginsHCL(name string) string {
	return fmt.Sprintf(`
resource "subako_agent" "a" {
  name              = %q
  model_provider_id = %q
  model             = { anthropic = { name = "claude-sonnet-5", max_tokens = 8192 } }
}
`, name, platformProviderID)
}

func checkSecurityPuts(api *fakeAPI, want int) resource.TestCheckFunc {
	return func(*terraform.State) error {
		api.mu.Lock()
		defer api.mu.Unlock()
		if api.securityPuts != want {
			return fmt.Errorf("the allowlist was written %d times, want %d", api.securityPuts, want)
		}
		return nil
	}
}

func TestAnUnsetAllowedOriginsIsNeverWritten(t *testing.T) {
	api, server := newFakeAPI(t)
	head := providerBlock(server.URL, api.workspaceID)

	resource.UnitTest(t, resource.TestCase{
		TerraformVersionChecks:   versionChecks,
		ProtoV6ProviderFactories: factories,
		CheckDestroy:             checkEmpty(api),
		Steps: []resource.TestStep{
			{
				Config: head + unmanagedOriginsHCL("support"),
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue("subako_agent.a", tfjsonpath.New("allowed_origins"),
						knownvalue.SetExact(nil)),
				},
				Check: checkSecurityPuts(api, 0),
			},
			{
				// An allowlist set outside Terraform is read back, not wiped.
				PreConfig: func() {
					api.mu.Lock()
					defer api.mu.Unlock()
					for _, agent := range api.agents {
						agent.Origins = []string{"https://set.example.test"}
					}
				},
				Config: head + unmanagedOriginsHCL("renamed"),
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue("subako_agent.a", tfjsonpath.New("allowed_origins"),
						knownvalue.SetExact([]knownvalue.Check{knownvalue.StringExact("https://set.example.test")})),
				},
				Check: checkSecurityPuts(api, 0),
			},
		},
	})
}

func TestAVersionPublishedElsewhereIsDrift(t *testing.T) {
	api, server := newFakeAPI(t)
	dir := t.TempDir()
	writeSkill(t, dir, "v1\n")
	config := providerBlock(server.URL, api.workspaceID) + agentConfigHCL(dir, "support", "be brief")

	resource.UnitTest(t, resource.TestCase{
		TerraformVersionChecks:   versionChecks,
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{
			{Config: config},
			{
				PreConfig: func() {
					api.mu.Lock()
					defer api.mu.Unlock()
					for _, agent := range api.agents {
						latest := agent.Versions[len(agent.Versions)-1]
						edited := map[string]any{}
						for k, v := range latest {
							edited[k] = v
						}
						stored := map[string]any{}
						for k, v := range latest["config"].(map[string]any) {
							stored[k] = v
						}
						stored["system_prompt"] = "changed by hand"
						edited["config"] = stored
						edited["version"] = len(agent.Versions) + 1
						agent.Versions = append(agent.Versions, edited)
					}
				},
				Config:             config,
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{
				Config: config,
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue("subako_agent.a", tfjsonpath.New("version"), knownvalue.Int64Exact(3)),
					statecheck.ExpectKnownValue("subako_agent.a", tfjsonpath.New("system_prompt"), knownvalue.StringExact("be brief")),
				},
			},
		},
	})
}

func TestChangingASkillsFilesPushesAVersion(t *testing.T) {
	api, server := newFakeAPI(t)
	dir := t.TempDir()
	writeSkill(t, dir, "v1\n")
	config := providerBlock(server.URL, api.workspaceID) + fmt.Sprintf(`
resource "subako_skill" "s" {
  source_dir   = %q
  display_name = "Docs"
}
`, dir)

	resource.UnitTest(t, resource.TestCase{
		TerraformVersionChecks:   versionChecks,
		ProtoV6ProviderFactories: factories,
		CheckDestroy:             checkEmpty(api),
		Steps: []resource.TestStep{
			{
				Config: config,
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue("subako_skill.s", tfjsonpath.New("display_name"), knownvalue.StringExact("Docs")),
					statecheck.ExpectKnownValue("subako_skill.s", tfjsonpath.New("latest_version"), knownvalue.Int64Exact(1)),
				},
			},
			{
				PreConfig: func() { writeSkill(t, dir, "v2\n") },
				Config:    config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction("subako_skill.s", plancheck.ResourceActionUpdate),
						plancheck.ExpectUnknownValue("subako_skill.s", tfjsonpath.New("latest_version")),
					},
				},
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue("subako_skill.s", tfjsonpath.New("latest_version"), knownvalue.Int64Exact(2)),
				},
			},
			{
				// Rewriting the same content changes nothing.
				PreConfig: func() { writeSkill(t, dir, "v2\n") },
				Config:    config,
				PlanOnly:  true,
			},
			{
				PreConfig: func() {
					if err := os.WriteFile(filepath.Join(dir, "extra.txt"), []byte("new"), 0o600); err != nil {
						t.Fatal(err)
					}
				},
				Config:             config,
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{
				ResourceName:            "subako_skill.s",
				ImportState:             true,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"source_dir", "content_hash", "latest_version"},
			},
		},
	})
}

func TestASkillDirectoryWithoutSkillMDFailsThePlan(t *testing.T) {
	api, server := newFakeAPI(t)
	dir := t.TempDir()
	resource.UnitTest(t, resource.TestCase{
		TerraformVersionChecks:   versionChecks,
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{{
			Config: providerBlock(server.URL, api.workspaceID) + fmt.Sprintf(`
resource "subako_skill" "s" {
  source_dir = %q
}
`, dir),
			ExpectError: regexp.MustCompile(`holds no\s+SKILL\.md`),
		}},
	})
}

// modelProviderAddress is the model provider every test below manages.
const modelProviderAddress = "subako_model_provider.p"

// modelProviderHCL is a workspace provider under `anthropic`, holding the key
// given as the version given.
func modelProviderHCL(name, key string, version int) string {
	return modelProviderFormatHCL("anthropic", "https://api.anthropic.com", name, key, version)
}

// modelProviderFormatHCL names the format and the base URL that format's own
// client spells.
func modelProviderFormatHCL(format, baseURL, name, key string, version int) string {
	return fmt.Sprintf(`
variable "key" {
  type      = string
  ephemeral = true
  default   = %q
}

resource "subako_model_provider" "p" {
  format             = %q
  display_name       = %q
  base_url           = %q
  api_key_wo         = var.key
  api_key_wo_version = %d
}
`, key, format, name, baseURL, version)
}

// providerKeyOf pins the key the server holds for the provider under test.
func providerKeyOf(api *fakeAPI, want string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		id := s.RootModule().Resources[modelProviderAddress].Primary.ID
		api.mu.Lock()
		defer api.mu.Unlock()
		if got := api.apiKeys[id]; got != want {
			return fmt.Errorf("provider %s holds key %q, want %q", id, got, want)
		}
		return nil
	}
}

// providerKeyRotations pins how many updates carried a key, so an apply that
// should send none can say so.
func providerKeyRotations(api *fakeAPI, want int) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		id := s.RootModule().Resources[modelProviderAddress].Primary.ID
		api.mu.Lock()
		defer api.mu.Unlock()
		if got := api.keyRotations[id]; got != want {
			return fmt.Errorf("provider %s took %d key updates, want %d", id, got, want)
		}
		return nil
	}
}

// recordProviderID keeps the id of the provider under test for a later step.
func recordProviderID(into *string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		*into = s.RootModule().Resources[modelProviderAddress].Primary.ID
		return nil
	}
}

// sameProviderAs pins that the provider under test is still the one recorded:
// a replacement would have minted another id.
func sameProviderAs(recorded *string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		if id := s.RootModule().Resources[modelProviderAddress].Primary.ID; id != *recorded {
			return fmt.Errorf("provider %s replaced %s", id, *recorded)
		}
		return nil
	}
}

// expectAction is the plan check every step below makes: what the apply is
// about to do to the provider under test.
func expectAction(action plancheck.ResourceActionType) resource.ConfigPlanChecks {
	return resource.ConfigPlanChecks{
		PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(modelProviderAddress, action)},
	}
}

func TestAModelProviderKeyNeverReachesTheState(t *testing.T) {
	api, server := newFakeAPI(t)
	head := providerBlock(server.URL, api.workspaceID)
	var firstID string

	resource.UnitTest(t, resource.TestCase{
		TerraformVersionChecks:   versionChecks,
		ProtoV6ProviderFactories: factories,
		CheckDestroy:             checkEmpty(api),
		Steps: []resource.TestStep{
			{
				Config: head + modelProviderHCL("Mine", "key-1", 1),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckNoResourceAttr(modelProviderAddress, "api_key_wo"),
					resource.TestCheckResourceAttr(modelProviderAddress, "description", ""),
					providerKeyOf(api, "key-1"),
					recordProviderID(&firstID),
				),
			},
			{
				// A new key value alone is invisible to the plan.
				Config:   head + modelProviderHCL("Mine", "key-2", 1),
				PlanOnly: true,
			},
			{
				Config:           head + modelProviderHCL("Renamed", "key-1", 1),
				ConfigPlanChecks: expectAction(plancheck.ResourceActionUpdate),
				Check:            sameProviderAs(&firstID),
			},
			{
				Config:           head + modelProviderHCL("Renamed", "key-2", 2),
				ConfigPlanChecks: expectAction(plancheck.ResourceActionUpdate),
				Check: resource.ComposeAggregateTestCheckFunc(
					sameProviderAs(&firstID),
					providerKeyOf(api, "key-2"),
				),
			},
		},
	})
}

func TestBumpingAModelProviderKeyVersionRotatesItInPlace(t *testing.T) {
	api, server := newFakeAPI(t)
	head := providerBlock(server.URL, api.workspaceID)
	var firstID string

	resource.UnitTest(t, resource.TestCase{
		TerraformVersionChecks:   versionChecks,
		ProtoV6ProviderFactories: factories,
		CheckDestroy:             checkEmpty(api),
		Steps: []resource.TestStep{
			{
				Config: head + modelProviderHCL("Mine", "key-1", 1),
				Check:  recordProviderID(&firstID),
			},
			{
				// A bump updates the provider that already exists, carrying
				// the key the configuration now names.
				Config:           head + modelProviderHCL("Mine", "key-2", 2),
				ConfigPlanChecks: expectAction(plancheck.ResourceActionUpdate),
				Check: resource.ComposeAggregateTestCheckFunc(
					sameProviderAs(&firstID),
					providerKeyOf(api, "key-2"),
					providerKeyRotations(api, 1),
				),
			},
			{
				// A rename leaves the version alone, so the update sends no
				// key -- even though the configuration names another one.
				Config:           head + modelProviderHCL("Renamed", "key-3", 2),
				ConfigPlanChecks: expectAction(plancheck.ResourceActionUpdate),
				Check: resource.ComposeAggregateTestCheckFunc(
					providerKeyOf(api, "key-2"),
					providerKeyRotations(api, 1),
				),
			},
		},
	})
}

// The API returns neither a provider's base URL nor anything about its key,
// so after an update that failed the state must go on saying what was last
// sent: a change recorded but never sent leaves the next plan nothing to retry.
func TestAFailedModelProviderUpdateIsRetriedByTheNextApply(t *testing.T) {
	api, server := newFakeAPI(t)
	head := providerBlock(server.URL, api.workspaceID)
	refuse := func(refused bool) func() {
		return func() {
			api.mu.Lock()
			defer api.mu.Unlock()
			api.refuseProviderUpdates = refused
		}
	}

	resource.UnitTest(t, resource.TestCase{
		TerraformVersionChecks:   versionChecks,
		ProtoV6ProviderFactories: factories,
		CheckDestroy:             checkEmpty(api),
		Steps: []resource.TestStep{
			{Config: head + modelProviderHCL("Mine", "key-1", 1)},
			{
				PreConfig:   refuse(true),
				Config:      head + modelProviderHCL("Mine", "key-2", 2),
				ExpectError: regexp.MustCompile(`Could not update model provider`),
			},
			{
				// The rotation that failed is still a change, and this apply
				// is the one that sends it.
				PreConfig:        refuse(false),
				Config:           head + modelProviderHCL("Mine", "key-2", 2),
				ConfigPlanChecks: expectAction(plancheck.ResourceActionUpdate),
				Check: resource.ComposeAggregateTestCheckFunc(
					providerKeyOf(api, "key-2"),
					providerKeyRotations(api, 1),
				),
			},
		},
	})
}

func TestChangingAModelProviderFormatReplacesIt(t *testing.T) {
	api, server := newFakeAPI(t)
	head := providerBlock(server.URL, api.workspaceID)

	resource.UnitTest(t, resource.TestCase{
		TerraformVersionChecks:   versionChecks,
		ProtoV6ProviderFactories: factories,
		CheckDestroy:             checkEmpty(api),
		Steps: []resource.TestStep{
			{Config: head + modelProviderHCL("Mine", "key-1", 1)},
			{
				// The API cannot change a format, so it is the one change to
				// a provider that is a replacement.
				Config: head + modelProviderFormatHCL("openai_responses",
					"https://api.openai.com/v1", "Mine", "key-1", 1),
				ConfigPlanChecks: expectAction(plancheck.ResourceActionDestroyBeforeCreate),
			},
		},
	})
}

func TestAnImportedModelProviderIsAdoptedRatherThanReplaced(t *testing.T) {
	api, server := newFakeAPI(t)
	head := providerBlock(server.URL, api.workspaceID)
	id := api.seedProvider("anthropic", "Mine", "key-on-the-server")
	importBlock := fmt.Sprintf(`
import {
  to = subako_model_provider.p
  id = %q
}
`, id)

	resource.UnitTest(t, resource.TestCase{
		TerraformVersionChecks:   versionChecks,
		ProtoV6ProviderFactories: factories,
		CheckDestroy:             checkEmpty(api),
		Steps: []resource.TestStep{
			{
				// An import writes only the id, so the prior key version is
				// null and the one the configuration names is a change: the
				// first apply updates the provider -- sending the base URL
				// the API does not return, and the configuration's own key --
				// rather than replacing it.
				Config:           head + importBlock + modelProviderHCL("Mine", "key-1", 1),
				ConfigPlanChecks: expectAction(plancheck.ResourceActionUpdate),
				Check:            providerKeyOf(api, "key-1"),
			},
			{
				ResourceName:      modelProviderAddress,
				Config:            head + modelProviderHCL("Mine", "key-1", 1),
				ImportState:       true,
				ImportStateVerify: true,
				// The API returns neither, so no import recovers them.
				ImportStateVerifyIgnore: []string{"base_url", "api_key_wo_version"},
			},
			{
				// The provider now holds the configuration's key, so the
				// adoption has nothing left to send.
				Config: head + modelProviderHCL("Mine", "key-1", 1),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
		},
	})
}

// agentGrantsHCL is an agent carrying the grant lines given: none at all, or
// lists spelled empty.
func agentGrantsHCL(grants string) string {
	return fmt.Sprintf(`
resource "subako_agent" "a" {
  name              = "support"
  model_provider_id = %q
  model             = { anthropic = { name = "claude-sonnet-5", max_tokens = 8192 } }
%s
}
`, platformProviderID, grants)
}

// An empty grant list and an absent one carry the same config, and a version
// is published only when the config differs.
func TestRespellingAnEmptyGrantListPublishesNoVersion(t *testing.T) {
	api, server := newFakeAPI(t)
	head := providerBlock(server.URL, api.workspaceID)
	empty := `  mcp_servers = []
  skills      = []`

	resource.UnitTest(t, resource.TestCase{
		TerraformVersionChecks:   versionChecks,
		ProtoV6ProviderFactories: factories,
		CheckDestroy:             checkEmpty(api),
		Steps: []resource.TestStep{
			{
				Config: head + agentGrantsHCL(""),
				Check:  checkVersionCount(api, 1),
			},
			{
				Config: head + agentGrantsHCL(empty),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction("subako_agent.a", plancheck.ResourceActionUpdate),
						plancheck.ExpectKnownValue("subako_agent.a", tfjsonpath.New("version"), knownvalue.Int64Exact(1)),
					},
				},
				Check: checkVersionCount(api, 1),
			},
			{
				Config: head + agentGrantsHCL(""),
				Check:  checkVersionCount(api, 1),
			},
		},
	})
}

// The server counts characters, so a value it takes is one the provider must
// take: 200 of these weigh 600 bytes.
func TestABoundIsCountedInCharacters(t *testing.T) {
	api, server := newFakeAPI(t)
	name := strings.Repeat("\u3042", 200)

	resource.UnitTest(t, resource.TestCase{
		TerraformVersionChecks:   versionChecks,
		ProtoV6ProviderFactories: factories,
		CheckDestroy:             checkEmpty(api),
		Steps: []resource.TestStep{{
			Config: providerBlock(server.URL, api.workspaceID) + fmt.Sprintf(`
resource "subako_vault" "v" {
  display_name = %q
}
`, name),
			ConfigStateChecks: []statecheck.StateCheck{
				statecheck.ExpectKnownValue("subako_vault.v", tfjsonpath.New("display_name"),
					knownvalue.StringExact(name)),
			},
		}},
	})
}

// A vault whose metadata another client stored as something this resource
// cannot hold stays readable, plannable, and destroyable; the next apply
// writes what the configuration names over it.
func TestAVaultWhoseMetadataIsNotLabelsStaysManageable(t *testing.T) {
	api, server := newFakeAPI(t)
	config := providerBlock(server.URL, api.workspaceID) + `
resource "subako_vault" "v" {
  display_name = "shared"
  metadata     = { team = "support" }
}
`
	storedMetadata := func(value any) func() {
		return func() {
			api.mu.Lock()
			defer api.mu.Unlock()
			for _, vault := range api.vaults {
				vault.Vault["metadata"] = value
			}
		}
	}
	labelsAreBack := []statecheck.StateCheck{
		statecheck.ExpectKnownValue("subako_vault.v", tfjsonpath.New("metadata"),
			knownvalue.MapExact(map[string]knownvalue.Check{"team": knownvalue.StringExact("support")})),
	}

	resource.UnitTest(t, resource.TestCase{
		TerraformVersionChecks:   versionChecks,
		ProtoV6ProviderFactories: factories,
		CheckDestroy:             checkEmpty(api),
		Steps: []resource.TestStep{
			{Config: config},
			{
				// Metadata that is no object at all.
				PreConfig:         storedMetadata([]any{"not", "an", "object"}),
				Config:            config,
				ConfigStateChecks: labelsAreBack,
			},
			{
				// An object, under a value no label can hold.
				PreConfig:         storedMetadata(map[string]any{"team": 3}),
				Config:            config,
				ConfigStateChecks: labelsAreBack,
			},
		},
	})
}

// An argument set to the empty string names nothing, and nothing is what the
// provider refuses: falling back would reach a server, or present a
// credential, the configuration did not name.
func TestAnEmptyProviderArgumentIsRefusedRatherThanFilledIn(t *testing.T) {
	api, server := newFakeAPI(t)
	cases := map[string]struct{ block, err string }{
		"an empty token": {fmt.Sprintf(`
provider "subako" {
  server       = %q
  token        = ""
  workspace_id = %q
}`, server.URL, api.workspaceID), `a token is required`},
		"an empty server": {fmt.Sprintf(`
provider "subako" {
  server       = ""
  token        = "sbk_ak_test"
  workspace_id = %q
}`, api.workspaceID), `server must be an http\(s\) URL`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			// Set, so an argument folded into "unset" would reach them.
			t.Setenv(envServer, server.URL)
			t.Setenv(envToken, "sbk_ak_test")
			t.Setenv(envWorkspace, api.workspaceID)
			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: factories,
				Steps: []resource.TestStep{{
					Config: tc.block + `
data "subako_workspace" "current" {}
`,
					ExpectError: regexp.MustCompile(tc.err),
				}},
			})
		})
	}
}

func vaultHCL(credentialName string) string {
	return fmt.Sprintf(`
resource "subako_vault" "v" {
  display_name = "shared"
  metadata     = { team = "support" }
}

resource "subako_vault_credential" "c" {
  vault_id       = subako_vault.v.id
  target         = "https://mcp.example.test/"
  display_name   = %q
  secrets_wo_version = 1

  static_bearer = {
    token_wo = "secret-token"
  }
}
`, credentialName)
}

func TestAVaultCredentialIsReplacedOnChange(t *testing.T) {
	api, server := newFakeAPI(t)
	head := providerBlock(server.URL, api.workspaceID)

	resource.UnitTest(t, resource.TestCase{
		TerraformVersionChecks:   versionChecks,
		ProtoV6ProviderFactories: factories,
		CheckDestroy:             checkEmpty(api),
		Steps: []resource.TestStep{
			{
				Config: head + vaultHCL("GitHub"),
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue("subako_vault.v", tfjsonpath.New("metadata"),
						knownvalue.MapExact(map[string]knownvalue.Check{"team": knownvalue.StringExact("support")})),
					statecheck.ExpectKnownValue("subako_vault_credential.c",
						tfjsonpath.New("static_bearer").AtMapKey("token_wo"), knownvalue.Null()),
					statecheck.ExpectKnownValue("subako_vault_credential.c", tfjsonpath.New("oauth"), knownvalue.Null()),
				},
				Check: func(s *terraform.State) error {
					id := s.RootModule().Resources["subako_vault_credential.c"].Primary.ID
					api.mu.Lock()
					defer api.mu.Unlock()
					payload := api.secrets[id]
					if payload["auth_scheme"] != "static_bearer" || payload["token"] != "secret-token" {
						return fmt.Errorf("the server holds payload %v", payload)
					}
					return nil
				},
			},
			{
				Config: head + vaultHCL("GitHub bot"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction("subako_vault_credential.c", plancheck.ResourceActionDestroyBeforeCreate),
						plancheck.ExpectResourceAction("subako_vault.v", plancheck.ResourceActionNoop),
					},
				},
			},
			{
				ResourceName:      "subako_vault_credential.c",
				ImportState:       true,
				ImportStateIdFunc: credentialImportID,
				ImportStateVerify: true,
				// The version names secrets the API does not return, so no
				// import can recover it.
				ImportStateVerifyIgnore: []string{"secrets_wo_version"},
			},
			{
				ResourceName:      "subako_vault.v",
				ImportState:       true,
				ImportStateVerify: true,
			},
		},
	})
}

// agentHCL is an agent carrying what the case is about, under a name and a
// model provider id the schema accepts, so it fails for that reason alone.
func agentHCL(body string) string {
	return fmt.Sprintf(`
resource "subako_agent" "a" {
  name              = "a"
  model_provider_id = %q
%s
}`, platformProviderID, body)
}

const anthropicModelHCL = `  model = { anthropic = { name = "c", max_tokens = 1 } }`

func TestInvalidConfigsFailValidation(t *testing.T) {
	api, server := newFakeAPI(t)
	head := providerBlock(server.URL, api.workspaceID)
	cases := map[string]struct {
		config string
		err    string
	}{
		"a model naming both formats": {agentHCL(`  model = {
    anthropic        = { name = "c", max_tokens = 1 }
    openai_responses = { name = "gpt", max_tokens = 1, context_window = 2 }
  }`), `Invalid Attribute Combination`},
		"a model naming neither format": {agentHCL(`  model = {}`), `Missing Attribute Configuration`},
		"openai without a context window": {
			agentHCL(`  model = { openai_responses = { name = "gpt", max_tokens = 1 } }`), `context_window`},
		"a bad grant name": {agentHCL(anthropicModelHCL + `
  mcp_servers = [{ name = "Bad Name", url = "https://x", default_policy = "allow" }]`),
			`lowercase alphanumeric`},
		// An id the API would answer with in another spelling never settles:
		// the read writes the canonical one back over what the configuration
		// names, so the plan refuses it instead.
		"a model provider id the API does not spell that way": {fmt.Sprintf(`
resource "subako_agent" "a" {
  name              = "a"
  model_provider_id = %q
%s
}`, strings.ToUpper(platformProviderID), anthropicModelHCL), `lowercase hyphenated UUID`},
		"a skill id the API does not spell that way": {agentHCL(anthropicModelHCL + `
  skills = [{ name = "docs", skill_id = "SKILL-1" }]`), `lowercase hyphenated UUID`},
		// The bound the server keeps counts characters, so 201 of them are
		// refused whatever they weigh in bytes.
		"a name past the server's bound": {fmt.Sprintf(`
resource "subako_agent" "a" {
  name              = %q
  model_provider_id = %q
%s
}`, strings.Repeat("\u3042", 201), platformProviderID, anthropicModelHCL),
			`between 1 and 200 characters, got: 201`},
		"a credential naming both auth schemes": {`
resource "subako_vault_credential" "c" {
  vault_id       = "v"
  target         = "https://x"
  secrets_wo_version = 1
  static_bearer  = { token_wo = "t" }
  oauth          = { access_token_wo = "t" }
}`, `Invalid Attribute Combination`},
		"a credential naming neither auth scheme": {`
resource "subako_vault_credential" "c" {
  vault_id       = "v"
  target         = "https://x"
  secrets_wo_version = 1
}`, `Missing Attribute Configuration`},
		"a refresh block without its token endpoint": {`
resource "subako_vault_credential" "c" {
  vault_id       = "v"
  target         = "https://x"
  secrets_wo_version = 1
  oauth = {
    access_token_wo = "t"
    refresh         = { client_id = "c", refresh_token_wo = "r" }
  }
}`, `token_endpoint`},
		"a token endpoint auth that is not an object": {`
resource "subako_vault_credential" "c" {
  vault_id       = "v"
  target         = "https://x"
  secrets_wo_version = 1
  oauth = {
    access_token_wo = "t"
    refresh = {
      token_endpoint         = "https://x/token"
      client_id              = "c"
      refresh_token_wo       = "r"
      token_endpoint_auth_wo = jsonencode(["not", "an", "object"])
    }
  }
}`, `Not a JSON object`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			resource.UnitTest(t, resource.TestCase{
				TerraformVersionChecks:   versionChecks,
				ProtoV6ProviderFactories: factories,
				Steps: []resource.TestStep{{
					Config:      head + tc.config,
					PlanOnly:    true,
					ExpectError: regexp.MustCompile(tc.err),
				}},
			})
		})
	}
}

const oauthCredentialHCL = `
resource "subako_vault" "v" {
  display_name = "user"
}

resource "subako_vault_credential" "c" {
  vault_id       = subako_vault.v.id
  target         = "https://mcp.example.test/"
  secrets_wo_version = 1

  oauth = {
    access_token_wo = "access"
    expires_at      = "2030-01-01T00:00:00Z"

    refresh = {
      token_endpoint         = "https://auth.example.test/token"
      client_id              = "client"
      refresh_token_wo       = "refresh"
      token_endpoint_auth_wo = jsonencode({ type = "client_secret_post", client_secret = "s" })
    }
  }
}
`

func credentialImportID(s *terraform.State) (string, error) {
	rs := s.RootModule().Resources["subako_vault_credential.c"]
	return rs.Primary.Attributes["vault_id"] + "/" + rs.Primary.ID, nil
}

func TestAnOAuthCredentialSendsItsRefreshBlock(t *testing.T) {
	api, server := newFakeAPI(t)
	resource.UnitTest(t, resource.TestCase{
		TerraformVersionChecks:   versionChecks,
		ProtoV6ProviderFactories: factories,
		CheckDestroy:             checkEmpty(api),
		Steps: []resource.TestStep{{
			Config: providerBlock(server.URL, api.workspaceID) + oauthCredentialHCL,
			ConfigStateChecks: []statecheck.StateCheck{
				statecheck.ExpectKnownValue("subako_vault_credential.c",
					tfjsonpath.New("oauth").AtMapKey("access_token_wo"), knownvalue.Null()),
				statecheck.ExpectKnownValue("subako_vault_credential.c",
					tfjsonpath.New("oauth").AtMapKey("refresh").AtMapKey("refresh_token_wo"), knownvalue.Null()),
				statecheck.ExpectKnownValue("subako_vault_credential.c",
					tfjsonpath.New("oauth").AtMapKey("refresh").AtMapKey("client_id"),
					knownvalue.StringExact("client")),
				statecheck.ExpectKnownValue("subako_vault_credential.c", tfjsonpath.New("static_bearer"), knownvalue.Null()),
			},
			Check: func(s *terraform.State) error {
				id := s.RootModule().Resources["subako_vault_credential.c"].Primary.ID
				api.mu.Lock()
				defer api.mu.Unlock()
				p := api.secrets[id]
				refresh, _ := p["refresh"].(map[string]any)
				auth, _ := refresh["token_endpoint_auth"].(map[string]any)
				if p["auth_scheme"] != "oauth" || p["access_token"] != "access" ||
					p["expires_at"] != "2030-01-01T00:00:00Z" ||
					refresh["refresh_token"] != "refresh" || refresh["client_id"] != "client" ||
					auth["type"] != "client_secret_post" {
					return fmt.Errorf("the server holds payload %v", p)
				}
				return nil
			},
		}},
	})
}

// No import recovers `secrets_wo_version`, so an imported bearer credential is
// replaced on the next apply like any other: that apply is what makes it hold
// the configuration's secret.
func TestAnImportedBearerCredentialIsReplacedOnTheNextApply(t *testing.T) {
	api, server := newFakeAPI(t)
	head := providerBlock(server.URL, api.workspaceID)

	resource.UnitTest(t, resource.TestCase{
		TerraformVersionChecks:   versionChecks,
		ProtoV6ProviderFactories: factories,
		CheckDestroy:             checkEmpty(api),
		Steps: []resource.TestStep{
			{Config: head + vaultHCL("GitHub")},
			{
				ResourceName:       "subako_vault_credential.c",
				ImportState:        true,
				ImportStateKind:    resource.ImportBlockWithID,
				ImportStateIdFunc:  credentialImportID,
				ExpectNonEmptyPlan: true,
				ImportPlanChecks: resource.ImportPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction("subako_vault_credential.c",
							plancheck.ResourceActionReplace),
					},
				},
			},
		},
	})
}

// The API answers a credential's scheme but never what is sealed under it, so
// importing an OAuth credential recovers neither its expiry nor its refresh
// settings, and the next apply replaces it -- which is also the only way to
// resend the secrets an import cannot recover either.
func TestAnImportedOAuthCredentialIsReplacedOnTheNextApply(t *testing.T) {
	api, server := newFakeAPI(t)
	head := providerBlock(server.URL, api.workspaceID)
	resource.UnitTest(t, resource.TestCase{
		TerraformVersionChecks:   versionChecks,
		ProtoV6ProviderFactories: factories,
		CheckDestroy:             checkEmpty(api),
		Steps: []resource.TestStep{
			{Config: head + oauthCredentialHCL},
			{
				ResourceName:            "subako_vault_credential.c",
				ImportState:             true,
				ImportStateIdFunc:       credentialImportID,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"secrets_wo_version", "oauth.expires_at", "oauth.refresh"},
			},
			{
				ResourceName:       "subako_vault_credential.c",
				ImportState:        true,
				ImportStateKind:    resource.ImportBlockWithID,
				ImportStateIdFunc:  credentialImportID,
				ExpectNonEmptyPlan: true,
				ImportPlanChecks: resource.ImportPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction("subako_vault_credential.c",
							plancheck.ResourceActionReplace),
					},
				},
			},
		},
	})
}

func TestDataSourcesReadTheWorkspace(t *testing.T) {
	api, server := newFakeAPI(t)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{{
			Config: providerBlock(server.URL, api.workspaceID) + `
data "subako_workspace" "current" {}
data "subako_model_providers" "all" {}
`,
			ConfigStateChecks: []statecheck.StateCheck{
				statecheck.ExpectKnownValue("data.subako_workspace.current", tfjsonpath.New("name"), knownvalue.StringExact("acme")),
				statecheck.ExpectKnownValue("data.subako_model_providers.all",
					tfjsonpath.New("providers").AtSliceIndex(0).AtMapKey("models").AtSliceIndex(0).AtMapKey("model"),
					knownvalue.StringExact("claude-sonnet-5")),
			},
		}},
	})
}

// With no token set anywhere, the provider asks the Subako CLI, and runs as
// the signed-in user in the workspace that CLI selected -- through Terraform
// itself, as `terraform plan` would.
func TestWithNoTokenTheProviderRunsAsTheSignedInUser(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in CLI is a shell script")
	}
	api, server := newFakeAPI(t)
	api.bearer = "sbk_at_signed_in"
	cli := filepath.Join(t.TempDir(), "subako")
	output := fmt.Sprintf(`{"version":1,"server":%q,"access_token":"sbk_at_signed_in",`+
		`"expires_at":"2099-01-01T00:00:00Z","workspace_id":%q,"workspace_name":"infra"}`,
		server.URL, api.workspaceID)
	if err := os.WriteFile(cli, []byte("#!/bin/sh\ncat <<'EOF'\n"+output+"\nEOF\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envCLI, cli)
	t.Setenv(envServer, "")
	t.Setenv(envToken, "")
	t.Setenv(envWorkspace, "")
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{{
			Config: `
provider "subako" {}
data "subako_workspace" "current" {}
`,
			ConfigStateChecks: []statecheck.StateCheck{
				statecheck.ExpectKnownValue("data.subako_workspace.current", tfjsonpath.New("name"),
					knownvalue.StringExact("acme")),
			},
		}},
	})
}

func TestAUserTokenWithoutAWorkspaceIsRefused(t *testing.T) {
	t.Setenv(envWorkspace, "")
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{{
			Config: `
provider "subako" {
  server = "https://api.example.test"
  token  = "sbk_at_user"
}
data "subako_model_providers" "all" {}
`,
			ExpectError: regexp.MustCompile(`needs a workspace id`),
		}},
	})
}
