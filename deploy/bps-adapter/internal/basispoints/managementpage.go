package basispoints

import (
	"strconv"
	"encoding/json"
	"html"
	"net/http"
	"strings"
	"time"
)

// Management page.
//
// The bundled management panel renders quota only for the providers it knows
// about natively (codex, claude, devin, antigravity, ...), each with its own
// hardcoded fetcher. It never calls /plugins/<id>/quota, so a plugin's
// QuotaProvider stays invisible there no matter what it reports.
//
// The supported way for a plugin to surface its own UI is a resource route: a
// GET route with a Menu label is published under
// /v0/resource/plugins/<pluginID>/ and the panel links to it from the plugin's
// menu. This file renders the same quota the QuotaProvider serves as a small
// self-contained page.
//
// Resource routes are NOT management-authenticated. Account identity (file
// name, email, account id) is shown in full because this is the operator's own
// credential inventory and the page is only reachable from the gateway itself,
// but no token material is ever rendered here.

const (
	quotaResourcePath  = "quota"
	guardResourcePath  = "guard"
	chargeResourcePath = "charge"
	probeResourcePath  = "probe"
)

func managementRegistration() map[string]any {
	// These structs carry no json tags on the host side, so the wire keys are
	// the Go field names.
	return map[string]any{
		"Resources": []map[string]any{
			{
				"Path":        quotaResourcePath,
				"Menu":        "额度",
				"Description": "查看 Basis Points 凭据的 ChatGPT 额度",
			},
			{
				// No Menu: this route exists for Basis Points to fetch relayed
				// images from, not for a human to browse.
				"Path":        imageRelayPath,
				"Description": "Serves inline images so Basis Points can fetch them",
			},
			{
				"Path":        concurrencyPagePath,
				"Menu":        "并发",
				"Description": "实时查看每个凭据的并发占用",
			},
			{
				// No Menu: the concurrency page polls this for fresh counts.
				"Path":        concurrencyDataPath,
				"Description": "Live per-credential concurrency, as JSON",
			},
			{
				// No Menu: the panel's card badges poll this.
				"Path":        costDataPath,
				"Description": "Per-credential spend and remaining quota value, as JSON",
			},
			{
				// No Menu: raw ledger, for checking the estimate against the
				// main site's own billing.
				"Path":        costDebugPath,
				"Description": "Raw cost ledger, as JSON",
			},
			{
				// No Menu: per-credential client identity, so a seat that fell
				// back to off is visible rather than merely quiet.
				"Path":        identityDataPath,
				"Description": "Per-credential client identity, as JSON",
			},
			{
				// No Menu: one real request on one credential, to find out
				// whether the upstream still answers it with the model it was
				// asked for. Spends quota, so it is rate limited per credential
				// and never runs on a timer; see probe.go.
				"Path":        probeResourcePath,
				"Description": "Ask one credential whether its model is being substituted",
			},
			{
				// No Menu: the panel's card badges read this for what the main
				// site charged users. The figures are produced outside the
				// plugin; see chargefile.go for why.
				"Path":        chargeResourcePath,
				"Description": "Per-credential user charges from the main site, as JSON",
			},
			{
				// No Menu: reads back the relay switches in effect, the
				// warnings of the last configuration and the decode counters.
				"Path":        relayStatusPath,
				"Description": "Relay switches in effect and relay decode counters, as JSON",
			},
			{
				// No Menu: the model-swap guard polls this to learn whether
				// the operator has it switched on. The flag lives in plugin
				// configuration so the panel can write it over the
				// authenticated management API and have it apply live, the
				// same path the concurrency caps already take.
				"Path":        guardResourcePath,
				"Description": "Whether the model-swap guard may disable credentials, as JSON",
			},
		},
	}
}

type managementRequest struct {
	Method string              `json:"Method"`
	Path   string              `json:"Path"`
	Query  map[string][]string `json:"Query"`
}

