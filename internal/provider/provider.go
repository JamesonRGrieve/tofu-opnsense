// SPDX-License-Identifier: AGPL-3.0-or-later

// Package provider implements the opnsense OpenTofu/Terraform provider — a
// native client for the OPNsense REST API. It is generic over the API surface
// (the opnsense_object resource/data source address any module/controller
// command), giving full feature coverage without per-feature code.
package provider

import (
	"context"
	"time"

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
	Timeout  types.Int64  `tfsdk:"timeout"`
	// SSH transport (optional) — only for opnsense_system_config, which manages the
	// config.xml <system> settings that have no REST API. Unset → that resource errors.
	SSHHost    types.String `tfsdk:"ssh_host"`
	SSHPort    types.Int64  `tfsdk:"ssh_port"`
	SSHUser    types.String `tfsdk:"ssh_user"`
	SSHKeyFile types.String `tfsdk:"ssh_key_file"`
	SSHKeyPEM  types.String `tfsdk:"ssh_key_pem"`
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
			"timeout": schema.Int64Attribute{
				Optional: true,
				MarkdownDescription: "Per-request HTTP timeout in seconds (default 30). Raise it for slow " +
					"operations that reconfigure the interface subsystem synchronously (e.g. creating a VXLAN " +
					"or bridge can exceed 30s on a box with many interfaces).",
			},
			"ssh_host": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "SSH address (host or host:port) for `opnsense_system_config` only — the " +
					"config.xml `<system>` settings (hostname/domain/timezone/DNS/NTP) have no REST API and are " +
					"applied via the PHP config framework over SSH. OPNsense often binds sshd to a different " +
					"interface/port than the API, so this is separate from `host`. Omit to disable that resource.",
			},
			"ssh_port": schema.Int64Attribute{
				Optional:            true,
				MarkdownDescription: "SSH port (default: the port in `ssh_host`, else 22).",
			},
			"ssh_user": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "SSH user (default `root`).",
			},
			"ssh_key_file": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Path to an SSH identity file (`ssh -i`). When empty, ssh_config/agent is used.",
			},
			"ssh_key_pem": schema.StringAttribute{
				Optional:  true,
				Sensitive: true,
				MarkdownDescription: "SSH private-key material (e.g. an OpenBao-signed key). Materialized to a temp " +
					"0600 file per call; available at plan time, unlike a Terraform-written key file.",
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
	var timeout time.Duration
	if !cfg.Timeout.IsNull() && !cfg.Timeout.IsUnknown() && cfg.Timeout.ValueInt64() > 0 {
		timeout = time.Duration(cfg.Timeout.ValueInt64()) * time.Second
	}
	client := opnsense.NewClient(opnsense.Config{
		Host:     cfg.Host.ValueString(),
		Key:      cfg.Key.ValueString(),
		Secret:   cfg.Secret.ValueString(),
		Insecure: insecure,
		Timeout:  timeout, // 0 -> client default (30s)
	})
	// Optional SSH transport — only wired when ssh_host is set. Powers
	// opnsense_system_config (the config.xml <system> tail has no REST API).
	if !cfg.SSHHost.IsNull() && cfg.SSHHost.ValueString() != "" {
		client.SSH = opnsense.NewSSHClient(opnsense.SSHConfig{
			Host:    cfg.SSHHost.ValueString(),
			Port:    int(cfg.SSHPort.ValueInt64()),
			User:    cfg.SSHUser.ValueString(),
			KeyFile: cfg.SSHKeyFile.ValueString(),
			KeyPEM:  cfg.SSHKeyPEM.ValueString(),
			Timeout: timeout,
		})
	}
	resp.ResourceData = client
	resp.DataSourceData = client
}

func (p *opnsenseProvider) Resources(_ context.Context) []func() resource.Resource {
	return []func() resource.Resource{NewObjectResource, NewReconcileResource, NewSystemConfigResource}
}

func (p *opnsenseProvider) DataSources(_ context.Context) []func() datasource.DataSource {
	return []func() datasource.DataSource{NewObjectDataSource}
}
