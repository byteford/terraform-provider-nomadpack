# Import uses a composite ID: "<pack>:<registry>:<ref>:<name>"
# Leave registry/ref empty between the colons if you don't use them —
# a bare deployment name isn't enough, since nomad-pack's plan/run/destroy
# commands are keyed by pack, not by deployment name alone.
#
# vars and var_files can't be recovered from the cluster and will show as
# needing to be set on the first `terraform plan` after import — fill
# those in from your pack source to match the existing deployment.

terraform import nomadpack_deployment.authentik "authentik::v0.3.1:authentik-prod"
