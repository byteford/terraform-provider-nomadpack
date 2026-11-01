package provider

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/byteford/terraform-provider-nomadpack/internal/nomadpack"
)

const rsSource = "github.com/byteford/nomad-packs"

// rsFake is a nomad-pack stand-in with a cache that starts wiped: `registry
// add` "caches" the registry, `registry list` shows it afterwards, `plan`
// reports no changes. Every call is logged.
func rsFake(t *testing.T) (bin, argLog string) {
	t.Helper()
	dir := t.TempDir()
	bin = filepath.Join(dir, "nomad-pack")
	argLog = filepath.Join(dir, "args.log")
	name := nomadpack.RegistryNameForSource(rsSource)
	script := `#!/bin/sh
echo "$*" >> '` + argLog + `'
case "$1 $2" in
"registry list")
  if [ -e '` + dir + `/cached' ]; then
    printf 'REGISTRY NAME  REF  LOCAL_REF  REGISTRY_URL\n` + name + `  v1.2.3  9f8e7d6c  ` + rsSource + `\n'
  else echo 'No registries present in the cache.'; fi ;;
"registry add") touch '` + dir + `/cached' ;;
esac
exit 0
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, argLog
}

func rsCalls(t *testing.T, argLog string) []string {
	t.Helper()
	b, err := os.ReadFile(argLog)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func rsModel(source, registry, ref types.String) *deploymentResourceModel {
	return &deploymentResourceModel{
		ID:             types.StringValue("mosquitto"),
		Pack:           types.StringValue("mosquitto"),
		Registry:       registry,
		RegistrySource: source,
		Ref:            ref,
		Name:           types.StringValue("mosquitto"),
		Vars:           types.MapNull(types.StringType),
		VarFiles:       types.ListNull(types.StringType),
		Detach:         types.BoolNull(),
		Stopped:        types.BoolValue(false),
		DiffHash:       types.StringValue(""),
	}
}

func rsSetState(t *testing.T, sch resource.SchemaResponse, m *deploymentResourceModel) tfsdk.State {
	t.Helper()
	s := tfsdk.State{Schema: sch.Schema}
	if diags := s.Set(context.Background(), m); diags.HasError() {
		t.Fatalf("building state: %v", diags)
	}
	return s
}

func TestDeploymentRegistrySourceValidation(t *testing.T) {
	d := &DeploymentResource{}
	var sch resource.SchemaResponse
	d.Schema(context.Background(), resource.SchemaRequest{}, &sch)

	null, str := types.StringNull(), types.StringValue
	tests := []struct {
		name            string
		source, reg, rf types.String
		wantErr         bool
	}{
		{"source with pinned ref", str(rsSource), null, str("v1.2.3"), false},
		{"registry only, as before", null, str("homelab"), null, false},
		{"neither", null, null, null, false},
		{"source and registry together", str(rsSource), str("homelab"), str("v1.2.3"), true},
		{"source without ref", str(rsSource), null, null, true},
		{"source with empty ref", str(rsSource), null, str(""), true},
		{"source with latest", str(rsSource), null, str("latest"), true},
		{"source with @latest", str(rsSource), null, str("@latest"), true},
		{"empty source", str(""), null, str("v1"), true},
		{"flag-like source", str("--ref=v1"), null, str("v1"), true},
		{"unknown ref is checked at apply", str(rsSource), null, types.StringUnknown(), false},
		{"unknown source is checked at apply", types.StringUnknown(), null, str("v1"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := rsSetState(t, sch, rsModel(tt.source, tt.reg, tt.rf))
			req := resource.ValidateConfigRequest{Config: tfsdk.Config{Schema: sch.Schema, Raw: st.Raw}}
			resp := &resource.ValidateConfigResponse{}
			d.ValidateConfig(context.Background(), req, resp)
			if resp.Diagnostics.HasError() != tt.wantErr {
				t.Fatalf("error = %v, want %v (%v)", resp.Diagnostics.HasError(), tt.wantErr, resp.Diagnostics)
			}
		})
	}
}

// Wiped cache with the deployment already in state: refresh adds the registry
// and then plans against it, instead of failing on a missing pack.
func TestDeploymentReadWithRegistrySourceRepopulatesWipedCache(t *testing.T) {
	bin, argLog := rsFake(t)
	d := &DeploymentResource{client: &nomadpack.Client{BinaryPath: bin}}
	var sch resource.SchemaResponse
	d.Schema(context.Background(), resource.SchemaRequest{}, &sch)
	name := nomadpack.RegistryNameForSource(rsSource)

	st := rsSetState(t, sch, rsModel(types.StringValue(rsSource), types.StringNull(), types.StringValue("v1.2.3")))
	resp := &resource.ReadResponse{State: st}
	d.Read(context.Background(), resource.ReadRequest{State: st}, resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("unexpected diagnostics: %v", resp.Diagnostics)
	}
	assertCalls(t, rsCalls(t, argLog), []string{
		"registry list",
		"registry add " + name + " " + rsSource + " --ref=v1.2.3",
		"plan mosquitto --diff --registry=" + name + " --ref=v1.2.3 --name=mosquitto",
	})
}

// The point of the feature: `terraform plan` for a brand-new deployment (or a
// ref bump) fetches the registry first, so the preview can run.
func TestDeploymentModifyPlanFetchesRegistryBeforePreview(t *testing.T) {
	bin, argLog := rsFake(t)
	d := &DeploymentResource{client: &nomadpack.Client{BinaryPath: bin}}
	var sch resource.SchemaResponse
	d.Schema(context.Background(), resource.SchemaRequest{}, &sch)
	name := nomadpack.RegistryNameForSource(rsSource)

	plan := tfsdk.Plan{Schema: sch.Schema, Raw: rsSetState(t, sch,
		rsModel(types.StringValue(rsSource), types.StringNull(), types.StringValue("v2.0.0"))).Raw}
	req := resource.ModifyPlanRequest{Plan: plan, State: tfsdk.State{Schema: sch.Schema}}
	resp := &resource.ModifyPlanResponse{Plan: plan}
	d.ModifyPlan(context.Background(), req, resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("unexpected diagnostics: %v", resp.Diagnostics)
	}
	assertCalls(t, rsCalls(t, argLog), []string{
		"registry list",
		"registry add " + name + " " + rsSource + " --ref=v2.0.0",
		"plan mosquitto --diff --registry=" + name + " --ref=v2.0.0 --name=mosquitto",
	})
}

func TestDeploymentModifyPlanWithUnknownSourceOrRefTouchesNothing(t *testing.T) {
	for name, m := range map[string]*deploymentResourceModel{
		"unknown ref":    rsModel(types.StringValue(rsSource), types.StringNull(), types.StringUnknown()),
		"unknown source": rsModel(types.StringUnknown(), types.StringNull(), types.StringValue("v1.2.3")),
	} {
		t.Run(name, func(t *testing.T) {
			bin, argLog := rsFake(t)
			d := &DeploymentResource{client: &nomadpack.Client{BinaryPath: bin}}
			var sch resource.SchemaResponse
			d.Schema(context.Background(), resource.SchemaRequest{}, &sch)

			plan := tfsdk.Plan{Schema: sch.Schema, Raw: rsSetState(t, sch, m).Raw}
			resp := &resource.ModifyPlanResponse{Plan: plan}
			d.ModifyPlan(context.Background(), resource.ModifyPlanRequest{Plan: plan, State: tfsdk.State{Schema: sch.Schema}}, resp)

			if resp.Diagnostics.HasError() {
				t.Fatalf("unexpected diagnostics: %v", resp.Diagnostics)
			}
			if got := rsCalls(t, argLog); len(got) != 0 {
				t.Fatalf("nomad-pack was invoked with an unknown source/ref: %v", got)
			}
		})
	}
}
