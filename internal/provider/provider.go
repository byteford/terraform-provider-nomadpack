package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/byteford/terraform-provider-nomadpack/internal/nomadpack"
)

// Ensure the implementation satisfies the expected interfaces.
var _ provider.Provider = &NomadPackProvider{}

// NomadPackProvider drives nomad-pack's own run/plan/destroy lifecycle as
// Terraform resources, rather than re-implementing pack rendering or
// submitting jobs directly. See the provider README for the reasoning.
type NomadPackProvider struct {
	// version is set by goreleaser at build time via ldflags.
	version string
}

// nomadPackProviderModel maps provider schema data to a Go type.
type nomadPackProviderModel struct {
	BinaryPath    types.String `tfsdk:"binary_path"`
	Address       types.String `tfsdk:"address"`
	Namespace     types.String `tfsdk:"namespace"`
	Region        types.String `tfsdk:"region"`
	Token         types.String `tfsdk:"token"`
	CACert        types.String `tfsdk:"ca_cert"`
	ClientCert    types.String `tfsdk:"client_cert"`
	ClientKey     types.String `tfsdk:"client_key"`
	TLSServerName types.String `tfsdk:"tls_server_name"`
	TLSSkipVerify types.Bool   `tfsdk:"tls_skip_verify"`
	RunDetach     types.Bool   `tfsdk:"run_detach"`
}

// ProviderData is what gets threaded into each resource's Configure call.
type ProviderData struct {
	Client    *nomadpack.Client
	RunDetach bool
}

func (p *NomadPackProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "nomadpack"
	resp.Version = p.version
}

func (p *NomadPackProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Drives nomad-pack (https://developer.hashicorp.com/nomad/tools/nomad-pack) " +
			"deployments as Terraform resources by wrapping the nomad-pack CLI's own " +
			"plan/run/destroy lifecycle, so nomad-pack's meta tracking remains the " +
			"single source of truth for what's deployed.",
		Attributes: map[string]schema.Attribute{
			"binary_path": schema.StringAttribute{
				Optional:    true,
				Description: "Path to the nomad-pack binary. Defaults to \"nomad-pack\" resolved from PATH.",
			},
			"address": schema.StringAttribute{
				Optional:    true,
				Description: "Nomad server address. Falls back to the NOMAD_ADDR environment variable read by nomad-pack itself if unset.",
			},
			"namespace": schema.StringAttribute{
				Optional:    true,
				Description: "Nomad namespace. Falls back to NOMAD_NAMESPACE if unset.",
			},
			"region": schema.StringAttribute{
				Optional:    true,
				Description: "Nomad region. Falls back to NOMAD_REGION if unset.",
			},
			"token": schema.StringAttribute{
				Optional:    true,
				Sensitive:   true,
				Description: "Nomad ACL token. Falls back to NOMAD_TOKEN if unset.",
			},
			"ca_cert": schema.StringAttribute{
				Optional:    true,
				Description: "Path to a PEM encoded CA cert to verify the Nomad server's TLS certificate.",
			},
			"client_cert": schema.StringAttribute{
				Optional:    true,
				Description: "Path to a PEM encoded client certificate for TLS authentication. Requires client_key.",
			},
			"client_key": schema.StringAttribute{
				Optional:    true,
				Sensitive:   true,
				Description: "Path to the unencrypted PEM encoded private key matching client_cert.",
			},
			"tls_server_name": schema.StringAttribute{
				Optional:    true,
				Description: "SNI host to use when connecting via TLS.",
			},
			"tls_skip_verify": schema.BoolAttribute{
				Optional:    true,
				Description: "Skip TLS certificate verification. Not recommended.",
			},
			"run_detach": schema.BoolAttribute{
				Optional: true,
				Description: "Pass -detach to nomad-pack run/destroy so Terraform doesn't block " +
					"waiting for deployments/evaluations to finish healing. Defaults to false.",
			},
		},
	}
}

func (p *NomadPackProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var cfg nomadPackProviderModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client := &nomadpack.Client{
		BinaryPath: cfg.BinaryPath.ValueString(),
		Cluster: nomadpack.ClusterConfig{
			Address:       cfg.Address.ValueString(),
			Namespace:     cfg.Namespace.ValueString(),
			Region:        cfg.Region.ValueString(),
			Token:         cfg.Token.ValueString(),
			CACert:        cfg.CACert.ValueString(),
			ClientCert:    cfg.ClientCert.ValueString(),
			ClientKey:     cfg.ClientKey.ValueString(),
			TLSServerName: cfg.TLSServerName.ValueString(),
			TLSSkipVerify: cfg.TLSSkipVerify.ValueBool(),
		},
	}

	if !cfg.ClientCert.IsNull() && cfg.ClientKey.IsNull() {
		resp.Diagnostics.AddAttributeError(
			path.Root("client_key"),
			"Missing client_key",
			"client_cert was set but client_key was not. nomad-pack requires both for TLS client authentication.",
		)
		return
	}

	data := &ProviderData{
		Client:    client,
		RunDetach: cfg.RunDetach.ValueBool(),
	}

	resp.DataSourceData = data
	resp.ResourceData = data
}

func (p *NomadPackProvider) Resources(_ context.Context) []func() resource.Resource {
	return []func() resource.Resource{
		NewDeploymentResource,
	}
}

func (p *NomadPackProvider) DataSources(_ context.Context) []func() datasource.DataSource {
	return []func() datasource.DataSource{}
}

// New returns a provider factory, as expected by providerserver.NewProtocol6.
func New(version string) func() provider.Provider {
	return func() provider.Provider {
		return &NomadPackProvider{version: version}
	}
}
