# Terraform Provider for Subako

Manage a [Subako](https://subako.ai) workspace as code: the agents it runs, the
skills they load, the model providers they call, and the vaults holding the
credentials they present.

```hcl
terraform {
  required_version = ">= 1.11"
  required_providers {
    subako = {
      source = "subako-ai/subako"
    }
  }
}

provider "subako" {}

resource "subako_agent" "release" {
  name              = "release"
  model_provider_id = subako_model_provider.anthropic.id
  system_prompt     = "You prepare releases. Read the merged pull requests and draft the notes."

  model = {
    anthropic = {
      name       = "claude-sonnet-5"
      max_tokens = 8192
    }
  }
}
```

Terraform 1.11 or later is required: credentials are passed as write-only
arguments, so no secret is ever written to your state.

## Installing

The provider is not on the Terraform Registry yet, so `terraform init` will not
find it on its own. The Subako CLI installs it for you:

```sh
subako login --org <your organization>
subako workspace use <your workspace>

subako terraform init
subako terraform plan
subako terraform apply
```

`subako terraform` downloads the release this CLI expects, verifies it against
the checksums published beside it, and hands Terraform a configuration that
installs it. Every other provider in your configuration installs as usual. It
also mints a short-lived API key for the run and revokes it when Terraform
exits, so there is nothing to rotate afterwards.

## Authenticating

The `provider "subako"` block reads three settings, each falling back to an
environment variable. `subako terraform` sets all three; a CI job sets them
itself.

| Argument | Environment variable | Meaning |
| --- | --- | --- |
| `token` | `SUBAKO_TOKEN` | An API key (`sbk_ak_…`) or a user access token (`sbk_at_…`) |
| `workspace_id` | `SUBAKO_WORKSPACE` | The workspace to act in. An API key implies its own |
| `server` | `SUBAKO_SERVER` | The API to reach. Defaults to `https://api.us.cloud.subako.ai` |

In CI, mint an API key with `subako api-key mint` and set it directly:

```sh
export SUBAKO_TOKEN=sbk_ak_...
export SUBAKO_WORKSPACE=<workspace id>
terraform apply -auto-approve
```

The key needs the agent, skill, model provider, vault, and credential
permissions, plus `workspace.read`. It cannot start a session, so a
configuration can never spend credit.

## What you can manage

| Resource | Manages |
| --- | --- |
| `subako_agent` | An agent: its model, system prompt, MCP servers, skills, and allowed origins |
| `subako_skill` | A skill, published from a directory of files |
| `subako_model_provider` | A model upstream bound to your own API key |
| `subako_vault` | A vault an agent's sessions draw credentials from |
| `subako_vault_credential` | One credential in a vault: a static bearer token or an OAuth grant |

| Data source | Reads |
| --- | --- |
| `subako_model_providers` | Every model provider the workspace can use, including the ones Subako publishes |
| `subako_workspace` | The workspace the credential acts in |

### Secrets stay out of your state

Every argument carrying a secret is write-only: `api_key_wo` on a model
provider, `token_wo` and `access_token_wo` on a credential. Terraform sends
them and forgets them. A companion `*_wo_version` argument is what you change
to send a new value:

```hcl
resource "subako_model_provider" "anthropic" {
  format       = "anthropic"
  display_name = "Anthropic"
  base_url     = "https://api.anthropic.com"
  api_key_wo   = var.anthropic_api_key

  # Bump this to rotate the key. The provider keeps its id, so the agent
  # versions naming it keep running.
  api_key_wo_version = 1
}
```

## Development

```sh
mise trust && mise install
mise run test    # the suite drives a real Terraform binary
mise run lint
mise run fmt
```

A tag `v<version>` builds an archive per platform, with a checksum list, and
publishes them as that tag's release.

## License

Apache License 2.0; see [LICENSE](LICENSE) and [NOTICE](NOTICE). The HashiCorp
plugin libraries this links stay under their own MPL-2.0 terms.
