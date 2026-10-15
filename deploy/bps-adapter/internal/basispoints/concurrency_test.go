package basispoints

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestConcurrencyGateCapsAndReleasesExactlyOnce(t *testing.T) {
	gate := newConcurrencyGate()
	var releases []func()
	for slot := 1; slot <= 5; slot++ {
		release, admitted := gate.acquire("bp-a", 5)
		if !admitted {
			t.Fatalf("slot %d refused below a cap of 5", slot)
		}
		releases = append(releases, release)
	}
	if _, admitted := gate.acquire("bp-a", 5); admitted {
		t.Fatal("a sixth request was admitted over a cap of 5")
	}
	// Each credential keeps its own count.
	if release, admitted := gate.acquire("bp-b", 5); !admitted {
		t.Fatal("a full credential blocked a different one")
	} else {
		release()
	}
	// Every exit path may call release; only the first call may free a slot,
	// or a doubly released request would hand out a slot it no longer holds.
	releases[0]()
	releases[0]()
	if got := gate.current("bp-a"); got != 4 {
		t.Fatalf("after one release, %d in flight, want 4", got)
	}
	if _, admitted := gate.acquire("bp-a", 5); !admitted {
		t.Fatal("the freed slot could not be reused")
	}
	if _, admitted := gate.acquire("bp-a", 5); admitted {
		t.Fatal("releasing twice freed two slots")
	}
}

func TestConcurrencyGateZeroMeansNoCap(t *testing.T) {
	gate := newConcurrencyGate()
	for request := 0; request < 100; request++ {
		if _, admitted := gate.acquire("bp-a", 0); !admitted {
			t.Fatalf("request %d refused with no cap configured", request)
		}
	}
}

