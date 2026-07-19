package main

import "testing"

// validate guards the one hard requirement to start: a conversation-store DSN.
// Everything else (auth, serving) is defaulted by RecommendedOptions.
func TestServerOptionsValidate(t *testing.T) {
	cases := []struct {
		name    string
		dsn     string
		wantErr bool
	}{
		{"has dsn", "postgres://u:p@h:5432/db", false},
		{"empty dsn", "", true},
		{"whitespace-only dsn", "   ", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := &serverOptions{PostgresDSN: tc.dsn}
			err := o.validate()
			if (err != nil) != tc.wantErr {
				t.Fatalf("validate() err = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}
