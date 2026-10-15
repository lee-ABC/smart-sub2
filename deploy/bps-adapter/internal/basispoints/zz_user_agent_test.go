package basispoints

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

// Nothing the upstream receives may name this plugin: the old User-Agent
// ("oai-basispoints/0.1.2") identified the relay on its own, with no
// correlation needed.
func TestNoOutboundHeaderNamesThePlugin(t *testing.T) {
	headers := authHeaders(credential{AccessToken: "t", AccountID: "a", AuthMode: "chatgpt"}, true)
	for name, values := range headers {
		for _, value := range values {
			if strings.Contains(strings.ToLower(value), "basispoints/") ||
				strings.Contains(strings.ToLower(value), "go-http-client") {
				t.Errorf("header %s leaks the client identity: %q", name, value)
			}
		}
	}
}

// The User-Agent has to agree with the rest of the headers, which all describe
// a browser, and with the handshake CPA performs for this host.
func TestUserAgentAgreesWithTheBrowserItClaims(t *testing.T) {
	headers := authHeaders(credential{AccessToken: "t", AccountID: "a", AuthMode: "chatgpt"}, true)
	ua := headers.Get("User-Agent")
	if runtime := headers.Get("X-Stainless-Runtime"); runtime != "browser:chrome" {
		t.Fatalf("X-Stainless-Runtime = %q; this test assumes the headers claim Chrome", runtime)
	}
	if !strings.HasPrefix(ua, "Mozilla/5.0 (") || !strings.Contains(ua, "Safari/537.36") {
		t.Errorf("User-Agent = %q, want a Chrome UA", ua)
	}
	major := regexp.MustCompile(`Chrome/(\d+)\.`).FindStringSubmatch(ua)
	if major == nil {
		t.Fatalf("User-Agent = %q, no Chrome version in it", ua)
	}
	// utls HelloChrome_Auto is HelloChrome_133 in v1.8.2; a UA claiming another
	// Chrome would just move the contradiction from the headers to the TLS.
	if major[1] != "133" {
		t.Errorf("User-Agent claims Chrome %s but the handshake emulates Chrome 133", major[1])
	}
	// The platform fields say PC, so the UA has to say Windows.
	if headers.Get("X-OpenAI-Internal-Basispoints-Office-Platform") == "PC" && !strings.Contains(ua, "Windows NT") {
		t.Errorf("headers say PC but User-Agent is %q", ua)
	}
}

// Token refresh and the usage lookup carry the same credentials, so they must
// not be the ones still announcing a Go client.
func TestRefreshAndQuotaSendTheSameUserAgent(t *testing.T) {
	for _, tc := range []struct {
		name   string
		invoke func(*Service) error
	}{
		{"token refresh", func(s *Service) error {
			raw, _ := json.Marshal(authRefreshRequest{
				AuthID:      "bp-test",
				StorageJSON: []byte(`{"refresh_token":"rt.1.fake","email":"t@example.test","access_token":"eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiJmaXh0dXJlLXVzZXIiLCJjbGllbnRfaWQiOiJhcHBfRU1vYW1FRVo3M2YwQ2tYYVhwN2hyYW5uIiwiZXhwIjo0MTAyNDQ0ODAwLCJ2ZXJzaW9uIjoxLCJodHRwczovL2FwaS5vcGVuYWkuY29tL2F1dGgiOnsiY2hhdGdwdF9hY2NvdW50X2lkIjoiYWNjdCIsInVzZXJfaWQiOiJmaXh0dXJlLXVzZXIiLCJjaGF0Z3B0X3VzZXJfaWQiOiJmaXh0dXJlLXVzZXIiLCJjaGF0Z3B0X3BsYW5fdHlwZSI6InRlYW0ifX0.fixture-signature"}`),
			})
			_, err := s.authRefresh(raw)
			return err
		}},
		{"usage lookup", func(s *Service) error {
			_, err := s.fetchUsage(credential{AccessToken: "t", AccountID: "a"})
			return err
		}},
	} {
		service := NewService()
		var seen string
		service.SetHost(func(method string, payload any, out any) error {
			if method != "host.http.do" {
				return nil
			}
			request, _ := payload.(map[string]any)
			if headers, ok := request["headers"].(map[string][]string); ok && len(headers["User-Agent"]) > 0 {
				seen = headers["User-Agent"][0]
			}
			if response, ok := out.(*upstreamResponse); ok {
				response.StatusCode = 500
				response.Body = []byte(`{}`)
			}
			return nil
		})
		_ = tc.invoke(service)
		if seen != chromeUserAgent {
			t.Errorf("%s sent User-Agent %q, want the same one as the responses path", tc.name, seen)
		}
	}
}

// The headers must stay byte-identical apart from the credential, so the UA
// change does not accidentally become a per-request variable.
func TestHeadersDifferOnlyByCredential(t *testing.T) {
	a := authHeaders(credential{AccessToken: "token-a", AccountID: "acct-a", AuthMode: "chatgpt"}, true)
	b := authHeaders(credential{AccessToken: "token-b", AccountID: "acct-b", AuthMode: "chatgpt"}, true)
	for name := range a {
		switch name {
		// Keys as authHeaders spells them: it builds the map literally, so Go
		// never canonicalises them.
		case "Authorization", "ChatGPT-Account-ID", "X-OpenAI-Account-ID":
			continue
		}
		if !jsonEqual(a[name], b[name]) {
			t.Errorf("header %s varies between credentials: %v vs %v", name, a[name], b[name])
		}
	}
}

func jsonEqual(a, b any) bool {
	left, _ := json.Marshal(a)
	right, _ := json.Marshal(b)
	return string(left) == string(right)
}
