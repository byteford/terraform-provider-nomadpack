package provider

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/byteford/terraform-provider-nomadpack/internal/nomadpack"
)

// registryFixture is a RegistryResource wired to a fake nomad-pack that
// records its arguments and answers `registry list` with listOutput.
type registryFixture struct {
	r      *RegistryResource
	schema resource.SchemaResponse
	argLog string
}

func newRegistryFixture(t *testing.T, listOutput string, failAdd bool) *registryFixture {
	t.Helper()

	dir := t.TempDir()
	argLog := filepath.Join(dir, "args.log")
	bin := filepath.Join(dir, "nomad-pack")
	script := "#!/bin/sh\necho \"$*\" >> '" + argLog + "'\n" +
		"if [ \"$1 $2\" = \"registry list\" ]; then\ncat <<'EOF'\n" + listOutput + "\nEOF\nfi\n"
	if failAdd {
		script += "if [ \"$1 $2\" = \"registry add\" ]; then echo 'clone failed' >&2; exit 1; fi\n"
	}
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	f := &registryFixture{
		r:      &RegistryResource{client: &nomadpack.Client{BinaryPath: bin}},
		argLog: argLog,
	}
	f.r.Schema(context.Background(), resource.SchemaRequest{}, &f.schema)
	return f
}

func (f *registryFixture) calls(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(f.argLog)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func (f *registryFixture) state(t *testing.T, m *registryResourceModel) tfsdk.State {
	t.Helper()
	s := tfsdk.State{Schema: f.schema.Schema}
	if diags := s.Set(context.Background(), m); diags.HasError() {
		t.Fatalf("building state: %v", diags)
	}
	return s
}

func (f *registryFixture) plan(t *testing.T, m *registryResourceModel) tfsdk.Plan {
	t.Helper()
	return tfsdk.Plan{Schema: f.schema.Schema, Raw: f.state(t, m).Raw}
}

func homelabModel(ref string) *registryResourceModel {
	return &registryResourceModel{
		ID:     types.StringUnknown(),
		Name:   types.StringValue("homelab"),
		Source: types.StringValue("github.com/byteford/nomad-packs"),
		Ref:    types.StringValue(ref),
	}
}

const registryListHomelabV1 = `REGISTRY NAME  REF     LOCAL_REF  REGISTRY_URL
homelab        v1.2.3  9f8e7d6c   github.com/byteford/nomad-packs`

func TestRegistrySchemaIsValid(t *testing.T) {
	f := newRegistryFixture(t, "", false)
	if diags := f.schema.Schema.ValidateImplementation(context.Background()); diags.HasError() {
		t.Fatalf("invalid schema: %v", diags)
	}
}

func TestRegistryResourceIsRegistered(t *testing.T) {
	p := &NomadPackProvider{}
	for _, newRes := range p.Resources(context.Background()) {
		var resp resource.MetadataResponse
		newRes().Metadata(context.Background(), resource.MetadataRequest{ProviderTypeName: "nomadpack"}, &resp)
		if resp.TypeName == "nomadpack_registry" {
			return
		}
	}
	t.Fatal("nomadpack_registry is not registered on the provider")
}

func TestRegistryCreateRunsAddWithPinnedRef(t *testing.T) {
	f := newRegistryFixture(t, "", false)

	req := resource.CreateRequest{Plan: f.plan(t, homelabModel("v1.2.3"))}
	resp := &resource.CreateResponse{State: tfsdk.State{Schema: f.schema.Schema}}
	f.r.Create(context.Background(), req, resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("unexpected diagnostics: %v", resp.Diagnostics)
	}
	want := []string{"registry add homelab github.com/byteford/nomad-packs --ref=v1.2.3"}
	assertCalls(t, f.calls(t), want)

	var got registryResourceModel
	resp.State.Get(context.Background(), &got)
	if got.ID.ValueString() != "homelab" {
		t.Fatalf("id = %q, want homelab", got.ID.ValueString())
	}
}

func TestRegistryCreateFailureLeavesNoState(t *testing.T) {
	f := newRegistryFixture(t, "", true)

	req := resource.CreateRequest{Plan: f.plan(t, homelabModel("v1.2.3"))}
	resp := &resource.CreateResponse{State: tfsdk.State{Schema: f.schema.Schema}}
	f.r.Create(context.Background(), req, resp)

	if !resp.Diagnostics.HasError() {
		t.Fatal("expected an error diagnostic")
	}
	if !strings.Contains(resp.Diagnostics.Errors()[0].Detail(), "clone failed") {
		t.Fatalf("error should carry nomad-pack output, got %q", resp.Diagnostics.Errors()[0].Detail())
	}
	if !resp.State.Raw.IsNull() {
		t.Fatal("state must stay empty when add fails")
	}
}

func TestRegistryReadKeepsPresentRegistry(t *testing.T) {
	f := newRegistryFixture(t, registryListHomelabV1, false)

	m := homelabModel("v1.2.3")
	m.ID = types.StringValue("homelab")
	st := f.state(t, m)
	resp := &resource.ReadResponse{State: st}
	f.r.Read(context.Background(), resource.ReadRequest{State: st}, resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("unexpected diagnostics: %v", resp.Diagnostics)
	}
	if resp.State.Raw.IsNull() {
		t.Fatal("registry present in `registry list` must stay in state")
	}
}

func TestRegistryReadRemovesFromStateWhenMissing(t *testing.T) {
	// Wiped nomad-pack cache: list no longer shows the registry, so state
	// must be dropped and the next apply plans a create (re-add).
	for name, list := range map[string]string{
		"empty cache":    "No registries present in the cache.",
		"other ref":      "REGISTRY NAME  REF     LOCAL_REF  REGISTRY_URL\nhomelab        v9.9.9  9f8e7d6c   github.com/byteford/nomad-packs",
		"other registry": "REGISTRY NAME  REF     LOCAL_REF  REGISTRY_URL\ncommunity      v1.2.3  9f8e7d6c   github.com/x/y",
	} {
		t.Run(name, func(t *testing.T) {
			f := newRegistryFixture(t, list, false)

			m := homelabModel("v1.2.3")
			m.ID = types.StringValue("homelab")
			st := f.state(t, m)
			resp := &resource.ReadResponse{State: st}
			f.r.Read(context.Background(), resource.ReadRequest{State: st}, resp)

			if resp.Diagnostics.HasError() {
				t.Fatalf("unexpected diagnostics: %v", resp.Diagnostics)
			}
			if !resp.State.Raw.IsNull() {
				t.Fatal("missing registry must be removed from state")
			}
		})
	}
}

func TestRegistryUpdateAddsNewRefAndKeepsOldOne(t *testing.T) {
	f := newRegistryFixture(t, "", false)

	old := homelabModel("v1.2.3")
	old.ID = types.StringValue("homelab")
	req := resource.UpdateRequest{Plan: f.plan(t, homelabModel("v1.3.0")), State: f.state(t, old)}
	resp := &resource.UpdateResponse{State: f.state(t, old)}
	f.r.Update(context.Background(), req, resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("unexpected diagnostics: %v", resp.Diagnostics)
	}
	// Exactly one add, no delete of v1.2.3.
	assertCalls(t, f.calls(t), []string{"registry add homelab github.com/byteford/nomad-packs --ref=v1.3.0"})
}

func TestRegistryDeleteDeletesOnlyTheStateRef(t *testing.T) {
	f := newRegistryFixture(t, registryListHomelabV1, false)

	m := homelabModel("v1.2.3")
	m.ID = types.StringValue("homelab")
	resp := &resource.DeleteResponse{}
	f.r.Delete(context.Background(), resource.DeleteRequest{State: f.state(t, m)}, resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("unexpected diagnostics: %v", resp.Diagnostics)
	}
	assertCalls(t, f.calls(t), []string{"registry list", "registry delete homelab --ref=v1.2.3"})
}

func TestRegistryAttributeValidators(t *testing.T) {
	f := newRegistryFixture(t, "", false)
	attrs := f.schema.Schema.Attributes

	tests := []struct {
		attr    string
		value   string
		wantErr bool
	}{
		{"ref", "v1.2.3", false},
		{"ref", "0123456789abcdef0123456789abcdef01234567", false},
		{"ref", "", true},
		{"ref", "latest", true},
		{"ref", "@latest", true},
		{"name", "homelab", false},
		{"name", "", true},
		{"name", "-homelab", true},
		{"name", "a/b", true},
		{"name", ".", true},
		{"name", "..", true},
		{"name", "a b", true},
		{"name", "a|b", true},
		{"name", "my_registry.v2-test", false},
		{"source", "github.com/byteford/nomad-packs", false},
		{"source", "", true},
		{"source", "--ref=v1", true},
	}
	for _, tt := range tests {
		gotErr := runStringValidators(attrs[tt.attr].(schema.StringAttribute), tt.attr, tt.value)
		if gotErr != tt.wantErr {
			t.Errorf("%s=%q: error = %v, want %v", tt.attr, tt.value, gotErr, tt.wantErr)
		}
	}
}

func runStringValidators(a schema.StringAttribute, attr, value string) (hasErr bool) {
	req := validator.StringRequest{Path: path.Root(attr), ConfigValue: types.StringValue(value)}
	for _, v := range a.Validators {
		resp := &validator.StringResponse{}
		v.ValidateString(context.Background(), req, resp)
		if resp.Diagnostics.HasError() {
			return true
		}
	}
	return false
}

func assertCalls(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("calls mismatch:\n got:  %q\n want: %q", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("call %d mismatch:\n got:  %q\n want: %q", i, got, want)
		}
	}
}

