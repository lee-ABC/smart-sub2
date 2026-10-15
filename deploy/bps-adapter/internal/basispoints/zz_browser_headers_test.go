package basispoints

import (
	"regexp"
	"strings"
	"testing"
)

// Asking for an uncompressed response is something no browser does, and it was
// only there because setting the header at all turns off Go's transparent
// decompression. Leaving it unset lets the transport handle both.
func TestNoHandWrittenAcceptEncoding(t *testing.T) {
	headers := authHeaders(credential{AccessToken: "t", AccountID: "a", AuthMode: "chatgpt"}, true)
	if got := headers.Get("Accept-Encoding"); got != "" {
		t.Errorf("Accept-Encoding = %q; leave it to the transport so the body is still decompressed for us", got)
	}
}

// Chrome sends these on every secure request; a Chrome User-Agent without them
// contradicts itself.
func TestClientHintsAndFetchMetadataAreSent(t *testing.T) {
	headers := authHeaders(credential{AccessToken: "t", AccountID: "a", AuthMode: "chatgpt"}, true)
	for name, want := range map[string]string{
		"Sec-Ch-Ua-Mobile":   "?0",
		"Sec-Ch-Ua-Platform": `"Windows"`,
		"Sec-Fetch-Dest":     "empty",
		"Sec-Fetch-Mode":     "cors",
		"Sec-Fetch-Site":     "same-origin",
	} {
		if got := headers.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

// The brand list, the User-Agent and the platform have to tell one story, and
// the same one the handshake does.
func TestClientHintsAgreeWithTheUserAgent(t *testing.T) {
	headers := authHeaders(credential{AccessToken: "t", AccountID: "a", AuthMode: "chatgpt"}, true)
	ua, brands := headers.Get("User-Agent"), headers.Get("Sec-Ch-Ua")

	major := regexp.MustCompile(`Chrome/(\d+)\.`).FindStringSubmatch(ua)
	if major == nil {
		t.Fatalf("User-Agent = %q", ua)
	}
	if major[1] != chromeMajorVersion {
		t.Errorf("User-Agent says Chrome %s, the shared constant says %s", major[1], chromeMajorVersion)
	}
	for _, brand := range []string{`"Google Chrome";v="` + chromeMajorVersion + `"`, `"Chromium";v="` + chromeMajorVersion + `"`} {
		if !strings.Contains(brands, brand) {
			t.Errorf("Sec-Ch-Ua = %q, missing %s", brands, brand)
		}
	}
	if strings.Count(brands, ";v=") != 3 {
		t.Errorf("Sec-Ch-Ua = %q, want three brands (two real plus the GREASE entry)", brands)
	}
	// A desktop Windows User-Agent must not be paired with a mobile hint.
	if strings.Contains(ua, "Windows NT") && headers.Get("Sec-Ch-Ua-Platform") != `"Windows"` {
		t.Errorf("User-Agent says Windows but Sec-Ch-Ua-Platform is %q", headers.Get("Sec-Ch-Ua-Platform"))
	}
	if headers.Get("Sec-Ch-Ua-Mobile") != "?0" {
		t.Error("a desktop User-Agent with Sec-Ch-Ua-Mobile other than ?0")
	}
	// Same-origin is only right while Origin is the host being called.
	if origin := headers.Get("Origin"); origin != "https://bps.openai.com" && headers.Get("Sec-Fetch-Site") == "same-origin" {
		t.Errorf("Origin is %q but Sec-Fetch-Site claims same-origin", origin)
	}
}
