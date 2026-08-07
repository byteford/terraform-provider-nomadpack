package provider

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
)

// packPathPlanModifier suppresses a plan diff on the `pack` attribute when
// the old and new values are just different spellings of the same
// filesystem path — e.g. an absolute path swapped for a relative one
// pointing at the same directory. Without this, moving a repo checkout or
// running Terraform from a different working directory would show a
// change (and previously, force a destroy/recreate) even though nothing
// about the actual deployed pack changed.
//
// Values that aren't path-like (a bare registry pack name such as
// "authentik") are left to ordinary string comparison, since there's
// nothing to normalize there — a real name change is a real change.
type packPathPlanModifier struct{}

func normalizesEquivalentPaths() planmodifier.String {
	return packPathPlanModifier{}
}

func (m packPathPlanModifier) Description(_ context.Context) string {
	return "Treats old and new values as unchanged if they resolve to the same filesystem path."
}

func (m packPathPlanModifier) MarkdownDescription(ctx context.Context) string {
	return m.Description(ctx)
}

func (m packPathPlanModifier) PlanModifyString(ctx context.Context, req planmodifier.StringRequest, resp *planmodifier.StringResponse) {
	if req.StateValue.IsNull() || req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}

	oldVal := req.StateValue.ValueString()
	newVal := req.ConfigValue.ValueString()

	if oldVal == newVal {
		return // Already no diff; nothing to normalize.
	}
	if !looksLikeFilesystemPath(oldVal) || !looksLikeFilesystemPath(newVal) {
		return // Registry pack name (or one side switched to/from one) — a real change.
	}

	oldAbs, errOld := filepath.Abs(oldVal)
	newAbs, errNew := filepath.Abs(newVal)
	if errOld != nil || errNew != nil {
		return
	}

	if filepath.Clean(oldAbs) == filepath.Clean(newAbs) {
		// Same directory on disk, just written differently. Keep the
		// prior state value so this shows as no change at all, rather
		// than an unnecessary re-run (or, before this fix, a forced
		// replace).
		resp.PlanValue = req.StateValue
	}
}

// looksLikeFilesystemPath uses the same heuristic nomad-pack itself
// effectively relies on: a pack argument containing a path separator is a
// filesystem path under development, not a registry pack name.
func looksLikeFilesystemPath(s string) bool {
	return strings.ContainsAny(s, "/\\")
}