func (s *Service) managementHandle(raw []byte) (map[string]any, error) {
	var request managementRequest
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &request); err != nil {
			return nil, fail(400, "invalid_request", "management request is invalid")
		}
	}
	switch path := strings.TrimRight(request.Path, "/"); {
	case strings.HasSuffix(path, imageRelayPath):
		return serveRelayedImage(request.Query), nil
	case strings.HasSuffix(path, relayStatusPath):
		return s.serveRelayStatus(), nil
	case strings.HasSuffix(path, identityDataPath):
		return s.serveIdentityData(), nil
	case strings.HasSuffix(path, costDebugPath):
		return s.serveCostDebug(), nil
	case strings.HasSuffix(path, costDataPath):
		return s.serveCostData(), nil
	case strings.HasSuffix(path, concurrencyDataPath):
		return s.serveConcurrencyData(), nil
	case strings.HasSuffix(path, concurrencyPagePath):
		return concurrencyHTML(s.renderConcurrencyPage()), nil
	case strings.HasSuffix(path, probeResourcePath):
		return s.serveProbe(request.Query), nil
	case strings.HasSuffix(path, chargeResourcePath):
		return s.serveChargeData(), nil
	case strings.HasSuffix(path, guardResourcePath):
		return s.serveGuardState(), nil
	case strings.HasSuffix(path, quotaResourcePath):
		// Naming one credential turns this into a single live read that is
		// filed in the ledger. Without a name it stays the whole-inventory
		// page it has always been.
		if firstQuery(request.Query, "file") != "" || firstQuery(request.Query, "auth_index") != "" {
			return s.serveQuotaRefresh(request.Query), nil
		}
		return managementHTML(http.StatusOK, s.renderQuotaPage()), nil
	default:
		// Experiment-only routes (bpsexp build): the shadow-rewrite flush and
		// the experiment status page. A no-op in the production build, which
		// falls through to the 404 below.
		if resp, ok := expManagementRoute(s, path, request.Method, request.Query); ok {
			return resp, nil
		}
		return managementHTML(http.StatusNotFound, "<p>未知的插件资源路径。</p>"), nil
	}
}

// serveRelayedImage hands a stashed image to Basis Points. Unknown, expired and
// exhausted tokens are all answered with a bare 404 so the route reveals
// nothing about what it holds.
func serveRelayedImage(query map[string][]string) map[string]any {
	var token string
	if values := query[imageRelayTokenParam]; len(values) > 0 {
		token = values[0]
	}
	data, mediaType, ok := takeImage(token)
	if !ok {
		return map[string]any{
			"StatusCode": http.StatusNotFound,
			"Headers":    map[string][]string{"Content-Type": {"text/plain; charset=utf-8"}},
			"Body":       []byte("not found"),
		}
	}
	return map[string]any{
		"StatusCode": http.StatusOK,
		"Headers": map[string][]string{
			"Content-Type": {mediaType},
			// no-store forbade the fetcher its own cache, so every turn of a
			// conversation re-downloaded the same pictures: 107k fetches in ten
			// hours, three quarters of them repeats, one image pulled 841
			// times. Each fetch is another chance to be dropped mid-transfer,
			// and a request carrying 37 images fails if any single one of them
			// is, which is where a per-image failure rate of 0.036% turned into
			// 1.3% of requests.
			//
			// "private" keeps the original intent: shared caches and CDNs still
			// must not store these bytes, which are one user's content briefly
			// published for the upstream to collect. Only the fetcher that was
			// handed the unguessable token may reuse it, and only for as long
			// as the entry exists anyway -- max-age matches imageRelayTTL, so a
			// cached copy can never outlive the relay's own.
			"Cache-Control": {"private, max-age=" + strconv.Itoa(int(imageRelayTTL/time.Second))},
			// The token is the only thing standing between this image and
			// anyone who gets the URL, so keep it out of search and scrapers.
			"X-Robots-Tag": {"noindex, nofollow"},
		},
		"Body": data,
	}
}

