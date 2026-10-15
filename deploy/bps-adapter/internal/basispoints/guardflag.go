package basispoints

import (
	"encoding/json"
	"net/http"
)

// The model-swap guard's on/off switch.
//
// The guard itself is an operations script, not part of the relay, and it has
// no business living in this plugin. What lives here is one bit: whether the
// operator currently wants it to act. That bit is here because of where it has
// to be writable from. The panel runs in a browser, so the only authenticated
// way it can change anything is CPA's management API, and the only thing that
// API writes which this host can serve back without authentication is plugin
// configuration. Putting the flag in the plugin's config therefore buys a
// switch that applies live, through plugin.reconfigure, with no restart and no
// new write path -- the same route the concurrency caps already take.
//
// The guard polls this route. It is deliberately a separate route from
// cost-data: a reader that needs one bit should not have to parse, or be able
// to be confused by, a document about money.
//
// When the switch is off the guard keeps watching and keeps reporting; it only
// stops disabling. An operator who turns it off wants to stop the automation,
// not to stop seeing what the automation would have seen.
func (s *Service) serveGuardState() map[string]any {
	body, err := json.Marshal(map[string]any{
		"enabled": s.config().DowngradeGuardEnabled,
	})
	if err != nil {
		// A guard that cannot read the flag must not infer one, so this is an
		// error rather than a default: see the guard's own handling.
		return map[string]any{
			"StatusCode": http.StatusInternalServerError,
			"Headers":    map[string][]string{"Content-Type": {"application/json; charset=utf-8"}},
			"Body":       []byte(`{"error":"开关状态无法序列化"}`),
		}
	}
	return map[string]any{
		"StatusCode": http.StatusOK,
		"Headers": map[string][]string{
			"Content-Type":  {"application/json; charset=utf-8"},
			"Cache-Control": {"no-store"},
		},
		"Body": body,
	}
}
