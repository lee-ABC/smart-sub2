package basispoints

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The refusal exactly as Basis Points sent it on 2026-09-30.
func rerouteRefusal(internal string) string {
	return `{"error":{"message":"The model ` + "`" + internal + "`" +
		` does not exist or you do not have access to it.","type":"invalid_request_error","param":null,"code":"model_not_found"}}`
}

func TestReroutedModelRecognisesTheSubstitution(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		requested string
		want      bool
	}{
		{"6-series degrade", fail(404, "upstream_error", rerouteRefusal("gpt-6-sol-degrade2-luna-1p-codexswic-ev3")), "gpt-6-sol", true},
		{"astra degrade", fail(404, "upstream_error", rerouteRefusal("gpt-6-astra-degrade2-luna-1p-codexswic-ev3")), "gpt-6-astra", true},
		{"5.6 abuse", fail(404, "upstream_error", rerouteRefusal("gpt-5.6-sol-codex-abuse-1p-codexswic-ev3")), "gpt-5.6-sol", true},
		{"a model that really does not exist", fail(404, "upstream_error", rerouteRefusal("gpt-6.1-sol")), "gpt-6.1-sol", false},
		{"another model named", fail(404, "upstream_error", rerouteRefusal("gpt-5.4-mini")), "gpt-6-sol", false},
		{"prefix without a dash", fail(404, "upstream_error", rerouteRefusal("gpt-6-solar")), "gpt-6-sol", false},
		{"not a 404", fail(403, "upstream_error", rerouteRefusal("gpt-6-sol-degrade2-luna-1p-codexswic-ev3")), "gpt-6-sol", false},
		{"404 about something else", fail(404, "upstream_error", `{"error":{"message":"Not found"}}`), "gpt-6-sol", false},
		{"model unknown to the caller", fail(404, "upstream_error", rerouteRefusal("gpt-6-sol-degrade2-luna-1p-codexswic-ev3")), "", false},
		{"not an APIError", errors.New(rerouteRefusal("gpt-6-sol-degrade2-luna-1p-codexswic-ev3")), "gpt-6-sol", false},
	}
	for _, tc := range cases {
		if _, got := reroutedModel(tc.err, tc.requested); got != tc.want {
			t.Errorf("%s: rerouted = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestMarkedRerouteKeepsTheUpstreamStory(t *testing.T) {
	original := fail(404, "upstream_error", rerouteRefusal("gpt-6-sol-degrade2-luna-1p-codexswic-ev3"))
	marked := markModelReroute(original, "gpt-6-sol")
	var apiErr *APIError
	if !errors.As(marked, &apiErr) || apiErr.Status != 404 {
		t.Fatalf("marked = %#v, want a 404 APIError", marked)
	}
	var envelope map[string]map[string]any
	if err := json.Unmarshal([]byte(apiErr.Message), &envelope); err != nil {
		t.Fatalf("marked message is no longer JSON: %v", err)
	}
	if code := envelope["error"]["code"]; code != modelRerouteMarker {
		t.Errorf("error.code = %v, want %s", code, modelRerouteMarker)
	}
	if message, _ := envelope["error"]["message"].(string); !strings.Contains(message, "gpt-6-sol-degrade2-luna-1p-codexswic-ev3") {
		t.Errorf("error.message lost the internal model name: %q", message)
	}
	if original.(*APIError).Message == apiErr.Message {
		t.Error("the caller's error was modified in place")
	}
	genuine := fail(404, "upstream_error", rerouteRefusal("gpt-6.1-sol"))
	if markModelReroute(genuine, "gpt-6.1-sol") != genuine {
		t.Error("a genuine not-found was rewritten")
	}
}

func TestUpstreamRetryGetsPastAReroute(t *testing.T) {
	calls := 0
	got, err := withUpstreamRetry("gpt-6-sol", func() (string, error) {
		calls++
		if calls < 3 {
			return "", fail(404, "upstream_error", rerouteRefusal("gpt-6-sol-degrade2-luna-1p-codexswic-ev3"))
		}
		return "answer", nil
	})
	if err != nil || got != "answer" || calls != 3 {
		t.Fatalf("got %q, %v after %d calls; want the answer on the third", got, err, calls)
	}
}

func TestUpstreamRetryMarksAPersistentReroute(t *testing.T) {
	calls := 0
	_, err := withUpstreamRetry("gpt-5.6-sol", func() (string, error) {
		calls++
		return "", fail(404, "upstream_error", rerouteRefusal("gpt-5.6-sol-codex-abuse-1p-codexswic-ev3"))
	})
	if calls != 3 {
		t.Fatalf("attempted %d times, want 3", calls)
	}
	if err == nil || !strings.Contains(err.Error(), modelRerouteMarker) {
		t.Fatalf("err = %v, want it marked %s", err, modelRerouteMarker)
	}
}

func TestUpstreamRetryLeavesOtherFailuresAlone(t *testing.T) {
	for _, failure := range []error{
		fail(404, "upstream_error", rerouteRefusal("gpt-6.1-sol")),
		fail(403, "upstream_error", `{"error":{"message":"This request was blocked by our usage policy."}}`),
		fail(429, "upstream_error", `{"error":{"message":"The usage limit has been reached","type":"usage_limit_reached"}}`),
	} {
		calls := 0
		_, err := withUpstreamRetry("gpt-6.1-sol", func() (string, error) { calls++; return "", failure })
		if calls != 1 || err != failure {
			t.Errorf("%v: %d calls, err %v; want one call and the error untouched", failure, calls, err)
		}
	}
	// The org-rate-limit entry point knows no model and so never treats a 404
	// as a re-route.
	calls := 0
	_, _ = withOrgRateLimitRetry(func() (string, error) {
		calls++
		return "", fail(404, "upstream_error", rerouteRefusal("gpt-6-sol-degrade2-luna-1p-codexswic-ev3"))
	})
	if calls != 1 {
		t.Errorf("withOrgRateLimitRetry retried a 404 %d times", calls)
	}
}

// cpaRequestScopedAction mirrors CPA v7.3.16 matchRequestScopedErrorAction:
// same status, and any match string contained in the error body.
func cpaRequestScopedAction(rules []map[string]any, status int, body string) string {
	for _, rule := range rules {
		ruleStatus, _ := rule["status"].(float64)
		if int(ruleStatus) != status {
			continue
		}
		matches, _ := rule["match"].([]any)
		for _, match := range matches {
			if text, _ := match.(string); text != "" && strings.Contains(body, text) {
				action, _ := rule["action"].(string)
				return action
			}
		}
	}
	return ""
}

func publishedRules(t *testing.T) []map[string]any {
	t.Helper()
	request, _ := json.Marshal(authParseRequest{
		Provider: Provider, FileName: "basispoints-account.json", RawJSON: streamTestCredential(),
	})
	response, err := NewService().authParse(request)
	if err != nil {
		t.Fatal(err)
	}
	metadata := objectValue(objectValue(response["Auth"])["Metadata"])
	var rules []map[string]any
	if err := json.Unmarshal(jsonBytes(metadata["request_scoped_errors"]), &rules); err != nil {
		t.Fatalf("rules do not decode: %v", err)
	}
	return rules
}

// What CPA will do with each outcome, judged by the rules this build publishes.
func TestCPAMovesOnFromAMarkedRerouteWithoutSuspending(t *testing.T) {
	rules := publishedRules(t)
	marked := markModelReroute(fail(404, "upstream_error", rerouteRefusal("gpt-6-sol-degrade2-luna-1p-codexswic-ev3")), "gpt-6-sol")
	if action := cpaRequestScopedAction(rules, 404, marked.Error()); action != "continue" {
		t.Errorf("marked re-route -> %q, want continue (next credential, no suspension)", action)
	}
	genuine := fail(404, "upstream_error", rerouteRefusal("gpt-6.1-sol"))
	if action := cpaRequestScopedAction(rules, 404, genuine.Error()); action != "" {
		t.Errorf("genuine not-found -> %q, want no rule (CPA keeps its suspension)", action)
	}
	capped := concurrencyLimitMarker + ": this credential is already serving 3 concurrent requests"
	if action := cpaRequestScopedAction(rules, 429, capped); action != "continue" {
		t.Errorf("concurrency cap -> %q, want continue", action)
	}
}

// rerouteHost fakes the host for streaming requests: the first refusals
// streams are refused with body, the rest answer with chunks.
type rerouteHost struct {
	mu        sync.Mutex
	refusals  int
	body      string
	chunks    []string
	doStreams int
	reads     map[string]int
	closeErr  string
	closed    chan struct{}
}

func newRerouteHost(refusals int, body string) *rerouteHost {
	return &rerouteHost{refusals: refusals, body: body, chunks: textChunks(),
		reads: map[string]int{}, closed: make(chan struct{})}
}

func (h *rerouteHost) handle(method string, payload any, out any) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	request, _ := payload.(map[string]any)
	switch method {
	case "host.http.do_stream":
		h.doStreams++
		stream := out.(*upstreamStream)
		stream.StreamID = "up-" + strconv.Itoa(h.doStreams)
		if h.doStreams <= h.refusals {
			stream.StatusCode = 404
		} else {
			stream.StatusCode = 200
		}
	case "host.http.stream_read":
		id, _ := request["stream_id"].(string)
		chunk := out.(*streamChunk)
		n, _ := strconv.Atoi(strings.TrimPrefix(id, "up-"))
		if n <= h.refusals {
			if h.reads[id] == 0 {
				chunk.Payload = []byte(h.body)
				h.reads[id]++
				return nil
			}
			chunk.Done = true
			return nil
		}
		if h.reads[id] < len(h.chunks) {
			chunk.Payload = []byte(h.chunks[h.reads[id]])
			h.reads[id]++
			return nil
		}
		chunk.Done = true
	case "host.stream.close":
		if message, ok := request["error"].(string); ok {
			h.closeErr = message
		}
		close(h.closed)
	}
	return nil
}

func TestStreamSurvivesARerouteOnTheSameCredential(t *testing.T) {
	service := NewService()
	host := newRerouteHost(1, rerouteRefusal("gpt-6-astra-degrade2-luna-1p-codexswic-ev3"))
	service.SetHost(host.handle)
	if _, err := service.Handle("executor.execute_stream", executorRaw("bp-reroute", true)); err != nil {
		t.Fatalf("execute_stream after one re-route: %v", err)
	}
	select {
	case <-host.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("stream never closed")
	}
	service.drainStreams(2 * time.Second)
	host.mu.Lock()
	defer host.mu.Unlock()
	if host.doStreams != 2 {
		t.Errorf("opened %d upstream streams, want 2 (one refused, one answered)", host.doStreams)
	}
	if host.closeErr != "" {
		t.Errorf("stream closed with error %q", host.closeErr)
	}
	if got := service.gate.current("bp-reroute"); got != 0 {
		t.Errorf("leaked %d slot(s)", got)
	}
}

func TestStreamGivesUpOnAPersistentRerouteMarked(t *testing.T) {
	service := NewService()
	host := newRerouteHost(99, rerouteRefusal("gpt-6-astra-degrade2-luna-1p-codexswic-ev3"))
	service.SetHost(host.handle)
	_, err := service.Handle("executor.execute_stream", executorRaw("bp-reroute-hard", true))
	if err == nil || !strings.Contains(err.Error(), modelRerouteMarker) {
		t.Fatalf("err = %v, want a marked re-route", err)
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	if host.doStreams != 3 {
		t.Errorf("opened %d upstream streams, want 3", host.doStreams)
	}
	if got := service.gate.current("bp-reroute-hard"); got != 0 {
		t.Errorf("leaked %d slot(s)", got)
	}
}

func TestStreamDoesNotRetryAGenuineNotFound(t *testing.T) {
	service := NewService()
	host := newRerouteHost(99, rerouteRefusal("gpt-6-astra"))
	service.SetHost(host.handle)
	_, err := service.Handle("executor.execute_stream", executorRaw("bp-notfound", true))
	if err == nil || strings.Contains(err.Error(), modelRerouteMarker) {
		t.Fatalf("err = %v, want the plain not-found", err)
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	if host.doStreams != 1 {
		t.Errorf("opened %d upstream streams for a genuine not-found, want 1", host.doStreams)
	}
}

// TestReplayProductionFailureLines runs every "upstream execution failed" line
// CPA logged (status<TAB>model<TAB>err, exported to REPLAY_ERRORS) through the
// detector: each refusal naming a -codexswic- variant must be recognised and
// nothing else may be.
func TestReplayProductionFailureLines(t *testing.T) {
	path := os.Getenv("REPLAY_ERRORS")
	if path == "" {
		t.Skip("REPLAY_ERRORS not set")
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1<<20), 1<<24)
	var total, reroutes, caught, falsePositives, missed int
	byStatus := map[int]int{}
	for scanner.Scan() {
		parts := strings.SplitN(scanner.Text(), "\t", 3)
		if len(parts) != 3 {
			continue
		}
		status, _ := strconv.Atoi(parts[0])
		model, message := parts[1], parts[2]
		total++
		byStatus[status]++
		truth := status == 404 && strings.Contains(message, "-codexswic-")
		_, got := reroutedModel(fail(status, "upstream_error", message), model)
		switch {
		case truth && got:
			reroutes++
			caught++
		case truth && !got:
			reroutes++
			missed++
		case !truth && got:
			falsePositives++
		}
	}
	t.Logf("%d logged failures %v: %d re-routes, %d recognised, %d missed, %d false positives",
		total, byStatus, reroutes, caught, missed, falsePositives)
	if missed > 0 || falsePositives > 0 {
		t.Errorf("detector disagrees with the log: %d missed, %d false positives", missed, falsePositives)
	}
	if reroutes == 0 {
		t.Error(fmt.Sprintf("no re-route found in %s; wrong export?", path))
	}
}