func managementHTML(status int, body string) map[string]any {
	page := `<!doctype html><html lang="zh"><head><meta charset="utf-8">` +
		`<meta name="viewport" content="width=device-width,initial-scale=1">` +
		`<title>Basis Points 额度</title><style>` +
		`:root{color-scheme:light dark;--fg:#1a1a1a;--muted:#6b7280;--line:#e5e7eb;--bg:#fff;--ok:#16a34a;--warn:#d97706;--bad:#dc2626}` +
		`@media(prefers-color-scheme:dark){:root{--fg:#e5e7eb;--muted:#9ca3af;--line:#374151;--bg:#111827}}` +
		`body{margin:0;padding:24px;background:var(--bg);color:var(--fg);` +
		`font:14px/1.5 -apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,"Helvetica Neue",Arial,sans-serif}` +
		`h1{margin:0 0 4px;font-size:18px}.sub{color:var(--muted);margin:0 0 20px;font-size:13px}` +
		`.card{border:1px solid var(--line);border-radius:10px;padding:16px;margin-bottom:14px}` +
		`.name{font-weight:600;margin-bottom:2px}.meta{color:var(--muted);font-size:12px;margin-bottom:14px}` +
		`.row{display:flex;align-items:center;gap:10px;margin:9px 0}` +
		`.lbl{width:132px;color:var(--muted);font-size:13px;flex:none}` +
		`.bar{flex:1;height:8px;border-radius:999px;background:var(--line);overflow:hidden}` +
		`.fill{height:100%;border-radius:999px}.val{width:118px;text-align:right;font-variant-numeric:tabular-nums;font-size:13px;flex:none}` +
		`.err{color:var(--bad)}</style></head><body>` + body + `</body></html>`
	return map[string]any{
		"StatusCode": status,
		"Headers":    map[string][]string{"Content-Type": {"text/html; charset=utf-8"}},
		"Body":       []byte(page),
	}
}

// renderQuotaPage lists the credentials and reads nothing.
//
// It used to query every credential while rendering. With 48 credentials that
// was 48 tokens hitting one chatgpt.com endpoint from one address inside a
// single second, every time the page was opened. The relay itself is built so
// that credentials do not look like each other or move together; a management
// page that sweeps the whole inventory on load undoes that from the side, and
// it does it on exactly the endpoint most likely to be watched.
//
// Reading is now something the operator asks for, one credential at a time,
// and the answer is filed in the ledger on the way back.
func (s *Service) renderQuotaPage() string {
	var page strings.Builder
	page.WriteString(quotaPageStyle)
	page.WriteString(`<h1>Basis Points 额度</h1>`)
	page.WriteString(`<p class="sub">打开这一页不会查任何东西。点卡片上的「刷新额度」才去问上游，` +
		`一次只问那一个号；读到的数会同时写回账本，认证文件页上的美元估算跟着更新。<br>` +
		`「依次刷新全部」是一个一个来的，中间留了随机间隔——几十个号在同一秒一起查，` +
		`本身就是个能把它们认成一伙的信号。</p>`)

	var list struct {
		Files []struct {
			AuthIndex string `json:"auth_index"`
			Name      string `json:"name"`
			Label     string `json:"label"`
			Provider  string `json:"provider"`
			Type      string `json:"type"`
		} `json:"files"`
	}
	if err := s.call("host.auth.list", map[string]any{}, &list); err != nil {
		return page.String() + `<p class="err">无法读取凭据列表：` + html.EscapeString(safeError(err)) + `</p>`
	}

	var cards strings.Builder
	rendered := 0
	for _, file := range list.Files {
		if !strings.EqualFold(file.Provider, Provider) && !strings.EqualFold(file.Type, Provider) {
			continue
		}
		rendered++
		cards.WriteString(s.renderCredentialCard(file.AuthIndex, file.Name, file.Label))
	}
	if rendered == 0 {
		page.WriteString(`<p class="sub">没有找到 ` + Provider + ` 类型的凭据。</p>`)
		return page.String()
	}
	page.WriteString(`<div class="toolbar"><button id="all" class="btn" type="button">依次刷新全部</button>` +
		`<span id="allnote" class="muted">` + itoa(int64(rendered)) + ` 个号</span></div>`)
	page.WriteString(cards.String())
	page.WriteString(`<script>` + quotaPageScript + `</script>`)
	return page.String()
}

