// SPDX-License-Identifier: AGPL-3.0-or-later

package provider

import (
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/types"
)

func TestBuildIfaceApplyPHP(t *testing.T) {
	m := ifaceAssignModel{
		Section:     types.StringValue("opt2"),
		Device:      types.StringValue("vlan04090"),
		Description: types.StringValue("IAC_EXERCISE"),
		IPv4Address: types.StringValue("203.0.113.241"),
		IPv4Prefix:  types.StringValue("29"),
		IPv6Address: types.StringNull(),
		IPv6Prefix:  types.StringNull(),
	}
	php := buildIfaceApplyPHP(m)
	for _, want := range []string{
		"$tif = 'vlan04090';",
		"$td = 'IAC_EXERCISE';",
		"$tip = '203.0.113.241';",
		`$iface["if"] = $tif;`,
		"interface_configure(false, $rs);", // SAFETY: single-interface, never whole-box
		`write_config(`,
	} {
		if !strings.Contains(php, want) {
			t.Errorf("apply PHP missing %q\n--- php ---\n%s", want, php)
		}
	}
	// MUST NOT bounce every interface (the 2026-06-04 outage).
	if strings.Contains(php, "interfaces_configure(") {
		t.Errorf("apply PHP must never call interfaces_configure() (whole-box bounce)\n%s", php)
	}
}

func TestBuildIfaceDeletePHP_TargetedTeardown(t *testing.T) {
	php := buildIfaceDeletePHP("opt2")
	if !strings.Contains(php, "interface_bring_down($section)") {
		t.Errorf("delete PHP must bring down only the one section\n%s", php)
	}
	if !strings.Contains(php, `unset($config["interfaces"][$section])`) {
		t.Errorf("delete PHP must unset the section\n%s", php)
	}
	if strings.Contains(php, "interfaces_configure(") {
		t.Errorf("delete PHP must never call interfaces_configure()\n%s", php)
	}
}

func TestBuildIfaceReadPHP(t *testing.T) {
	m := ifaceAssignModel{
		Section: types.StringValue("opt2"), Device: types.StringValue("vlan04090"),
		Description: types.StringValue("IAC_EXERCISE"),
	}
	php := buildIfaceReadPHP(m)
	for _, want := range []string{"$tif = 'vlan04090'", `echo "{}\n"`, "JSON_UNESCAPED_SLASHES"} {
		if !strings.Contains(php, want) {
			t.Errorf("read PHP missing %q\n%s", want, php)
		}
	}
}
