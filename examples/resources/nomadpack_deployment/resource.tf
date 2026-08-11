resource "nomadpack_deployment" "authentik" {
  pack     = "authentik"      # pack name in the registry, or a local path like "./packs/authentik"
  registry = "homelab"        # omit to use nomad-pack's default registry
  ref      = "v0.3.1"         # pin this; @latest makes applies non-reproducible
  name     = "authentik-prod" # -name flag; defaults to `pack` if omitted

  vars = {
    postgres_host = "10.0.10.5"
    replicas      = "2"
  }

  # nomad-pack resolves var_files relative to the current working
  # directory (wherever terraform is run from), never relative to `pack`.
  # path.module is correct here because this file itself lives in the
  # Terraform module — it is NOT relative to the pack's own directory.
  var_files = [
    "${path.module}/vars/authentik.hcl",
  ]
}

# For a local-path pack (as opposed to a registry pack like above), if
# your var file lives inside the pack's own directory, build the path
# from the same local rather than writing it as if relative to the pack —
# nomad-pack won't resolve it that way.
locals {
  postgres_pack_path = "/mnt/e/devcastops/nomad-pack/postgres"
}

resource "nomadpack_deployment" "postgres" {
  pack      = local.postgres_pack_path
  var_files = ["${local.postgres_pack_path}/var_files/postgres.nomadpack.hcl"]
}

# Pending changes and any drift detected from the deployed jobs show up
# both as a `Will redeploy`/`Drift detected` warning in `terraform plan`
# output (the readable diff text), and as a hash in this attribute (which
# is what actually makes `terraform apply` fix drift, not just report it —
# see the `diff_hash` description for why it's a hash and not the text).
output "authentik_diff_hash" {
  value = nomadpack_deployment.authentik.diff_hash
}
