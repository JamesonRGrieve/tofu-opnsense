// SPDX-License-Identifier: AGPL-3.0-or-later

package provider

import (
	"context"
	"fmt"
	"strings"

	"github.com/JamesonRGrieve/tofu-opnsense/internal/opnsense"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ datasource.DataSource              = (*objectDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*objectDataSource)(nil)
)

// NewObjectDataSource constructs the generic opnsense_object data source.
func NewObjectDataSource() datasource.DataSource { return &objectDataSource{} }

type objectDataSource struct {
	client *opnsense.Client
}

type objectDataModel struct {
	Path     types.String `tfsdk:"path"`
	Response types.String `tfsdk:"response"`
}

func (d *objectDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_object"
}

func (d *objectDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Read any OPNsense API GET command by its `/api` path.",
		Attributes: map[string]schema.Attribute{
			"path": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Command path under `/api` (leading slash optional), e.g. " +
					"`firewall/alias/get`, `unbound/settings/get`, `firewall/alias/searchItem`.",
			},
			"response": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The raw JSON response body from the firewall.",
			},
		},
	}
}

func (d *objectDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	client, ok := req.ProviderData.(*opnsense.Client)
	if !ok {
		resp.Diagnostics.AddError("Unexpected provider data", fmt.Sprintf("expected *opnsense.Client, got %T", req.ProviderData))
		return
	}
	d.client = client
}

func (d *objectDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var m objectDataModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	p := strings.TrimSpace(m.Path.ValueString())
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	raw, err := d.client.Get(p)
	if err != nil {
		resp.Diagnostics.AddError("OPNsense read failed", err.Error())
		return
	}
	compact, err := compactJSON(raw)
	if err != nil {
		resp.Diagnostics.AddError("OPNsense read: invalid JSON from device", err.Error())
		return
	}
	m.Response = types.StringValue(compact)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}
