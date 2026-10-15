package basispoints

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// What the main site charged users, per credential.
//
// This plugin prices what a response was worth at list price; it has no idea
// what anyone was billed, because the main site multiplies by the user's group
// rate and the group never reaches this side. A reader outside the plugin joins
// the two by the hour and writes the result here; this route hands that file to
// the panel and says how old it is.
//
// The plugin deliberately does not query the main site's database itself. It
// would mean a database driver, credentials and a second failure mode inside
// the process that relays traffic, to populate one line of display. A file
// written by something that can fail on its own is the cheaper arrangement.
//
// Age is attached rather than left to the reader to work out. A figure about
// money that has quietly stopped updating is exactly the kind that gets
// believed, which is the same defect the quota reading had.
// chargeFileName lives in the plugin's own data directory, which is the one
// place both sides can name.
//
// The first version hard-coded the host path, /opt/cliproxyapi/charge.json.
// The plugin runs inside the container, where that path does not exist at all:
// the host's /opt/cliproxyapi/plugins is mounted at /CLIProxyAPI/plugins and
// nothing else of it is visible. The route would have answered 404 forever,
// and the only symptom would have been a missing line on a card.
//
// Deriving it from DataDir instead means the two sides agree by construction,
// and keep agreeing if the mounts are ever rearranged.
const chargeFileName = "charge.json"

// chargeFilePathOverride is set by tests, which need a file they control: every
// failure mode here is about the file being absent, stale or malformed, and
// none of those can be exercised against a path the test cannot write.
var chargeFilePathOverride string

func (s *Service) chargeFilePath() string {
	if chargeFilePathOverride != "" {
		return chargeFilePathOverride
	}
	dir := strings.TrimSpace(s.config().DataDir)
	if dir == "" {
		return chargeFileName
	}
	return filepath.Join(dir, chargeFileName)
}

func (s *Service) serveChargeData() map[string]any {
	path := s.chargeFilePath()
	info, statErr := os.Stat(path)
	if statErr != nil {
		return chargeError(http.StatusNotFound,
			"还没有扣费数据：采集器没在跑，或者还没跑完第一轮")
	}
	raw, readErr := os.ReadFile(path)
	if readErr != nil {
		return chargeError(http.StatusBadGateway, "扣费数据读不出来："+safeError(readErr))
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil || payload == nil {
		return chargeError(http.StatusBadGateway, "扣费数据不是 JSON 对象")
	}
	payload["age_seconds"] = int64(time.Since(info.ModTime()).Seconds())
	body, err := json.Marshal(payload)
	if err != nil {
		return chargeError(http.StatusInternalServerError, "扣费数据无法重新序列化")
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

func chargeError(status int, reason string) map[string]any {
	return map[string]any{
		"StatusCode": status,
		"Headers": map[string][]string{
			"Content-Type":  {"application/json; charset=utf-8"},
			"Cache-Control": {"no-store"},
		},
		"Body": jsonBytes(map[string]any{"error": reason}),
	}
}
