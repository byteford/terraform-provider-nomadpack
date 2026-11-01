# Adds the registry to nomad-pack's cache on the machine running Terraform
# (`nomad-pack registry add homelab <source> --ref=v1.2.3`).
resource "nomadpack_registry" "homelab" {
  name   = "homelab"
  source = "github.com/byteford/nomad-packs"
  ref    = "v1.2.3" # tag or commit SHA; empty and "latest" are rejected
}

# Reference the registry's attributes from the deployment so Terraform
# creates the registry first, and so the deployment moves to a new ref in
# the same apply as the registry.
resource "nomadpack_deployment" "mosquitto" {
  pack     = "mosquitto"
  registry = nomadpack_registry.homelab.name
  ref      = nomadpack_registry.homelab.ref
}
