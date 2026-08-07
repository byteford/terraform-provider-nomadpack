package provider

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

func TestPackPathPlanModifierSuppressesEquivalentPaths(t *testing.T) {
	tests := []struct {
		name      string
		state     string
		config    string
		wantPlan  string // expected resp.PlanValue after modification
		wantEqual bool   // whether wantPlan should equal the state value (i.e. diff suppressed)
	}{
		{
			name:      "absolute vs relative to same dir",
			state:     "/mnt/e/devcastops/nomad-pack/monitoring",
			config:    "../../../devcastops/nomad-pack/monitoring",
			wantEqual: true,
		},
		{
			name:      "identical values",
			state:     "./packs/monitoring",
			config:    "./packs/monitoring",
			wantEqual: true,
		},
		{
			name:      "genuinely different path",
			state:     "/mnt/e/devcastops/nomad-pack/monitoring",
			config:    "/mnt/e/devcastops/nomad-pack/grafana",
			wantEqual: false,
		},
		{
			name:      "registry name to registry name, unchanged treated normally",
			state:     "authentik",
			config:    "grafana",
			wantEqual: false,
		},
		{
			name:      "switching from a path to a registry name is a real change",
			state:     "/mnt/e/devcastops/nomad-pack/monitoring",
			config:    "monitoring",
			wantEqual: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := planmodifier.StringRequest{
				StateValue:  types.StringValue(tt.state),
				ConfigValue: types.StringValue(tt.config),
			}
			// PlanValue starts as the config value, same as the real
			// framework does before any modifier runs.
			resp := &planmodifier.StringResponse{PlanValue: types.StringValue(tt.config)}

			normalizesEquivalentPaths().PlanModifyString(context.Background(), req, resp)

			gotSuppressed := resp.PlanValue.Equal(req.StateValue)
			if gotSuppressed != tt.wantEqual {
				t.Fatalf(
					"PlanModifyString(state=%q, config=%q): plan value = %q, diff-suppressed = %v, want %v",
					tt.state, tt.config, resp.PlanValue.ValueString(), gotSuppressed, tt.wantEqual,
				)
			}
		})
	}
}
