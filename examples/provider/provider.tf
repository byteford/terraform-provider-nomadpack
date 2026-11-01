terraform {
  required_providers {
    nomadpack = {
      source  = "byteford/nomadpack"
      version = "~> 0.1"
    }
  }
}

provider "nomadpack" {
  # All of these are optional and fall back to the same NOMAD_ADDR,
  # NOMAD_NAMESPACE, NOMAD_TOKEN, etc. environment variables nomad-pack
  # itself reads, so in most setups you don't need to set anything here.
  address    = "https://nomad.example.internal:4646"
  namespace  = "default"
  run_detach = false

  # Recommended: makes relative paths in `pack`/`var_files` resolve
  # against your Terraform root, instead of depending on whatever working
  # directory Terraform happens to launch the provider subprocess with.
  working_directory = path.root

  # If more than one machine (or user) runs Terraform, set this to the same
  # absolute path everywhere. nomad-pack records the path of a registry pack
  # in each job's `pack.path` meta, so without it, applying from a different
  # machine changes the meta and restarts the job. Linux/Unix only.
  cache_dir = "/var/cache/homelab"
}
