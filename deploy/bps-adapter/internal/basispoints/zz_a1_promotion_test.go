package basispoints

// A1 native promotion (a1_promotion.go): catalogue shape against the real
// Lite capture, the not-promotable list, the switches (reset list, fallback,
// freezing), history h1/h2, the pre-stream 400 fallback, the counters, and --
// the hard requirement -- off and shadow leave every S5 turn exactly as h25
// sends and delivers it.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------- helpers

func a1ResetDenyCache() {
	a1DenyCache.Lock()
	a1DenyCache.items = map[string]time.Time{}
	a1DenyCache.order = nil
	a1DenyCache.Unlock()
}

func a1Cfg(mode, history, block string) Config {
	cfg := defaultConfig()
	cfg.RelayNativePromotion, cfg.RelayNativePromotionHistory = mode, history
	if block != "" {
		cfg.RelayNativePromotionBlock = block
	}
	return cfg
}

// a1LarkPatch is apply_patch as Codex declares it: a lark grammar.
func a1LarkPatch() map[string]any {
	return map[string]any{"type": "custom", "name": "apply_patch", "description": "Apply a patch.",
		"format": map[string]any{"type": "grammar", "syntax": "lark", "definition": "start: \"*** Begin Patch\" /(.|\\n)*/ \"*** End Patch\""}}
}

// a1Tools is a relay catalogue that the default block list lets through:
// plain functions, a lark custom tool, a client namespace and a hosted tool.
func a1Tools() []any {
	return []any{
		s5ExecTool(),
		map[string]any{"type": "web_search"},
		a1LarkPatch(),
		map[string]any{"type": "namespace", "name": "mcp__docs", "description": "Docs server.", "tools": []any{
			map[string]any{"type": "function", "name": "lookup", "description": "Look a term up.", "parameters": map[string]any{
				"$schema": "http://json-schema.org/draft-07/schema#", "type": "object",
				"properties": map[string]any{"q": map[string]any{"type": "string", "minLength": 1, "title": "Query"}},
				"required":   []any{"q"}, "additionalProperties": false}},
		}},
		s5ListFiles(),
	}
}

func a1Relay(key string, input ...any) map[string]any {
	return s5RelayWith(key, a1Tools(), input...)
}

// a1MaskRef hides the random bpdiag reference of a close message
// (tool_diagnostics.go): two runs of the same failure differ only there.
var a1BPDiagRef = regexp.MustCompile(`ref=bpdiag_[0-9a-f]+`)

func a1MaskRef(text string) string { return a1BPDiagRef.ReplaceAllString(text, "ref=bpdiag_<random>") }

func a1CounterDelta(before map[string]uint64) map[string]uint64 {
	after := relayCounterSnapshot()
	delta := map[string]uint64{}
	for key, value := range after {
		if value != before[key] {
			delta[key] = value - before[key]
		}
	}
	return delta
}

func a1BodyInput(t *testing.T, body string) []map[string]any {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(body))
	decoder.UseNumber()
	var document map[string]any
	if err := decoder.Decode(&document); err != nil {
		t.Fatalf("upstream body is not JSON: %v", err)
	}
	if _, present := document["tools"]; present {
		t.Fatal("upstream body carries top-level tools")
	}
	raw, _ := document["input"].([]any)
	items := make([]map[string]any, 0, len(raw))
	for _, value := range raw {
		items = append(items, objectValue(value))
	}
	return items
}

func a1FindItem(items []map[string]any, kind string) (int, map[string]any) {
	for index, item := range items {
		if stringValue(item["type"]) == kind {
			return index, item
		}
	}
	return -1, nil
}

// ---------------------------------------------------------------- harness

type a1Reply struct {
	status int
	body   string
	chunks []string
}

type a1Result struct {
	status        int
	kind, message string
	events        []string
	raw           string
	closeErr      string
	payload       string
	bodies        []string
}

type a1Harness struct {
	t        *testing.T
	service  *Service
	mu       sync.Mutex
	reply    func(call int, body string) a1Reply
	during   func(call int)
	bodies   []string
	reads    []string
	emits    []string
	closeMsg string
	closed   chan struct{}
	isClosed bool
	logs     []map[string]any
}

func newA1Harness(t *testing.T, yaml string) *a1Harness {
	t.Helper()
	h := &a1Harness{t: t, service: NewService()}
	h.service.SetHost(h.host)
	config := "data_dir: \"" + filepath.ToSlash(t.TempDir()) + "\"\nquota_sample_seconds: 0\nclient_identity:\n  pepper: \"" + s5Pepper + "\"\n" + yaml
	raw, _ := json.Marshal(map[string]any{"config_yaml": []byte(config)})
	if _, err := h.service.Handle("plugin.register", raw); err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *a1Harness) reconfigure(yaml string) {
	config := "data_dir: \"" + filepath.ToSlash(h.t.TempDir()) + "\"\nquota_sample_seconds: 0\nclient_identity:\n  pepper: \"" + s5Pepper + "\"\n" + yaml
	raw, _ := json.Marshal(map[string]any{"config_yaml": []byte(config)})
	if _, err := h.service.Handle("plugin.reconfigure", raw); err != nil {
		h.t.Fatal(err)
	}
}

func (h *a1Harness) host(method string, payload any, out any) error {
	request, _ := payload.(map[string]any)
	switch method {
	case "host.http.do", "host.http.do_stream":
		if stringValue(request["url"]) != DefaultResponsesURL {
			return errors.New("a1: network disabled")
		}
		body, _ := request["body"].([]byte)
		h.mu.Lock()
		call := len(h.bodies)
		h.bodies = append(h.bodies, string(body))
		reply, during := h.reply, h.during
		h.mu.Unlock()
		if during != nil {
			during(call)
		}
		r := reply(call, string(body))
		if r.status == 0 {
			r.status = 200
		}
		if method == "host.http.do" {
			response := out.(*upstreamResponse)
			response.StatusCode = r.status
			if r.status == 200 {
				response.Headers = http.Header{"Content-Type": []string{"text/event-stream"}}
				response.Body = []byte(strings.Join(r.chunks, ""))
			} else {
				response.Headers = http.Header{"Content-Type": []string{"application/json"}}
				response.Body = []byte(r.body)
			}
			return nil
		}
		stream := out.(*upstreamStream)
		stream.StatusCode = r.status
		stream.StreamID = fmt.Sprintf("up-a1-%d", call)
		h.mu.Lock()
		if r.status == 200 {
			h.reads = append([]string(nil), r.chunks...)
		} else {
			h.reads = []string{r.body}
		}
		h.mu.Unlock()
	case "host.http.stream_read":
		h.mu.Lock()
		defer h.mu.Unlock()
		chunk := out.(*streamChunk)
		if len(h.reads) > 0 {
			chunk.Payload = []byte(h.reads[0])
			h.reads = h.reads[1:]
			return nil
		}
		chunk.Done = true
	case "host.stream.emit":
		h.mu.Lock()
		if data, ok := request["payload"].([]byte); ok {
			h.emits = append(h.emits, string(data))
		}
		h.mu.Unlock()
	case "host.stream.close":
		h.mu.Lock()
		if message, ok := request["error"].(string); ok {
			h.closeMsg = message
		}
		if !h.isClosed && h.closed != nil {
			h.isClosed = true
			close(h.closed)
		}
		h.mu.Unlock()
	case "host.log":
		h.mu.Lock()
		h.logs = append(h.logs, request)
		h.mu.Unlock()
	}
	return nil
}