// renderCredentialCard draws what is known without going upstream: the name,
// the plan recorded in the credential, and when its token expires. The quota
// itself stays blank until asked for.
func (s *Service) renderCredentialCard(authIndex, name, label string) string {
	var card strings.Builder
	card.WriteString(`<div class="card" data-file="` + html.EscapeString(name) + `">`)
	card.WriteString(`<div class="name">` + html.EscapeString(displayName(name, label)) + `</div>`)

	meta := ""
	var record struct {
		JSON json.RawMessage `json:"json"`
	}
	if err := s.call("host.auth.get", map[string]any{"auth_index": authIndex}, &record); err != nil {
		meta = "读取凭据失败：" + safeError(err)
	} else if c, parseErr := parseCredential(record.JSON); parseErr != nil {
		meta = "凭据无法解析"
	} else {
		meta = "套餐 " + orDash(credentialPlan(c))
		if c.AccountID != "" {
			meta += " · 账号 " + c.AccountID
		}
		if !c.ExpiresAt.IsZero() {
			meta += " · 令牌 " + humanDuration(time.Until(c.ExpiresAt)) + "后过期"
		}
	}
	card.WriteString(`<div class="meta">` + html.EscapeString(meta) + `</div>`)
	card.WriteString(`<div class="rows"></div>`)
	card.WriteString(`<div class="foot"><button class="btn refresh" type="button">刷新额度</button>` +
		`<span class="state muted">还没有读过</span></div>`)
	card.WriteString(`</div>`)
	return card.String()
}

const quotaPageStyle = `<style>` +
	// The page frame already uses .bar for a progress track, so the toolbar
	// gets its own name rather than quietly restyling every meter on the page.
	`.toolbar{display:flex;align-items:center;gap:10px;margin:0 0 14px}` +
	`.btn{font:inherit;font-size:13px;padding:5px 12px;border-radius:7px;cursor:pointer;` +
	`border:1px solid var(--line);background:transparent;color:inherit}` +
	`.btn:hover:not(:disabled){border-color:var(--muted)}.btn:disabled{cursor:default;opacity:.5}` +
	`.foot{display:flex;align-items:center;gap:10px;margin-top:12px}` +
	`.muted{color:var(--muted);font-size:12px}.state.bad{color:var(--bad)}.state.good{color:var(--ok)}` +
	`</style>`

