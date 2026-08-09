package provider

import (
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/diag"
)

func TestAddWarningOnceSuppressesExactRepeat(t *testing.T) {
	r := &DeploymentResource{}
	var diags diag.Diagnostics

	r.addWarningOnce(&diags, "authentik", "Will redeploy", "some diff text")
	r.addWarningOnce(&diags, "authentik", "Will redeploy", "some diff text")

	if len(diags) != 1 {
		t.Fatalf("expected exactly 1 warning after two identical calls, got %d", len(diags))
	}
}

func TestAddWarningOnceAllowsDifferentContentForSameResource(t *testing.T) {
	r := &DeploymentResource{}
	var diags diag.Diagnostics

	r.addWarningOnce(&diags, "authentik", "Will redeploy", "first diff")
	r.addWarningOnce(&diags, "authentik", "Will redeploy", "second diff") // drift changed between calls

	if len(diags) != 2 {
		t.Fatalf("expected 2 warnings for genuinely different content, got %d", len(diags))
	}
}

func TestAddWarningOnceKeepsResourcesIndependent(t *testing.T) {
	r := &DeploymentResource{}
	var diags diag.Diagnostics

	r.addWarningOnce(&diags, "authentik", "Will redeploy", "same text")
	r.addWarningOnce(&diags, "monitoring", "Will redeploy", "same text")

	if len(diags) != 2 {
		t.Fatalf("expected warnings for two different resources to both show, got %d", len(diags))
	}
}
