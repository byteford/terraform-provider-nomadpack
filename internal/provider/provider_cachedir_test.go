package provider

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

func configureWithCacheDir(t *testing.T, cacheDir types.String) (*provider.ConfigureResponse, *ProviderData) {
	t.Helper()
	ctx := context.Background()
	p := &NomadPackProvider{}

	var sch provider.SchemaResponse
	p.Schema(ctx, provider.SchemaRequest{}, &sch)

	m := &nomadPackProviderModel{
		BinaryPath: types.StringNull(), WorkingDir: types.StringNull(),
		Address: types.StringNull(), Namespace: types.StringNull(), Region: types.StringNull(),
		Token: types.StringNull(), CACert: types.StringNull(), ClientCert: types.StringNull(),
		ClientKey: types.StringNull(), TLSServerName: types.StringNull(),
		TLSSkipVerify: types.BoolNull(), RunDetach: types.BoolNull(),
		CacheDir: cacheDir,
	}
	st := tfsdk.State{Schema: sch.Schema}
	if diags := st.Set(ctx, m); diags.HasError() {
		t.Fatalf("building config: %v", diags)
	}

	resp := &provider.ConfigureResponse{}
	p.Configure(ctx, provider.ConfigureRequest{Config: tfsdk.Config{Schema: sch.Schema, Raw: st.Raw}}, resp)
	data, _ := resp.ResourceData.(*ProviderData)
	return resp, data
}

func TestProviderCacheDirAbsolutePathIsUsed(t *testing.T) {
	resp, data := configureWithCacheDir(t, types.StringValue("/var/cache/homelab"))
	if resp.Diagnostics.HasError() {
		t.Fatalf("unexpected diagnostics: %v", resp.Diagnostics)
	}
	if data == nil || data.Client.CacheDir != "/var/cache/homelab" {
		t.Fatalf("client cache dir not set: %+v", data)
	}
}

func TestProviderCacheDirUnsetIsEmpty(t *testing.T) {
	resp, data := configureWithCacheDir(t, types.StringNull())
	if resp.Diagnostics.HasError() {
		t.Fatalf("unexpected diagnostics: %v", resp.Diagnostics)
	}
	if data.Client.CacheDir != "" {
		t.Fatalf("CacheDir = %q, want empty", data.Client.CacheDir)
	}
}

func TestProviderCacheDirMustBeAbsoluteAndKnown(t *testing.T) {
	// A relative path would depend on the working directory, which is the
	// machine-to-machine difference this setting exists to remove.
	for name, v := range map[string]types.String{
		"relative":     types.StringValue("cache/nomad"),
		"dot relative": types.StringValue("./cache"),
		"empty":        types.StringValue(""),
		"unknown":      types.StringUnknown(),
	} {
		t.Run(name, func(t *testing.T) {
			resp, _ := configureWithCacheDir(t, v)
			if !resp.Diagnostics.HasError() {
				t.Fatalf("expected an error for cache_dir %s", name)
			}
		})
	}
}
