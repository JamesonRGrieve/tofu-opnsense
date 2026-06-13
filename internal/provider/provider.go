// SPDX-License-Identifier: AGPL-3.0-or-later

// Package provider implements the opnsense OpenTofu/Terraform provider — a
// native client for the OPNsense REST API. It is generic over the API surface
// (the opnsense_object resource/data source address any module/controller
// command), giving full feature coverage without per-feature code.
package provider

import (
	"context"

	"github.com/JamesonRGrieve/tofu-opnsense/internal/opnsense"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var _ provider.Provider = (*opnsenseProvider)(nil)

// New returns the provider factory for a given version.
func New(version string) func() provider.Provider {
	return func() provider.Provider { return &opnsenseProvider{version: version} }
}

type opnsenseProvider struct {
	version string
}

type providerModel struct {
	Host     types.String `tfsdk:"host"`
	Key      types.String `tfsdk:"key"`
	Secret   types.String `tfsdk:"secret"`
	Insecure types.Bool   `tfsdk:"insecure"`
}

func (p *opnsenseProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	// Single-token type name -> resources are `opnsense_object`, so Terraform's
	// prefix-before-first-underscore inference resolves the local name cleanly
	// (the source address is still jamesonrgrieve/opnsense).
	resp.TypeName = "opnsense"
	resp.Version = p.version
}

func (p *opnsenseProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Native provider for OPNsense firewalls via the REST API " +
			"(`https://<host>/api`, HTTP Basic auth with an API key/secret).",
		Attributes: map[string]schema.Attribute{
			"host": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "OPNsense address (host or host:port), no scheme.",
			},
			"key": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "OPNsense API key (the Basic-auth username).",
			},
			"secret": schema.StringAttribute{
				Required:            true,
				Sensitive:           true,
				MarkdownDescription: "OPNsense API secret (the Basic-auth password).",
			},
			"insecure": schema.BoolAttribute{
				Optional: true,
				MarkdownDescription: "Skip TLS verification (default true — OPNsense ships a self-signed cert). " +
					"Set false only with a trusted cert installed.",
			},
		},
	}
}

func (p *opnsenseProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var cfg providerModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}
	insecure := true
	if !cfg.Insecure.IsNull() && !cfg.Insecure.IsUnknown() {
		insecure = cfg.Insecure.ValueBool()
	}
	client := opnsense.NewClient(opnsense.Config{
		Host:     cfg.Host.ValueString(),
		Key:      cfg.Key.ValueString(),
		Secret:   cfg.Secret.ValueString(),
		Insecure: insecure,
	})
	resp.ResourceData = client
	resp.DataSourceData = client
}

func (p *opnsenseProvider) Resources(_ context.Context) []func() resource.Resource {
	return []func() resource.Resource{NewObjectResource}
}

func (p *opnsenseProvider) DataSources(_ context.Context) []func() datasource.DataSource {
	return []func() datasource.DataSource{NewObjectDataSource}
}
