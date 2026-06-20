// SPDX-License-Identifier: AGPL-3.0-or-later

package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
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

// opnsense_system_config owns the config.xml <system> general settings that have
// NO REST API — hostname, domain, timezone, DNS servers, timeservers/NTP. These
// live only in config.xml and are applied through OPNsense's PHP config framework
// (config_read_array + write_config + the *_configure() functions), reached over
// SSH (see internal/opnsense/ssh.go). It folds the consuming module's
// scottwinkler/shell hostname/timezone/dns_upstream/dns_search_domain/ntp scripts
// into this provider. Singleton (one per device); every attribute is OPTIONAL and
// only the declared (non-null) ones are managed (subset semantics — an unset
// attribute is neither read for drift nor written).
var (
	_ resource.Resource                = (*systemConfigResource)(nil)
	_ resource.ResourceWithConfigure   = (*systemConfigResource)(nil)
	_ resource.ResourceWithImportState = (*systemConfigResource)(nil)
)

// NewSystemConfigResource constructs the opnsense_system_config resource.
func NewSystemConfigResource() resource.Resource { return &systemConfigResource{} }

type systemConfigResource struct {
	client *opnsense.Client
}

type systemConfigModel struct {
	ID               types.String `tfsdk:"id"`
	Hostname         types.String `tfsdk:"hostname"`
	Domain           types.String `tfsdk:"domain"`
	Timezone         types.String `tfsdk:"timezone"`
	DNSServers       types.List   `tfsdk:"dns_servers"`
	NTPServers       types.List   `tfsdk:"ntp_servers"`
	NTPServeLAN      types.Bool   `tfsdk:"ntp_serve_lan"`
	Tunables         types.Map    `tfsdk:"tunables"`
	LogRetentionDays types.Int64  `tfsdk:"log_retention_days"`
	SSHAllowUsers    types.List   `tfsdk:"ssh_allow_users"`
	SNMPTrapTarget   types.String `tfsdk:"snmp_trap_target"`
}

func (r *systemConfigResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_system_config"
}

func (r *systemConfigResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "OPNsense config.xml `<system>` general settings that have no REST API " +
			"(hostname, domain, timezone, DNS servers, timeservers/NTP), applied via the PHP config " +
			"framework over SSH. Singleton per device; every attribute is optional and only declared " +
			"attributes are managed (an unset attribute is left untouched). Requires the provider's SSH " +
			"transport to be configured (ssh_host/ssh_user + ssh_key_file or ssh_key_pem).",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"hostname": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "System hostname (`config.system.hostname`).",
			},
			"domain": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "DNS search domain (`config.system.domain`).",
			},
			"timezone": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "IANA timezone (`config.system.timezone`).",
			},
			"dns_servers": schema.ListAttribute{
				Optional:            true,
				ElementType:         types.StringType,
				MarkdownDescription: "Upstream DNS servers (`config.system.dnsserver[]`), in order.",
			},
			"ntp_servers": schema.ListAttribute{
				Optional:            true,
				ElementType:         types.StringType,
				MarkdownDescription: "NTP time servers (`config.system.timeservers`); the first is set as `ntpd.prefer`.",
			},
			"ntp_serve_lan": schema.BoolAttribute{
				Optional:            true,
				MarkdownDescription: "Serve NTP to the LAN interface (`ntpd.interface = lan`).",
			},
			"tunables": schema.MapNestedAttribute{
				Optional:            true,
				MarkdownDescription: "System tunables (`config.sysctl.item[]`), keyed by tunable name. Each is upserted and applied live with `sysctl`.",
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"value":       schema.StringAttribute{Required: true, MarkdownDescription: "Tunable value."},
						"description": schema.StringAttribute{Optional: true, MarkdownDescription: "Tunable description."},
					},
				},
			},
			"log_retention_days": schema.Int64Attribute{
				Optional:            true,
				MarkdownDescription: "Log retention in days (`config.OPNsense.Syslog.general.maxpreserve`); restarts syslog.",
			},
			"ssh_allow_users": schema.ListAttribute{
				Optional:            true,
				ElementType:         types.StringType,
				MarkdownDescription: "sshd `AllowUsers` (drop-in `/usr/local/etc/ssh/sshd_config.d/allow_users.conf`; restarts sshd). The provider's own SSH user is always appended so it can never lock the provider out.",
			},
			"snmp_trap_target": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "net-snmp trap2sink target `host` or `host:port` (`snmpd.local.conf`; restarts netsnmp). Empty leaves it unmanaged.",
			},
		},
	}
}