func TestConcurrencyGateHoldsItsCapUnderContention(t *testing.T) {
	gate := newConcurrencyGate()
	const limit = 5
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		peak int
	)
	for worker := 0; worker < 300; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, admitted := gate.acquire("bp-a", limit)
			if !admitted {
				return
			}
			defer release()
			if held := gate.current("bp-a"); held > 0 {
				mu.Lock()
				if held > peak {
					peak = held
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if peak > limit {
		t.Fatalf("%d requests in flight at once over a cap of %d", peak, limit)
	}
	if got := gate.current("bp-a"); got != 0 {
		t.Fatalf("%d slots still held after every request finished", got)
	}
}

func TestConcurrencyLimitLookup(t *testing.T) {
	cfg := defaultConfig()
	cfg.CredentialConcurrency = map[string]int{"VIP@Example.com ": 2, "team-file.json": 9}
	if err := cfg.normalize(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		email, file  string
		limit        int
		overriddenBy bool
	}{
		{"vip@example.com", "whatever.json", 2, true},
		{"someone@example.com", "team-file.json", 9, true},
		{"nobody@example.com", "plain.json", 5, false},
	} {
		limit, override := cfg.concurrencyLimit(tc.email, tc.file)
		if limit != tc.limit || override != tc.overriddenBy {
			t.Fatalf("%s/%s -> (%d, %v), want (%d, %v)", tc.email, tc.file, limit, override, tc.limit, tc.overriddenBy)
		}
	}
}

func executorRaw(authID string, stream bool) json.RawMessage {
	request := map[string]any{"model": "gpt-6-astra", "input": "hi"}
	raw, _ := json.Marshal(map[string]any{
		"AuthID":          authID,
		"OriginalRequest": jsonBytes(request),
		"Payload":         jsonBytes(request),
		"StorageJSON":     streamTestCredential(),
		"Model":           "gpt-6-astra",
		"Stream":          stream,
		"stream_id":       "client-1",
	})
	return raw
}

// A credential at its cap must refuse with a 429 carrying the marker, since the
// marker is what the continue rule matches -- without it CPA would read an
// ordinary 429 and cool the credential down.
func TestExecuteRefusesACredentialAtItsCap(t *testing.T) {
	service := NewService()
	for slot := 0; slot < 5; slot++ {
		service.gate.acquire("bp-capped", 5)
	}
	_, err := service.Handle("executor.execute", executorRaw("bp-capped", false))
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 429 {
		t.Fatalf("over-cap request returned %v, want a 429", err)
	}
	if !strings.Contains(err.Error(), concurrencyLimitMarker) {
		t.Fatalf("refusal %q lacks the marker the continue rule matches", err.Error())
	}
	if got := service.gate.current("bp-capped"); got != 5 {
		t.Fatalf("the refusal changed the count to %d", got)
	}
}

func TestNonStreamingReleasesItsSlotOnEveryPath(t *testing.T) {
	service := NewService()
	upstreamStatus := 200
	heldDuringCall := -1
	service.SetHost(func(method string, payload any, out any) error {
		if method != "host.http.do" {
			return nil
		}
		heldDuringCall = service.gate.current("bp-ns")
		response := out.(*upstreamResponse)
		response.StatusCode = upstreamStatus
		if upstreamStatus == 200 {
			response.Body = jsonBytes(map[string]any{"id": "r", "status": "completed", "output": []any{}})
		} else {
			response.Body = []byte(`{"error":{"message":"boom","type":"server_error"}}`)
		}
		return nil
	})

	if _, err := service.Handle("executor.execute", executorRaw("bp-ns", false)); err != nil {
		t.Fatalf("successful call failed: %v", err)
	}
	if heldDuringCall != 1 {
		t.Fatalf("slot count during the upstream call was %d, want 1", heldDuringCall)
	}
	if got := service.gate.current("bp-ns"); got != 0 {
		t.Fatalf("a successful call leaked %d slot(s)", got)
	}

	upstreamStatus = 500
	if _, err := service.Handle("executor.execute", executorRaw("bp-ns", false)); err == nil {
		t.Fatal("failing upstream reported success")
	}
	if got := service.gate.current("bp-ns"); got != 0 {
		t.Fatalf("a failed call leaked %d slot(s)", got)
	}
}

// A stream holds its slot for as long as it runs, not merely until the handler
// returns -- that happens almost at once, while the answer is still streaming.
func TestStreamingHoldsItsSlotUntilTheRelayEnds(t *testing.T) {
	service := NewService()
	chunks := textChunks()
	var (
		mu             sync.Mutex
		reads          int
		heldMidStream  = -1
		streamFinished = make(chan struct{})
	)
	service.SetHost(func(method string, payload any, out any) error {
		switch method {
		case "host.http.do_stream":
			stream := out.(*upstreamStream)
			stream.StatusCode = 200
			stream.StreamID = "upstream-1"
		case "host.http.stream_read":
			mu.Lock()
			defer mu.Unlock()
			if reads == 1 {
				heldMidStream = service.gate.current("bp-stream")
			}
			chunk := out.(*streamChunk)
			if reads < len(chunks) {
				chunk.Payload = []byte(chunks[reads])
				reads++
			} else {
				chunk.Done = true
			}
		case "host.stream.close":
			close(streamFinished)
		}
		return nil
	})
	if _, err := service.Handle("executor.execute_stream", executorRaw("bp-stream", true)); err != nil {
		t.Fatalf("execute_stream: %v", err)
	}
	select {
	case <-streamFinished:
	case <-time.After(5 * time.Second):
		t.Fatal("stream never closed")
	}
	if !service.drainStreams(2 * time.Second) {
		t.Fatal("relay did not finish")
	}
	mu.Lock()
	defer mu.Unlock()
	if heldMidStream != 1 {
		t.Fatalf("slot count mid-stream was %d, want 1", heldMidStream)
	}
	if got := service.gate.current("bp-stream"); got != 0 {
		t.Fatalf("a finished stream leaked %d slot(s)", got)
	}
}

// A stream that fails before the relay starts must give its slot back itself;
// otherwise each upstream rejection would shrink the credential's cap for good.
func TestStreamingReleasesTheSlotWhenItNeverStarts(t *testing.T) {
	service := NewService()
	service.SetHost(func(method string, payload any, out any) error {
		switch method {
		case "host.http.do_stream":
			stream := out.(*upstreamStream)
			stream.StatusCode = 503
			stream.StreamID = "upstream-1"
		case "host.http.stream_read":
			chunk := out.(*streamChunk)
			chunk.Payload = []byte(`{"error":{"message":"down","type":"server_error"}}`)
			chunk.Done = true
		}
		return nil
	})
	if _, err := service.Handle("executor.execute_stream", executorRaw("bp-fail", true)); err == nil {
		t.Fatal("a rejected upstream stream reported success")
	}
	if got := service.gate.current("bp-fail"); got != 0 {
		t.Fatalf("a stream that never started leaked %d slot(s)", got)
	}
}

// The rule is what keeps a full credential from being cooled down. CPA reads it
// by marshalling the metadata into its own RequestScopedErrorRule, so it has to
// survive that round trip with the field names CPA expects.
func TestAuthRecordCarriesTheConcurrencyContinueRule(t *testing.T) {
	request, _ := json.Marshal(authParseRequest{
		Provider: Provider,
		FileName: "basispoints-account.json",
		RawJSON:  streamTestCredential(),
	})
	response, err := NewService().authParse(request)
	if err != nil {
		t.Fatal(err)
	}
	metadata := objectValue(objectValue(response["Auth"])["Metadata"])
	var rules []struct {
		Status int      `json:"status"`
		Match  []string `json:"match"`
		Action string   `json:"action"`
	}
	if err := json.Unmarshal(jsonBytes(metadata["request_scoped_errors"]), &rules); err != nil {
		t.Fatalf("rules do not decode as CPA reads them: %v", err)
	}
	if len(rules) != 2 {
		t.Fatalf("got %d rules, want 2", len(rules))
	}
	rule := rules[0]
	if rule.Status != 429 || rule.Action != "continue" ||
		len(rule.Match) != 1 || rule.Match[0] != concurrencyLimitMarker {
		t.Fatalf("rule = %+v", rule)
	}
	reroute := rules[1]
	if reroute.Status != 404 || reroute.Action != "continue" ||
		len(reroute.Match) != 1 || reroute.Match[0] != modelRerouteMarker {
		t.Fatalf("re-route rule = %+v", reroute)
	}
}

// The caps are edited live in the control panel. Removing an override there, or
// deleting the default to go back to 5, must actually take effect rather than
// being resurrected from the plugin's settings cache.
func TestConfigYAMLIsAuthoritativeForConcurrencyCaps(t *testing.T) {
	t.Chdir(t.TempDir())
	service := NewService()
	reconfigure := func(yaml string) {
		t.Helper()
		raw, _ := json.Marshal(map[string]any{"config_yaml": []byte(yaml)})
		if _, err := service.Handle("plugin.reconfigure", raw); err != nil {
			t.Fatalf("reconfigure: %v", err)
		}
	}

	reconfigure("max_concurrency_per_credential: 3\ncredential_concurrency:\n  vip@example.com: 8\n")
	if limit, _ := service.config().concurrencyLimit("vip@example.com", ""); limit != 8 {
		t.Fatalf("override not applied: %d", limit)
	}
	if limit, _ := service.config().concurrencyLimit("other@example.com", ""); limit != 3 {
		t.Fatalf("default not applied: %d", limit)
	}

	reconfigure("timeout_seconds: 300\n")
	cfg := service.config()
	if len(cfg.CredentialConcurrency) != 0 {
		t.Fatalf("removed override survived: %v", cfg.CredentialConcurrency)
	}
	if cfg.MaxConcurrencyPerCredential != 5 {
		t.Fatalf("deleted default kept %d, want the built-in 5", cfg.MaxConcurrencyPerCredential)
	}
}

func TestConcurrencySnapshotReportsLiveCounts(t *testing.T) {
	service := NewService()
	service.cfg.CredentialConcurrency = map[string]int{"b@example.com": 2}
	service.SetHost(func(method string, payload any, out any) error {
		if method != "host.auth.list" {
			return nil
		}
		return json.Unmarshal([]byte(`{"files":[
			{"name":"a.json","label":"a@example.com","provider":"oai-basispoints"},
			{"name":"b.json","label":"b@example.com","provider":"oai-basispoints"},
			{"name":"c.json","label":"c@example.com","provider":"codex"}]}`), out)
	})
	// The page only knows file names; it must land on the same key the
	// executor uses, which is the auth record id derived from that name.
	service.gate.acquire(credentialID("a.json"), 5)

	snapshot := service.concurrencySnapshot()
	if len(snapshot.Rows) != 2 {
		t.Fatalf("got %d rows, want the 2 Basis Points credentials: %+v", len(snapshot.Rows), snapshot.Rows)
	}
	byFile := map[string]concurrencyRow{}
	for _, row := range snapshot.Rows {
		byFile[row.File] = row
	}
	if a := byFile["a.json"]; a.InFlight != 1 || a.Limit != 5 || a.Override {
		t.Fatalf("a = %+v", a)
	}
	if b := byFile["b.json"]; b.InFlight != 0 || b.Limit != 2 || !b.Override {
		t.Fatalf("b = %+v", b)
	}
	if snapshot.TotalInFlight != 1 || snapshot.TotalLimit != 7 || snapshot.Unlimited {
		t.Fatalf("totals = %+v", snapshot)
	}
}

// Account names reach the page as data inside a script; one must not be able to
// close that script and run its own.
func TestConcurrencyPageCannotBeBrokenOutOfByALabel(t *testing.T) {
	service := NewService()
	service.SetHost(func(method string, payload any, out any) error {
		if method != "host.auth.list" {
			return nil
		}
		return json.Unmarshal(jsonBytes(map[string]any{"files": []any{map[string]any{
			"name": "x.json", "label": "</script><script>alert(1)</script>", "provider": Provider,
		}}}), out)
	})
	page := service.renderConcurrencyPage()
	if strings.Contains(page, "</script><script>alert") {
		t.Fatal("a credential label broke out of the page script")
	}
}
