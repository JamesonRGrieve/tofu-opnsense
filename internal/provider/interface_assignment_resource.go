// SPDX-License-Identifier: AGPL-3.0-or-later

package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/JamesonRGrieve/tofu-opnsense/internal/opnsense"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// opnsense_interface_assignment manages an OPNsense logical-interface assignment
// (config.xml <interfaces><optN>): binding a VLAN/physical device to a logical
// interface (optN) with an IPv4/IPv6 SVI address. This is the interfaces
// "assign + configure" flow, which has NO MVC REST API — only the PHP config
// framework reached over SSH. It folds the consuming module's
// scottwinkler/shell vlan_interface scripts into this provider.
//
// SAFETY (the 2026-06-04 house-opnsense outage): apply/teardown NEVER call
// interfaces_configure() (it walks + bounces EVERY interface, severing the
// management session mid-run). They target the SINGLE resolved section via
// interface_configure(false, $section) / interface_bring_down($section).
var (
	_ resource.Resource                = (*ifaceAssignResource)(nil)
	_ resource.ResourceWithConfigure   = (*ifaceAssignResource)(nil)
	_ resource.ResourceWithImportState = (*ifaceAssignResource)(nil)
)

// NewInterfaceAssignmentResource constructs the opnsense_interface_assignment resource.
func NewInterfaceAssignmentResource() resource.Resource { return &ifaceAssignResource{} }

type ifaceAssignResource struct {
	client *opnsense.Client
}

type ifaceAssignModel struct {
	ID          types.String `tfsdk:"id"`
	Section     types.String `tfsdk:"section"`
	Device      types.String `tfsdk:"device"`
	Description types.String `tfsdk:"description"`
	IPv4Address types.String `tfsdk:"ipv4_address"`
	IPv4Prefix  types.String `tfsdk:"ipv4_prefix"`
	IPv6Address types.String `tfsdk:"ipv6_address"`
	IPv6Prefix  types.String `tfsdk:"ipv6_prefix"`
}

func (r *ifaceAssignResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_interface_assignment"
}

func (r *ifaceAssignResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "OPNsense logical-interface assignment (`config.interfaces.optN`): binds a VLAN/" +
			"physical `device` to a logical interface with an IPv4/IPv6 SVI, via the PHP config framework over " +
			"SSH (no MVC REST API exists). Requires the provider SSH transport. Applies only the single resolved " +
			"interface (never the whole-box bounce).",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Resolved OPNsense section name (optN) the assignment landed on.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"section": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Requested logical-interface section (e.g. `opt2`). The actual section is " +
					"resolved by matching `device` first, then `description`, then this positional name — so a " +
					"re-tag follows the same section instead of clobbering an unrelated interface.",
			},
			"device": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Underlying interface device (`if`), e.g. a VLAN device `vlan04090`.",
			},
			"description": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Interface description (`descr`) — OPNsense interface names are unique, so this is the re-tag match key.",
			},
			"ipv4_address": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Static IPv4 SVI address (`ipaddr`). Empty/unset leaves it blank.",
			},
			"ipv4_prefix": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "IPv4 prefix length (`subnet`), e.g. `24`.",
			},
			"ipv6_address": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Static IPv6 SVI address (`ipaddrv6`). Empty/unset clears the v6 SVI.",
			},
			"ipv6_prefix": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "IPv6 prefix length (`subnetv6`), e.g. `64`.",
			},
		},
	}
}

func (r *ifaceAssignResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	client, ok := req.ProviderData.(*opnsense.Client)
	if !ok {
		resp.Diagnostics.AddError("Unexpected provider data",
			fmt.Sprintf("expected *opnsense.Client, got %T", req.ProviderData))
		return
	}
	r.client = client
}