func (r *systemConfigResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

func (r *systemConfigResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var m systemConfigModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if r.apply(ctx, m, &resp.Diagnostics); resp.Diagnostics.HasError() {
		return
	}
	m.ID = types.StringValue("system")
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *systemConfigResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var m systemConfigModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if r.apply(ctx, m, &resp.Diagnostics); resp.Diagnostics.HasError() {
		return
	}
	m.ID = types.StringValue("system")
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

// Delete is a no-op: system settings persist on the box; we simply stop managing
// them (consistent with the singleton get/set pattern's no-op delete).
func (r *systemConfigResource) Delete(_ context.Context, _ resource.DeleteRequest, _ *resource.DeleteResponse) {
}

func (r *systemConfigResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), "system")...)
}

// deviceSystem is the JSON shape the read PHP echoes.
type deviceSystem struct {
	Hostname    string   `json:"hostname"`
	Domain      string   `json:"domain"`
	Timezone    string   `json:"timezone"`
	DNSServers  []string `json:"dns_servers"`
	NTPServers  string   `json:"ntp_servers"`
	NTPServeLAN bool     `json:"ntp_serve_lan"`
}

func (r *systemConfigResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var m systemConfigModel
	resp.Diagnostics.Append(req.State.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if r.client == nil || r.client.SSH == nil {
		return
	}
	dev, err := r.readDevice()
	if err != nil {
		resp.Diagnostics.AddError("OPNsense system_config read failed", err.Error())
		return
	}
	// Subset refresh: only managed (non-null) attributes are reconciled from the
	// device; unset attributes stay null so they are never tracked for drift.
	if !m.Hostname.IsNull() {
		m.Hostname = types.StringValue(dev.Hostname)
	}
	if !m.Domain.IsNull() {
		m.Domain = types.StringValue(dev.Domain)
	}
	if !m.Timezone.IsNull() {
		m.Timezone = types.StringValue(dev.Timezone)
	}
	if !m.DNSServers.IsNull() {
		lv, d := types.ListValueFrom(ctx, types.StringType, dev.DNSServers)
		resp.Diagnostics.Append(d...)
		m.DNSServers = lv
	}
	if !m.NTPServers.IsNull() {
		var servers []string
		if strings.TrimSpace(dev.NTPServers) != "" {
			servers = strings.Fields(dev.NTPServers)
		} else {
			servers = []string{}
		}
		lv, d := types.ListValueFrom(ctx, types.StringType, servers)
		resp.Diagnostics.Append(d...)
		m.NTPServers = lv
	}
	if !m.NTPServeLAN.IsNull() {
		m.NTPServeLAN = types.BoolValue(dev.NTPServeLAN)
	}
	m.ID = types.StringValue("system")
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *systemConfigResource) readDevice() (*deviceSystem, error) {
	out, err := r.client.SSH.Run("/usr/local/bin/php", []byte(readPHP))
	if err != nil {
		return nil, err
	}
	// The PHP echoes a single JSON object; tolerate any leading warnings by taking
	// the last non-empty line.
	line := lastJSONLine(out)
	var dev deviceSystem
	if err := json.Unmarshal([]byte(line), &dev); err != nil {
		return nil, fmt.Errorf("decode system read %q: %w", line, err)
	}
	return &dev, nil
}

func lastJSONLine(b []byte) string {
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		s := strings.TrimSpace(lines[i])
		if strings.HasPrefix(s, "{") {
			return s
		}
	}
	return strings.TrimSpace(string(b))
}

