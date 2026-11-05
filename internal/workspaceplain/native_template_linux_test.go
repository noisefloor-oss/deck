package workspaceplain

import (
	"bytes"
	"strings"
	"testing"
	"text/template"
)

func TestTransportFormatEncryptedStateAliasPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name       string
		env        []string
		wantMarker bool
	}{
		{"legacy", []string{"NOISEDECK_REQUIRE_ENCRYPTED_STATE=false"}, true},
		{"canonical", []string{"NOISEDECK_SEALED_REQUIRE_ENCRYPTED_STATE=false"}, true},
		{"both", []string{"NOISEDECK_REQUIRE_ENCRYPTED_STATE=false", "NOISEDECK_SEALED_REQUIRE_ENCRYPTED_STATE=false"}, true},
		{"canonical true", []string{"NOISEDECK_REQUIRE_ENCRYPTED_STATE=false", "NOISEDECK_SEALED_REQUIRE_ENCRYPTED_STATE=true"}, false},
		{"malformed canonical", []string{"NOISEDECK_REQUIRE_ENCRYPTED_STATE=false", "NOISEDECK_SEALED_REQUIRE_ENCRYPTED_STATE=malformed"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := struct{ Config struct{ Env []string } }{}
			input.Config.Env = append([]string{
				"NOISEDECK_SERVER_BIND=",
				"NOISEDECK_SERVER_UNIX_SOCKET=/data/gateway/backend.sock",
				"NOISEDECK_PROJECT_PREVIEWS_BIND=",
				"NOISEDECK_EXPERIMENTS_UNCONFINED_CAPACITY_ROUTING=false",
				"NOISEDECK_AUTH_MODE=oidc",
			}, tc.env...)
			var out bytes.Buffer
			if err := template.Must(template.New("transport").Parse(transportFormat)).Execute(&out, input); err != nil {
				t.Fatal(err)
			}
			got := out.String()
			if tc.wantMarker && (len(got) != 6 || strings.Count(got, "u") != 1) {
				t.Fatalf("markers = %q, want exactly one encrypted-state marker", got)
			}
			if !tc.wantMarker && strings.Contains(got, "u") {
				t.Fatalf("markers = %q, canonical presence must suppress the legacy marker", got)
			}
		})
	}
}