func (r *ifaceAssignResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var m ifaceAssignModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	r.applyAssign(ctx, &m, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *ifaceAssignResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var m ifaceAssignModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	r.applyAssign(ctx, &m, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

// deviceIface is the JSON shape the read/apply PHP echoes.
type deviceIface struct {
	Section string `json:"section"`
	Device  string `json:"device"`
	Descr   string `json:"descr"`
	IPv4    string `json:"ipv4"`
	Prefix4 string `json:"prefix4"`
	IPv6    string `json:"ipv6"`
	Prefix6 string `json:"prefix6"`
}

func (r *ifaceAssignResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var m ifaceAssignModel
	resp.Diagnostics.Append(req.State.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if r.client == nil || r.client.SSH == nil {
		return
	}
	out, err := r.client.SSH.Run("/usr/local/bin/php", []byte(buildIfaceReadPHP(m)))
	if err != nil {
		resp.Diagnostics.AddError("OPNsense interface_assignment read failed", err.Error())
		return
	}
	line := lastJSONLine(out)
	if line == "{}" || line == "" {
		resp.State.RemoveResource(ctx)
		return
	}
	var dev deviceIface
	if err := json.Unmarshal([]byte(line), &dev); err != nil {
		resp.Diagnostics.AddError("OPNsense interface_assignment read decode failed",
			fmt.Sprintf("%v (got %q)", err, line))
		return
	}
	m.ID = types.StringValue(dev.Section)
	m.Section = types.StringValue(dev.Section)
	m.Device = types.StringValue(dev.Device)
	// Subset refresh: only reconcile attributes the config declares (non-null).
	if !m.Description.IsNull() {
		m.Description = types.StringValue(dev.Descr)
	}
	if !m.IPv4Address.IsNull() {
		m.IPv4Address = types.StringValue(dev.IPv4)
	}
	if !m.IPv4Prefix.IsNull() {
		m.IPv4Prefix = types.StringValue(dev.Prefix4)
	}
	if !m.IPv6Address.IsNull() {
		m.IPv6Address = types.StringValue(dev.IPv6)
	}
	if !m.IPv6Prefix.IsNull() {
		m.IPv6Prefix = types.StringValue(dev.Prefix6)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *ifaceAssignResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var m ifaceAssignModel
	resp.Diagnostics.Append(req.State.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if r.client == nil || r.client.SSH == nil {
		return
	}
	section := m.ID.ValueString()
	if section == "" {
		return
	}
	if _, err := r.client.SSH.Run("/usr/local/bin/php", []byte(buildIfaceDeletePHP(section))); err != nil {
		resp.Diagnostics.AddError("OPNsense interface_assignment delete failed", err.Error())
	}
}

func (r *ifaceAssignResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	// Import id is the section name (optN).
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("section"), req.ID)...)
}

// applyAssign runs the apply PHP, captures the resolved section, and stores it as
// the resource id.
func (r *ifaceAssignResource) applyAssign(_ context.Context, m *ifaceAssignModel, diags *diag.Diagnostics) {
	if r.client == nil || r.client.SSH == nil {
		diags.AddError("OPNsense SSH transport not configured",
			"opnsense_interface_assignment requires the provider's ssh_host/ssh_user + ssh_key_file or ssh_key_pem.")
		return
	}
	out, err := r.client.SSH.Run("/usr/local/bin/php", []byte(buildIfaceApplyPHP(*m)))
	if err != nil {
		diags.AddError("OPNsense interface_assignment apply failed", err.Error())
		return
	}
	var dev deviceIface
	if e := json.Unmarshal([]byte(lastJSONLine(out)), &dev); e == nil && dev.Section != "" {
		m.ID = types.StringValue(dev.Section)
		m.Section = types.StringValue(dev.Section)
	} else {
		m.ID = m.Section
	}
}

// buildIfaceApplyPHP reproduces the shell vlan_interface create logic: resolve the
// section (by device, then descr, then positional), set the interface fields, write
// config, and apply ONLY that interface. Pure for testability.
func buildIfaceApplyPHP(m ifaceAssignModel) string {
	var b strings.Builder
	b.WriteString("<?php\nini_set('display_errors','stderr');\nerror_reporting(E_ALL);\n")
	b.WriteString("require_once(\"globals.inc\");\nrequire_once(\"config.inc\");\nrequire_once(\"util.inc\");\nrequire_once(\"interfaces.inc\");\n")
	b.WriteString("global $config;\n$config = parse_config(true);\n")
	b.WriteString("if (!isset($config[\"interfaces\"])) { $config[\"interfaces\"] = array(); }\n")
	fmt.Fprintf(&b, "$ts = '%s';\n", phpQuote(m.Section.ValueString()))
	fmt.Fprintf(&b, "$tif = '%s';\n", phpQuote(m.Device.ValueString()))
	fmt.Fprintf(&b, "$td = '%s';\n", phpQuote(m.Description.ValueString()))
	fmt.Fprintf(&b, "$tip = '%s';\n", phpQuote(m.IPv4Address.ValueString()))
	fmt.Fprintf(&b, "$tpfx = '%s';\n", phpQuote(m.IPv4Prefix.ValueString()))
	fmt.Fprintf(&b, "$tip6 = '%s';\n", phpQuote(m.IPv6Address.ValueString()))
	fmt.Fprintf(&b, "$tpfx6 = '%s';\n", phpQuote(m.IPv6Prefix.ValueString()))
	// Section resolution: device match → descr match → positional.
	b.WriteString(`$rs = $ts; $matched = false;
foreach ($config["interfaces"] as $section => $iface) { if ((string)($iface["if"] ?? "") === $tif) { $rs = $section; $matched = true; break; } }
if (!$matched && $td !== "") { foreach ($config["interfaces"] as $section => $iface) { if ((string)($iface["descr"] ?? "") === $td) { $rs = $section; $matched = true; break; } } }
if (!isset($config["interfaces"][$rs])) { $config["interfaces"][$rs] = array(); }
$iface = &$config["interfaces"][$rs];
$iface["if"] = $tif; $iface["enable"] = "1"; $iface["descr"] = $td;
$iface["ipaddr"] = $tip; $iface["subnet"] = $tpfx; $iface["gateway"] = "";
if ($tip6 !== "") { $iface["ipaddrv6"] = $tip6; $iface["subnetv6"] = $tpfx6; } else { $iface["ipaddrv6"] = ""; $iface["subnetv6"] = ""; }
$iface["gatewayv6"] = "";
write_config("opnsense_interface_assignment (managed by OpenTofu)");
interface_configure(false, $rs);
echo json_encode(array("section"=>$rs,"device"=>$tif,"descr"=>$td,"ipv4"=>$tip,"prefix4"=>$tpfx,"ipv6"=>$tip6,"prefix6"=>$tpfx6), JSON_UNESCAPED_SLASHES);
echo "\n";
`)
	return b.String()
}

// buildIfaceReadPHP reproduces the shell vlan_interface read logic.
func buildIfaceReadPHP(m ifaceAssignModel) string {
	var b strings.Builder
	b.WriteString("<?php\nini_set('display_errors','stderr');\nrequire_once(\"config.inc\");\n")
	b.WriteString("$config = parse_config(true);\n$ifs = $config[\"interfaces\"] ?? array();\n")
	fmt.Fprintf(&b, "$ts = '%s'; $tif = '%s'; $td = '%s';\n",
		phpQuote(m.Section.ValueString()), phpQuote(m.Device.ValueString()), phpQuote(m.Description.ValueString()))
	b.WriteString(`$fs = ""; $f = null;
if (isset($ifs[$ts]) && (string)($ifs[$ts]["if"] ?? "") === $tif) { $fs = $ts; $f = $ifs[$ts]; }
else { foreach ($ifs as $section => $iface) { if ((string)($iface["if"] ?? "") === $tif) { $fs = $section; $f = $iface; break; } } }
if ($f === null && $td !== "") { foreach ($ifs as $section => $iface) { if ((string)($iface["descr"] ?? "") === $td) { $fs = $section; $f = $iface; break; } } }
if ($f === null) { echo "{}\n"; exit(0); }
echo json_encode(array("section"=>$fs,"device"=>(string)($f["if"]??""),"descr"=>(string)($f["descr"]??""),"ipv4"=>(string)($f["ipaddr"]??""),"prefix4"=>(string)($f["subnet"]??""),"ipv6"=>(string)($f["ipaddrv6"]??""),"prefix6"=>(string)($f["subnetv6"]??"")), JSON_UNESCAPED_SLASHES);
echo "\n";
`)
	return b.String()
}

// buildIfaceDeletePHP reproduces the shell vlan_interface delete logic: targeted
// teardown of ONLY this section (bring down, unset, write) — never the whole box.
func buildIfaceDeletePHP(section string) string {
	var b strings.Builder
	b.WriteString("<?php\nini_set('display_errors','stderr');\n")
	b.WriteString("require_once(\"globals.inc\");\nrequire_once(\"config.inc\");\nrequire_once(\"util.inc\");\nrequire_once(\"interfaces.inc\");\n")
	b.WriteString("global $config;\n$config = parse_config(true);\n")
	fmt.Fprintf(&b, "$section = '%s';\n", phpQuote(section))
	b.WriteString(`if (isset($config["interfaces"][$section])) { interface_bring_down($section); unset($config["interfaces"][$section]); write_config("opnsense_interface_assignment removed by OpenTofu"); }
echo "OK\n";
`)
	return b.String()
}