// apply builds and runs the PHP that sets the declared keys, writes config, and
// runs the matching configure functions. Only declared (non-null) attributes are
// emitted, so unset attributes are left untouched on the device.
func (r *systemConfigResource) apply(ctx context.Context, m systemConfigModel, diags *diag.Diagnostics) {
	if r.client == nil || r.client.SSH == nil {
		diags.AddError("OPNsense SSH transport not configured",
			"opnsense_system_config requires the provider's ssh_host/ssh_user + ssh_key_file or ssh_key_pem.")
		return
	}
	if _, err := r.client.SSH.Run("/usr/local/bin/php", []byte(buildApplyPHP(ctx, m))); err != nil {
		diags.AddError("OPNsense system_config apply failed", err.Error())
		return
	}
	// NTP has no system_*_configure() in core, so restart the daemon via configctl
	// as a plain shell command (the shell tier did the same — mwexec() is not
	// available in this php require set, and an undefined-function call is a fatal).
	if !m.NTPServers.IsNull() {
		if _, err := r.client.SSH.Run("/usr/local/sbin/configctl ntpd restart", nil); err != nil {
			diags.AddError("OPNsense system_config ntpd restart failed", err.Error())
		}
	}
	// Apply tunables live (write_config persists; boot-only tunables ignore the live
	// set harmlessly). One `sysctl k=v` per declared tunable.
	for _, t := range sortedKeys(tunablesToMap(ctx, m.Tunables)) {
		tv := tunablesToMap(ctx, m.Tunables)[t]
		_, _ = r.client.SSH.Run(fmt.Sprintf("sysctl %s=%s", shellArg(t), shellArg(tv.value)), nil)
	}
	// Log retention is a syslog-ng setting — restart syslog to apply maxpreserve.
	if !m.LogRetentionDays.IsNull() {
		if _, err := r.client.SSH.Run("/usr/local/sbin/configctl syslog restart", nil); err != nil {
			diags.AddError("OPNsense system_config syslog restart failed", err.Error())
		}
	}
	// sshd AllowUsers — a config.xml-less drop-in file (no MVC API). Always include
	// the provider's own SSH user so a bad list can't lock the transport out.
	if !m.SSHAllowUsers.IsNull() {
		conf := buildAllowUsersConf(listToStrings(ctx, m.SSHAllowUsers), r.client.SSH.User())
		if conf != "" {
			cmd := fmt.Sprintf("mkdir -p /usr/local/etc/ssh/sshd_config.d && printf '%%s\\n' %s > /usr/local/etc/ssh/sshd_config.d/allow_users.conf", shellArg(conf))
			if _, err := r.client.SSH.Run(cmd, nil); err != nil {
				diags.AddError("OPNsense system_config ssh AllowUsers write failed", err.Error())
				return
			}
			_, _ = r.client.SSH.Run("/usr/local/sbin/configctl openssh restart", nil)
		}
	}
	// SNMP trap2sink — net-snmp reads snmpd.local.conf; rewrite the tofu-managed
	// block (marker-delimited, BSD sed) and restart netsnmp.
	if !m.SNMPTrapTarget.IsNull() && m.SNMPTrapTarget.ValueString() != "" {
		line := buildTrapSink(m.SNMPTrapTarget.ValueString())
		cmd := fmt.Sprintf("touch /usr/local/share/snmp/snmpd.local.conf && "+
			"sed -i '' '/^# tofu-managed-trap/,/^trap2sink/{/^# tofu-managed-trap/d;/^trap2sink/d;}' /usr/local/share/snmp/snmpd.local.conf && "+
			"printf '%%s\\n' '# tofu-managed-trap' %s >> /usr/local/share/snmp/snmpd.local.conf", shellArg(line))
		if _, err := r.client.SSH.Run(cmd, nil); err != nil {
			diags.AddError("OPNsense system_config snmp trap2sink write failed", err.Error())
			return
		}
		_, _ = r.client.SSH.Run("/usr/local/sbin/configctl netsnmp restart", nil)
	}
}

// buildAllowUsersConf renders the sshd AllowUsers drop-in line, always including
// connUser so the provider can never lock its own SSH transport out. Empty when
// there are no users.
func buildAllowUsersConf(users []string, connUser string) string {
	all := append([]string{}, users...)
	present := false
	for _, u := range all {
		if u == connUser {
			present = true
			break
		}
	}
	if connUser != "" && !present {
		all = append(all, connUser)
	}
	if len(all) == 0 {
		return ""
	}
	return "AllowUsers " + strings.Join(all, " ")
}

