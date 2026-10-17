package basispoints

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// Ask one credential, right now, whether the upstream still answers it with the
// model it was asked for.
//
// The ledger answers this too, but only after the fact: a credential has to
// serve real traffic before a substitution shows up in its counters, and by
// then real users have had the cheap model. This route answers it before any
// traffic is routed there -- after an import, after a credential has been
// disabled and is being considered again, or simply on suspicion.
//
// It is deliberately a button and not a timer. A background sweep would spend
// quota on every credential forever to learn something the ledger already
// reports for the ones that are busy; what it cannot do is ask about a
// credential nobody is using, and that is the only question left.
//
// Why it goes through upstreamRequest rather than building its own call
// ---------------------------------------------------------------------
// A probe that reaches the upstream by a different route than the relay would
// measure a different thing, and worse, would *be* a different thing: a second
// client shape on a token whose every other request is an Excel add-in. That is
// precisely the defect this session removed from the control panel's quota
// card. So the probe builds the same body the relay builds, resolves the same
// seat, and hands it to the same transport. The seat resolves identically
// because personaKey falls through to credentialID(file_name) -- the ledger's
// own keys are that derivation, which is how we know nothing else is in play.
//
// In thread mode the probe therefore carries the seat's constant task_id: it
// looks like one more turn in the same unending conversation, which is what any
// other request on that credential looks like.

const probePrompt = "Reply with the single word OK."

// probeModels is an allow-list. The route is unauthenticated (resource routes
// are), so without one, anything reachable could spend this pool's quota on an
// arbitrary model string.
var probeModels = map[string]bool{
	"gpt-6-astra":   true,
	"gpt-6-sol":     true,
	"gpt-6.1-sol":   true,
	"gpt-5.6-sol":   true,
	"gpt-5.6-terra": true,
}

// There is no rate limit here. One was written and then removed at the
// operator's request: they drive this button by hand, one card at a time, and
// wanted to be able to repeat a probe immediately when a result looked wrong.
//
// What still prevents a double click from becoming two calls is on the page --
// the button is disabled while its request is in flight. What is gone is the
// limit on asking again once an answer has come back. Each call spends a little
// of that credential's quota, so the cost of repeating is real but small.

type probeResult struct {
	File      string `json:"file,omitempty"`
	Label     string `json:"label,omitempty"`
	Requested string `json:"requested"`
	Served    string `json:"served,omitempty"`
	Swapped   bool   `json:"swapped"`
	Status    int    `json:"status,omitempty"`
	Error     string `json:"error,omitempty"`
}

func (s *Service) serveProbe(query map[string][]string) map[string]any {
	file := firstQuery(query, "file")
	model := firstQuery(query, "model")
	if model == "" {
		model = "gpt-6-astra"
	}
	if !probeModels[model] {
		return probeJSON(http.StatusBadRequest, probeResult{
			Requested: model, Error: "不认识这个模型，或者它不在允许探测的名单里"})
	}
	if file == "" {
		return probeJSON(http.StatusBadRequest, probeResult{
			Requested: model, Error: "要指定 file"})
	}

	authIndex, name, label, findErr := s.findCredentialFile(file, "")
	if findErr != nil {
		return probeJSON(http.StatusNotFound, probeResult{
			File: file, Requested: model, Error: safeError(findErr)})
	}
	var record struct {
		JSON json.RawMessage `json:"json"`
	}
	if err := s.call("host.auth.get", map[string]any{"auth_index": authIndex}, &record); err != nil {
		return probeJSON(http.StatusBadGateway, probeResult{
			File: name, Label: label, Requested: model,
			Error: "读取凭据失败：" + safeError(err)})
	}
	c, parseErr := parseCredential(record.JSON)
	if parseErr != nil {
		return probeJSON(http.StatusBadGateway, probeResult{
			File: name, Label: label, Requested: model, Error: "凭据无法解析"})
	}

	request := ExecutorRequest{
		Model: model,
		// Only file_name, and no AuthID: that is what the relay's requests
		// carry too, which is why the seat comes out the same. See the ledger
		// keys, which are this same derivation.
		AuthMetadata: map[string]any{"file_name": name},
	}
	body := s.probeBody(request, c, model)

	response, err := s.upstreamRequest(request, body, c, false)
	result := probeResult{File: name, Label: label, Requested: model, Status: response.StatusCode}
	if err != nil {
		result.Error = safeError(err)
		return probeJSON(http.StatusOK, result)
	}

	served := modelFromAnswer(response.Body, model)
	if served == "" {
		result.Error = "上游的回答里没有模型名"
		return probeJSON(http.StatusOK, result)
	}
	result.Served = served
	result.Swapped = normalizePriceModel(served) != normalizePriceModel(model)
	return probeJSON(http.StatusOK, result)
}

// probeBody builds the same shape the relay sends, down to the key set the real
// add-in uses. A probe that looked different would answer a question about a
// client that does not exist.
func (s *Service) probeBody(request ExecutorRequest, c credential, model string) map[string]any {
	cfg := s.config()
	who, mode := s.personaFor(request, c)
	metadata := map[string]any{}
	// The turn fingerprint has to differ per probe or two probes would share a
	// turn_id. The conversation does not: in thread mode the seat has exactly
	// one, and a probe is one more turn in it.
	fingerprint := "probe/" + model + "/" + time.Now().UTC().Format(time.RFC3339Nano)
	applyConvergedIdentity(map[string]any{}, metadata, mode, who, "", "probe", fingerprint, "1")
	return map[string]any{
		"model": model,
		"input": []any{map[string]any{
			"type": "message", "role": "user",
			"content": []any{map[string]any{"type": "input_text", "text": probePrompt}},
		}},
		"stream":             false,
		"store":              false,
		"context_management": contextManagement(map[string]any{}, cfg),
		"metadata":           metadata,
		"reasoning_effort":   "low",
		"model_selection":    "explicit",
	}
}

func probeJSON(status int, result probeResult) map[string]any {
	body, err := json.Marshal(result)
	if err != nil {
		body = []byte(`{"error":"结果无法序列化"}`)
		status = http.StatusInternalServerError
	}
	return map[string]any{
		"StatusCode": status,
		"Headers": map[string][]string{
			"Content-Type":  {"application/json; charset=utf-8"},
			"Cache-Control": {"no-store"},
		},
		"Body": body,
	}
}

// modelFromAnswer reads which model actually answered.
//
// Basis Points replies with an event stream even when the request sets
// stream:false -- the relay already knows this (see the comment above
// parseFinalStreamResponse in service.go), and the first version of this probe
// read the body as JSON anyway and failed on every call. The deployment's own
// verification caught it, which is the whole reason that check exists.
//
// The first event already names the model, so this scans rather than waiting
// for the completed response: a stream that fails halfway still answers the
// question being asked, and that is exactly the kind of credential worth
// asking about. A bare JSON object is still accepted, in case the endpoint
// ever honours stream:false.
func modelFromAnswer(body []byte, requested string) string {
	if final, err := parseFinalStreamResponse(body); err == nil && final != nil {
		if name := servedModel(final, requested); name != "" {
			return name
		}
	}
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			continue
		}
		node := event
		if inner := objectValue(event["response"]); inner != nil {
			node = inner
		}
		if name := strings.TrimSpace(stringValue(node["model"])); name != "" {
			return name
		}
	}
	return ""
}
