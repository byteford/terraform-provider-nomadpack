package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"

	"github.com/byteford/terraform-provider-nomadpack/internal/nomadpack"
)

// diffHash fingerprints a nomad-pack diff for storage in state. Only the
// fact that it *changed* matters for triggering Terraform's plan/apply —
// the actual human-readable text is surfaced separately via a warning
// diagnostic, not through this value, so there's no reason to duplicate
// potentially large multi-line diff text into the state file and into
// Terraform's own native attribute-diff display. Empty input maps to an
// empty hash (rather than the hash of an empty string) so "in sync" reads
// as a plain empty value, not a mysterious constant hash. 16 hex chars
// (64 bits) is overkill for accidental-collision odds at this scale —
// this only needs to detect "something changed," not resist attack.
func diffHash(diff string) string {
	if diff == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(diff))
	return hex.EncodeToString(sum[:])[:16]
}

// addCLIError renders a nomad-pack failure as a single, non-duplicated
// diagnostic. For a *nomadpack.CLIError it shows the one most relevant
// captured stream (stderr if nomad-pack wrote one, else stdout) exactly
// once; for anything else (e.g. the binary wasn't found at all) it falls
// back to the plain error text.
func addCLIError(diags *diag.Diagnostics, summary string, err error) {
	var cliErr *nomadpack.CLIError
	if errors.As(err, &cliErr) {
		diags.AddError(
			fmt.Sprintf("%s (nomad-pack %s exited %d)", summary, cliErr.Op, cliErr.ExitCode),
			cliErr.Output(),
		)
		return
	}
	diags.AddError(summary, err.Error())
}

var (
	_ resource.Resource                = &DeploymentResource{}
	_ resource.ResourceWithConfigure   = &DeploymentResource{}
	_ resource.ResourceWithImportState = &DeploymentResource{}
	_ resource.ResourceWithModifyPlan  = &DeploymentResource{}
)

func NewDeploymentResource() resource.Resource {
	return &DeploymentResource{}
}

// DeploymentResource represents one named `nomad-pack run` deployment,
// which may contain any number of jobs as defined by the pack itself.
// nomad-pack's own injected job metadata remains the source of truth on
// the cluster; this resource just drives its CLI lifecycle and keeps a
// human-readable diff in state for visibility.
type DeploymentResource struct {
	client    *nomadpack.Client
	runDetach bool

	// warnedOnce suppresses showing the exact same ModifyPlan warning
	// twice within a single provider process lifetime. Terraform Core
	// internally calls PlanResourceChange (which runs ModifyPlan) twice
	// for a bare `terraform apply` — once for the plan you review and
	// approve, once more immediately before actually executing it, as an
	// integrity check — and shows every diagnostic from both calls. This
	// is a confirmed Terraform Core bug pattern (see
	// https://github.com/hashicorp/terraform/issues/35931), but the fix
	// that landed only covers `check` blocks, not general resource
	// diagnostics like these, so there's nothing to rely on upstream yet.
	//
	// A single `terraform apply` invocation keeps the same provider
	// process running for its whole duration, so an in-memory,
	// process-lifetime cache is enough to recognize "I already showed
	// this exact warning for this exact resource in this run" and skip
	// the repeat — without losing the accuracy of the underlying live
	// check, which is the whole point of ModifyPlan existing.
	//
	// Deliberately without a TTL or eviction: for normal local CLI use,
	// each `terraform` invocation spawns a fresh provider subprocess, so
	// this resets naturally between commands. It would NOT reset within
	// a single long-lived provider process (e.g. some remote-execution
	// setups that reuse a provider server across multiple applies), where
	// this could suppress a warning that's genuinely new in a later,
	// separate apply. Worth knowing if you ever move off local CLI use.
	warnedOnce sync.Map // key: string -> struct{}
}

// addWarningOnce emits a warning diagnostic unless the exact same
// (resourceKey, summary, detail) combination was already shown earlier in
// this provider process — see the warnedOnce field doc for why that's
// sufficient to fix Terraform's plan/apply double-invocation without
// risking silently dropping a warning that's actually different.
func (r *DeploymentResource) addWarningOnce(diags *diag.Diagnostics, resourceKey, summary, detail string) {
	key := resourceKey + "\x00" + summary + "\x00" + detail
	if _, alreadyShown := r.warnedOnce.LoadOrStore(key, struct{}{}); alreadyShown {
		return
	}
	diags.AddWarning(summary, detail)
}

