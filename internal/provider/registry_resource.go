package provider

import (
	"context"
	"fmt"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/byteford/terraform-provider-nomadpack/internal/nomadpack"
)

var (
	_ resource.Resource              = &RegistryResource{}
	_ resource.ResourceWithConfigure = &RegistryResource{}
)

func NewRegistryResource() resource.Resource {
	return &RegistryResource{}
}

// RegistryResource manages one pinned ref of a pack registry in nomad-pack's
// global cache on the machine running Terraform, so a nomadpack_deployment
// can use `registry`/`ref` without a manual `nomad-pack registry add`.
//
// The cache is outside Terraform's control, so Read checks it against
// `nomad-pack registry list` and drops the resource from state when the
// entry is gone (e.g. the cache was wiped), which makes the next apply
// add it again.
type RegistryResource struct {
	client *nomadpack.Client
}

type registryResourceModel struct {
	ID     types.String `tfsdk:"id"`
	Name   types.String `tfsdk:"name"`
	Source types.String `tfsdk:"source"`
	Ref    types.String `tfsdk:"ref"`
}

func (m *registryResourceModel) toRegistry() nomadpack.Registry {
	return nomadpack.Registry{
		Name:   m.Name.ValueString(),
		Source: m.Source.ValueString(),
		Ref:    m.Ref.ValueString(),
	}
}

func (r *RegistryResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_registry"
}

func (r *RegistryResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Ensures a nomad-pack pack registry is present, at a pinned ref, in nomad-pack's " +
			"cache on the machine running Terraform (`nomad-pack registry add`). Reference its " +
			"`name` and `ref` from a `nomadpack_deployment` so the registry exists before the " +
			"deployment runs.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:    true,
				Description: "Same as `name`. Present for Terraform resource identity conventions.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"name": schema.StringAttribute{
				Required: true,
				Description: "Name the registry is added under; this is the value a " +
					"`nomadpack_deployment` sets as `registry`. Letters, digits, `.`, `_` and `-` only, " +
					"starting with a letter or digit (nomad-pack uses it as a directory name). " +
					"Changing it replaces the resource.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
				Validators: []validator.String{
					stringCheck("must use only letters, digits, '.', '_' and '-', and start with a letter or digit", func(s string) bool {
						return nomadpack.ValidateRegistryName(s) == nil
					}),
				},
			},
			"source": schema.StringAttribute{
				Required: true,
				Description: "Git URL of the registry, as accepted by `nomad-pack registry add` " +
					"(e.g. `github.com/org/nomad-packs`). Changing it replaces the resource.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
				Validators: []validator.String{
					stringCheck("must be a non-empty git URL that does not start with \"-\"", func(s string) bool {
						return strings.TrimSpace(s) != "" && !strings.HasPrefix(s, "-")
					}),
				},
			},
			"ref": schema.StringAttribute{
				Required: true,
				Description: "Git tag or commit SHA to add (`--ref`). Required, and must not be empty or " +
					"`latest`/`@latest`: without a pinned ref nomad-pack overwrites its cached `@latest`. " +
					"Use a tag or SHA, not a branch, so applies are reproducible. Changing it adds the new " +
					"ref in place and leaves the old ref cached, so deployments still pinned to it keep working.",
				Validators: []validator.String{
					stringCheck("must be a tag or commit SHA, not empty or \"latest\"", func(s string) bool {
						return nomadpack.ValidateRegistryRef(s) == nil
					}),
				},
			},
		},
	}
}

func (r *RegistryResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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
}

// add is shared by Create and Update: `registry add` with an explicit ref
// is idempotent, so "ensure present at this ref" is the same call in both.
func (r *RegistryResource) add(ctx context.Context, plan *registryResourceModel, diags *diag.Diagnostics) bool {
	if _, err := r.client.RegistryAdd(ctx, plan.toRegistry()); err != nil {
		addCLIError(diags, "Unable to add nomad-pack registry", err)
		return false
	}
	plan.ID = plan.Name
	return true
}

func (r *RegistryResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan registryResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if !r.add(ctx, &plan, &resp.Diagnostics) {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *RegistryResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state registryResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	exists, err := r.client.RegistryExists(ctx, state.Name.ValueString(), state.Ref.ValueString())
	if err != nil {
		addCLIError(&resp.Diagnostics, "Unable to list nomad-pack registries", err)
		return
	}
	if !exists {
		// Gone from nomad-pack's cache: forget it so the next apply re-adds it.
		resp.State.RemoveResource(ctx)
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *RegistryResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan registryResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// Only `ref` can change in place (name and source force replacement).
	// The previous ref is left in the cache on purpose: removing it here
	// could break deployments that are still pinned to it until they are
	// updated later in the same apply.
	if !r.add(ctx, &plan, &resp.Diagnostics) {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *RegistryResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state registryResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if _, err := r.client.RegistryDelete(ctx, state.Name.ValueString(), state.Ref.ValueString()); err != nil {
		addCLIError(&resp.Diagnostics, "Unable to delete nomad-pack registry", err)
	}
}

// stringCheck is a minimal string validator: it reports an attribute error
// "<attr> <what>" when ok returns false for a known, non-null value. It
// avoids pulling in terraform-plugin-framework-validators for three checks.
type stringCheckValidator struct {
	what string
	ok   func(string) bool
}

func stringCheck(what string, ok func(string) bool) validator.String {
	return stringCheckValidator{what: what, ok: ok}
}

func (v stringCheckValidator) Description(context.Context) string { return v.what }

func (v stringCheckValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (v stringCheckValidator) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	if !v.ok(req.ConfigValue.ValueString()) {
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid value", fmt.Sprintf("%s %s.", req.Path, v.what))
	}
}