func (h *a1Harness) run(request map[string]any, stream bool, reply func(call int, body string) a1Reply) a1Result {
	h.t.Helper()
	h.mu.Lock()
	h.reply, h.bodies, h.emits, h.closeMsg = reply, nil, nil, ""
	h.closed, h.isClosed = make(chan struct{}), false
	h.mu.Unlock()
	body := cloneObject(request)
	body["stream"] = stream
	envelope := map[string]any{
		"OriginalRequest": jsonBytes(body), "Payload": jsonBytes(body),
		"StorageJSON": streamTestCredential(), "Model": stringValue(body["model"]), "Stream": stream,
		"AuthID": "cred-a1", "AuthProvider": Provider, "Metadata": map[string]any{"caller_scope": "caller-a1"},
		"AuthMetadata": map[string]any{"file_name": "cred-a1.json"}, "host_callback_id": "cb-a1",
	}
	method := "executor.execute"
	if stream {
		envelope["stream_id"] = "client-a1"
		method = "executor.execute_stream"
	}
	raw, _ := json.Marshal(envelope)
	response, err := h.service.Handle(method, raw)
	var result a1Result
	if err != nil {
		var api *APIError
		if errors.As(err, &api) {
			result.status, result.kind, result.message = api.Status, api.Kind, api.Message
		} else {
			result.kind, result.message = "error", err.Error()
		}
	} else if stream {
		select {
		case <-h.closed:
		case <-time.After(10 * time.Second):
			h.t.Fatal("stream never closed")
		}
	} else if object, ok := response.(map[string]any); ok {
		if payload, ok := object["Payload"].([]byte); ok {
			result.payload = string(payload)
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	result.raw = strings.Join(h.emits, "")
	result.events = s2NormalizeStream(result.raw)
	result.closeErr = h.closeMsg
	result.bodies = append([]string(nil), h.bodies...)
	return result
}

func a1Always(reply a1Reply) func(int, string) a1Reply {
	return func(int, string) a1Reply { return reply }
}

func a1Text(id, text string) a1Reply {
	return a1Reply{chunks: s5Stream(id).text(text).completed()}
}

// ---------------------------------------------------------------- catalogue

// tool_spec.rs:95-142: flat function and custom tools go into a "functions"
// namespace (empty description) at the place of the first of them, a client
// namespace named "functions" merges into it, other namespaces keep their
// place; every function is {type, name, description, strict, parameters}
// and every custom tool {type, name, description, format}. Hosted tools are
// dropped and counted.
func TestA1CatalogueFollowsCodexLiteRules(t *testing.T) {
	ns := func(name string, tools ...any) map[string]any {
		return map[string]any{"type": "namespace", "name": name, "description": "NS " + name, "tools": tools}
	}
	fn := func(name string) map[string]any {
		return map[string]any{"type": "function", "name": name, "description": "Fn " + name,
			"parameters": map[string]any{"type": "object", "properties": map[string]any{"x": map[string]any{"type": "string"}}}}
	}
	result := buildA1Catalogue([]any{
		ns("alpha", fn("a1")), fn("flat1"), map[string]any{"type": "image_generation"}, ns("beta", fn("b1"), map[string]any{"type": "web_search"}),
		a1LarkPatch(), map[string]any{"type": "namespace", "name": "functions", "description": "Own words.", "tools": []any{fn("merged")}},
		ns("alpha", fn("a2")), map[string]any{"type": "function", "name": "", "description": "nameless"}, "not an object",
	})
	if result.reason != "" {
		t.Fatalf("reason %q", result.reason)
	}
	var shape []string
	for _, raw := range result.tools {
		group := objectValue(raw)
		var members []string
		for _, member := range group["tools"].([]any) {
			members = append(members, stringValue(objectValue(member)["name"]))
		}
		shape = append(shape, fmt.Sprintf("%s(%q)[%s]", group["name"], group["description"], strings.Join(members, ",")))
	}
	want := `alpha("NS alpha")[a1,a2] functions("Own words.")[flat1,apply_patch,merged] beta("NS beta")[b1]`
	if strings.Join(shape, " ") != want {
		t.Fatalf("catalogue shape\n got %s\nwant %s", strings.Join(shape, " "), want)
	}
	if result.count != 6 || result.hosted["image_generation"] != 1 || result.hosted["web_search"] != 1 {
		t.Fatalf("count %d hosted %v", result.count, result.hosted)
	}
	// Without a "functions" namespace of its own, the default namespace's
	// description is empty (responses_api.rs:65-71).
	plain := buildA1Catalogue([]any{fn("x"), ns("zeta", fn("z"))})
	if group := objectValue(plain.tools[0]); group["name"] != "functions" || group["description"] != "" {
		t.Fatalf("functions namespace %v", group)
	}
	// Key sets, as in the capture: function = description,name,parameters,
	// strict,type (strict false); custom = description,format,name,type with a
	// lark grammar; namespace = description,name,tools,type.
	keys := func(object map[string]any) string {
		list := make([]string, 0, len(object))
		for key := range object {
			list = append(list, key)
		}
		return strings.Join(sortedStrings(list), ",")
	}
	for _, raw := range result.tools {
		group := objectValue(raw)
		if keys(group) != "description,name,tools,type" || group["type"] != "namespace" {
			t.Fatalf("namespace keys %s", keys(group))
		}
		for _, rawMember := range group["tools"].([]any) {
			member := objectValue(rawMember)
			switch member["type"] {
			case "function":
				if keys(member) != "description,name,parameters,strict,type" || member["strict"] != false {
					t.Fatalf("function keys %s strict %v", keys(member), member["strict"])
				}
			case "custom":
				format := objectValue(member["format"])
				if keys(member) != "description,format,name,type" || format["type"] != "grammar" || format["syntax"] != "lark" {
					t.Fatalf("custom %v", member)
				}
			default:
				t.Fatalf("member type %v", member["type"])
			}
		}
	}
}

func sortedStrings(list []string) []string {
	out := append([]string(nil), list...)
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

// The promoted request is the relay request turned into a Lite request: its
// upstream body is byte for byte the body the existing Lite pipeline makes
// for a client that sends the same additional_tools item itself (the item at
// input[0], role developer, an "at_" id), so the prologue, namespace handling
// and history replay are the Lite ones. The item follows the 907 captured
// Lite requests: keys id,role,tools,type, top level all namespaces.
func TestA1PromotedBodyIsTheLiteBody(t *testing.T) {
	s5ResetCaches()
	a1ResetDenyCache()
	cfg := a1Cfg(a1ModeOn, a1HistoryH1, "")
	off := defaultConfig()
	for _, plainShapes := range []bool{false, true} {
		cfg.ClientIdentity.PlainPrologue, cfg.ClientIdentity.ClientShapes = plainShapes, plainShapes
		off.ClientIdentity.PlainPrologue, off.ClientIdentity.ClientShapes = plainShapes, plainShapes
		mode := identityOff
		if plainShapes {
			mode = identityHygiene
		}
		for _, source := range []map[string]any{
			a1Relay("a1-lite-1", s5User("list the files")),
			s5Set(a1Relay("", s5User("no cache key")), "instructions", "Be brief."),
			s5Set(a1Relay("a1-lite-3"), "input", "a plain string prompt"),
		} {
			scope := "scope-a1-lite"
			plan, promoted := planNativePromotion(source, cfg, scope)
			if plan == nil || !plan.applied || promoted == nil {
				t.Fatalf("not promoted: %+v", plan)
			}
			body, err := prepareResponsesBodyFor(cloneObject(source), cfg, scope, persona{}, mode)
			if err != nil {
				t.Fatal(err)
			}
			lite := cloneObject(promoted)
			delete(lite, a1HistoryKey)
			liteBody, err := prepareResponsesBodyFor(lite, off, scope, persona{}, mode)
			if err != nil {
				t.Fatal(err)
			}
			got, want := cloneObject(body), cloneObject(liteBody)
			// The conversation identity (task_id, turn_id) stays rooted on the
			// client's own items: the same as the relay body's, not the Lite
			// body's, whose first item is the additional_tools item.
			delete(got, "metadata")
			delete(want, "metadata")
			if string(jsonBytes(got)) != string(jsonBytes(want)) {
				t.Fatalf("promoted body is not the Lite body\n got %s\nwant %s", jsonBytes(got), jsonBytes(want))
			}
			relayBody, err := prepareResponsesBodyFor(cloneObject(source), off, scope, persona{}, mode)
			if err != nil {
				t.Fatal(err)
			}
			if string(jsonBytes(body["metadata"])) != string(jsonBytes(relayBody["metadata"])) {
				t.Fatalf("promotion moved the conversation identity: %s vs %s", jsonBytes(body["metadata"]), jsonBytes(relayBody["metadata"]))
			}
			items := a1BodyInput(t, string(jsonBytes(body)))
			index, item := a1FindItem(items, "additional_tools")
			if index < 1 || item["role"] != "developer" {
				t.Fatalf("additional_tools at %d: %v", index, item)
			}
			for _, earlier := range items[:index] {
				if earlier["type"] != "message" || earlier["role"] != "developer" {
					t.Fatalf("before additional_tools: %v (only the prologue may be)", earlier)
				}
			}
			if !plainShapes {
				if !regexp.MustCompile(`^at_[0-9a-f]{8}-[0-9a-f]{4}-5[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(stringValue(item["id"])) {
					t.Fatalf("item id %q", item["id"])
				}
				var keys []string
				for key := range item {
					keys = append(keys, key)
				}
				if strings.Join(sortedStrings(keys), ",") != "id,role,tools,type" {
					t.Fatalf("item keys %v", keys)
				}
			} else if _, hasID := item["id"]; hasID {
				// client_shapes drops developer ids, for a real Lite client's
				// item just the same.
				t.Fatal("client_shapes kept the developer item's id")
			}
			for _, raw := range item["tools"].([]any) {
				if objectValue(raw)["type"] != "namespace" {
					t.Fatalf("top-level catalogue entry %v", raw)
				}
			}
			// The prologue is h22's Lite text, and there is no relay reminder.
			text := string(jsonBytes(body))
			lead := relayAdditional
			if plainShapes {
				lead = plainAdditional
			}
			if !strings.Contains(text, lead) || strings.Contains(text, "run_officejs transport") || strings.Contains(text, toolCatalogReminder) {
				t.Fatal("promoted prologue is not the Lite prologue")
			}
		}
	}
}

// The item id is stable for a conversation (the cached prefix does not
// move) and the promoted body is a pure function of the request.
func TestA1PromotionIsDeterministic(t *testing.T) {
	s5ResetCaches()
	a1ResetDenyCache()
	cfg := a1Cfg(a1ModeOn, a1HistoryH2, "none")
	first := s5Relay("a1-det", s5User("one"))
	second := s5Relay("a1-det", s5User("one"), s5Assistant("msg_a1_det", "ok"), s5User("two"))
	id := func(source map[string]any, scope string) string {
		body, err := prepareResponsesBodyFor(cloneObject(source), cfg, scope, persona{}, identityOff)
		if err != nil {
			t.Fatal(err)
		}
		_, item := a1FindItem(a1BodyInput(t, string(jsonBytes(body))), "additional_tools")
		return stringValue(item["id"])
	}
	if id(first, "s1") != id(second, "s1") || id(first, "s1") == id(first, "s2") {
		t.Fatal("item id is not per conversation and catalogue")
	}
	if id(first, "s1") == id(s5RelayWith("a1-det", []any{s5ExecTool()}, s5User("one")), "s1") {
		t.Fatal("item id does not follow the catalogue")
	}
	want := ""
	for round := 0; round < 20; round++ {
		body, err := prepareResponsesBodyFor(cloneObject(second), cfg, "s1", persona{}, identityOff)
		if err != nil {
			t.Fatal(err)
		}
		if text := string(jsonBytes(body)); round == 0 {
			want = text
		} else if text != want {
			t.Fatalf("round %d prepared a different body", round)
		}
	}
}

// ---------------------------------------------------------------- not-promotable list

func TestA1NotPromotableList(t *testing.T) {
	a1ResetDenyCache()
	base := a1Relay("a1-np", s5User("x"))
	decide := func(source map[string]any, configure func(*Config)) a1Decision {
		cfg := a1Cfg(a1ModeShadow, a1HistoryH1, "")
		if configure != nil {
			configure(&cfg)
		}
		plan, _ := planNativePromotion(source, cfg, "np-scope")
		if plan == nil {
			t.Fatal("no plan for a relayed request")
		}
		return plan.decision
	}
	if d := decide(base, nil); !d.promotable || d.reason != "" || d.hosted["web_search"] != 1 || d.toolCount != 4 {
		t.Fatalf("base: %+v", d)
	}
	dotted := map[string]any{"type": "function", "name": "agents.send", "parameters": map[string]any{"type": "object"}}
	conflict := s5ExecTool()
	conflict["description"] = "Another meaning."
	cases := []struct {
		name      string
		source    map[string]any
		configure func(*Config)
		reason    string
	}{
		{"references", base, func(c *Config) { c.RelayProtocol = relayProtocolReferences }, a1ReasonRelayOptions},
		{"full_contracts", base, func(c *Config) { c.RelayToolContracts = relayToolContractsFull }, a1ReasonRelayOptions},
		{"reject_history", base, func(c *Config) { c.HistoryConflict = historyConflictReject }, a1ReasonRelayOptions},
		// a1wide2: a forced choice is promoted unless "tool_choice" is blocked.
		{"tool_choice_required", s5Set(base, "tool_choice", "required"), nil, ""},
		{"tool_choice_forced", s5Set(base, "tool_choice", map[string]any{"type": "function", "name": "exec_command"}), nil, ""},
		{"tool_choice_required_blocked", s5Set(base, "tool_choice", "required"), func(c *Config) { c.RelayNativePromotionBlock = "custom_format_other,tool_choice" }, a1ReasonToolChoice},
		{"tool_choice_forced_blocked", s5Set(base, "tool_choice", map[string]any{"type": "function", "name": "exec_command"}), func(c *Config) { c.RelayNativePromotionBlock = "tool_choice" }, a1ReasonToolChoice},
		{"tool_choice_forced_undeclared", s5Set(base, "tool_choice", map[string]any{"type": "function", "name": "not_declared"}), nil, a1ReasonToolChoice},
		{"tool_choice_unsupported", s5Set(base, "tool_choice", map[string]any{"type": "web_search"}), nil, a1ReasonToolChoice},
		// a1wide: a compaction item is promoted unless "compaction" is blocked.
		{"compaction_item", s5Set(base, "input", []any{map[string]any{"type": "compaction", "encrypted_content": "gAAAA-a1"}, s5User("x")}), nil, ""},
		{"compaction_trigger", s5Set(base, "input", []any{s5User("x"), map[string]any{"type": "compaction_trigger"}}), nil, ""},
		{"compaction_item_blocked", s5Set(base, "input", []any{map[string]any{"type": "compaction", "encrypted_content": "gAAAA-a1"}, s5User("x")}), func(c *Config) { c.RelayNativePromotionBlock = "compaction,custom_format_other,strict_true" }, a1ReasonCompaction},
		{"compaction_trigger_blocked", s5Set(base, "input", []any{s5User("x"), map[string]any{"type": "compaction_trigger"}}), func(c *Config) { c.RelayNativePromotionBlock = "compaction" }, a1ReasonCompaction},
		// Blocking compaction keeps a request without one promotable.
		{"compaction_blocked_no_item", base, func(c *Config) { c.RelayNativePromotionBlock = "compaction" }, ""},
		{"lite_and_relay", s5Set(base, "input", []any{s5LiteDecl(), s5User("x")}), nil, a1ReasonAdditionalTools},
		{"dotted_name", s5RelayWith("", []any{s5ExecTool(), dotted}, s5User("x")), nil, a1ReasonToolShape},
		{"transport_name", s5RelayWith("", []any{map[string]any{"type": "function", "name": "run_officejs"}}, s5User("x")), nil, a1ReasonToolShape},
		{"nested_namespace", s5RelayWith("", []any{map[string]any{"type": "namespace", "name": "outer", "tools": []any{map[string]any{"type": "namespace", "name": "inner"}}}, s5ExecTool()}, s5User("x")), nil, a1ReasonToolShape},
		{"conflicting_duplicate", s5RelayWith("", []any{s5ExecTool(), conflict}, s5User("x")), nil, a1ReasonDuplicateTool},
		{"schema_invalid", s5RelayWith("", []any{map[string]any{"type": "function", "name": "bad", "parameters": map[string]any{"type": "object", "required": "x"}}}, s5User("x")), nil, a1ReasonSchemaInvalid},
		{"function_without_parameters", s5RelayWith("", []any{map[string]any{"type": "function", "name": "only"}, map[string]any{"type": "web_search"}}, s5User("x")), nil, ""},
		{"enum_blocked", s5Relay("", s5User("x")), nil, a1ReasonSchemaFeature},
		// a1wide: strict:true is promoted (sent as strict:false) unless blocked.
		{"strict_promoted", s5RelayWith("", []any{s5Set(s5ExecTool(), "strict", true)}, s5User("x")), nil, ""},
		{"strict_blocked", s5RelayWith("", []any{s5Set(s5ExecTool(), "strict", true)}, s5User("x")), func(c *Config) { c.RelayNativePromotionBlock = "custom_format_other,strict_true" }, a1ReasonSchemaFeature},
		{"text_custom_blocked", s5RelayWith("", []any{s5Patch()}, s5User("x")), nil, a1ReasonSchemaFeature},
		{"enum_unblocked", s5Relay("", s5User("x")), func(c *Config) { c.RelayNativePromotionBlock = a1BlockNone }, ""},
		{"custom_tool_blocked", base, func(c *Config) { c.RelayNativePromotionBlock = "custom_tool" }, a1ReasonSchemaFeature},
	}
	for _, c := range cases {
		d := decide(c.source, c.configure)
		if d.reason != c.reason || d.promotable != (c.reason == "") {
			t.Errorf("%s: promotable %v reason %q, want %q (blocked %v)", c.name, d.promotable, d.reason, c.reason, d.blocked)
		}
	}
	// Identical duplicates (one tool declared twice) are one tool.
	if d := decide(s5RelayWith("", []any{s5ExecTool(), s5ExecTool()}, s5User("x")), nil); !d.promotable || d.toolCount != 1 {
		t.Fatalf("identical duplicate: %+v", d)
	}
	// tool_choice "none" or no tools at all: not a relayed request, no plan.
	for _, source := range []map[string]any{s5Set(base, "tool_choice", "none"), s5Lite("a1-np", s5LiteDecl(), s5User("x")), s5Body("a1-np", s5User("x"))} {
		if plan, _ := planNativePromotion(source, a1Cfg(a1ModeOn, a1HistoryH1, ""), "np"); plan != nil {
			t.Fatalf("plan for a request that is not relayed: %+v", plan)
		}
	}
	// A catalogue the upstream refused stays on the relay until it expires.
	d := decide(base, nil)
	denyPromotionCatalog(d.hash)
	if d := decide(base, nil); d.reason != a1ReasonCatalogRejected {
		t.Fatalf("denied catalogue: %+v", d)
	}
	a1DenyCache.Lock()
	a1DenyCache.items[d.hash] = time.Now().Add(-a1DenyTTL - time.Minute)
	a1DenyCache.Unlock()
	if d := decide(base, nil); !d.promotable {
		t.Fatalf("expired denial still applies: %+v", d)
	}
	// Bounded.
	for index := 0; index < a1DenyEntries+10; index++ {
		denyPromotionCatalog(fmt.Sprintf("h%d", index))
	}
	a1DenyCache.Lock()
	size := len(a1DenyCache.items)
	a1DenyCache.Unlock()
	if size != a1DenyEntries || promotionCatalogDenied("h0") || !promotionCatalogDenied(fmt.Sprintf("h%d", a1DenyEntries+9)) {
		t.Fatalf("deny cache size %d", size)
	}
	a1ResetDenyCache()
}

// ---------------------------------------------------------------- switches

func TestA1Switches(t *testing.T) {
	t.Chdir(t.TempDir())
	status := func(service *Service) (map[string]any, []string) { return s5Status(t, service) }
	for line, want := range map[string]map[string]any{
		"relay_native_promotion: on\n":                            {"relay_native_promotion": "on"},
		"relay_native_promotion: off\n":                           {"relay_native_promotion": "off"},
		"relay_native_promotion: SHADOW\n":                        {"relay_native_promotion": "shadow"},
		"relay_native_promotion_history: H2\n":                    {"relay_native_promotion_history": "h2"},
		"relay_native_promotion_block: none\n":                    {"relay_native_promotion_block": "none"},
		"relay_native_promotion_block: \"ENUM, ref\"\n":           {"relay_native_promotion_block": "enum,ref"},
		"relay_native_promotion_block: [type_list, enum, enum]\n": {"relay_native_promotion_block": "enum,type_list"},
		"relay_native_promotion_block: []\n":                      {"relay_native_promotion_block": "none"},
		"relay_native_promotion_block:\n":                         {"relay_native_promotion_block": a1DefaultBlock},
	} {
		effective, warnings := status(s5Register(t, line, nil))
		for key, value := range want {
			if effective[key] != value || len(warnings) != 0 {
				t.Errorf("%q: %s = %v (warnings %v)", line, key, effective[key], warnings)
			}
		}
	}
	for line, key := range map[string]string{
		"relay_native_promotion: true\n":                 "relay_native_promotion",
		"relay_native_promotion: [on]\n":                 "relay_native_promotion",
		"relay_native_promotion_history: h3\n":           "relay_native_promotion_history",
		"relay_native_promotion_block: none,ref\n":       "relay_native_promotion_block",
		"relay_native_promotion_block: \"ref,,bogus\"\n": "relay_native_promotion_block",
		"relay_native_promotion_block: [ref, [enum]]\n":  "relay_native_promotion_block",
		"relay_native_promotion_block: {ref: true}\n":    "relay_native_promotion_block",
	} {
		var logs []map[string]any
		service := s5Register(t, line+"max_concurrency_per_credential: 3\n", &logs)
		effective, warnings := status(service)
		if effective[key] != defaultRelayOptions().describe()[key] || len(warnings) != 1 || !strings.HasPrefix(warnings[0], key+":") {
			t.Errorf("%q: %v (warnings %v)", line, effective[key], warnings)
		}
		if service.config().MaxConcurrencyPerCredential != 3 || len(logs) != 0 {
			t.Errorf("%q: cost the rest of the file or logged during registration", line)
		}
	}
	// The reset list: a deleted line is the default again, live and after a
	// restart with only settings.json left.
	service := s5Register(t, "relay_native_promotion: on\nrelay_native_promotion_history: h2\nrelay_native_promotion_block: none\n", nil)
	s1Reconfigure(t, service, "timeout_seconds: 300\n")
	for key, want := range map[string]any{"relay_native_promotion": "off", "relay_native_promotion_history": "h1", "relay_native_promotion_block": a1DefaultBlock} {
		if got := relayOptionsFromConfig(service.config()).describe()[key]; got != want {
			t.Errorf("deleted %s kept %v", key, got)
		}
	}
	s5Register(t, "relay_native_promotion: on\n", nil)
	if got := relayOptionsFromConfig(s5Register(t, s5NoConfig, nil).config()).NativePromotion; got != a1ModeOff {
		t.Fatalf("restart without config.yaml restored %q", got)
	}
}

// The decision is taken from the switches frozen when the request was
// prepared: a reconfigure while the promoted body is upstream changes
// neither its fallback nor how its response is decoded and counted.
func TestA1SwitchesFrozenPerRequest(t *testing.T) {
	s5ResetCaches()
	a1ResetDenyCache()
	h := newA1Harness(t, "relay_native_promotion: on\n")
	h.during = func(call int) {
		if call == 0 {
			h.reconfigure("relay_native_promotion: off\n")
		}
	}
	before := relayCounterSnapshot()
	result := h.run(a1Relay("a1-frozen", s5User("x")), true, func(call int, body string) a1Reply {
		if strings.Contains(body, `"additional_tools"`) {
			return a1Reply{status: 400, body: `{"error":{"message":"Invalid schema for function 'exec_command'.","type":"invalid_request_error"}}`}
		}
		return a1Text("resp_a1_frozen", "fine")
	})
	if len(result.bodies) != 2 || result.closeErr != "" || !s5Completed(s5Result{Events: result.events}) {
		t.Fatalf("frozen plan did not fall back: %d calls, close %q", len(result.bodies), result.closeErr)
	}
	if delta := a1CounterDelta(before); delta["a1_fallback:attempted"] != 1 || delta["relay_requests:legacy:promoted"] != 1 {
		t.Fatalf("counters %v", delta)
	}
	// The next request sees the new value.
	h.during = nil
	result = h.run(a1Relay("a1-frozen-2", s5User("x")), true, a1Always(a1Text("resp_a1_frozen2", "fine")))
	if len(result.bodies) != 1 || strings.Contains(result.bodies[0], `"additional_tools"`) {
		t.Fatal("promotion still on after reconfigure")
	}

	// Mid-stream: the response of a promoted request keeps being decoded as
	// one after the switch is turned off. (The first part denied the
	// catalogue: forget that.)
	s5ResetCaches()
	a1ResetDenyCache()
	h = newA1Harness(t, "relay_native_promotion: on\n")
	native := s5Native("call_a1_frozen_native", "", "exec_command", `{"cmd":"ls"}`)
	h.during = func(call int) { h.reconfigure("relay_native_promotion: off\n") }
	before = relayCounterSnapshot()
	result = h.run(a1Relay("a1-frozen-3", s5User("x")), true, a1Always(a1Reply{chunks: s5Stream("resp_a1_frozen3").call(native).completed()}))
	if result.closeErr != "" || a1CounterDelta(before)["a1_native_calls:function:declared"] != 1 {
		t.Fatalf("native call not counted under the frozen plan: %q %v", result.closeErr, a1CounterDelta(before))
	}
}

// ---------------------------------------------------------------- off and shadow = h25

// The hard requirement: with relay_native_promotion off -- whatever the other
// two A1 switches say -- and in shadow, every S5 turn (80 cases, both
// profiles) sends upstream exactly the bytes h25 sends and hands the client
// exactly what h25 hands it. h25 itself is held to h22 by
// TestH25S5DefaultConfigurationIsH22EndToEnd on the same cases.
func TestA1OffAndShadowLeaveEveryS5TurnAsH25(t *testing.T) {
	overlays := []struct{ name, yaml string }{
		{"off_explicit", "relay_native_promotion: off\nrelay_native_promotion_history: h1\nrelay_native_promotion_block: " + a1DefaultBlock + "\n"},
		{"off_other_knobs", "relay_native_promotion: off\nrelay_native_promotion_history: h2\nrelay_native_promotion_block: none\n"},
		{"shadow", "relay_native_promotion: shadow\n"},
		{"shadow_unblocked", "relay_native_promotion: shadow\nrelay_native_promotion_history: h2\nrelay_native_promotion_block: none\n"},
	}
	a1ResetDenyCache()
	before := relayCounterSnapshot()
	turns := 0
	for _, profile := range s5Profiles() {
		for _, c := range s5Cases() {
			s5ResetCaches()
			want := s5RunCase(t, profile, c, "")
			for _, overlay := range overlays {
				s5ResetCaches()
				runBefore := relayCounterSnapshot()
				got := s5RunCase(t, profile, c, overlay.yaml)
				if strings.HasPrefix(overlay.name, "off") {
					// off does not even decide: no a1_* counter moves.
					for key := range a1CounterDelta(runBefore) {
						if strings.HasPrefix(key, "a1_") {
							t.Fatalf("%s/%s/%s: off counted %s", profile.Name, overlay.name, c.Name, key)
						}
					}
				}
				for index := range c.Turns {
					label := fmt.Sprintf("%s/%s/%s turn %d", profile.Name, overlay.name, c.Name, index+1)
					g, w := got[index], want[index]
					if g.Upstream != w.Upstream || g.Headers != w.Headers || g.UpstreamCalls != w.UpstreamCalls {
						t.Errorf("%s: upstream request differs from h25\n got: %s\nwant: %s", label, g.Upstream, w.Upstream)
					}
					if g.Status != w.Status || g.Kind != w.Kind || a1MaskRef(g.Message) != a1MaskRef(w.Message) ||
						a1MaskRef(g.CloseErr) != a1MaskRef(w.CloseErr) || g.Payload != w.Payload ||
						strings.Join(g.Events, "\n") != strings.Join(w.Events, "\n") {
						t.Errorf("%s: client result differs from h25", label)
					}
					turns++
				}
			}
		}
	}
	delta := a1CounterDelta(before)
	t.Logf("%d turns compared with h25; shadow: %d promotable, %d not promotable", turns,
		delta["a1_requests:shadow:promotable"], delta["a1_requests:shadow:not_promotable"])
	if turns < 600 || delta["a1_requests:shadow:promotable"] == 0 || delta["a1_requests:shadow:not_promotable"] == 0 {
		t.Fatalf("%d turns compared; shadow counted %v", turns, delta)
	}
	for key := range delta {
		if strings.HasPrefix(key, "a1_") && (strings.Contains(key, ":on") || strings.Contains(key, ":off")) || strings.Contains(key, "promoted") ||
			strings.HasPrefix(key, "a1_native_calls") || strings.HasPrefix(key, "a1_transport_calls") || strings.HasPrefix(key, "a1_fallback") {
			t.Fatalf("off/shadow promoted something: %s", key)
		}
	}
}

// With promotion on (block list off, so the S5 catalogues qualify), the fake
// upstream still answers in run_officejs: every turn h22 completed is still
// completed with the same tool calls. Promotion changes what goes up, never
// how a relay answer is decoded.
func TestA1OnKeepsEveryH22SuccessOnS5(t *testing.T) {
	golden := loadS5Golden(t)
	profile := s5Profiles()[0]
	want := golden.Profiles[profile.Name]
	for _, history := range []string{a1HistoryH1, a1HistoryH2} {
		a1ResetDenyCache()
		before := relayCounterSnapshot()
		kept := 0
		for _, c := range s5Cases() {
			s5ResetCaches()
			got := s5RunCase(t, profile, c, "relay_native_promotion: on\nrelay_native_promotion_block: none\nrelay_native_promotion_history: "+history+"\n")
			for index, turn := range c.Turns {
				recorded := want[c.Name][index]
				if recorded.Status != 0 || recorded.CloseErr != "" || !s5Completed(recorded) {
					continue
				}
				label := history + "/" + c.Name + " turn " + string(rune('1'+index))
				if got[index].Status != 0 || got[index].CloseErr != "" || !s5Completed(got[index]) {
					t.Errorf("%s: h22 completed this turn, A1 did not: %d %s %q", label, got[index].Status, got[index].Kind, got[index].CloseErr)
					continue
				}
				if gotCalls, wantCalls := s5DeliveredCalls(t, got[index], turn.Blocked), s5DeliveredCalls(t, recorded, turn.Blocked); gotCalls != wantCalls {
					t.Errorf("%s: delivered calls differ\n got: %s\nwant: %s", label, gotCalls, wantCalls)
				}
				kept++
			}
		}
		delta := a1CounterDelta(before)
		if kept < 60 || delta["relay_requests:legacy:promoted"] < 30 || delta["a1_transport_calls:ok"] == 0 {
			t.Errorf("%s: %d turns kept, counters %v", history, kept, delta)
		}
	}
}

// ---------------------------------------------------------------- on, end to end

// A promoted turn: the client's tools go upstream as additional_tools, the
// upstream's native call reaches the client unchanged (no mapping on the way
// back), a run_officejs call is still decoded, and the counters say so.
func TestA1OnEndToEnd(t *testing.T) {
	s5ResetCaches()
	a1ResetDenyCache()
	h := newA1Harness(t, "relay_native_promotion: on\n")
	native := s5Native("call_a1_e2e_native", "", "exec_command", `{"cmd":"dir C:\\work"}`)
	nativeCustom := s5NativeCustom("call_a1_e2e_patch", "apply_patch", "*** Begin Patch\n*** End Patch")
	relayed := s5Transport("call_a1_e2e_relay", s5Code("list_files", map[string]any{"path": "src"}), []any{})
	before := relayCounterSnapshot()
	result := h.run(a1Relay("a1-e2e", s5User("go")), true, a1Always(a1Reply{chunks: s5Stream("resp_a1_e2e").reasoning("thinking").call(native).call(nativeCustom).call(relayed).completed()}))
	if result.closeErr != "" || len(result.bodies) != 1 {
		t.Fatalf("close %q, %d upstream calls", result.closeErr, len(result.bodies))
	}
	items := a1BodyInput(t, result.bodies[0])
	if index, _ := a1FindItem(items, "additional_tools"); index < 1 {
		t.Fatal("no additional_tools upstream")
	}
	// The native calls arrive exactly as the upstream made them.
	if !strings.Contains(result.raw, `"arguments":"{\"cmd\":\"dir C:\\\\work\"}"`) || !strings.Contains(result.raw, `"name":"apply_patch"`) ||
		!strings.Contains(result.raw, `"name":"list_files"`) || strings.Contains(result.raw, "run_officejs") {
		t.Fatalf("client stream: %s", result.raw)
	}
	delta := a1CounterDelta(before)
	for key, want := range map[string]uint64{
		"relay_requests:legacy:promoted": 1, "relay_outcome:legacy:promoted:succeeded": 1,
		"a1_requests:on:promoted": 1, "a1_history:h1": 1,
		"a1_native_calls:function:declared": 1, "a1_native_calls:custom:declared": 1,
		"a1_transport_calls:ok": 1, "a1_hosted_dropped:on:web_search": 1, "a1_feature:on:namespace": 1,
	} {
		if delta[key] != want {
			t.Errorf("%s = %d, want %d (all: %v)", key, delta[key], want, delta)
		}
	}

	// The next turn (h1): the native calls go back natively (namespace
	// restored as for any Lite call), the relay call replays its own
	// run_officejs, and each output follows its call.
	history := a1Relay("a1-e2e", s5User("go"),
		s5Echo("call_a1_e2e_native", "", "exec_command", `{"cmd":"dir C:\\work"}`), s5Output("call_a1_e2e_native", "a.txt"),
		map[string]any{"type": "custom_tool_call", "call_id": "call_a1_e2e_patch", "name": "apply_patch", "input": "*** Begin Patch\n*** End Patch"},
		s5CustomOutput("call_a1_e2e_patch", "done"),
		s5Echo("call_a1_e2e_relay", "", "list_files", `{"path":"src"}`), s5Output("call_a1_e2e_relay", "x.go"),
		s5User("next"))
	result = h.run(history, true, a1Always(a1Text("resp_a1_e2e2", "ok")))
	items = a1BodyInput(t, result.bodies[0])
	var shapes []string
	for _, item := range items {
		switch stringValue(item["type"]) {
		case "function_call", "custom_tool_call":
			shapes = append(shapes, fmt.Sprintf("%s:%s:%s", item["type"], item["name"], stringValue(item["namespace"])))
		case "function_call_output", "custom_tool_call_output":
			shapes = append(shapes, fmt.Sprintf("%s:%s", item["type"], item["call_id"]))
		}
	}
	want := "function_call:exec_command:functions function_call_output:call_a1_e2e_native " +
		"custom_tool_call:apply_patch:functions custom_tool_call_output:call_a1_e2e_patch " +
		"function_call:run_officejs: function_call_output:call_a1_e2e_relay"
	if strings.Join(shapes, " ") != want {
		t.Fatalf("h1 history\n got %s\nwant %s", strings.Join(shapes, " "), want)
	}
	if !strings.Contains(result.bodies[0], `s5 summary`) {
		t.Fatal("the relay call did not replay the model's own run_officejs")
	}
}

// ---------------------------------------------------------------- history h1 / h2

// The two history renderings on one history: a relay call the cache knows
// (X), a call answered natively on a promoted turn (Y), a cold call (Z) and
// a cold custom call (C).
func TestA1HistoryRenderings(t *testing.T) {
	scope := "scope-a1-history"
	seed := func() {
		s5ResetCaches()
		rememberRelayCall(scope, s5Transport("call_hx", s5Code("exec_command", map[string]any{"cmd": "x"}), []any{}),
			s5Echo("call_hx", "", "exec_command", `{"cmd":"x"}`))
		rememberDeliveredNativeCall(scope, s5Native("call_hy", "", "exec_command", `{"cmd":"y"}`))
	}
	source := s5RelayWith("a1-history", []any{s5ExecTool(), a1LarkPatch()}, s5User("start"),
		s5Echo("call_hx", "", "exec_command", `{"cmd":"x"}`), s5Output("call_hx", "X"),
		s5Echo("call_hy", "", "exec_command", `{"cmd":"y"}`), s5Output("call_hy", "Y"),
		s5Echo("call_hz", "", "exec_command", `{"cmd":"z"}`), s5Output("call_hz", "Z"),
		map[string]any{"type": "custom_tool_call", "call_id": "call_hc", "name": "apply_patch", "input": "*** Begin Patch\n*** End Patch"},
		s5CustomOutput("call_hc", "C"),
		s5User("continue"))
	render := func(cfg Config) (string, map[string]uint64) {
		seed()
		before := relayCounterSnapshot()
		body, err := prepareResponsesBodyFor(cloneObject(source), cfg, scope, persona{}, identityOff)
		if err != nil {
			t.Fatal(err)
		}
		var shapes []string
		for _, item := range a1BodyInput(t, string(jsonBytes(body))) {
			switch kind := stringValue(item["type"]); kind {
			case "function_call", "custom_tool_call":
				name := stringValue(item["name"])
				if name == transportName {
					inner := decodeTransportCode(parseArguments(item["arguments"])["code"])
					name += "(" + stringValue(inner["tool"]) + ")"
				}
				shapes = append(shapes, fmt.Sprintf("%s %s %s/%s", stringValue(item["call_id"]), kind, name, stringValue(item["namespace"])))
			case "function_call_output", "custom_tool_call_output":
				shapes = append(shapes, fmt.Sprintf("%s %s id=%v", stringValue(item["call_id"]), kind, item["id"] != nil))
			}
		}
		return strings.Join(shapes, "\n"), a1CounterDelta(before)
	}
	h1, h1Counts := render(a1Cfg(a1ModeOn, a1HistoryH1, ""))
	wantH1 := strings.Join([]string{
		"call_hx function_call run_officejs(exec_command)/", "call_hx function_call_output id=true",
		"call_hy function_call exec_command/functions", "call_hy function_call_output id=false",
		"call_hz function_call run_officejs(exec_command)/", "call_hz function_call_output id=true",
		"call_hc function_call run_officejs(apply_patch)/", "call_hc function_call_output id=true",
	}, "\n")
	if h1 != wantH1 || h1Counts["a1_history:h1_native_kept"] != 1 || h1Counts["history_relay_replayed"] != 1 || h1Counts["history_cold_wrapped"] != 1 {
		t.Fatalf("h1\n%s\nwant\n%s\ncounters %v", h1, wantH1, h1Counts)
	}
	h2, h2Counts := render(a1Cfg(a1ModeOn, a1HistoryH2, ""))
	wantH2 := strings.Join([]string{
		"call_hx function_call exec_command/functions", "call_hx function_call_output id=false",
		"call_hy function_call exec_command/functions", "call_hy function_call_output id=false",
		"call_hz function_call exec_command/functions", "call_hz function_call_output id=false",
		"call_hc custom_tool_call apply_patch/functions", "call_hc custom_tool_call_output id=false",
	}, "\n")
	if h2 != wantH2 || h2Counts["a1_history:h2_rewritten"] != 1 || h2Counts["history_cold_wrapped"] != 0 {
		t.Fatalf("h2\n%s\nwant\n%s\ncounters %v", h2, wantH2, h2Counts)
	}
	// Relay (h25) on the same history and caches: every call run_officejs.
	relay, _ := render(defaultConfig())
	if strings.Count(relay, "run_officejs(") != 4 {
		t.Fatalf("relay rendering\n%s", relay)
	}
	// The promotion's own declarations are not remembered for the scope: an
	// unpromoted turn after a promoted one sees only what clients declared.
	seed()
	if _, err := prepareResponsesBodyFor(cloneObject(source), a1Cfg(a1ModeOn, a1HistoryH2, ""), scope, persona{}, identityOff); err != nil {
		t.Fatal(err)
	}
	if remembered := rememberedNamespaces(scope, nil); len(remembered) != 0 {
		t.Fatalf("synthetic namespaces remembered: %v", remembered)
	}
}

// ---------------------------------------------------------------- fallback

const a1SchemaRefusal = `{"error":{"message":"Invalid schema for function 'exec_command': not supported.","type":"invalid_request_error","param":"input[2].tools[0]","code":"invalid_function_parameters"}}`

func a1RefusePromoted(okID string) func(int, string) a1Reply {
	return func(call int, body string) a1Reply {
		if strings.Contains(body, `"additional_tools"`) {
			return a1Reply{status: 400, body: a1SchemaRefusal}
		}
		// The relay answer carries a relay call: a fallen-back response is
		// a relay response and is not counted as a promoted one.
		relayed := s5Transport("call_"+okID, s5Code("list_files", map[string]any{"path": "src"}), []any{})
		return a1Reply{chunks: s5Stream(okID).text("relay answer").call(relayed).completed()}
	}
}

// A 400 to the promoted body before the stream opens: the same credential gets
// the relay body -- exactly h25's -- once, the client sees only the relay
// answer, the catalogue is remembered as not promotable, and the next
// request with it is not promoted at all.
func TestA1FallbackOn400BeforeTheStream(t *testing.T) {
	for _, stream := range []bool{true, false} {
		s5ResetCaches()
		a1ResetDenyCache()
		request := a1Relay(fmt.Sprintf("a1-fallback-%v", stream), s5User("go"))
		// What h25 sends for this request.
		reference := newA1Harness(t, "")
		want := reference.run(request, stream, a1RefusePromoted("resp_a1_ref"))
		s5ResetCaches()
		h := newA1Harness(t, "relay_native_promotion: on\n")
		before := relayCounterSnapshot()
		got := h.run(request, stream, a1RefusePromoted("resp_a1_ref"))
		if len(got.bodies) != 2 || !strings.Contains(got.bodies[0], `"additional_tools"`) || strings.Contains(got.bodies[1], `"additional_tools"`) {
			t.Fatalf("stream=%v: %d upstream calls", stream, len(got.bodies))
		}
		if s5MaskBody(got.bodies[1]) != s5MaskBody(want.bodies[0]) {
			t.Fatalf("stream=%v: fallback body is not h25's\n got %s\nwant %s", stream, got.bodies[1], want.bodies[0])
		}
		if got.status != 0 || got.closeErr != "" || got.payload != want.payload || strings.Join(got.events, "\n") != strings.Join(want.events, "\n") {
			t.Fatalf("stream=%v: the client saw something other than the relay answer: %d %q %q", stream, got.status, got.closeErr, got.raw)
		}
		delta := a1CounterDelta(before)
		for key, value := range map[string]uint64{
			"a1_fallback:attempted": 1, "a1_fallback_reason:schema": 1, "a1_fallback_result:succeeded": 1,
			"a1_catalog_denied": 1, "relay_requests:legacy:promoted": 1, "relay_outcome:legacy:promoted:succeeded": 1,
			"a1_transport_calls:ok": 0, "relay_call:legacy:ok": 1,
		} {
			if delta[key] != value {
				t.Errorf("stream=%v: %s = %d, want %d (%v)", stream, key, delta[key], value, delta)
			}
		}
		// The log line carries labels, a hash prefix and sizes; no tool name.
		found := false
		for _, entry := range h.logs {
			message := stringValue(entry["message"])
			if !strings.Contains(message, "relay_native_promotion") {
				continue
			}
			found = true
			if !strings.Contains(message, `"stage":"fallback"`) || !strings.Contains(message, `"upstream_code":"invalid_function_parameters"`) ||
				strings.Contains(message, "exec_command") || strings.Contains(message, "list_files") {
				t.Fatalf("log: %s", message)
			}
		}
		if !found {
			t.Fatal("no relay_native_promotion log line")
		}
		// Same catalogue again: not promoted, one upstream call.
		before = relayCounterSnapshot()
		again := h.run(request, stream, a1RefusePromoted("resp_a1_ref"))
		if len(again.bodies) != 1 || strings.Contains(again.bodies[0], `"additional_tools"`) || a1CounterDelta(before)["a1_not_promotable:on:catalog_rejected"] != 1 {
			t.Fatalf("stream=%v: denied catalogue promoted again", stream)
		}
	}
}

// A fallback whose relay body is refused too proves nothing about the
// catalogue: the client gets that refusal and the catalogue is not denied.
// Any other status, and any failure after the stream opened, is not a
// fallback at all.
func TestA1FallbackOnlyForAPreStream400(t *testing.T) {
	s5ResetCaches()
	a1ResetDenyCache()
	h := newA1Harness(t, "relay_native_promotion: on\n")
	request := a1Relay("a1-nofallback", s5User("go"))
	before := relayCounterSnapshot()
	both := h.run(request, true, a1Always(a1Reply{status: 400, body: `{"error":{"message":"Bad input.","type":"invalid_request_error"}}`}))
	if len(both.bodies) != 2 || both.status != 400 || a1CounterDelta(before)["a1_fallback_result:failed:400"] != 1 || a1CounterDelta(before)["a1_catalog_denied"] != 0 {
		t.Fatalf("double refusal: %d calls, status %d, %v", len(both.bodies), both.status, a1CounterDelta(before))
	}
	again := h.run(request, true, a1Always(a1Text("resp_a1_nf", "ok")))
	if len(again.bodies) != 1 || !strings.Contains(again.bodies[0], `"additional_tools"`) {
		t.Fatal("a catalogue was denied without evidence")
	}
	for _, reply := range []a1Reply{
		{status: 500, body: `{"error":{"message":"oops","type":"server_error"}}`},
		{status: 401, body: `{"error":{"message":"expired","type":"invalid_request_error"}}`},
		{status: 422, body: `{"error":{"message":"nope","type":"invalid_request_error"}}`},
		{chunks: s5Stream("resp_a1_failed").finish("response.failed", map[string]any{"error": map[string]any{"code": "invalid_prompt", "message": "Invalid schema"}})},
		{chunks: s5Stream("resp_a1_errevent").errorEvent("invalid_request_error", "Invalid schema for function")},
	} {
		before := relayCounterSnapshot()
		result := h.run(request, true, a1Always(reply))
		if len(result.bodies) != 1 || a1CounterDelta(before)["a1_fallback:attempted"] != 0 {
			t.Fatalf("status %d: %d upstream calls", reply.status, len(result.bodies))
		}
	}
}

// ---------------------------------------------------------------- counters

// Every a1_* counter name is built from code-owned labels: no tool name, no
// argument, no hash.
func TestA1CountersCarryNoRequestContent(t *testing.T) {
	label := regexp.MustCompile(`^a1_[a-z_]+(:[a-z0-9_]+){0,3}(:[0-9]+-[0-9]+|:[0-9]+\+|:<16k|:16k-64k|:64k-256k|:256k\+|:0)?$`)
	for key := range relayCounterSnapshot() {
		if !strings.HasPrefix(key, "a1_") {
			continue
		}
		if !label.MatchString(key) {
			t.Errorf("counter %q", key)
		}
		for _, name := range []string{"exec_command", "list_files", "apply_patch", "lookup", "mcp__docs", "get_weather"} {
			if strings.Contains(key, name) {
				t.Errorf("counter %q names a tool", key)
			}
		}
	}
	// The relay-status summary shows them under "a1", and only when any exist.
	if _, present := relayStatusSummary(map[string]uint64{"relay_requests:legacy:relay": 1})["a1"]; present {
		t.Fatal("a1 summary without a1 counters")
	}
	summary := relayStatusSummary(map[string]uint64{"a1_requests:shadow:promotable": 2, "history_cold_wrapped": 1})
	if a1, _ := summary["a1"].(map[string]uint64); a1["a1_requests:shadow:promotable"] != 2 || len(a1) != 1 {
		t.Fatalf("a1 summary %v", summary["a1"])
	}
}

// ---------------------------------------------------------------- cost

// The decision runs on every relayed request in shadow and on. The largest
// catalogue in the capture is 164 tools / ~175 KB; this fixture has 164 tools
// and about twice the bytes. The bound is loose on purpose (a slow CI
// machine); the measured figure is logged (Windows dev box: ~6 ms).
func TestA1DecisionCostOnALargeCatalogue(t *testing.T) {
	a1ResetDenyCache()
	long := strings.Repeat("Describes what the tool does and when to use it. ", 16)
	var tools []any
	for index := 0; index < 164; index++ {
		properties := map[string]any{}
		for field := 0; field < 6; field++ {
			properties[fmt.Sprintf("field_%d", field)] = map[string]any{"type": "string", "description": long[:120], "minLength": 1}
		}
		properties["options"] = map[string]any{"type": "object", "properties": map[string]any{
			"depth": map[string]any{"type": "integer", "minimum": 0}, "flags": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		}}
		tools = append(tools, map[string]any{"type": "function", "name": fmt.Sprintf("tool_%03d", index), "description": long,
			"parameters": map[string]any{"type": "object", "properties": properties, "required": []any{"field_0"}, "additionalProperties": false}})
	}
	source := s5RelayWith("a1-cost", tools, s5User("x"))
	size := len(jsonBytes(tools))
	cfg := a1Cfg(a1ModeShadow, a1HistoryH1, "")
	const rounds = 20
	started := time.Now()
	var plan *a1Plan
	for round := 0; round < rounds; round++ {
		plan, _ = planNativePromotion(source, cfg, "cost-scope")
	}
	average := time.Since(started) / rounds
	t.Logf("164 tools, %d bytes of tools: decision %v per request, catalogue %d bytes", size, average, plan.decision.bytes)
	if plan == nil || !plan.decision.promotable || plan.decision.toolCount != 164 || size < 150<<10 {
		t.Fatalf("fixture: %+v (%d bytes)", plan, size)
	}
	if average > 250*time.Millisecond {
		t.Fatalf("decision takes %v per request", average)
	}
}

// a1wide (2026-10-15): strict:true and compaction-carrying requests go
// upstream promoted by default -- strict is sent as false -- and listing the
// feature in relay_native_promotion_block puts each back on the relay.
func TestA1WidePromotesStrictAndCompaction(t *testing.T) {
	a1ResetDenyCache()
	cfg := a1Cfg(a1ModeOn, a1HistoryH2, "")
	strictSource := s5RelayWith("", []any{s5Set(s5ExecTool(), "strict", true)}, s5User("x"))
	plan, promoted := planNativePromotion(strictSource, cfg, "a1wide-strict")
	if plan == nil || !plan.applied || promoted == nil {
		t.Fatalf("strict:true not promoted: %+v", plan)
	}
	if !plan.decision.features[a1FeatureStrictTrue] {
		t.Fatalf("strict_true not counted: %v", plan.decision.features)
	}
	catalogue := string(jsonBytes(promoted["input"]))
	if strings.Contains(catalogue, `"strict":true`) || !strings.Contains(catalogue, `"strict":false`) {
		t.Fatalf("promoted catalogue strict: %s", catalogue)
	}
	compactionSource := s5Set(a1Relay("a1wide-cmp", s5User("x")), "input",
		[]any{map[string]any{"type": "compaction", "encrypted_content": "gAAAA-a1wide"}, s5User("y")})
	plan, promoted = planNativePromotion(compactionSource, cfg, "a1wide-cmp")
	if plan == nil || !plan.applied || !plan.decision.features[a1FeatureCompaction] {
		t.Fatalf("compaction not promoted: %+v", plan)
	}
	input, _ := promoted["input"].([]any)
	if len(input) != 3 || stringValue(objectValue(input[0])["type"]) != "additional_tools" ||
		stringValue(objectValue(input[1])["type"]) != "compaction" {
		t.Fatalf("promoted input %s", jsonBytes(input))
	}
	old := a1Cfg(a1ModeOn, a1HistoryH2, "compaction,custom_format_other,strict_true")
	if plan, promoted := planNativePromotion(compactionSource, old, "a1wide-cmp"); plan == nil || plan.applied || promoted != nil || plan.decision.reason != a1ReasonCompaction {
		t.Fatalf("blocked compaction: %+v", plan)
	}
	if plan, promoted := planNativePromotion(strictSource, old, "a1wide-strict"); plan == nil || plan.applied || promoted != nil || plan.decision.reason != a1ReasonSchemaFeature {
		t.Fatalf("blocked strict: %+v", plan)
	}
	if canonical, ok := normalizeA1Block("Compaction, strict_true"); !ok || canonical != "compaction,strict_true" {
		t.Fatalf("block list %q %v", canonical, ok)
	}
}
