package provider

import (
	"testing"

	"github.com/byteford/terraform-provider-nomadpack/internal/nomadpack"
)

func TestPackRefsEqual(t *testing.T) {
	base := nomadpack.PackRef{Pack: "authentik", Registry: "homelab", Ref: "v0.3.1"}

	tests := []struct {
		name string
		b    nomadpack.PackRef
		want bool
	}{
		{"identical", base, true},
		{"different pack", nomadpack.PackRef{Pack: "grafana", Registry: "homelab", Ref: "v0.3.1"}, false},
		{"different registry", nomadpack.PackRef{Pack: "authentik", Registry: "other", Ref: "v0.3.1"}, false},
		{"different ref", nomadpack.PackRef{Pack: "authentik", Registry: "homelab", Ref: "v0.4.0"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := packRefsEqual(base, tt.b); got != tt.want {
				t.Fatalf("packRefsEqual() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDeploymentsEqual(t *testing.T) {
	base := nomadpack.Deployment{
		Name:     "authentik-prod",
		VarFiles: []string{"vars/a.hcl", "vars/b.hcl"},
		Vars:     map[string]string{"replicas": "2", "host": "10.0.10.5"},
	}

	same := nomadpack.Deployment{
		Name:     "authentik-prod",
		VarFiles: []string{"vars/a.hcl", "vars/b.hcl"},
		Vars:     map[string]string{"host": "10.0.10.5", "replicas": "2"}, // different insertion order
	}
	if !deploymentsEqual(base, same) {
		t.Fatal("expected deployments with identical content (different map order) to be equal")
	}

	tests := []struct {
		name string
		b    nomadpack.Deployment
	}{
		{"different name", nomadpack.Deployment{Name: "other", VarFiles: base.VarFiles, Vars: base.Vars}},
		{"different var value", nomadpack.Deployment{Name: base.Name, VarFiles: base.VarFiles, Vars: map[string]string{"replicas": "3", "host": "10.0.10.5"}}},
		{"missing var", nomadpack.Deployment{Name: base.Name, VarFiles: base.VarFiles, Vars: map[string]string{"host": "10.0.10.5"}}},
		{"extra var", nomadpack.Deployment{Name: base.Name, VarFiles: base.VarFiles, Vars: map[string]string{"replicas": "2", "host": "10.0.10.5", "extra": "x"}}},
		{"different var_files order", nomadpack.Deployment{Name: base.Name, VarFiles: []string{"vars/b.hcl", "vars/a.hcl"}, Vars: base.Vars}},
		{"fewer var_files", nomadpack.Deployment{Name: base.Name, VarFiles: []string{"vars/a.hcl"}, Vars: base.Vars}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if deploymentsEqual(base, tt.b) {
				t.Fatalf("expected deploymentsEqual to report a difference for %q", tt.name)
			}
		})
	}
}
