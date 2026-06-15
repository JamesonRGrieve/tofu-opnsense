// SPDX-License-Identifier: AGPL-3.0-or-later

package provider

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/types"
)

func TestSubsetMatches(t *testing.T) {
	cases := []struct {
		name        string
		prior, cfg  string
		wantMatched bool
	}{
		{
			name:        "config subset of full device object — match (0-diff)",
			prior:       `{"enabled":"1","name":"lab_hosts","type":"host","content":"10.0.0.1","description":"lab"}`,
			cfg:         `{"name":"lab_hosts","content":"10.0.0.1"}`,
			wantMatched: true,
		},
		{
			name:        "declared key drifted — no match (update)",
			prior:       `{"enabled":"1","name":"lab_hosts-OLD","content":"10.0.0.1"}`,
			cfg:         `{"name":"lab_hosts","content":"10.0.0.1"}`,
			wantMatched: false,
		},
		{
			name:        "declared key missing on device — no match",
			prior:       `{"enabled":"1","content":"10.0.0.1"}`,
			cfg:         `{"name":"lab_hosts","content":"10.0.0.1"}`,
			wantMatched: false,
		},
		{
			name:        "key order / whitespace insensitive — match",
			prior:       `{"content":"10.0.0.1","name":"lab_hosts"}`,
			cfg:         "{\n  \"name\": \"lab_hosts\",\n  \"content\": \"10.0.0.1\"\n}",
			wantMatched: true,
		},
		{
			name:        "nested object value compared structurally — match",
			prior:       `{"proto":{"IPv4":{"value":"IPv4","selected":1},"IPv6":{"value":"IPv6","selected":0}},"name":"a"}`,
			cfg:         `{"proto":{"IPv6":{"value":"IPv6","selected":0},"IPv4":{"value":"IPv4","selected":1}}}`,
			wantMatched: true,
		},
		{
			name:        "nested object value drift — no match",
			prior:       `{"proto":{"IPv4":{"selected":1}}}`,
			cfg:         `{"proto":{"IPv4":{"selected":0}}}`,
			wantMatched: false,
		},
		{
			name:        "list value compared in order — match",
			prior:       `{"servers":["1.1.1.1","8.8.8.8"],"name":"dns"}`,
			cfg:         `{"servers":["1.1.1.1","8.8.8.8"]}`,
			wantMatched: true,
		},
		{
			name:        "invalid prior JSON — no match (fall back to diff)",
			prior:       `not json`,
			cfg:         `{"a":1}`,
			wantMatched: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := subsetMatches(tc.prior, tc.cfg); got != tc.wantMatched {
				t.Fatalf("subsetMatches() = %v, want %v", got, tc.wantMatched)
			}
		})
	}
}

func TestCollapseOptionFields(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "single-select option field -> selected key (numeric selected)",
			in:   `{"ipprotocol":{"inet":{"value":"IPv4","selected":1},"inet6":{"value":"IPv6","selected":0}},"name":"gw"}`,
			want: `{"ipprotocol":"inet","name":"gw"}`,
		},
		{
			name: "string selected flag",
			in:   `{"type":{"network":{"value":"Network","selected":"1"},"host":{"value":"Host","selected":"0"}}}`,
			want: `{"type":"network"}`,
		},
		{
			name: "multi-select -> comma-joined sorted keys",
			in:   `{"iface":{"opt5":{"value":"B","selected":1},"opt2":{"value":"A","selected":1},"wan":{"value":"W","selected":0}}}`,
			want: `{"iface":"opt2,opt5"}`,
		},
		{
			name: "none selected -> empty string",
			in:   `{"iface":{"wan":{"value":"W","selected":0},"lan":{"value":"L","selected":0}}}`,
			want: `{"iface":""}`,
		},
		{
			name: "plain string fields untouched",
			in:   `{"name":"x","content":"10.0.0.0/24"}`,
			want: `{"content":"10.0.0.0/24","name":"x"}`,
		},
		{
			name: "non-option nested object recursed, not collapsed",
			in:   `{"opt":{"a":{"value":"A"},"b":{"value":"B"}}}`,
			want: `{"opt":{"a":{"value":"A"},"b":{"value":"B"}}}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := collapseOptionFields(tc.in)
			// compare structurally (key order from map marshal is non-deterministic)
			var g, w any
			if err := json.Unmarshal([]byte(got), &g); err != nil {
				t.Fatalf("output not JSON: %s", got)
			}
			_ = json.Unmarshal([]byte(tc.want), &w)
			if !reflect.DeepEqual(g, w) {
				t.Fatalf("collapseOptionFields() = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestCmdPath(t *testing.T) {
	cases := []struct {
		module, controller, command, uuid string
		want                              string
	}{
		{"firewall", "alias", "addItem", "", "/firewall/alias/addItem"},
		{"firewall", "alias", "getItem", "abc-123", "/firewall/alias/getItem/abc-123"},
		{"unbound", "general", "get", "", "/unbound/general/get"},
		{"/firewall/", "/filter/", "setItem", "u1", "/firewall/filter/setItem/u1"},
	}
	for _, tc := range cases {
		if got := cmdPath(tc.module, tc.controller, tc.command, tc.uuid); got != tc.want {
			t.Errorf("cmdPath(%q,%q,%q,%q) = %q, want %q", tc.module, tc.controller, tc.command, tc.uuid, got, tc.want)
		}
	}
}

func TestWrap(t *testing.T) {
	got, err := wrap("alias", []byte(`{"name":"lab","content":"10.0.0.1"}`))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"alias":{"name":"lab","content":"10.0.0.1"}}`
	if string(got) != want {
		t.Fatalf("wrap = %q, want %q", string(got), want)
	}
}

