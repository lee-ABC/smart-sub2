package basispoints

import "sync"

const schemaObserverKey = "\x00cpa_schema_observer"

// The observer is injected after parsing the caller's JSON. An untrusted JSON
// value cannot acquire this type. MarshalJSON prevents future source snapshots
// from accidentally serializing service state or callbacks.
type selectedSchemaObserver struct {
	mu        sync.Mutex
	remaining int
	observe   func(toolSpec, any)
}

func (*selectedSchemaObserver) MarshalJSON() ([]byte, error) { return []byte("null"), nil }

func observeSelectedToolSchema(source map[string]any, spec toolSpec, value any) {
	observer, _ := source[schemaObserverKey].(*selectedSchemaObserver)
	if observer == nil {
		return
	}
	// Observation must never change a tool decision, even if the host logger
	// panics. Bound the total work per request, including repeated snapshots.
	defer func() { _ = recover() }()
	observer.mu.Lock()
	if observer.remaining <= 0 {
		observer.mu.Unlock()
		return
	}
	observer.remaining--
	observer.mu.Unlock()
	observer.observe(spec, value)
}

func (s *Service) attachSchemaObservation(source map[string]any, callbackID string) {
	delete(source, schemaObserverKey)
	enabled := s.config().SchemaShadow
	if frozen, ok := source[relayOptionsKey].(*frozenRelayOptions); ok && frozen != nil {
		// The request's own snapshot wins over a reconfigure since then.
		enabled = frozen.options.SchemaShadow
	}
	if !enabled {
		return
	}
	source[schemaObserverKey] = &selectedSchemaObserver{remaining: 8, observe: func(spec toolSpec, value any) {
		schema := firstMap(spec.Spec, "parameters", "inputSchema", "input_schema")
		if schema == nil || spec.Type != "function" {
			return
		}
		observed := observeSchemaShadow(schema, value)
		compatibility := "unknown"
		switch checkSchemaCompatibility(value, schema) {
		case schemaCompatible:
			compatibility = "compatible"
		case schemaMismatch:
			compatibility = "mismatch"
		}
		fields := map[string]any{
			"event": "relay_schema_shadow", "plugin_version": Version,
			"compatibility": compatibility, "shadow_result": observed.Code,
			"shadow_reason": observed.Reason, "draft": observed.Draft,
			"enforced": false,
		}
		_ = s.call("host.log", map[string]any{
			"host_callback_id": callbackID, "level": "info",
			"message": "basispoints schema observation " + string(jsonBytes(fields)),
			"fields":  fields,
		}, nil)
	}}
}