// deploymentFixture builds a DeploymentResource over the same fake
// nomad-pack, whose `plan` fails the way nomad-pack does when the pack is
// not in its cache.
func newDeploymentFixture(t *testing.T, bin string) (*DeploymentResource, resource.SchemaResponse) {
	t.Helper()
	d := &DeploymentResource{client: &nomadpack.Client{BinaryPath: bin}}
	var sch resource.SchemaResponse
	d.Schema(context.Background(), resource.SchemaRequest{}, &sch)
	return d, sch
}

func deploymentState(t *testing.T, sch resource.SchemaResponse, pack, registry, ref string) tfsdk.State {
	t.Helper()
	reg := types.StringNull()
	if registry != "" {
		reg = types.StringValue(registry)
	}
	m := &deploymentResourceModel{
		ID:       types.StringValue("mosquitto"),
		Pack:     types.StringValue(pack),
		Registry: reg,
		Ref:      types.StringValue(ref),
		Name:     types.StringValue("mosquitto"),
		Vars:     types.MapNull(types.StringType),
		VarFiles: types.ListNull(types.StringType),
		Detach:   types.BoolNull(),
		Stopped:  types.BoolValue(false),
		DiffHash: types.StringValue("abc123"),
	}
	s := tfsdk.State{Schema: sch.Schema}
	if diags := s.Set(context.Background(), m); diags.HasError() {
		t.Fatalf("building state: %v", diags)
	}
	return s
}