type deploymentResourceModel struct {
	ID       types.String `tfsdk:"id"`
	Pack     types.String `tfsdk:"pack"`
	Registry types.String `tfsdk:"registry"`
	Ref      types.String `tfsdk:"ref"`
	Name     types.String `tfsdk:"name"`
	Vars     types.Map    `tfsdk:"vars"`
	VarFiles types.List   `tfsdk:"var_files"`
	Detach   types.Bool   `tfsdk:"detach"`
	DiffHash types.String `tfsdk:"diff_hash"`
}

func (r *DeploymentResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_deployment"
}

func (r *DeploymentResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Manages a single nomad-pack deployment (one `run`/`plan`/`destroy` lifecycle), " +
			"which may render and manage multiple Nomad jobs as defined by the pack.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:    true,
				Description: "Same as `name`. Present for Terraform resource identity conventions.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"pack": schema.StringAttribute{
				Required: true,
				Description: "Pack name (as known to the registry) or a filesystem path to a pack " +
					"under development. Changing this re-runs `nomad-pack run` under the existing " +
					"deployment name rather than destroying and recreating — nomad-pack itself " +
					"handles swapping what a named deployment points to. Equivalent path spellings " +
					"(absolute vs relative to the same directory) aren't treated as a change at all.",
				PlanModifiers: []planmodifier.String{
					normalizesEquivalentPaths(),
				},
			},
			"registry": schema.StringAttribute{
				Optional:    true,
				Description: "Registry name containing the pack. Omit to use nomad-pack's default registry.",
			},
			"ref": schema.StringAttribute{
				Optional: true,
				Description: "Git ref of the pack to deploy (tag, SHA, or `pack-name/vX.Y.Z`). " +
					"Defaults to nomad-pack's own @latest resolution if unset. Pin this in " +
					"production so `terraform apply` is reproducible instead of tracking a moving ref.",
			},
			"name": schema.StringAttribute{
				Optional:    true,
				Computed:    true,
				Description: "Unique deployment name passed to nomad-pack's -name flag. Defaults to the pack name if unset.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"vars": schema.MapAttribute{
				Optional:    true,
				ElementType: types.StringType,
				Description: "Variable overrides passed to nomad-pack as repeated -var=key=value flags.",
			},
			"var_files": schema.ListAttribute{
				Optional:    true,
				ElementType: types.StringType,
				Description: "Paths to HCL variable override files, passed as repeated -var-file flags.",
			},
			"detach": schema.BoolAttribute{
				Optional: true,
				Description: "Override the provider's run_detach setting for this deployment. " +
					"When true, Terraform does not wait for the deployment/evaluation to finish.",
			},
			"diff_hash": schema.StringAttribute{
				Computed: true,
				Description: "Fingerprint of the filtered `nomad-pack plan` diff (jobs nomad-pack " +
					"marked as actually changed) as of the last refresh. Empty when in sync. This " +
					"holds a hash rather than the diff text itself — the readable diff is surfaced " +
					"via a `Will redeploy`/`Drift detected` warning instead, so this stays terse in " +
					"`terraform plan`'s own attribute-level diff display rather than duplicating " +
					"potentially large multi-line text there. It's deliberately NOT suppressed with " +
					"UseStateForUnknown: its whole purpose is to change when drift is detected (e.g. " +
					"someone stops a job manually outside Terraform), which is what makes `terraform " +
					"plan` show `~ update in-place` and `terraform apply` actually call `nomad-pack " +
					"run` to reconcile it — a plan-time warning alone doesn't cause anything to " +
					"happen on `apply`; this attribute is what does.",
			},
		},
	}
}

func (r *DeploymentResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	data, ok := req.ProviderData.(*ProviderData)
	if !ok {
		resp.Diagnostics.AddError(
			"Unexpected Resource Configure Type",
			fmt.Sprintf("Expected *provider.ProviderData, got: %T.", req.ProviderData),
		)
		return
	}
	r.client = data.Client
	r.runDetach = data.RunDetach
}

// toPackRef / toDeployment convert the Terraform model into the plain Go
// types the nomadpack.Client understands.
func (m *deploymentResourceModel) toPackRef() nomadpack.PackRef {
	return nomadpack.PackRef{
		Pack:     m.Pack.ValueString(),
		Registry: m.Registry.ValueString(),
		Ref:      m.Ref.ValueString(),
	}
}

