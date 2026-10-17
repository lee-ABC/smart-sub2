package adapter

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestImageRelayBaseValidation(t *testing.T) {
	for _, cfg := range []string{"public_base_url: 123", "public_base_url: /relative", "public_base_url: ftp://example.com", "public_base_url: https://u:p@example.com", "public_base_url: https://example.com/?x=1", "public_base_url: https://example.com/#frag"} {
		a, err := New(testKey, []byte(cfg))
		if err == nil {
			a.Shutdown()
			t.Errorf("accepted %s", cfg)
		}
	}
}

func TestImageRelayEndToEndAndPublicIsolation(t *testing.T) {
	a, err := New(testKey, []byte("public_base_url: https://images.example.test/bps-images"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Shutdown()
	image := []byte("synthetic-image-payload")
	var fetched bool
	a.transport = func(string, string) (http.RoundTripper, error) {
		return roundTripFunc(func(r *http.Request) (*http.Response, error) {
			body, _ := io.ReadAll(r.Body)
			if bytes.Contains(body, []byte("data:image")) {
				t.Fatal("inline bytes leaked upstream")
			}
			var tree any
			if err := json.Unmarshal(body, &tree); err != nil {
				t.Fatal(err)
			}
			var relayURL string
			var walk func(any)
			walk = func(v any) {
				switch x := v.(type) {
				case map[string]any:
					for _, v := range x {
						walk(v)
					}
				case []any:
					for _, v := range x {
						walk(v)
					}
				case string:
					if strings.HasPrefix(x, "https://images.example.test/bps-images/image?t=") {
						relayURL = x
					}
				}
			}
			walk(tree)
			if relayURL == "" {
				t.Fatalf("no rewritten image URL: %s", body)
			}
			for _, method := range []string{"HEAD", "GET"} {
				w := httptest.NewRecorder()
				a.ImageHandler().ServeHTTP(w, httptest.NewRequest(method, relayURL, nil))
				if w.Code != 200 || w.Header().Get("Content-Type") != "image/png" {
					t.Fatalf("image fetch: %d %s", w.Code, w.Body.String())
				}
				if method == "GET" && !bytes.Equal(w.Body.Bytes(), image) {
					t.Fatal("image bytes changed")
				}
				if method == "HEAD" && w.Body.Len() != 0 {
					t.Fatal("HEAD returned body")
				}
			}
			fetched = true
			return mockResponse(200, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":"+finalResponse("gpt-6-astra", "image OK")+"}\n\ndata: [DONE]\n\n"), nil
		}), nil
	}
	w := httptest.NewRecorder()
	a.ServeHTTP(w, testRequest(`{"model":"gpt-6-astra","input":[{"role":"user","content":[{"type":"input_text","text":"describe"},{"type":"input_image","image_url":"data:image/png;base64,c3ludGhldGljLWltYWdlLXBheWxvYWQ="}]}]}`))
	if w.Code != 200 || !fetched {
		t.Fatalf("request failed: %d %s", w.Code, w.Body.String())
	}
	for _, path := range []string{"/internal/v1/responses", "/quota", "/healthz", "/bps-images/image?t=bad", "/bps-images/image?t=" + strings.Repeat("a", 64), "/bps-images/image?t=" + strings.Repeat("a", 64) + "&t=" + strings.Repeat("b", 64)} {
		w := httptest.NewRecorder()
		a.ImageHandler().ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 404 {
			t.Errorf("exposed %s: %d", path, w.Code)
		}
	}
	w = httptest.NewRecorder()
	a.ImageHandler().ServeHTTP(w, httptest.NewRequest("POST", "/bps-images/image", nil))
	if w.Code != 405 {
		t.Fatalf("POST status=%d", w.Code)
	}
}

func TestImageRelayDisabledByDefault(t *testing.T) {
	a := newTestAdapter(t, func(*http.Request) (*http.Response, error) {
		t.Fatal("disabled image reached upstream")
		return nil, nil
	})
	w := httptest.NewRecorder()
	a.ImageHandler().ServeHTTP(w, httptest.NewRequest("GET", "/image?t="+strings.Repeat("a", 64), nil))
	if w.Code != 404 {
		t.Fatal(w.Code)
	}
	w = httptest.NewRecorder()
	a.ServeHTTP(w, testRequest(`{"model":"gpt-6-astra","input":[{"role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,aGk="}]}]}`))
	if w.Code != 400 || !strings.Contains(w.Body.String(), "image_relay_disabled") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}
