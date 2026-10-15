package basispoints

import (
	"net/http"
	"strings"
)

const identityDataPath = "identity-data"

// serveIdentityData reports, per credential, what identity it would present and
// where that identity came from.
//
// The point of the "source" field is to make a silent fall-back visible. A
// credential whose seat key cannot be resolved quietly drops back to off, and
// without somewhere to read that, the only symptom would be that one seat keeps
// behaving like the old build for reasons nobody can see. The derived ids are
// shown truncated: enough to tell two seats apart on screen, not enough to
// reproduce what goes on the wire.
func (s *Service) serveIdentityData() map[string]any {
	cfg := s.config()
	var list struct {
		Files []struct {
			Name     string `json:"name"`
			Label    string `json:"label"`
			Provider string `json:"provider"`
			Type     string `json:"type"`
		} `json:"files"`
	}
	payload := map[string]any{
		"mode":                 cfg.ClientIdentity.Mode,
		"pepper_set":           strings.TrimSpace(cfg.ClientIdentity.Pepper) != "",
		"ua_profile":           profileFor(cfg).name,
		"send_browser_headers": cfg.ClientIdentity.SendBrowserHeaders,
		"send_sentry_trace":    cfg.ClientIdentity.SendSentryTrace,
		"keep_cookies":         cfg.ClientIdentity.KeepCookies,
		"plain_prologue":       cfg.ClientIdentity.PlainPrologue,
		"client_shapes":        cfg.ClientIdentity.ClientShapes,
	}
	if err := s.call("host.auth.list", map[string]any{}, &list); err != nil {
		payload["error"] = safeError(err)
		return jsonResult(payload)
	}
	seats := make([]map[string]any, 0, len(list.Files))
	for _, file := range list.Files {
		if !strings.EqualFold(file.Provider, Provider) && !strings.EqualFold(file.Type, Provider) {
			continue
		}
		mode := cfg.identityMode(file.Label, file.Name)
		seat := map[string]any{
			"label": file.Label,
			"file":  file.Name,
			"mode":  mode.String(),
		}
		// The seat key here is the file name, because host.auth.list is not a
		// request and carries no AuthID. The request path prefers AuthID, so a
		// seat shown here as "file_name" may be keyed by AuthID in flight --
		// both are per-credential, which is the property that matters.
		key := credentialID(file.Name)
		if key == "" {
			seat["source"] = "none (falls back to off)"
			seat["mode"] = identityOff.String()
		} else {
			seat["source"] = "file_name"
			seat["cookies"] = s.cookies.header(key) != ""
			// trace_id needs only the seed, so it is always correct here.
			trace, _ := sentryTrace(persona{seed: key})
			seat["trace_id"] = shortID(trace)
			// task_id and the session's age depend on the subscription date
			// inside the credential, which this page cannot read. Show them
			// only once the seat has served a request and the real values are
			// known; recomputing from the file name would print an id that is
			// not the one being sent.
			if who, known := s.lastPersona(key); known {
				seat["task_id"] = shortID(seatTaskID(who))
				seat["session_born"] = seatBirthMs(who) / 1000
				seat["subscription_start"] = who.subscriptionStart.Unix()
			} else {
				seat["task_id"] = "(尚未服务过请求)"
			}
		}
		seats = append(seats, seat)
	}
	payload["seats"] = seats
	return jsonResult(payload)
}

// jsonResult wraps a payload in the management response shape the other
// endpoints use.
func jsonResult(payload map[string]any) map[string]any {
	return map[string]any{
		"StatusCode": http.StatusOK,
		"Headers": map[string][]string{
			"Content-Type":  {"application/json; charset=utf-8"},
			"Cache-Control": {"no-store"},
		},
		"Body": jsonBytes(payload),
	}
}

func shortID(value string) string {
	value = strings.ReplaceAll(value, "-", "")
	if len(value) <= 8 {
		return value
	}
	return value[:8]
}