func (m *deploymentResourceModel) toDeployment(ctx context.Context) (nomadpack.Deployment, error) {
	d := nomadpack.Deployment{
		Name: m.Name.ValueString(),
	}
	if d.Name == "" {
		d.Name = m.Pack.ValueString()
	}

	if !m.Vars.IsNull() && !m.Vars.IsUnknown() {
		vars := make(map[string]string, len(m.Vars.Elements()))
		if diags := m.Vars.ElementsAs(ctx, &vars, false); diags.HasError() {
			return d, fmt.Errorf("invalid vars: %v", diags)
		}
		d.Vars = vars
	}

	if !m.VarFiles.IsNull() && !m.VarFiles.IsUnknown() {
		var files []string
		if diags := m.VarFiles.ElementsAs(ctx, &files, false); diags.HasError() {
			return d, fmt.Errorf("invalid var_files: %v", diags)
		}
		d.VarFiles = files
	}

	return d, nil
}

func (r *DeploymentResource) detachFor(m deploymentResourceModel) bool {
	if !m.Detach.IsNull() {
		return m.Detach.ValueBool()
	}
	return r.runDetach
}

// ModifyPlan previews the effect of the pending config against the live
// cluster, using `nomad-pack plan` targeted at the *new* pack/vars rather
// than what's currently deployed. It is the SOLE source of user-facing
// warnings for this resource — Read (see its own doc comment) silently
// updates `diff_hash` but never warns, specifically to avoid two
// independent checks (Read's old-state comparison vs. this one's new-config
// comparison) ever disagreeing and showing two conflicting messages in the
// same plan, which is exactly what happened when moving a var from a
// pack default into an explicit override: Read's old-state check found
// "drift" that a fresh check against the actual pending config showed was
// a no-op. Only ModifyPlan has visibility into both prior state AND the
// pending config, so only it can give the answer that actually matters:
// "what will `terraform apply` really do."
//
// This always makes a live `nomad-pack plan` call — even when the target
// is identical to prior state — rather than skipping it when nothing
// target-relevant changed. That's a deliberate trade of a bit of extra
// nomad-pack invocation cost (one more call per plan, negligible at
// homelab scale) for a hard guarantee against the double-warning problem:
// with a single check driving a single decision, there's no way to end up
// with two disagreeing messages.
//
// Exactly one of three things happens:
//   - Something will actually change on the cluster: "Will redeploy" (if
//     your config also changed) or "Drift detected" (if your config
//     didn't — the change is purely external) — wording differs only to
//     say clearly whether this is your doing or not; the underlying check
//     and the `diff_hash` mechanics are identical either way.
//   - Your config changed but nothing on the cluster will: "Will NOT
//     redeploy", so you're not left guessing whether a var actually did
//     anything.
//   - Nothing changed and nothing needs reconciling: silence. No warning
//     box on a plan where there's genuinely nothing to say.
//
// It also sets the planned `diff_hash` attribute — but deliberately to
// UNKNOWN, not a concrete guessed value, whenever Create/Update is going
// to run. Create/Update always resolve diff_hash to "" on success (see
// their own comments) regardless of what was previewed here; committing
// to a concrete pre-apply value that Update might not actually match is
// exactly what caused a real "Provider produced inconsistent result after
// apply" error in practice — Terraform requires a Computed attribute's
// final applied value to exactly equal what was planned, unless it was
// explicitly left unknown. Marking it unknown here is what gives
// Create/Update the freedom to report the real post-apply value without
// violating that contract. The one case that sets a concrete value is
// when nothing is going to run at all (silent, no-op plan) — there's
// nothing Create/Update will contradict, since neither gets called.
func (r *DeploymentResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() {
		return // Destroy: nothing planned to preview against.
	}

	var plan deploymentResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if plan.Pack.IsUnknown() {
		return // pack depends on another not-yet-known value; nothing to preview yet.
	}

	dep, err := plan.toDeployment(ctx)
	if err != nil {
		return // Surfaced properly by Create/Update — avoid double-reporting here.
	}
	packRef := plan.toPackRef()

	// Used to choose wording ("Will redeploy" vs "Drift detected") and to
	// decide whether Create/Update will run at all even if the live query
	// below fails — NOT to decide whether to query; the query always
	// runs regardless.
	var priorDiffHash types.String
	haveState := false
	configTargetChanged := true // assume changed (Create, or state unreadable) unless proven otherwise below
	if !req.State.Raw.IsNull() {
		var state deploymentResourceModel
		if diags := req.State.Get(ctx, &state); !diags.HasError() {
			if priorDep, err := state.toDeployment(ctx); err == nil {
				haveState = true
				priorDiffHash = state.DiffHash
				configTargetChanged = !(packRefsEqual(state.toPackRef(), packRef) && deploymentsEqual(priorDep, dep))
			}
		}
	}

	hasChanges, diff, _, err := r.client.Plan(ctx, packRef, dep)
	if err != nil {
		// Best-effort preview only. Don't fail the whole plan over this —
		// e.g. a briefly unreachable Nomad cluster shouldn't block
		// `terraform plan`. Create/Update will surface a real error at
		// apply time if the problem persists.
		r.addWarningOnce(&resp.Diagnostics, packRef.Pack,
			"Couldn't preview nomad-pack diff",
			"nomad-pack plan failed while previewing this change; no preview is available "+
				"for this plan, but apply will still surface a real error if the problem persists. "+
				err.Error(),
		)
		if configTargetChanged {
			// Update will run regardless (forced by some other attribute
			// difference like vars/pack), so avoid guessing a concrete
			// diff_hash it might not match — same reasoning as below.
			resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("diff_hash"), types.StringUnknown())...)
		} else if haveState {
			// Nothing else is forcing an update; safest is to leave
			// diff_hash exactly as Read last established it, so this
			// preview failure alone doesn't spuriously trigger a
			// reconcile Update never actually needed to run.
			resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("diff_hash"), priorDiffHash)...)
		}
		return
	}

	switch {
	case hasChanges && configTargetChanged:
		r.addWarningOnce(&resp.Diagnostics, packRef.Pack,
			fmt.Sprintf("Will redeploy: %q", packRef.Pack),
			diff,
		)
	case hasChanges && !configTargetChanged:
		// Nothing in config changed, so this is purely external —
		// someone (or something) modified the deployed jobs outside
		// Terraform. Same underlying check as above, different wording
		// so it's clear this isn't a result of your own config change.
		r.addWarningOnce(&resp.Diagnostics, packRef.Pack,
			fmt.Sprintf("Drift detected for %q", packRef.Pack),
			diff,
		)
	case !hasChanges && configTargetChanged:
		r.addWarningOnce(&resp.Diagnostics, packRef.Pack,
			fmt.Sprintf("Will NOT redeploy: %q", packRef.Pack),
			"This change doesn't alter what nomad-pack renders for any job in this pack — "+
				"nomad-pack run will still execute (refreshing its own tracking metadata) but "+
				"nothing on the cluster will actually change. If a var was expected to affect "+
				"the job, check that the pack's templates actually reference it.",
		)
	default:
		// !hasChanges && !configTargetChanged: nothing changed, nothing
		// drifted. Deliberately silent — a plan with genuinely nothing
		// to report about this resource shouldn't show a warning box.
	}

	if hasChanges || configTargetChanged {
		// Create/Update is about to run (see the function doc comment
		// for why this must be unknown, not a concrete guess).
		resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("diff_hash"), types.StringUnknown())...)
	} else {
		resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("diff_hash"), types.StringValue(""))...)
	}
}

