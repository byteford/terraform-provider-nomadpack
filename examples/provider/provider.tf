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
}
