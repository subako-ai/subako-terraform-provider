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
[`examples/`](examples) is a working configuration using every resource below.

## Installing

`terraform init` installs it from the Terraform Registry, as it does any other
provider.

## Authenticating

Signed in with the Subako CLI, there is nothing to set:

```sh
subako login --org <your organization>
subako workspace use <your workspace>
terraform apply
```

With no token set, the provider runs `subako token` for the signed-in user's
access token, and runs it again as that nears its expiry, so an apply that
outlives one token carries on with the next. `$SUBAKO_CLI` names the program
when `subako` is not on the `PATH`.

Each setting is looked for in the provider block, then in its environment
variable, then from the Subako CLI:

| Argument | Environment variable | From the Subako CLI | Otherwise |
| --- | --- | --- | --- |
| `token` | `SUBAKO_TOKEN` | the signed-in user's access token | — |
| `server` | `SUBAKO_SERVER` | the server it is signed in to | `https://api.us.cloud.subako.ai` |
| `workspace_id` | `SUBAKO_WORKSPACE` | the workspace `subako workspace use` selected | — |

The CLI is asked only when no token is set, so a token from the block or the
environment never picks up a server or a workspace from it. An API key names
its own workspace.

### Pin the workspace in a shared configuration

A workspace taken from the CLI follows whatever `subako workspace use` last
chose, and the provider warns when it takes one. Name it in a configuration
others apply:

```hcl
provider "subako" {
  workspace_id = "01a07f90-..."
}
```

### In CI

A job has no login. Mint an API key and set it:

```sh
export SUBAKO_TOKEN=sbk_ak_...
terraform apply -auto-approve
```

Grant it the agent, skill, model provider, vault, and credential permissions,
plus `workspace.read`. Leave out the session permissions, and the
configuration cannot spend credit.

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

To run a local change through Terraform, build it and point a
[`dev_overrides`](https://developer.hashicorp.com/terraform/cli/config/config-file#development-overrides-for-provider-developers)
block at the result:

```sh
mise run build    # writes bin/terraform-provider-subako
```

```hcl
# ~/.terraformrc
provider_installation {
  dev_overrides {
    "subako-ai/subako" = "/path/to/terraform-provider-subako/bin"
  }
  direct {}
}
```

Terraform then runs that binary with no `terraform init`, and warns on every
run that it is doing so.

## Releasing

A tag `v<version>` builds, signs, and publishes a release with GoReleaser
([`.goreleaser.yml`](.goreleaser.yml)), and the Terraform Registry picks it up
from there. The release is signed with the key in the repository secrets
`GPG_PRIVATE_KEY` and `PASSPHRASE`, whose public half the registry holds.

## License

Apache License 2.0; see [LICENSE](LICENSE) and [NOTICE](NOTICE). The HashiCorp
plugin libraries this links stay under their own MPL-2.0 terms.
