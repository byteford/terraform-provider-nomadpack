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

# Pending changes and any drift detected from the deployed jobs show up as
# a warning in `terraform plan` output rather than as an attribute here —
# there's no `diff` output to reference.
