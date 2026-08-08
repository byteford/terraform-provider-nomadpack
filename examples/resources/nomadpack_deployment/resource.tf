resource "nomadpack_deployment" "authentik" {
  pack     = "authentik"      # pack name in the registry, or a local path like "./packs/authentik"
  registry = "homelab"        # omit to use nomad-pack's default registry
  ref      = "v0.3.1"         # pin this; @latest makes applies non-reproducible
  name     = "authentik-prod" # -name flag; defaults to `pack` if omitted

  vars = {
    postgres_host = "10.0.10.5"
    replicas      = "2"
  }

  var_files = [
    "${path.module}/vars/authentik.hcl",
  ]
}

# Pending changes and any drift detected from the deployed jobs show up
# both as a `Will redeploy`/`Drift detected` warning in `terraform plan`
# output (the readable diff text), and as a hash in this attribute (which
# is what actually makes `terraform apply` fix drift, not just report it —
# see the `diff_hash` description for why it's a hash and not the text).
output "authentik_diff_hash" {
  value = nomadpack_deployment.authentik.diff_hash
}
