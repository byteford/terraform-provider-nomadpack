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
provider just automates driving the CLI consistently. `diff_hash` holds a
fingerprint of the last filtered `nomad-pack plan` diff, and it's more than
informational: writing a non-empty value there when `Read` detects drift
is what causes `terraform plan` to show `~ update in-place` and
`terraform apply` to actually run `nomad-pack run` to reconcile it — a
plan-time warning alone doesn't cause Terraform to schedule anything,
since Terraform's apply behavior is driven entirely by whether recorded
state differs from what's expected, not by diagnostic text. It's a hash
rather than the diff text itself so Terraform's own attribute-diff display
stays terse instead of duplicating potentially large multi-line text that
the `Will redeploy`/`Drift detected` warning already shows more readably.
`ModifyPlan` (in `deployment_resource.go`) additionally previews this for
your *pending config change* (not just externally-caused drift), surfacing
the same information as a warning so you don't have to go decode a hash to
know what's about to happen.

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
go mod tidy   # generates/updates go.sum — required once after cloning
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

- **nomad-pack's exit code isn't fully trusted, and neither is a bare
  "in-place update" count.** In practice, `nomad-pack plan` has been
  observed reporting exit code `0` ("no changes") while its diff shows a
  job's spec genuinely changed. Separately, nomad-pack also reports a
  routine `(N in-place update)` annotation on essentially every job on
  essentially every plan — apparently correlating with its own injected
  metadata (e.g. a timestamp field) churning regardless of whether the
  job's real spec changed — so treating that annotation alone as "this
  job changed" produces constant false positives across every job in a
  pack. Both are the same underlying class of issue as
  [hashicorp/nomad-pack#59](https://github.com/hashicorp/nomad-pack/issues/59)
  (nomad-pack's own reported status disagreeing with its printed diff),
  just surfacing in different ways. The signal actually used here
  (`filterChangedJobBlocks` in `client.go`) is narrower and more
  reliable: whether nomad-pack marked the top-level `Job:` line itself
  with a `+`/`-`/`+/-` prefix — the same convention `nomad job plan` uses
  to mean "something nested under this job actually differs." Only jobs
  marked that way are treated as changed, and only their blocks are shown
  in the `Will redeploy`/`Drift detected` warnings — unmarked jobs'
  routine churn is dropped from the displayed diff entirely rather than
  drowning out the one job that actually matters. If a warning ever looks
  wrong, the fastest way to check is running the exact command from
  `TF_LOG=debug` output by hand and inspecting which `Job:` lines (if any)
  carry a change marker yourself.
- `nomad-pack plan`'s exit codes (`0` = no changes, `1` = changes,
  `255` = error) are documented and used directly for drift detection,
  but "resource no longer exists" is inferred from stderr/stdout text
  matching rather than a dedicated exit code, since nomad-pack doesn't
  expose one. If a nomad-pack version phrases that error differently,
  `Read` will surface it as an error rather than silently removing the
  resource from state — check the error text if this happens.
- `terraform import` requires a composite ID —
  `terraform import nomadpack_deployment.example "<pack>:<registry>:<ref>:<name>"`
  (leave `registry`/`ref` empty between colons if unset), e.g.:
  ```sh
  terraform import nomadpack_deployment.authentik "authentik::v0.3.1:authentik-prod"
  ```
  A bare deployment name isn't enough — nomad-pack's `plan`/`run`/`destroy`
  are keyed by pack, not by deployment name alone, and this provider's
  `Read` needs `pack` already known to query anything. `vars` and
  `var_files` still can't be recovered from the cluster and will show as
  needing to be set on the first `terraform plan` after import — fill
  those in from your pack source to match.
- This was written and reviewed but **not compiled** in the environment
  it was authored in (no Go toolchain / module proxy access there) — that
  also means `go.sum` was never generated and isn't included. Run
  `go mod tidy` once after cloning (this creates/updates `go.sum`), then
  `go build ./...` and `go vet ./...` before relying on it. `ci.yml`
  checks `go.sum` is committed and up to date, then runs build/vet/test
  on every push, so once it's in a repo you'll get a real signal.