func TestUnwrap(t *testing.T) {
	cases := []struct {
		name       string
		controller string
		raw        string
		wantOK     bool
		wantObj    string
	}{
		{
			name:       "getItem envelope unwraps to compact object",
			controller: "alias",
			raw:        `{"alias":{"name":"lab","enabled":"1","content":"10.0.0.1"}}`,
			wantOK:     true,
			wantObj:    `{"content":"10.0.0.1","enabled":"1","name":"lab"}`,
		},
		{
			name:       "singleton get envelope unwraps",
			controller: "general",
			raw:        `{"general":{"enabled":"1","port":"53"}}`,
			wantOK:     true,
			wantObj:    `{"enabled":"1","port":"53"}`,
		},
		{
			name:       "missing uuid returns empty top object — not found",
			controller: "alias",
			raw:        `[]`,
			wantOK:     false,
		},
		{
			name:       "envelope present but empty node — not found",
			controller: "alias",
			raw:        `{"alias":{}}`,
			wantOK:     false,
		},
		{
			name:       "wrong controller key — not found",
			controller: "alias",
			raw:        `{"filter":{"name":"x"}}`,
			wantOK:     false,
		},
		{
			name:       "invalid JSON — not found",
			controller: "alias",
			raw:        `not json`,
			wantOK:     false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			obj, ok := unwrap(tc.controller, []byte(tc.raw))
			if ok != tc.wantOK {
				t.Fatalf("unwrap ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && obj != tc.wantObj {
				t.Fatalf("unwrap obj = %q, want %q", obj, tc.wantObj)
			}
		})
	}
}

func TestCheckResult(t *testing.T) {
	cases := []struct {
		name     string
		raw      string
		wantUUID string
		wantErr  bool
	}{
		{name: "addItem saved with uuid", raw: `{"result":"saved","uuid":"abc-123"}`, wantUUID: "abc-123"},
		{name: "setItem saved no uuid", raw: `{"result":"saved"}`, wantUUID: ""},
		{name: "delItem deleted", raw: `{"result":"deleted"}`, wantUUID: ""},
		{name: "reconfigure status ok (no result key)", raw: `{"status":"ok"}`, wantUUID: ""},
		{name: "failed with validations", raw: `{"result":"failed","validations":{"alias.name":"required"}}`, wantErr: true},
		{name: "failed bare", raw: `{"result":"failed"}`, wantErr: true},
		{name: "not found", raw: `{"result":"not found"}`, wantErr: true},
		{name: "invalid JSON", raw: `boom`, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			uuid, err := checkResult("op", []byte(tc.raw))
			if (err != nil) != tc.wantErr {
				t.Fatalf("checkResult err = %v, wantErr %v", err, tc.wantErr)
			}
			if err == nil && uuid != tc.wantUUID {
				t.Fatalf("checkResult uuid = %q, want %q", uuid, tc.wantUUID)
			}
		})
	}
}

