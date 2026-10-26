package harnesslogin

import (
	"net/url"
	"os"
	"strings"
	"testing"
)

func TestNativeAnthropicAuthorizationFlagIsNotAnAuthorizationCode(t *testing.T) {
	u, _ := url.Parse(authURL("native-state"))
	u.Host = "claude.ai"
	q := u.Query()
	q.Set("redirect_uri", "http://localhost:53692/callback")
	q.Set("code", "true")
	u.RawQuery = q.Encode()
	flow, cb, err := authorization("pi", u.String())
	if err != nil || flow != PiAnthropic || cb.port() != 53692 {
		t.Fatal("native Anthropic launch refused")
	}
	q.Set("code", "secret-canary")
	u.RawQuery = q.Encode()
	if _, _, err := authorization("pi", u.String()); err == nil {
		t.Fatal("unexpected authorization code accepted")
	}
}

func TestShippedVersionsMatchImagePins(t *testing.T) {
	data, err := os.ReadFile("../../Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	for name, output := range shippedVersions {
		version := strings.TrimPrefix(output, "codex-cli ")
		if !strings.Contains(string(data), "ARG "+strings.ToUpper(name)+"_VERSION="+version+"\n") {
			t.Errorf("%s changed: image pin and remote-login binding must move together", name)
		}
	}
}

func TestAnthropicCallbackDoesNotAcceptOpenAIStateMetadata(t *testing.T) {
	u, _ := url.Parse(authURL("native-state"))
	u.Host = "claude.ai"
	q := u.Query()
	q.Set("redirect_uri", "http://localhost:53692/callback")
	u.RawQuery = q.Encode()
	_, cb, err := authorization("pi", u.String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cb.requestTarget("http://localhost:53692/callback?code=synthetic&state=" + url.QueryEscape("native-state"+lifeSciencesStateSuffix)); err == nil {
		t.Fatal("OpenAI metadata accepted for Anthropic")
	}
}
