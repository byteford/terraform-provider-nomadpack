# terraform-provider-nomadpack

A Terraform provider that drives [nomad-pack](https://developer.hashicorp.com/nomad/tools/nomad-pack)
deployments as Terraform resources.

## Design

This provider deliberately does **not** re-implement pack rendering or talk
to the Nomad API directly. Instead it shells out to the `nomad-pack` binary
and drives its own lifecycle commands:

| Terraform op | nomad-pack command                    |
|--------------|----------------------------------------|
| Create       | `nomad-pack run <pack> -name=<name>`   |
| Read         | `nomad-pack plan <pack> -name=<name> -diff` |
| Update       | `nomad-pack run <pack> -name=<name>`   |
| Delete       | `nomad-pack destroy <pack> -name=<name>` |

nomad-pack's own injected job metadata (`pack.name`, `pack.version`, etc.)
stays the single source of truth for what's deployed on the cluster — this
provider just automates driving the CLI consistently, and keeps the last
plan's diff visible in Terraform state (see the `diff` computed attribute
on `nomadpack_deployment`).

**Requirement:** the `nomad-pack` binary must be present on PATH wherever
Terraform runs (or point `provider.binary_path` at it). It is not vendored
or downloaded by this provider.

**Known caveat:** editing a pack-managed job directly in the Nomad UI can
desync nomad-pack's tracking metadata (see
[hashicorp/nomad-pack#425](https://github.com/hashicorp/nomad-pack/issues/425),
fixed in recent releases but worth knowing about) — avoid manual UI edits
on jobs this provider manages, the same way you would with Helm-managed
Kubernetes resources.

## Usage

See `examples/provider/provider.tf` and
`examples/resources/nomadpack_deployment/resource.tf`.

## Building locally

```sh
go build -o terraform-provider-nomadpack
```

## Testing against a local Terraform without publishing

Add a dev override so Terraform uses your locally built binary instead of
fetching from a registry:

```hcl
# ~/.terraformrc
provider_installation {
  dev_overrides {
    "byteford/nomadpack" = "/absolute/path/to/this/repo"
  }
  direct {}
}
```

Then `terraform plan`/`apply` in a directory using the provider will use
your local build. No `terraform init` needed while the override is active.

## Releasing

Tag and push:

```sh
git tag v0.1.0
git push origin v0.1.0
```

`.github/workflows/release.yml` runs [GoReleaser](https://goreleaser.com)
against `.goreleaser.yml`, building binaries for linux/darwin/windows/freebsd
across amd64/386/arm/arm64, zipping them, and attaching them plus a
`terraform-registry-manifest.json` and SHA256SUMS to a GitHub Release.

- **Private/self-hosted use (your homelab):** nothing further needed —
  point a [filesystem or network mirror](https://developer.hashicorp.com/terraform/cli/config/config-file#provider-installation)
  at the GitHub release, or just use the dev override above. Unsigned
  releases work fine here; the workflow skips GPG signing automatically if
  no `GPG_PRIVATE_KEY` secret is set.
- **Public Terraform Registry:** requires the SHA256SUMS file to be GPG
  signed with a key registered to your Registry account. Add repo secrets
  `GPG_PRIVATE_KEY` (armored: `gpg --export-secret-keys --armor <key-id>`)
  and `PASSPHRASE`, and the workflow will sign automatically.

## Known limitations

- `nomad-pack plan`'s exit codes (`0` = no changes, `1` = changes,
  `255` = error) are documented and used directly for drift detection,
  but "resource no longer exists" is inferred from stderr/stdout text
  matching rather than a dedicated exit code, since nomad-pack doesn't
  expose one. If a nomad-pack version phrases that error differently,
  `Read` will surface it as an error rather than silently removing the
  resource from state — check the error text if this happens.
- `terraform import` only recovers the deployment `name`; `pack`,
  `registry`, and `ref` must be filled in manually afterward, since
  nomad-pack's job metadata doesn't retain the registry name or exact ref
  used at deploy time.
- This was written and reviewed but **not compiled** in the environment
  it was authored in (no Go toolchain / module proxy access there). Run
  `go build ./...` and `go vet ./...` before relying on it — see
  `.github/workflows/ci.yml` for the same checks running on every push.