func TestItemSuffix(t *testing.T) {
	cases := []struct {
		name string
		m    objectModel
		want string
	}{
		{"unset → base model Item", objectModel{ItemSuffix: types.StringNull()}, "Item"},
		{"empty string → Item", objectModel{ItemSuffix: types.StringValue("")}, "Item"},
		{"os-haproxy server", objectModel{ItemSuffix: types.StringValue("Server")}, "Server"},
		{"os-acme certificate", objectModel{ItemSuffix: types.StringValue("Certificate")}, "Certificate"},
		{"sentinel none → bare", objectModel{ItemSuffix: types.StringValue("none")}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := itemSuffix(tc.m); got != tc.want {
				t.Fatalf("itemSuffix() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSetVerb(t *testing.T) {
	cases := []struct {
		name string
		m    objectModel
		want string
	}{
		{"default base model → setItem", objectModel{ItemSuffix: types.StringNull(), SetCommand: types.StringNull()}, "setItem"},
		{"suffix → set<Suffix>", objectModel{ItemSuffix: types.StringValue("Server"), SetCommand: types.StringNull()}, "setServer"},
		{"bare sentinel → set", objectModel{ItemSuffix: types.StringValue("none"), SetCommand: types.StringNull()}, "set"},
		{"override → verbatim (os-acme update)", objectModel{ItemSuffix: types.StringValue("none"), SetCommand: types.StringValue("update")}, "update"},
		{"override wins over suffix", objectModel{ItemSuffix: types.StringValue("Server"), SetCommand: types.StringValue("update")}, "update"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := setVerb(tc.m); got != tc.want {
				t.Fatalf("setVerb() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestBareVerbPaths exercises the bare-verb controllers: os-acme (add/get/del
// bare, update for set) and core IPsec VTI (add/get/set/del bare).
func TestBareVerbPaths(t *testing.T) {
	acme := objectModel{
		Module:     types.StringValue("acmeclient"),
		Controller: types.StringValue("accounts"),
		ItemSuffix: types.StringValue("none"),
		Envelope:   types.StringValue("account"),
		SetCommand: types.StringValue("update"),
	}
	am, ac := acme.Module.ValueString(), acme.Controller.ValueString()
	if got := cmdPath(am, ac, "add"+itemSuffix(acme), ""); got != "/acmeclient/accounts/add" {
		t.Errorf("acme add path = %q", got)
	}
	if got := cmdPath(am, ac, "get"+itemSuffix(acme), "u1"); got != "/acmeclient/accounts/get/u1" {
		t.Errorf("acme get path = %q", got)
	}
	if got := cmdPath(am, ac, setVerb(acme), "u1"); got != "/acmeclient/accounts/update/u1" {
		t.Errorf("acme set(update) path = %q", got)
	}
	if got := cmdPath(am, ac, "del"+itemSuffix(acme), "u1"); got != "/acmeclient/accounts/del/u1" {
		t.Errorf("acme del path = %q", got)
	}
	if w, _ := wrap(envelopeKey(acme), []byte(`{"name":"le"}`)); string(w) != `{"account":{"name":"le"}}` {
		t.Errorf("acme wrap = %q", string(w))
	}

	vti := objectModel{
		Module:     types.StringValue("ipsec"),
		Controller: types.StringValue("vti"),
		ItemSuffix: types.StringValue("none"),
		Envelope:   types.StringValue("vti"),
	}
	vm, vc := vti.Module.ValueString(), vti.Controller.ValueString()
	if got := cmdPath(vm, vc, "add"+itemSuffix(vti), ""); got != "/ipsec/vti/add" {
		t.Errorf("vti add path = %q", got)
	}
	if got := cmdPath(vm, vc, setVerb(vti), "u1"); got != "/ipsec/vti/set/u1" {
		t.Errorf("vti set path = %q", got)
	}
}

func TestEnvelopeKey(t *testing.T) {
	cases := []struct {
		name string
		m    objectModel
		want string
	}{
		{"unset → controller (base model)", objectModel{Controller: types.StringValue("alias"), Envelope: types.StringNull()}, "alias"},
		{"empty → controller", objectModel{Controller: types.StringValue("settings"), Envelope: types.StringValue("")}, "settings"},
		{"override → item noun (haproxy server)", objectModel{Controller: types.StringValue("settings"), Envelope: types.StringValue("server")}, "server"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := envelopeKey(tc.m); got != tc.want {
				t.Fatalf("envelopeKey() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestPluginVerbPaths exercises the full os-haproxy contract: the computed
// add/get/set/del<Suffix> verbs and the item-noun envelope (NOT the controller).
func TestPluginVerbPaths(t *testing.T) {
	m := objectModel{
		Module:     types.StringValue("haproxy"),
		Controller: types.StringValue("settings"),
		ItemSuffix: types.StringValue("Server"),
		Envelope:   types.StringValue("server"),
	}
	module, controller := m.Module.ValueString(), m.Controller.ValueString()

	if got := cmdPath(module, controller, "add"+itemSuffix(m), ""); got != "/haproxy/settings/addServer" {
		t.Errorf("addServer path = %q", got)
	}
	if got := cmdPath(module, controller, "get"+itemSuffix(m), "u1"); got != "/haproxy/settings/getServer/u1" {
		t.Errorf("getServer path = %q", got)
	}
	if got := cmdPath(module, controller, "set"+itemSuffix(m), "u1"); got != "/haproxy/settings/setServer/u1" {
		t.Errorf("setServer path = %q", got)
	}
	if got := cmdPath(module, controller, "del"+itemSuffix(m), "u1"); got != "/haproxy/settings/delServer/u1" {
		t.Errorf("delServer path = %q", got)
	}
	// Envelope wraps under the item noun, not the controller.
	wrapped, err := wrap(envelopeKey(m), []byte(`{"name":"web1","address":"10.0.0.1"}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(wrapped) != `{"server":{"name":"web1","address":"10.0.0.1"}}` {
		t.Errorf("wrap envelope = %q", string(wrapped))
	}
	if obj, ok := unwrap(envelopeKey(m), []byte(`{"server":{"name":"web1","enabled":"1"}}`)); !ok || obj != `{"enabled":"1","name":"web1"}` {
		t.Errorf("unwrap envelope = %q ok=%v", obj, ok)
	}
}

func TestCompactJSON(t *testing.T) {
	out, err := compactJSON([]byte("{\n \"b\": 2,\n \"a\": 1\n}"))
	if err != nil {
		t.Fatal(err)
	}
	// json.Marshal of a map sorts keys; whitespace is removed.
	if out != `{"a":1,"b":2}` {
		t.Fatalf("compactJSON = %q", out)
	}
}
