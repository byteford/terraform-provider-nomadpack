# terraform-provider-nomadpack

A Terraform provider that drives [nomad-pack](https://developer.hashicorp.com/nomad/tools/nomad-pack)
deployments as Terraform resources.

## Design

This provider deliberately does **not** re-implement pack rendering or talk
to the Nomad API directly. Instead it shells out to the `nomad-pack` binary
and drives its own lifecycle commands:

| Terraform op | nomad-pack command                    |
|--------------|----------------------------------------|
| Create       | `nomad-pack run <pack> -name=<name>` (then `stop` too if `stopped=true`) |
| Read         | `nomad-pack plan <pack> -name=<name> -diff` (skipped while `stopped=true`) |
| Update       | `nomad-pack run <pack> -name=<name>`, or `nomad-pack stop <pack> -name=<name>` if `stopped=true` |
| Delete       | `nomad-pack destroy <pack> -name=<name>` |

Setting `stopped = true` stops the deployment's jobs (`nomad-pack stop`,
without `-purge`) without forgetting it: it stays queryable and can be
brought back by setting `stopped = false` again, which re-runs the pack.
Use this instead of removing the resource when you want to pause a pack
temporarily — removing the resource (`Delete`) always calls
`nomad-pack destroy`, which purges the deployment entirely.

nomad-pack's own injected job metadata (`pack.name`, `pack.version`, etc.)
stays the single source of truth for what's deployed on the cluster — this
provider just automates driving the CLI consistently. `diff_hash` holds a
fingerprint of the last `nomad-pack plan` diff, and it's more than
informational: writing a non-empty value there is what causes `terraform
plan` to show `~ update in-place` and `terraform apply` to actually run
`nomad-pack run` to reconcile it — a plan-time warning alone doesn't cause
Terraform to schedule anything, since apply behavior is driven entirely by
whether recorded state differs from what's expected. It's a hash rather
than the diff text itself so Terraform's own attribute-diff display stays
terse instead of duplicating potentially large multi-line text that the
warning already shows more readably.

`diff_hash` is set by both `Read` and `ModifyPlan`, but only `ModifyPlan`
ever emits a warning. Read runs during refresh, using state as it stood
*before* your pending config change — it can't know what you're about to
apply, so it can't reliably say what `terraform apply` will actually do.
Early versions of this provider had Read warn too, which produced
genuinely confusing plans: moving a var from a pack default into an
explicit Terraform override could show a scary `Drift detected` (Read,
checking the old vars) right next to a reassuring `Will NOT redeploy`
(ModifyPlan, checking the new ones) — both individually accurate, jointly
confusing. `ModifyPlan` alone decides what to say now, always via a live
`nomad-pack plan` call against your actual pending config (even when the
target hasn't changed from state — this is deliberately not skipped
anymore, to guarantee there's only ever one check and one message):
`Will redeploy` / `Drift detected` (wording only, same underlying check —
whether the change was yours or purely external) when something on the
cluster will actually change, `Will NOT redeploy` when your config changed
but the render didn't, or nothing at all when there's genuinely nothing to
report.

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

### Registries

`nomadpack_registry` makes sure a pack registry is in nomad-pack's cache on
the machine running Terraform, so a `nomadpack_deployment` with `registry`/`ref`
doesn't need a manual `nomad-pack registry add` first (see
`examples/resources/nomadpack_registry/resource.tf`).

| Terraform op | nomad-pack command |
|---|---|
| Create / Update (`ref` change) | `nomad-pack registry add <name> <source> --ref=<ref>` |
| Read | `nomad-pack registry list` (entry missing => removed from state, so the next apply re-adds it, e.g. after the cache was wiped) |
| Delete | `nomad-pack registry delete <name> --ref=<ref>` (skipped if the entry is already gone) |

- `ref` is required and `""`, `latest` and `@latest` are rejected: without a
  pinned ref `registry add` overwrites nomad-pack's cached `@latest`. Use a
  tag or SHA. Branches can't be told apart from tags, so they aren't rejected,
  but they aren't reproducible.
- `name` is limited to letters, digits, `.`, `_` and `-`, starting with a
  letter or digit: nomad-pack uses it as a directory name in its cache.
- Registry commands run one at a time inside the provider process, because
  nomad-pack stages every `registry add` in one shared temp directory. Two
  Terraform runs (or provider aliases, if Terraform starts a separate
  process for each) against the same cache are not coordinated.