// packRefsEqual and deploymentsEqual back ModifyPlan's wording decision
// ("Will redeploy" vs "Drift detected") — they no longer decide whether to
// query nomad-pack, only whether the target changed from state.
func packRefsEqual(a, b nomadpack.PackRef) bool {
	return a.Pack == b.Pack && a.Registry == b.Registry && a.Ref == b.Ref
}

func deploymentsEqual(a, b nomadpack.Deployment) bool {
	if a.Name != b.Name {
		return false
	}
	if len(a.VarFiles) != len(b.VarFiles) {
		return false
	}
	for i := range a.VarFiles {
		if a.VarFiles[i] != b.VarFiles[i] {
			return false
		}
	}
	if len(a.Vars) != len(b.Vars) {
		return false
	}
	for k, v := range a.Vars {
		if bv, ok := b.Vars[k]; !ok || bv != v {
			return false
		}
	}
	return true
}

func (r *DeploymentResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan deploymentResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	dep, err := plan.toDeployment(ctx)
	if err != nil {
		resp.Diagnostics.AddError("Invalid configuration", err.Error())
		return
	}
	if dep.Name != "" {
		plan.Name = types.StringValue(dep.Name)
	}

	tflog.Debug(ctx, "running nomad-pack run", map[string]interface{}{
		"pack": plan.Pack.ValueString(),
		"name": dep.Name,
	})

	res, err := r.client.Run(ctx, plan.toPackRef(), dep, r.detachFor(plan))
	if err != nil {
		addCLIError(&resp.Diagnostics, "nomad-pack run failed", err)
		return
	}
	tflog.Debug(ctx, "nomad-pack run succeeded", map[string]interface{}{"output": res.Stdout})

	plan.ID = types.StringValue(dep.Name)
	plan.DiffHash = types.StringValue("")

	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Read compares the deployed jobs against what the pack currently renders
// for this resource's recorded config, and writes the result into
// `diff_hash` — not just for display, but because that's what makes
// externally-caused drift (e.g. someone stopping a job by hand) actually
// get reconciled on the next `terraform apply`. A plan-time warning alone
// doesn't cause Terraform to schedule anything; a changed attribute value
// between the last committed state and this refresh does.
//
// Deliberately silent (no warning diagnostic here): this check uses OLD
// state, which can't know about a pending config change you haven't
// applied yet. ModifyPlan runs afterward with visibility into both old
// state AND the new config, and is the sole place that decides what to
// tell you — see its doc comment for why having two independent checks
// warn separately caused confusing, sometimes-contradictory messages.
func (r *DeploymentResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state deploymentResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	dep, err := state.toDeployment(ctx)
	if err != nil {
		resp.Diagnostics.AddError("Invalid state", err.Error())
		return
	}

	hasChanges, diff, _, err := r.client.Plan(ctx, state.toPackRef(), dep)
	if err != nil {
		// nomad-pack doesn't document a distinct "no such deployment"
		// exit code (see the 255 catch-all in the plan command docs),
		// so this is a best-effort match on typical CLI wording. If it
		// doesn't match your nomad-pack version's phrasing, Read will
		// surface the plan error instead of removing the resource,
		// which is the safer failure mode.
		var cliErr *nomadpack.CLIError
		if errors.As(err, &cliErr) {
			combined := strings.ToLower(cliErr.Output())
			if strings.Contains(combined, "no jobs found") ||
				strings.Contains(combined, "not found") ||
				strings.Contains(combined, "does not exist") {
				resp.State.RemoveResource(ctx)
				return
			}
		}
		addCLIError(&resp.Diagnostics, "nomad-pack plan failed", err)
		return
	}

	if hasChanges {
		state.DiffHash = types.StringValue(diffHash(diff))
	} else {
		state.DiffHash = types.StringValue("")
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *DeploymentResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan deploymentResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	dep, err := plan.toDeployment(ctx)
	if err != nil {
		resp.Diagnostics.AddError("Invalid configuration", err.Error())
		return
	}

	res, err := r.client.Run(ctx, plan.toPackRef(), dep, r.detachFor(plan))
	if err != nil {
		addCLIError(&resp.Diagnostics, "nomad-pack run failed", err)
		return
	}
	tflog.Debug(ctx, "nomad-pack run succeeded", map[string]interface{}{"output": res.Stdout})

	plan.ID = types.StringValue(dep.Name)
	plan.DiffHash = types.StringValue("")

	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *DeploymentResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state deploymentResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	dep, err := state.toDeployment(ctx)
	if err != nil {
		resp.Diagnostics.AddError("Invalid state", err.Error())
		return
	}

	res, err := r.client.Destroy(ctx, state.toPackRef(), dep, r.detachFor(state))
	if err != nil {
		addCLIError(&resp.Diagnostics, "nomad-pack destroy failed", err)
		return
	}
	tflog.Debug(ctx, "nomad-pack destroy succeeded", map[string]interface{}{"output": res.Stdout})
}

// ImportState expects an import ID of the form "<pack>:<registry>:<ref>:<name>"
// (leave registry/ref empty between colons if unset), e.g.
// "authentik::v0.3.1:authentik-prod". A bare deployment name isn't enough:
// nomad-pack's plan/run/destroy commands are keyed by pack, not by
// deployment name alone, so Read (which this provider calls automatically
// right after import to fill in the rest of state) needs pack to already
// be known before it can query anything.
func (r *DeploymentResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	parts := strings.Split(req.ID, ":")
	if len(parts) != 4 {
		resp.Diagnostics.AddError(
			"Unexpected Import Identifier",
			`Expected an import ID of the form "<pack>:<registry>:<ref>:<name>" `+
				`(leave registry/ref empty between the colons if unset), e.g. `+
				`"authentik::v0.3.1:authentik-prod". Got: "`+req.ID+`".`,
		)
		return
	}

	pack, registry, ref, name := parts[0], parts[1], parts[2], parts[3]
	if pack == "" || name == "" {
		resp.Diagnostics.AddError(
			"Unexpected Import Identifier",
			"Both <pack> and <name> are required (registry and ref may be left empty).",
		)
		return
	}

	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), name)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("name"), name)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("pack"), pack)...)
	if registry != "" {
		resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("registry"), registry)...)
	}
	if ref != "" {
		resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("ref"), ref)...)
	}
}
