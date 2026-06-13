// SPDX-License-Identifier: AGPL-3.0-or-later

package provider

import "testing"

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