- After a wiped cache, a `nomadpack_deployment` that uses the registry keeps
  its state on refresh (its `nomad-pack plan` fails with "failed to find
  pack", which is treated as a cache miss, not drift) while the registry
  resource is re-added on apply. This applies only to registry packs, not
  local-path packs.
- Deleting a registry that nomad-pack lists but has no packs left for (an
  earlier ref-scoped `registry delete` leaves the entry behind) succeeds.
  Read can't tell such an entry from a complete one: no nomad-pack command
  shows packs per ref.
- The cache lives on the machine running Terraform; nothing is shared with
  other machines or CI runners.
- Changing `ref` adds the new ref and leaves the old one cached, so
  deployments still pinned to it keep working. `name` or `source` changes
  replace the resource.
- Creating a registry that is already in the cache is safe: `registry add`
  with a pinned ref re-clones the source (so it needs network access to the
  git host even when cached), leaves the packs already cached at that ref
  untouched, and succeeds. Terraform then owns the entry, so destroying the
  resource deletes that ref from the cache.
- Read only compares name and ref. If the name and ref were already cached
  from a *different* source, the cached packs are kept (not replaced) while
  `registry list` starts showing the new source, so the mismatch is hidden.
  Remove the entry first with `nomad-pack registry delete <name> --ref=<ref>`.

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

- **Relative paths in `pack`/`var_files` depend on the nomad-pack
  subprocess's working directory, which isn't guaranteed to match the
  directory you run `terraform apply` from.** Terraform launches provider
  plugin binaries as separate subprocesses, and this provider doesn't
  otherwise control what CWD that subprocess starts with — a relative
  path that looks correct can still fail to resolve. Set
  `working_directory = path.root` on the provider block to remove the
  ambiguity entirely (see `examples/provider/provider.tf`); it's applied
  as `exec.Cmd.Dir` for every nomad-pack invocation. This is unrelated to
  the `pack`-vs-CWD distinction below — that one's about nomad-pack's own
  path resolution rules; this one's about which directory nomad-pack even
  starts in.

- **`pack` and `var_files` are always resolved to absolute paths before
  nomad-pack ever sees them** (`resolvePath` in `client.go`), against
  `working_directory` if set, otherwise the provider process's own
  working directory. This isn't just about resolving relative paths
  correctly — nomad-pack appears to track a job's identity partly by the
  literal pack path *string*, so a relative and absolute path pointing
  at the exact same directory can be treated as two different
  deployments. That surfaced concretely as `terraform import` failing
  with `job with id "x" already exists and is part of deployment
  "<the relative path just typed>"` when the original `nomad-pack run`
  had used an absolute path and the import used a differently-spelled
  relative one for the same directory. Always canonicalizing to a single
  absolute form removes the ambiguity — you no longer need to remember
  or match whatever spelling was originally used.

- **`diff_hash`'s planned value must be unknown, not a concrete guess,
  whenever Create/Update will run.** `Create`/`Update` always resolve
  `diff_hash` to `""` on success, regardless of what was previewed.
  `ModifyPlan` originally committed to a concrete pre-apply hash whenever
  something would change, which produced a real `Provider produced
  inconsistent result after apply` error the first time an actual change
  was applied — Terraform requires a Computed attribute's final value to
  exactly match what was planned unless it was explicitly left unknown.
  `ModifyPlan` now sets `diff_hash` unknown whenever Create/Update is
  about to run (`hasChanges || configTargetChanged`), and only commits to
  a concrete value (`""`) in the one case neither will run at all. If
  you're extending this resource with more Computed attributes that
  Create/Update touch, this is the pattern to follow — don't let
  `ModifyPlan` promise a value the actual apply might not deliver.
- **The `Will redeploy`/`Drift detected`/`Will NOT redeploy` warning can
  appear twice during a normal `terraform apply`** — once during plan
  review, once again after that resource's "Modifications complete" line.
  This was investigated in depth and is very likely a Terraform Core
  rendering bug (the same class as
  [hashicorp/terraform#35931](https://github.com/hashicorp/terraform/issues/35931),
  which HashiCorp's own team confirmed and labeled `bug`, but whose fix
  only covers `check` blocks) rather than anything this provider can
  control: the duplicate content is byte-identical between occurrences,
  appears strictly after the resource's apply completes rather than
  before it begins, and persisted even after adding a process-lifetime
  dedup cache — none of which fit "the provider was asked twice." If
  that diagnosis is right, Terraform CLI is re-displaying a diagnostic it
  already has rather than asking the provider again, which means there's
  nothing to intercept on this side. If you want this fixed, filing it
  against `hashicorp/terraform` (referencing #35931 as the related, known
  bug class) is the right venue — the `warnedOnce` field in
  `deployment_resource.go` documents the failed hypothesis and the
  reasoning that ruled it out, in case anyone revisits this later.
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