// buildTrapSink renders a net-snmp trap2sink directive from a "host" or
// "host:port" target (default port 162).
func buildTrapSink(target string) string {
	host, port := target, "162"
	if i := strings.LastIndex(target, ":"); i > 0 {
		host, port = target[:i], target[i+1:]
	}
	return "trap2sink " + host + " public " + port
}

// shellArg single-quotes a value for safe use in a remote shell command.
func shellArg(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// buildApplyPHP renders the PHP that sets the declared keys, writes config, and
// runs the matching configure functions. Pure (no SSH) so it is unit-testable.
// Only declared (non-null) attributes are emitted — an unset attribute is left
// untouched on the device.
func buildApplyPHP(ctx context.Context, m systemConfigModel) string {
	var b strings.Builder
	b.WriteString("<?php\n")
	// Surface runtime PHP errors on stderr so a fatal is diagnosable via SSH
	// (OPNsense CLI php has display_errors off by default → a fatal would just
	// exit 255 with nothing on the wire).
	b.WriteString("ini_set('display_errors', 'stderr');\nerror_reporting(E_ALL);\n")
	b.WriteString("require_once(\"config.inc\");\n")
	b.WriteString("require_once(\"util.inc\");\n")
	b.WriteString("require_once(\"system.inc\");\n")
	b.WriteString("config_read_array(\"system\");\n")

	doHostname := !m.Hostname.IsNull()
	doDomain := !m.Domain.IsNull()
	doTZ := !m.Timezone.IsNull()
	doDNS := !m.DNSServers.IsNull()
	doNTP := !m.NTPServers.IsNull()

	if doHostname {
		fmt.Fprintf(&b, "$config[\"system\"][\"hostname\"] = '%s';\n", phpQuote(m.Hostname.ValueString()))
	}
	if doDomain {
		fmt.Fprintf(&b, "$config[\"system\"][\"domain\"] = '%s';\n", phpQuote(m.Domain.ValueString()))
	}
	if doTZ {
		fmt.Fprintf(&b, "$config[\"system\"][\"timezone\"] = '%s';\n", phpQuote(m.Timezone.ValueString()))
	}
	if doDNS {
		servers := listToStrings(ctx, m.DNSServers)
		b.WriteString("$config[\"system\"][\"dnsserver\"] = array(")
		for i, s := range servers {
			if i > 0 {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "'%s'", phpQuote(s))
		}
		b.WriteString(");\n")
	}
	if doNTP {
		servers := listToStrings(ctx, m.NTPServers)
		fmt.Fprintf(&b, "$config[\"system\"][\"timeservers\"] = '%s';\n", phpQuote(strings.Join(servers, " ")))
		b.WriteString("config_read_array(\"ntpd\");\n")
		if len(servers) > 0 {
			fmt.Fprintf(&b, "$config[\"ntpd\"][\"prefer\"] = '%s';\n", phpQuote(servers[0]))
		}
		if !m.NTPServeLAN.IsNull() && m.NTPServeLAN.ValueBool() {
			b.WriteString("$config[\"ntpd\"][\"interface\"] = 'lan';\n")
		} else if !m.NTPServeLAN.IsNull() {
			b.WriteString("unset($config[\"ntpd\"][\"interface\"]);\n")
		}
	}

	// Tunables (config.sysctl.item[]) — upsert each by tunable name (via the
	// _tofu_sysctl helper defined in the preamble). The live `sysctl` set is done
	// in apply() (boot-only tunables ignore it harmlessly).
	tunables := tunablesToMap(ctx, m.Tunables)
	if len(tunables) > 0 {
		b.WriteString("config_read_array(\"sysctl\", \"item\");\n")
		b.WriteString("function _tofu_sysctl(&$config,$t,$v,$d){ if(!isset($config[\"sysctl\"][\"item\"])||!is_array($config[\"sysctl\"][\"item\"]))$config[\"sysctl\"][\"item\"]=array(); $f=false; foreach($config[\"sysctl\"][\"item\"] as &$it){ if(($it[\"tunable\"]??\"\")===$t){$it[\"value\"]=$v;$it[\"descr\"]=$d;$f=true;break;} } unset($it); if(!$f)$config[\"sysctl\"][\"item\"][]=array(\"tunable\"=>$t,\"value\"=>$v,\"descr\"=>$d); }\n")
		for _, t := range sortedKeys(tunables) {
			fmt.Fprintf(&b, "_tofu_sysctl($config, '%s', '%s', '%s');\n",
				phpQuote(t), phpQuote(tunables[t].value), phpQuote(tunables[t].description))
		}
	}
	// Log retention (config.OPNsense.Syslog.general.maxpreserve).
	if !m.LogRetentionDays.IsNull() {
		b.WriteString("config_read_array(\"OPNsense\", \"Syslog\", \"general\");\n")
		fmt.Fprintf(&b, "$config[\"OPNsense\"][\"Syslog\"][\"general\"][\"maxpreserve\"] = '%d';\n", m.LogRetentionDays.ValueInt64())
	}

	// One write_config after all sets, then the matching configure functions.
	b.WriteString("write_config(\"opnsense_system_config (managed by OpenTofu)\");\n")
	if doHostname || doDomain {
		// system_hostname_configure requires its $verbose arg (the shell tier this
		// replaced called it as system_hostname_configure(true)).
		b.WriteString("if (function_exists('system_hostname_configure')) system_hostname_configure(true);\n")
	}
	if doTZ {
		b.WriteString("if (function_exists('system_timezone_configure')) system_timezone_configure();\n")
	}
	if doDNS || doDomain {
		b.WriteString("if (function_exists('system_resolvconf_generate')) system_resolvconf_generate();\n")
	}
	b.WriteString("echo \"OK\\n\";\n")
	return b.String()
}

// readPHP echoes the managed <system> settings as one JSON object.
const readPHP = `<?php
ini_set('display_errors', 'stderr');
require_once("config.inc");
config_read_array("system");
$ds = array();
if (isset($config["system"]["dnsserver"])) {
  foreach ((array)$config["system"]["dnsserver"] as $d) { if ($d !== "") $ds[] = (string)$d; }
}
$serve = isset($config["ntpd"]["interface"]) && $config["ntpd"]["interface"] !== "";
echo json_encode(array(
  "hostname" => isset($config["system"]["hostname"]) ? (string)$config["system"]["hostname"] : "",
  "domain" => isset($config["system"]["domain"]) ? (string)$config["system"]["domain"] : "",
  "timezone" => isset($config["system"]["timezone"]) ? (string)$config["system"]["timezone"] : "",
  "dns_servers" => $ds,
  "ntp_servers" => isset($config["system"]["timeservers"]) ? (string)$config["system"]["timeservers"] : "",
  "ntp_serve_lan" => $serve,
), JSON_UNESCAPED_SLASHES);
echo "\n";
`

// phpQuote escapes a value for a PHP single-quoted string literal.
func phpQuote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `'`, `\'`)
	return s
}

func listToStrings(ctx context.Context, l types.List) []string {
	var out []string
	if l.IsNull() || l.IsUnknown() {
		return out
	}
	_ = l.ElementsAs(ctx, &out, false)
	return out
}

type tunableVal struct {
	Value       types.String `tfsdk:"value"`
	Description types.String `tfsdk:"description"`
}

// tunablesToMap flattens the tunables map attribute to name -> {value, description}.
func tunablesToMap(ctx context.Context, m types.Map) map[string]struct{ value, description string } {
	out := map[string]struct{ value, description string }{}
	if m.IsNull() || m.IsUnknown() {
		return out
	}
	elems := map[string]tunableVal{}
	if d := m.ElementsAs(ctx, &elems, false); d.HasError() {
		return out
	}
	for k, v := range elems {
		out[k] = struct{ value, description string }{value: v.Value.ValueString(), description: v.Description.ValueString()}
	}
	return out
}

func sortedKeys(m map[string]struct{ value, description string }) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
