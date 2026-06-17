// SPDX-License-Identifier: AGPL-3.0-or-later

package provider

import (
	"errors"
	"testing"
)

func TestRunReconcile(t *testing.T) {
	okResp := []byte(`{"status":"ok"}`) // action command success (no "result" key)
	failResp := []byte(`{"result":"failed"}`)

	tests := []struct {
		name      string
		endpoints []string
		post      func(string) ([]byte, error)
		wantWarn  int
		wantAll   bool
	}{
		{
			name:      "all ok",
			endpoints: []string{"firewall/filter/apply", "firewall/alias/reconfigure"},
			post:      func(string) ([]byte, error) { return okResp, nil },
			wantWarn:  0, wantAll: false,
		},
		{
			name:      "no endpoints is a no-op",
			endpoints: nil,
			post:      func(string) ([]byte, error) { return nil, errors.New("should not be called") },
			wantWarn:  0, wantAll: false,
		},
		{
			name:      "transport error on the only endpoint escalates",
			endpoints: []string{"firewall/filter/apply"},
			post:      func(string) ([]byte, error) { return nil, errors.New("dial tcp: i/o timeout") },
			wantWarn:  1, wantAll: true,
		},
		{
			name:      "api-level failure counts as failure",
			endpoints: []string{"firewall/filter/apply"},
			post:      func(string) ([]byte, error) { return failResp, nil },
			wantWarn:  1, wantAll: true,
		},
		{
			name:      "partial failure warns but does not escalate",
			endpoints: []string{"firewall/filter/apply", "haproxy/service/reconfigure"},
			post: func(p string) ([]byte, error) {
				if p == "/haproxy/service/reconfigure" {
					return nil, errors.New("404 not found") // plugin absent on this box
				}
				return okResp, nil
			},
			wantWarn: 1, wantAll: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			warns, all := runReconcile(tt.endpoints, tt.post)
			if len(warns) != tt.wantWarn {
				t.Errorf("warnings = %d (%v), want %d", len(warns), warns, tt.wantWarn)
			}
			if all != tt.wantAll {
				t.Errorf("allFailed = %v, want %v", all, tt.wantAll)
			}
		})
	}
}