// quotaPageScript fills a card from the single-credential route.
//
// Refresh-all walks the cards one at a time with a randomised gap. Spacing
// them is the whole point: the figures would arrive just as well in parallel,
// and arriving in parallel is the problem.
const quotaPageScript = `
(function(){
  "use strict";
  function q(el,sel){return el.querySelector(sel);}
  function cards(){return Array.prototype.slice.call(document.querySelectorAll(".card[data-file]"));}
  function setState(card,text,cls){
    var el=q(card,".state"); if(!el)return;
    el.textContent=text; el.className="state muted"+(cls?" "+cls:"");
  }
  function bar(label,used,note){
    var remaining=Math.max(0,100-used);
    var colour=remaining<=10?"var(--bad)":(remaining<=30?"var(--warn)":"var(--ok)");
    var row=document.createElement("div"); row.className="row";
    var l=document.createElement("div"); l.className="lbl"; l.textContent=label; row.appendChild(l);
    var track=document.createElement("div"); track.className="bar";
    var fill=document.createElement("div"); fill.className="fill";
    fill.style.width=remaining.toFixed(1)+"%"; fill.style.background=colour;
    track.appendChild(fill); row.appendChild(track);
    var v=document.createElement("div"); v.className="val";
    v.textContent="剩余 "+Math.round(remaining)+"%"+(note?" · "+note:""); row.appendChild(v);
    return row;
  }
  function until(iso){
    if(!iso)return "";
    var at=Date.parse(iso); if(!isFinite(at))return "";
    var mins=Math.round((at-Date.now())/60000);
    if(mins<=0)return "即将重置";
    if(mins<60)return mins+" 分钟后重置";
    var hours=mins/60;
    if(hours<48)return hours.toFixed(hours<10?1:0)+" 小时后重置";
    return Math.round(hours/24)+" 天后重置";
  }
  function fill(card,data){
    var rows=q(card,".rows"); while(rows.firstChild)rows.removeChild(rows.firstChild);
    if(!data||!isFinite(Number(data.used_percent))){
      setState(card,data&&data.error?data.error:"没有读到额度窗口","bad"); return;
    }
    var label="额度";
    var secs=Number(data.window_secs);
    if(isFinite(secs)&&secs>0){
      var days=Math.round(secs/86400), hours=Math.round(secs/3600);
      label=days>=1?("额度（"+days+" 天）"):("额度（"+hours+" 小时）");
    }
    rows.appendChild(bar(label,Number(data.used_percent),until(data.reset_at)));
    var note="已读到";
    if(data.limit_reached)note+="（已触发限流）";
    note+=data.noted?"，已写回账本":"，账本里没有这个号，只显示不写回";
    setState(card,note,data.limit_reached?"bad":"good");
  }
  function refresh(card){
    var file=card.getAttribute("data-file"), btn=q(card,".refresh");
    btn.disabled=true; var was=btn.textContent; btn.textContent="正在读…";
    setState(card,"正在问上游…","");
    return fetch("quota?format=json&file="+encodeURIComponent(file),
                 {cache:"no-store",credentials:"same-origin"})
      .then(function(r){return r.json().then(function(j){return {ok:r.ok,body:j};},
                                            function(){return {ok:false,body:{error:"返回的不是 JSON"}};});})
      .then(function(x){ if(x.ok)fill(card,x.body); else setState(card,(x.body&&x.body.error)||"读取失败","bad"); })
      .catch(function(e){ setState(card,"读取失败："+(e&&e.message?e.message:e),"bad"); })
      .then(function(){ btn.disabled=false; btn.textContent=was; });
  }
  function refreshAll(){
    var all=cards(), i=0, btn=document.getElementById("all"), note=document.getElementById("allnote");
    if(!all.length)return;
    btn.disabled=true;
    (function step(){
      if(i>=all.length){ btn.disabled=false; btn.textContent="依次刷新全部"; note.textContent=all.length+" 个号，刚刷完"; return; }
      note.textContent="正在刷新 "+(i+1)+" / "+all.length+"（一个一个来）";
      refresh(all[i++]).then(function(){ setTimeout(step,1200+Math.floor(Math.random()*1400)); });
    })();
  }
  document.addEventListener("click",function(e){
    var btn=e.target&&e.target.closest?e.target.closest(".refresh"):null;
    if(btn){ var card=btn.closest(".card[data-file]"); if(card)refresh(card); return; }
    if(e.target&&e.target.id==="all")refreshAll();
  });
})();
`

func humanUntil(d time.Duration) string {
	if d <= 0 {
		return "即将重置"
	}
	return humanDuration(d) + "后重置"
}

func humanDuration(d time.Duration) string {
	if d <= 0 {
		return "0 分钟"
	}
	if hours := int64(d / time.Hour); hours >= 24 {
		return itoa(hours/24) + " 天"
	} else if hours >= 1 {
		return itoa(hours) + " 小时"
	}
	return itoa(int64(d/time.Minute)) + " 分钟"
}

func displayName(name, label string) string {
	name = strings.TrimSuffix(strings.TrimSpace(name), ".json")
	label = strings.TrimSpace(label)
	switch {
	case name != "" && label != "" && !strings.EqualFold(name, label):
		return name + "（" + label + "）"
	case name != "":
		return name
	case label != "":
		return label
	}
	return "未命名凭据"
}

func orDash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "—"
	}
	return value
}