const planPackMissing = `echo 'Failed To Find Pack' ; echo '  Error: stat /home/x/.cache/nomad-pack/homelab/v1.2.3/mosquitto@v1.2.3: no such file or directory'; exit 255`

// With a wiped nomad-pack cache and both resources already in state, refresh
// must not abort on the deployment, otherwise the registry is never
// re-added: the registry drops out of state, the deployment keeps its state
// (the jobs are still on the cluster), and apply recreates the registry.
func TestWipedCacheRefreshKeepsDeploymentAndDropsRegistry(t *testing.T) {
	script := "#!/bin/sh\n" +
		"if [ \"$1 $2\" = \"registry list\" ]; then echo 'No registries present in the cache.'; exit 0; fi\n" +
		"if [ \"$1\" = \"plan\" ]; then " + planPackMissing + "; fi\nexit 0\n"
	bin := filepath.Join(t.TempDir(), "nomad-pack")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	// Registry resource: removed from state.
	rr := &RegistryResource{client: &nomadpack.Client{BinaryPath: bin}}
	var rsch resource.SchemaResponse
	rr.Schema(context.Background(), resource.SchemaRequest{}, &rsch)
	rm := homelabModel("v1.2.3")
	rm.ID = types.StringValue("homelab")
	rst := tfsdk.State{Schema: rsch.Schema}
	rst.Set(context.Background(), rm)
	rresp := &resource.ReadResponse{State: rst}
	rr.Read(context.Background(), resource.ReadRequest{State: rst}, rresp)
	if rresp.Diagnostics.HasError() || !rresp.State.Raw.IsNull() {
		t.Fatalf("registry: want removed from state without error, got err=%v null=%v", rresp.Diagnostics, rresp.State.Raw.IsNull())
	}

	// Deployment resource: refresh survives and keeps its state.
	d, dsch := newDeploymentFixture(t, bin)
	dst := deploymentState(t, dsch, "mosquitto", "homelab", "v1.2.3")
	dresp := &resource.ReadResponse{State: dst}
	d.Read(context.Background(), resource.ReadRequest{State: dst}, dresp)
	if dresp.Diagnostics.HasError() {
		t.Fatalf("deployment refresh must not fail when the registry pack is missing from the cache: %v", dresp.Diagnostics)
	}
	if dresp.State.Raw.IsNull() {
		t.Fatal("deployment must stay in state: its jobs are still on the cluster")
	}
	var got deploymentResourceModel
	dresp.State.Get(context.Background(), &got)
	if got.DiffHash.ValueString() != "abc123" {
		t.Fatalf("diff_hash changed to %q; it must be left as it was", got.DiffHash.ValueString())
	}
}

func TestDeploymentRefreshStillFailsForMissingLocalPack(t *testing.T) {
	// A local-path pack that vanished is a real error, not a cache miss the
	// registry resource can repair.
	script := "#!/bin/sh\n" + planPackMissing + "\n"
	bin := filepath.Join(t.TempDir(), "nomad-pack")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	d, dsch := newDeploymentFixture(t, bin)
	dst := deploymentState(t, dsch, "/mnt/e/packs/mosquitto", "", "")
	resp := &resource.ReadResponse{State: dst}
	d.Read(context.Background(), resource.ReadRequest{State: dst}, resp)
	if !resp.Diagnostics.HasError() {
		t.Fatal("expected an error for a missing local pack")
	}
}
