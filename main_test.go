package main

import "testing"

func TestEffectiveDryRun(t *testing.T) {
	for _, tc := range []struct {
		name       string
		configured bool
		payload    string
		want       bool
		wantErr    bool
	}{
		{"no payload, live", false, "", false, false},
		{"no payload, dry", true, "", true, false},
		{"scheduler event", false, "{}", false, false},
		{"force dry run", false, `{"dryRun": true}`, true, false},
		{"explicit false stays live", false, `{"dryRun": false}`, false, false},
		{"event cannot disable dry run", true, `{"dryRun": false}`, true, false},
		{"null", false, "null", false, false},
		{"string instead of bool", false, `{"dryRun": "true"}`, true, true},
		{"misspelled field", false, `{"dry_run": true}`, true, true},
		{"not JSON", false, `dryRun`, true, true},
		{"trailing data", false, `{} {"dryRun": false}`, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := effectiveDryRun(tc.configured, []byte(tc.payload))
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Fatalf("dryRun = %v, want %v", got, tc.want)
			}
		})
	}
}
