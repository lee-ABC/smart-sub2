package adapter

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
)

// configuredImageBase is operator configuration, never inferred from caller
// Host/forwarded headers. An empty base keeps image publishing disabled.
func configuredImageBase(value any) (string, error) {
	if value == nil {
		return "", nil
	}
	raw, ok := value.(string)
	if !ok {
		return "", errors.New("public_base_url must be an absolute HTTP(S) URL")
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" || strings.ContainsAny(raw, "\r\n") {
		return "", errors.New("public_base_url must be an absolute HTTP(S) URL without credentials, query or fragment")
	}
	return strings.TrimRight(u.String(), "/"), nil
}

// ImageHandler exposes only the expiring, unguessable-token image resource.
// Never expose CPA management or the selected-account transport on this listener.
func (a *Adapter) ImageHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Robots-Tag", "noindex, nofollow")
		if a.imagePath == "" || r.URL.Path != a.imagePath {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		tokens := r.URL.Query()["t"]
		if len(tokens) != 1 || len(tokens[0]) != 64 {
			http.NotFound(w, r)
			return
		}
		if _, err := hex.DecodeString(tokens[0]); err != nil {
			http.NotFound(w, r)
			return
		}
		raw, _ := json.Marshal(map[string]any{"Method": r.Method, "Path": "image", "Query": map[string][]string{"t": tokens}})
		result, err := a.service.Handle("management.handle", raw)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		response, ok := result.(map[string]any)
		if !ok {
			http.Error(w, "image resource unavailable", http.StatusInternalServerError)
			return
		}
		status, ok := response["StatusCode"].(int)
		if !ok {
			http.Error(w, "image resource unavailable", http.StatusInternalServerError)
			return
		}
		headers, _ := response["Headers"].(map[string][]string)
		for _, key := range []string{"Content-Type", "Cache-Control", "X-Robots-Tag"} {
			for _, value := range headers[key] {
				w.Header().Set(key, value)
			}
		}
		body, _ := response["Body"].([]byte)
		w.WriteHeader(status)
		if r.Method != http.MethodHead {
			_, _ = w.Write(body)
		}
	})
}
