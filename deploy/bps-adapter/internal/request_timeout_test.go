package adapter

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRequestTimeoutConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name, config string
		want         time.Duration
		invalid      bool
	}{
		{"default", "", 300 * time.Second, false},
		{"long reasoning", "timeout_seconds: 900\n", 900 * time.Second, false},
		{"minimum", "timeout_seconds: 10\n", 10 * time.Second, false},
		{"maximum", "timeout_seconds: 1800\n", 1800 * time.Second, false},
		{"too small", "timeout_seconds: 9\n", 0, true},
		{"too large", "timeout_seconds: 1801\n", 0, true},
		{"wrong type", "timeout_seconds: bogus\n", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, err := New(testKey, []byte(tc.config))
			if tc.invalid {
				if err == nil {
					a.Shutdown()
					t.Fatal("invalid timeout accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer a.Shutdown()
			if a.requestTimeout != tc.want {
				t.Fatalf("timeout=%v want %v", a.requestTimeout, tc.want)
			}
		})
	}
}

func TestRequestTimeoutReachesHTTPClientAndContext(t *testing.T) {
	a, err := New(testKey, []byte("timeout_seconds: 900\n"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Shutdown()
	a.transport = func(string, string) (http.RoundTripper, error) {
		return roundTripFunc(func(r *http.Request) (*http.Response, error) {
			deadline, ok := r.Context().Deadline()
			if !ok || time.Until(deadline) < 899*time.Second {
				t.Errorf("upstream still has short deadline: %v %v", deadline, ok)
			}
			a.mu.Lock()
			for _, c := range a.calls {
				if c.client.Timeout != 900*time.Second {
					t.Errorf("HTTP timeout=%v", c.client.Timeout)
				}
			}
			a.mu.Unlock()
			return mockResponse(200, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":"+finalResponse("gpt-6-sol", "ok")+"}\n\n"), nil
		}), nil
	}
	w := httptest.NewRecorder()
	a.ServeHTTP(w, testRequest(`{"model":"gpt-6-sol","input":"hi","stream":true}`))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "response.completed") {
		t.Fatalf("response=%d %s", w.Code, w.Body.String())
	}
}

func TestConfiguredNativePromotionAvoidsCodeEnvelope(t *testing.T) {
	a, err := New(testKey, []byte("timeout_seconds: 900\nrelay_native_promotion: on\nrelay_native_promotion_history: h2\n"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Shutdown()
	a.transport = func(string, string) (http.RoundTripper, error) {
		return roundTripFunc(func(r *http.Request) (*http.Response, error) {
			raw, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(raw, []byte(`"type":"additional_tools"`)) {
				t.Errorf("tool catalog not promoted")
			}
			return mockResponse(200, `{"id":"resp_native","status":"completed","output":[{"type":"function_call","id":"fc_native","call_id":"call_native","name":"read_value","arguments":"{\"cell\":\"A1\"}","status":"completed"}]}`), nil
		}), nil
	}
	w := httptest.NewRecorder()
	a.ServeHTTP(w, testRequest(`{"model":"gpt-6-sol","input":"read A1","tools":[{"type":"function","name":"read_value","parameters":{"type":"object","properties":{"cell":{"type":"string"}},"required":["cell"]}}]}`))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "read_value") || strings.Contains(w.Body.String(), "run_officejs") {
		t.Fatalf("native tool roundtrip=%d %s", w.Code, w.Body.String())
	}
}
