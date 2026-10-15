package adapter

import (
	"encoding/json"
	"log"
	"regexp"
	"strconv"
	"time"
)

var identityUUID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// SetIdentityLogger must be called before serving. Only the fixed, typed
// projection below is logged: never headers, tokens, cookies, prompts or tools.
func (a *Adapter) SetIdentityLogger(logger *log.Logger) { a.identityLogger = logger }

type identityLogRecord struct {
	Event             string `json:"event"`
	Time              string `json:"time"`
	RequestID         string `json:"request_id"`
	AccountID         int64  `json:"account_id"`
	UpstreamAttempt   uint32 `json:"upstream_attempt"`
	ConfiguredMode    string `json:"configured_identity_mode"`
	UpstreamRequestID string `json:"upstream_request_id,omitempty"`
	ReasoningEffort   string `json:"reasoning_effort,omitempty"`
	RequestedModel    string `json:"requested_model,omitempty"`
	UpstreamModel     string `json:"upstream_model,omitempty"`
	TaskID            string `json:"task_id,omitempty"`
	TurnID            string `json:"turn_id,omitempty"`
	AgentIteration    int    `json:"agent_iteration,omitempty"`
	Stream            bool   `json:"stream"`
	Transport         string `json:"transport"`
	HTTPStatus        int    `json:"http_status,omitempty"`
	HeaderLatencyMS   int64  `json:"header_latency_ms,omitempty"`
}

func (a *Adapter) identityTrace(call *invocation, callback string, raw []byte) identityLogRecord {
	record := identityLogRecord{RequestID: callback, AccountID: call.accountID, UpstreamAttempt: call.upstreamAttempts.Add(1), ConfiguredMode: a.identityMode, RequestedModel: call.requestedModel, Transport: "https_sse"}
	var body struct {
		Model           string `json:"model"`
		ReasoningEffort string `json:"reasoning_effort"`
		Stream          bool   `json:"stream"`
		Metadata        struct {
			Task      string `json:"task_id"`
			Turn      string `json:"turn_id"`
			Iteration string `json:"agent_iteration"`
		} `json:"metadata"`
	}
	if json.Unmarshal(raw, &body) != nil {
		return record
	}
	for _, model := range models {
		if body.Model == model {
			record.UpstreamModel = body.Model
			break
		}
	}
	switch body.ReasoningEffort {
	case "none", "minimal", "low", "medium", "high", "xhigh":
		record.ReasoningEffort = body.ReasoningEffort
	}
	record.Stream = body.Stream
	if identityUUID.MatchString(body.Metadata.Task) {
		record.TaskID = body.Metadata.Task
	}
	if identityUUID.MatchString(body.Metadata.Turn) {
		record.TurnID = body.Metadata.Turn
	}
	if iteration, err := strconv.Atoi(body.Metadata.Iteration); err == nil && iteration > 0 && iteration <= 1000000 {
		record.AgentIteration = iteration
	}
	return record
}
func (a *Adapter) logIdentity(record identityLogRecord, event string, status int, latency int64) {
	if a.identityLogger == nil {
		return
	}
	record.Event = event
	record.Time = time.Now().UTC().Format(time.RFC3339Nano)
	record.HTTPStatus = status
	record.HeaderLatencyMS = latency
	encoded, err := json.Marshal(record)
	if err == nil {
		a.identityLogger.Print(string(encoded))
	}
}
