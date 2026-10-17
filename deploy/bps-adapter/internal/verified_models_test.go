package adapter

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestVerifiedLunaPreservesModelAndReasoning(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, effort := range []string{"low", "medium", "high", "xhigh"} {
			t.Run(fmt.Sprintf("stream=%t/%s", stream, effort), func(t *testing.T) {
				called := false
				a := newTestAdapter(t, func(r *http.Request) (*http.Response, error) {
					called = true
					var wire map[string]any
					if err := json.NewDecoder(r.Body).Decode(&wire); err != nil {
						t.Fatal(err)
					}
					if wire["model"] != "gpt-6-luna" || wire["reasoning_effort"] != effort || wire["model_selection"] != "explicit" {
						t.Fatalf("model/effort changed: model=%v effort=%v selection=%v", wire["model"], wire["reasoning_effort"], wire["model_selection"])
					}
					return mockResponse(200, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":"+finalResponse("gpt-6-luna", "OK")+"}\n\ndata: [DONE]\n\n"), nil
				})
				raw := fmt.Sprintf(`{"model":"gpt-6-luna","input":"hi","stream":%t,"reasoning":{"effort":%q}}`, stream, effort)
				w := httptest.NewRecorder()
				a.ServeHTTP(w, testRequest(raw))
				if !called || w.Code != 200 || !strings.Contains(w.Body.String(), "gpt-6-luna") {
					t.Fatalf("missing exact luna response: called=%t status=%d body=%s", called, w.Code, w.Body.String())
				}
			})
		}
	}
}
