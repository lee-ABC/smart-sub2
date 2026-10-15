package basispoints

import (
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// Basis Points now and then answers a request by re-routing the model to an
// internal variant it then cannot serve, and refuses the whole call with a
// 404:
//
//	The model `gpt-6-sol-degrade2-luna-1p-codexswic-ev3` does not exist or
//	you do not have access to it.
//
// Measured on 2026-09-30 against 22 freshly imported credentials: 12 of 330
// probes (3.6%), spread over seven credentials and every model, and each pair
// that failed also answered normally on another try. CPA, however, reads a 404
// as "this credential does not serve this model" and suspends the model on it
// for 12 hours, so every stray re-route took a credential out of a model for
// half a day -- 55 suspensions within half an hour, gpt-6-sol among them on 20
// of the 22 new credentials.
//
// The re-route is recognised by the model the refusal names: the one that was
// requested, with a suffix appended. A model that genuinely does not exist is
// named as requested and keeps its ordinary handling.

// modelRerouteMarker replaces error.code on a re-route that outlived its
// retries; the request-scoped rule published with every credential matches it.
const modelRerouteMarker = "basispoints_model_rerouted"

// The re-route is decided per request, so the same credential is retried at
// once; the pause only keeps a burst from hammering the upstream.
const modelRerouteBackoff = 100 * time.Millisecond

var modelNotFoundPattern = regexp.MustCompile("The model `([^`]+)` does not exist")

// reroutedModel returns the internal model name when err is Basis Points
// refusing a model it substituted for the one requested.
func reroutedModel(err error, requested string) (string, bool) {
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr == nil || apiErr.Status != http.StatusNotFound {
		return "", false
	}
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return "", false
	}
	match := modelNotFoundPattern.FindStringSubmatch(apiErr.Message)
	if match == nil || !strings.HasPrefix(match[1], requested+"-") {
		return "", false
	}
	return match[1], true
}

// markModelReroute tags a re-route refusal so CPA can tell it apart from a
// model the credential really lacks. Anything else is returned untouched.
func markModelReroute(err error, requested string) error {
	if _, rerouted := reroutedModel(err, requested); !rerouted {
		return err
	}
	var apiErr *APIError
	errors.As(err, &apiErr)
	message := apiErr.Message
	var envelope map[string]any
	if json.Unmarshal([]byte(message), &envelope) == nil {
		if inner, ok := envelope["error"].(map[string]any); ok {
			inner["code"] = modelRerouteMarker
			if raw, marshalErr := json.Marshal(envelope); marshalErr == nil {
				message = string(raw)
			}
		}
	}
	if !strings.Contains(message, modelRerouteMarker) {
		message = modelRerouteMarker + ": " + message
	}
	marked := *apiErr
	marked.Message = message
	return &marked
}

// modelRerouteRequestScopedRule lets CPA move a request whose re-route kept
// failing on to the next credential without suspending the model on this one.
// A 404 without the marker keeps CPA's ordinary 12-hour suspension.
func modelRerouteRequestScopedRule() map[string]any {
	return map[string]any{
		"status": http.StatusNotFound,
		"match":  []string{modelRerouteMarker},
		"action": "continue",
	}
}
