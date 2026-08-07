package provider

import (
	"context"
	"errors"
	"fmt"
	"strings"

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
	Diff     types.String `tfsdk:"diff"`
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
				Required:    true,
				Description: "Pack name (as known to the registry) or a filesystem path to a pack under development.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"registry": schema.StringAttribute{
				Optional:    true,
				Description: "Registry name containing the pack. Omit to use nomad-pack's default registry.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
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
			"diff": schema.StringAttribute{
				Computed: true,
				Description: "Output of `nomad-pack plan -diff` as of the last Read. Empty when the " +
					"deployed jobs match the pack as currently rendered. A non-empty value here " +
					"after `terraform refresh` means something changed outside Terraform — most " +
					"often a manual edit in the Nomad UI — and needs reconciling.",
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
	plan.Diff = types.StringValue("")

	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

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
		state.Diff = types.StringValue(diff)
	} else {
		state.Diff = types.StringValue("")
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
	plan.Diff = types.StringValue("")

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
