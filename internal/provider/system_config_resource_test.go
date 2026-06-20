// SPDX-License-Identifier: AGPL-3.0-or-later

package provider

import (
	"context"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

func strList(vals ...string) types.List {
	elems := make([]attr.Value, len(vals))
	for i, v := range vals {
		elems[i] = types.StringValue(v)
	}
	return types.ListValueMust(types.StringType, elems)
}

func TestBuildApplyPHP_OnlyDeclaredKeys(t *testing.T) {
	ctx := context.Background()
	// Only hostname + timezone declared; everything else null → must not appear.
	m := systemConfigModel{
		Hostname:    types.StringValue("fw-lab"),
		Timezone:    types.StringValue("America/Vancouver"),
		Domain:      types.StringNull(),
		DNSServers:  types.ListNull(types.StringType),
		NTPServers:  types.ListNull(types.StringType),
		NTPServeLAN: types.BoolNull(),
	}
	php := buildApplyPHP(ctx, m)
	for _, want := range []string{
		`$config["system"]["hostname"] = 'fw-lab';`,
		`$config["system"]["timezone"] = 'America/Vancouver';`,
		"system_hostname_configure(true)",
		"system_timezone_configure()",
		`write_config(`,
	} {
		if !strings.Contains(php, want) {
			t.Errorf("apply PHP missing %q\n--- php ---\n%s", want, php)
		}
	}
	for _, notWant := range []string{`["domain"]`, `["dnsserver"]`, `["timeservers"]`, "ntpd"} {
		if strings.Contains(php, notWant) {
			t.Errorf("apply PHP should not touch unset key %q\n--- php ---\n%s", notWant, php)
		}
	}
}

func TestBuildApplyPHP_DNSAndNTP(t *testing.T) {
	ctx := context.Background()
	m := systemConfigModel{
		Hostname:    types.StringNull(),
		Domain:      types.StringNull(),
		Timezone:    types.StringNull(),
		DNSServers:  strList("100.64.92.1", "1.1.1.1"),
		NTPServers:  strList("100.64.92.1", "pool.ntp.org"),
		NTPServeLAN: types.BoolValue(true),
	}
	php := buildApplyPHP(ctx, m)
	for _, want := range []string{
		`$config["system"]["dnsserver"] = array('100.64.92.1', '1.1.1.1');`,
		`$config["system"]["timeservers"] = '100.64.92.1 pool.ntp.org';`,
		`$config["ntpd"]["prefer"] = '100.64.92.1';`,
		`$config["ntpd"]["interface"] = 'lan';`,
		"system_resolvconf_generate()",
	} {
		if !strings.Contains(php, want) {
			t.Errorf("apply PHP missing %q\n--- php ---\n%s", want, php)
		}
	}
	// serve_lan=true must not also emit the unset branch.
	if strings.Contains(php, "unset($config[\"ntpd\"][\"interface\"])") {
		t.Errorf("serve_lan=true should not unset the interface\n%s", php)
	}
}

func TestBuildApplyPHP_TunablesAndLogRetention(t *testing.T) {
	ctx := context.Background()
	tun := types.MapValueMust(
		types.ObjectType{AttrTypes: map[string]attr.Type{"value": types.StringType, "description": types.StringType}},
		map[string]attr.Value{
			"net.inet.ip.forwarding": types.ObjectValueMust(
				map[string]attr.Type{"value": types.StringType, "description": types.StringType},
				map[string]attr.Value{"value": types.StringValue("1"), "description": types.StringValue("IP forwarding")},
			),
		},
	)
	m := systemConfigModel{
		Hostname: types.StringNull(), Domain: types.StringNull(), Timezone: types.StringNull(),
		DNSServers: types.ListNull(types.StringType), NTPServers: types.ListNull(types.StringType),
		NTPServeLAN: types.BoolNull(), Tunables: tun, LogRetentionDays: types.Int64Value(30),
	}
	php := buildApplyPHP(ctx, m)
	for _, want := range []string{
		"function _tofu_sysctl(",
		"_tofu_sysctl($config, 'net.inet.ip.forwarding', '1', 'IP forwarding');",
		`$config["OPNsense"]["Syslog"]["general"]["maxpreserve"] = '30';`,
		"write_config(",
	} {
		if !strings.Contains(php, want) {
			t.Errorf("apply PHP missing %q\n--- php ---\n%s", want, php)
		}
	}
}

func TestPhpQuote(t *testing.T) {
	cases := map[string]string{
		"plain": "plain",
		"a'b":   `a\'b`,
		`a\b`:   `a\\b`,
		`x'y\z`: `x\'y\\z`,
	}
	for in, want := range cases {
		if got := phpQuote(in); got != want {
			t.Errorf("phpQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLastJSONLine(t *testing.T) {
	in := []byte("PHP Warning: something\n{\"hostname\":\"fw\"}\n")
	if got := lastJSONLine(in); got != `{"hostname":"fw"}` {
		t.Errorf("lastJSONLine = %q", got)
	}
}
