# Everything this provider manages, in one workspace: an agent, the model
# provider it calls, the skill it loads, and the vault its sessions draw
# credentials from.
#
#   subako workspace use <name>
#   subako terraform init
#   TF_VAR_anthropic_api_key=... TF_VAR_github_token=... subako terraform apply
#
# Neither secret below reaches the state: both are write-only arguments.

terraform {
  # Write-only arguments need Terraform 1.11.
  required_version = ">= 1.11"

  # No backend: state is the configuration's own, and this example keeps it
  # beside itself. A real one names S3, HCP Terraform, or whatever the rest of
  # the estate uses.

  required_providers {
    subako = {
      source = "subako-ai/subako"
    }
  }
}

# `subako terraform` supplies the server, the credential, and the workspace.
provider "subako" {}

variable "anthropic_api_key" {
  description = "The key this workspace's own Anthropic provider holds. Never stored in the state."
  type        = string
  sensitive   = true
  ephemeral   = true
}

variable "github_token" {
  description = "A token for the GitHub MCP server. Never stored in the state."
  type        = string
  sensitive   = true
  ephemeral   = true
}

data "subako_model_providers" "all" {}

resource "subako_model_provider" "anthropic" {
  format       = "anthropic"
  display_name = "Anthropic"
  base_url     = "https://api.anthropic.com"
  api_key_wo   = var.anthropic_api_key

  # Which key the provider holds. Bump to send a new one, which rotates the
  # key in place: the provider keeps its id.
  api_key_wo_version = 1
}

resource "subako_skill" "release_notes" {
  source_dir = "${path.module}/skills/release-notes"
}

resource "subako_agent" "release" {
  name              = "release"
  model_provider_id = subako_model_provider.anthropic.id
  system_prompt     = "You prepare releases. Read the merged pull requests and draft the notes."

  model = {
    anthropic = {
      name                   = "claude-sonnet-5"
      max_tokens             = 8192
      thinking_budget_tokens = 2048
    }
  }

  mcp_servers = [{
    name           = "github"
    url            = "https://api.githubcopilot.com/mcp/"
    default_policy = "allow"
    tools = [
      { name = "merge_pull_request", policy = "require_approval" },
      { name = "delete_repository", policy = "deny" },
    ]
  }]

  skills = [{
    name     = "release-notes"
    skill_id = subako_skill.release_notes.id
  }]

  allowed_origins = ["https://release.example.com"]
}

resource "subako_vault" "team" {
  display_name = "release team"
  metadata = {
    owner = "platform"
  }
}

resource "subako_vault_credential" "github" {
  vault_id     = subako_vault.team.id
  target       = "https://api.githubcopilot.com/mcp/"
  display_name = "GitHub"

  static_bearer = {
    token_wo = var.github_token
  }

  # Bump to send a new token.
  secrets_wo_version = 1
}

output "agent_id" {
  value = subako_agent.release.id
}

output "agent_version" {
  value = subako_agent.release.version
}

output "vault_id" {
  value = subako_vault.team.id
}

# The providers the platform offers, which an agent can name instead of the
# one above.
output "platform_model_providers" {
  value = [for p in data.subako_model_providers.all.providers : p.id if p.type == "platform"]
}
