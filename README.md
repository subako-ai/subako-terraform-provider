# subako-terraform-provider

The Terraform provider for [Subako](https://subako.ai): a workspace's agents,
skills, model providers, vaults, and vault credentials declared as a
configuration.

It is not on the Terraform Registry. A configuration still names it by the
registry address it would hold, `subako-ai/subako`, and `subako terraform`
installs it from a mirror it fills from this repository's releases:

```hcl
terraform {
  required_version = ">= 1.11"
  required_providers {
    subako = { source = "subako-ai/subako" }
  }
}

provider "subako" {}
```

The provider reads `SUBAKO_TOKEN`, `SUBAKO_WORKSPACE`, and `SUBAKO_SERVER` from
its environment. `subako terraform`, in the Subako CLI, mints a short-lived key
and sets all three for the run; a CI job sets them itself. Usage is documented
with the CLI, not here.

## Working on it

```sh
mise trust && mise install
mise run test         # the suite drives a real Terraform binary
mise run lint
mise run fmt
```

The suite uses `terraform-plugin-testing`, which runs the Terraform binary mise
pins. Write-only attributes need Terraform 1.11 or later.

## Releasing

A tag `v<version>` builds one archive per platform and publishes them, with
their checksum list, as that tag's GitHub release. `PROVIDER_VERSION` in the
Subako CLI is what pins which release it installs.

```sh
git tag v0.1.0 && git push origin v0.1.0
```

`scripts/build.sh` writes `terraform-provider-subako_<version>_<os>_<arch>.zip`
and `terraform-provider-subako_<version>_SHA256SUMS`. The names keep the
`terraform-provider-subako` spelling that Terraform's packed mirror layout
requires, which is not this repository's name. Publishing to the Terraform
Registry would need both: a repository named `terraform-provider-subako`, under
the `subako-ai` namespace, with GPG-signed releases. `subako terraform` downloads
exactly those names from the release page and checks each archive against the
listed digest, so renaming an asset breaks the CLI.

The repository has to be public for that download to work without a token.

## Where the other half lives

Subako's server, CLI, and infrastructure are in the private `Kikuvi-Inc/subako`.
Two things cross the boundary:

- **`CODESYNC` keys.** Several values here are duplicated there: the wire
  headers, the credential marker, the page limit, the field length bounds, the
  grant-name charset, and the registry address. `rg 'CODESYNC\(<key>\)'` lists
  only one repository's sites, so changing one means changing the other
  repository too.
- **The release platforms.** `scripts/build.sh` builds for the platforms the
  Subako CLI is released for. A platform the CLI has but this does not is a
  download that 404s.
