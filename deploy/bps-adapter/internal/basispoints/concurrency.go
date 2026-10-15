package basispoints

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Per-credential concurrency.
//
// CPA can cap how many requests one credential serves at once, but only under
// Home, its remote control plane: CredentialConcurrencyConfig is documented as
// "managed by Home", and a standalone gateway has no local limiter at all.
// Without one, fill-first routing piles every request onto the first credential
// until the upstream answers "429: Rate limit exceeded" -- six of the last
// eight failures on this gateway before the cap existed.
//
// So the cap is enforced here, which every Basis Points request passes through.
// A credential already at its cap refuses the request with a marker that the
// rule authData attaches to every credential recognises as "continue": CPA then
// tries the next credential without cooling this one down, where an ordinary
// 429 would have benched it.

// concurrencyLimitMarker is matched by the request-scoped rule in authData.
const concurrencyLimitMarker = "basispoints_concurrency_limit"

// maxConcurrencyLimit bounds a configured cap to something meaningful.
const maxConcurrencyLimit = 10000

type concurrencyGate struct {
	mu       sync.Mutex
	inFlight map[string]int
}

func newConcurrencyGate() *concurrencyGate {
	return &concurrencyGate{inFlight: map[string]int{}}
}

// acquire claims a slot for key when it has room; a limit of zero or less means
// no cap. The returned release may be called any number of times and frees the
// slot exactly once, so every exit path can call it without bookkeeping.
func (g *concurrencyGate) acquire(key string, limit int) (func(), bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if limit > 0 && g.inFlight[key] >= limit {
		return nil, false
	}
	g.inFlight[key]++
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			defer g.mu.Unlock()
			if g.inFlight[key] <= 1 {
				delete(g.inFlight, key)
				return
			}
			g.inFlight[key]--
		})
	}, true
}

func (g *concurrencyGate) current(key string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.inFlight[key]
}

func concurrencyLimitError(limit int) error {
	return fail(http.StatusTooManyRequests, concurrencyLimitMarker,
		fmt.Sprintf("%s: this credential is already serving %d concurrent requests", concurrencyLimitMarker, limit))
}

// earlyCapBodyBytes is the body size from which the per-credential cap is
// looked at before the body is parsed.
const earlyCapBodyBytes = 8 << 20

// fullBeforeDecoding answers a large request that landed on a credential
// already at its cap, before any of the body is parsed.
//
// The cap is claimed only after prepareRequest, so that a malformed request
// never holds a slot. For an ordinary request that order costs nothing. For
// one carrying tens of megabytes of images it means parsing the body and
// decoding, hashing and stashing every image, only to be told the credential
// is busy, after which the host sends the same body to the next credential and
// it is all done again. On 2026-10-06, with routing filling one credential
// before the next, 1,567 of 35,097 requests were bounced this way in three
// and a half hours, some of them four times.
//
// This is only a look, not a claim: the slot is still acquired in the usual
// place, and a request that passes here can still be bounced there if the
// credential fills in between. The one visible difference is that a large
// request which is also malformed is told "busy" by a full credential before
// it is told what is wrong with it by the next one.
func (s *Service) fullBeforeDecoding(request ExecutorRequest) error {
	if len(request.OriginalRequest) < earlyCapBodyBytes && len(request.Payload) < earlyCapBodyBytes {
		return nil
	}
	c, err := credentialFromExecutor(request)
	if err != nil {
		return nil // prepareRequest reports it
	}
	limit, _ := s.config().concurrencyLimit(c.Email, stringValue(request.AuthMetadata["file_name"]))
	if limit > 0 && s.gate.current(concurrencyKey(request, c)) >= limit {
		return concurrencyLimitError(limit)
	}
	return nil
}

// concurrencyKey identifies the credential a request runs on. It is the id
// authData gives the auth record, so the status page, which only knows file
// names, can derive the same key with credentialID.
func concurrencyKey(request ExecutorRequest, c credential) string {
	if key := strings.TrimSpace(request.AuthID); key != "" {
		return key
	}
	if name := strings.TrimSpace(stringValue(request.AuthMetadata["file_name"])); name != "" {
		return credentialID(name)
	}
	return "account:" + c.AccountID
}

// concurrencyRequestScopedRule lets CPA route around a credential that is at
// its cap without cooling it down. Genuine upstream 429s do not carry the
// marker, so they keep their ordinary cooldown.
func concurrencyRequestScopedRule() map[string]any {
	return map[string]any{
		"status": http.StatusTooManyRequests,
		"match":  []string{concurrencyLimitMarker},
		"action": "continue",
	}
}

// Status page.
//
// Resource routes are not management-authenticated, so the page itself only
// reads: it shows live counts and the caps. Editing a cap saves through CPA's
// management API with the key typed at save time (see renderConcurrencyPage),
// the same authenticated path the control panel's plugin configuration uses,
// applied live through plugin.reconfigure without restarting anything. The
// gateway port is bound to localhost and the reverse proxy publishes only the
// image relay route, so the page is not reachable from outside the host either.

const (
	concurrencyPagePath = "concurrency"
	concurrencyDataPath = "concurrency-data"
)

type concurrencyRow struct {
	Label    string `json:"label"`
	File     string `json:"file"`
	Key      string `json:"key"`
	InFlight int    `json:"in_flight"`
	Limit    int    `json:"limit"`
	Override bool   `json:"override"`
}

type concurrencySnapshot struct {
	Rows          []concurrencyRow `json:"rows"`
	TotalInFlight int              `json:"total_in_flight"`
	TotalLimit    int              `json:"total_limit"`
	Unlimited     bool             `json:"unlimited"`
	DefaultLimit  int              `json:"default_limit"`
	At            string           `json:"at"`
	Error         string           `json:"error,omitempty"`
}

func (s *Service) concurrencySnapshot() concurrencySnapshot {
	cfg := s.config()
	snapshot := concurrencySnapshot{
		DefaultLimit: cfg.MaxConcurrencyPerCredential,
		At:           time.Now().Format("2006-01-02 15:04:05"),
	}
	var list struct {
		Files []struct {
			Name     string `json:"name"`
			Label    string `json:"label"`
			Provider string `json:"provider"`
			Type     string `json:"type"`
		} `json:"files"`
	}
	if err := s.call("host.auth.list", map[string]any{}, &list); err != nil {
		snapshot.Error = safeError(err)
		return snapshot
	}
	for _, file := range list.Files {
		if !strings.EqualFold(file.Provider, Provider) && !strings.EqualFold(file.Type, Provider) {
			continue
		}
		limit, override := cfg.concurrencyLimit(file.Label, file.Name)
		row := concurrencyRow{
			Label:    displayName(file.Name, file.Label),
			File:     file.Name,
			Key:      cfg.concurrencyOverrideKey(file.Label, file.Name),
			InFlight: s.gate.current(credentialID(file.Name)),
			Limit:    limit,
			Override: override,
		}
		snapshot.Rows = append(snapshot.Rows, row)
		snapshot.TotalInFlight += row.InFlight
		if limit <= 0 {
			snapshot.Unlimited = true
		} else {
			snapshot.TotalLimit += limit
		}
	}
	sort.Slice(snapshot.Rows, func(i, j int) bool { return snapshot.Rows[i].Label < snapshot.Rows[j].Label })
	return snapshot
}

func (s *Service) serveConcurrencyData() map[string]any {
	return map[string]any{
		"StatusCode": http.StatusOK,
		"Headers": map[string][]string{
			"Content-Type":  {"application/json; charset=utf-8"},
			"Cache-Control": {"no-store"},
		},
		"Body": jsonBytes(s.concurrencySnapshot()),
	}
}

// renderConcurrencyPage renders the current state server-side, so the page is
// complete even before its script runs, and then refreshes it in place.
//
// Caps are edited on the page itself. Saving goes through CPA's own management
// API (/v0/management/plugins/<id>/config) with the management key typed at
// save time: the write is authenticated there, persisted to config.yaml with
// its comments, and applied live through plugin.reconfigure. The key is held
// only for that one save and never stored.
func (s *Service) renderConcurrencyPage() string {
	snapshot := s.concurrencySnapshot()
	initial, _ := json.Marshal(snapshot)
	api, _ := json.Marshal("/v0/management/plugins/" + PluginID + "/config")
	var page strings.Builder
	page.WriteString(concurrencyPageStyle)
	page.WriteString(`<h1>Basis Points 并发</h1>`)
	page.WriteString(`<p class="sub">每个凭据同时在处理的请求数,每 2 秒自动刷新。` +
		`可以直接在下面修改上限,点"保存修改"后立即生效,无需重启。` +
		`某个凭据满了之后,请求会自动转给下一个有空位的凭据,不会让它进入冷却。</p>`)
	page.WriteString(concurrencyToolbar)
	page.WriteString(`<div id="summary" class="meta"></div><div id="rows"></div>`)
	page.WriteString(concurrencyKeyDialog)
	page.WriteString(`<script>var CONFIG_API=` + string(api) + `;` + concurrencyPageScript + `render(` + string(initial) + `);</script>`)
	return page.String()
}

const concurrencyPageStyle = `<style>` +
	`.toolbar{display:flex;flex-wrap:wrap;align-items:center;gap:10px;margin:0 0 14px}` +
	`input,button{font:inherit;color:inherit;background:transparent;border:1px solid var(--line);border-radius:6px;padding:4px 8px}` +
	`input[type=number]{width:76px}input[type=checkbox]{padding:0}button{cursor:pointer}` +
	`button.primary{background:var(--ok);border-color:var(--ok);color:#fff}#q{width:200px}` +
	`.name label{cursor:pointer}.muted{color:var(--muted);font-size:12px}.edited{color:var(--warn);font-size:12px}` +
	`.reset{font-size:12px;color:var(--muted)}#status{font-size:13px}#status.bad{color:var(--bad)}#status.good{color:var(--ok)}` +
	`.modal{position:fixed;inset:0;background:rgba(0,0,0,.45);display:flex;align-items:center;justify-content:center}` +
	`.modal[hidden]{display:none}.box{background:var(--bg);border:1px solid var(--line);border-radius:10px;padding:18px;width:340px}` +
	`.box input{width:100%;box-sizing:border-box;margin:10px 0}` +
	`</style>`

const concurrencyToolbar = `<div class="toolbar">` +
	`<input id="q" type="search" placeholder="搜索邮箱">` +
	`<label>默认上限 <input id="def" type="number" min="0" max="10000"></label>` +
	`<label><input id="all" type="checkbox"> 全选</label>` +
	`<label>选中的设为 <input id="bulk" type="number" min="0" max="10000"></label>` +
	`<button id="applyBulk" type="button">应用</button>` +
	`<button id="resetBulk" type="button">选中的恢复默认</button>` +
	`<button id="save" type="button" class="primary">保存修改</button>` +
	`<span id="status"></span></div>`

const concurrencyKeyDialog = `<div id="auth" class="modal" hidden><div class="box">` +
	`<div><b>输入管理密钥以保存</b></div>` +
	`<div class="muted">只用于这一次保存,不会被记住。连续输错 5 次,这台机器会被 CPA 锁定 30 分钟。</div>` +
	`<input id="key" type="password" autocomplete="off">` +
	`<button id="ok" type="button" class="primary">确认保存</button> <button id="cancel" type="button">取消</button>` +
	`</div></div>`

const concurrencyPageScript = `
function esc(v){return String(v==null?"":v).replace(/[&<>"']/g,function(c){return {"&":"&amp;","<":"&lt;",">":"&gt;","\"":"&quot;","'":"&#39;"}[c];});}
function colour(ratio){return ratio>=1?"var(--bad)":ratio>=0.8?"var(--warn)":"var(--ok)";}
var S={keys:"",rows:[],def:0,edits:{},defEdit:null,selected:{}};
function $(id){return document.getElementById(id);}
function keyOf(x){return x.key||x.file;}
function valid(v){return /^\d+$/.test(v)&&parseInt(v,10)<=10000;}
function setStatus(text,cls){var e=$("status");e.textContent=text;e.className=cls||"";}
function cards(){return document.querySelectorAll("#rows .card");}
function build(){
  var h="";
  for(var i=0;i<S.rows.length;i++){
    h+='<div class="card"><div class="name"><label><input type="checkbox" class="sel"> '+esc(S.rows[i].label)+'</label></div>'+
       '<div class="row"><div class="lbl">并发</div><div class="bar"><div class="fill"></div></div><div class="val"></div></div>'+
       '<div class="row"><div class="lbl">上限</div><input class="lim" type="number" min="0" max="10000"> '+
       '<a href="#" class="reset">恢复默认</a> <span class="note"></span></div></div>';
  }
  $("rows").innerHTML=h;
  var list=cards();
  for(var j=0;j<list.length;j++)(function(card,key){
    var sel=card.querySelector(".sel"),lim=card.querySelector(".lim");
    sel.checked=!!S.selected[key];
    sel.onchange=function(){S.selected[key]=sel.checked;};
    lim.oninput=function(){S.edits[key]=lim.value.trim();paint();};
    card.querySelector(".reset").onclick=function(e){e.preventDefault();S.edits[key]="";lim.value="";paint();};
  })(list[j],keyOf(S.rows[j]));
  filter();
}
function paint(){
  var list=cards();
  for(var j=0;j<list.length;j++){
    var x=S.rows[j],key=keyOf(x),card=list[j],lim=x.limit>0?x.limit:0,ratio=lim?x.in_flight/lim:0;
    var fill=card.querySelector(".fill");
    fill.style.width=(lim?Math.min(100,ratio*100):0).toFixed(1)+"%";fill.style.background=colour(ratio);
    card.querySelector(".val").innerHTML="<b>"+x.in_flight+"</b> / "+(lim?lim:"不限");
    var input=card.querySelector(".lim"),pending=Object.prototype.hasOwnProperty.call(S.edits,key);
    if(!pending&&document.activeElement!==input){input.value=x.override?String(x.limit):"";}
    input.placeholder="默认 "+(S.def>0?S.def:"不限");
    var note=card.querySelector(".note");
    if(pending){note.className="note edited";note.textContent=S.edits[key]===""?"恢复默认,未保存":valid(S.edits[key])?"已修改,未保存":"请填 0–10000 的整数";}
    else{note.className="note muted";note.textContent=x.override?"单独设置":"";}
  }
}
function filter(){
  var q=$("q").value.trim().toLowerCase(),list=cards();
  for(var j=0;j<list.length;j++){list[j].style.display=!q||String(S.rows[j].label).toLowerCase().indexOf(q)>=0?"":"none";}
}
function render(d){
  var s=$("summary");
  if(d.error){s.innerHTML='<span class="err">读取凭据失败:'+esc(d.error)+'</span>';return;}
  S.rows=d.rows||[];S.def=d.default_limit;
  if(S.defEdit===null&&document.activeElement!==$("def")){$("def").value=String(d.default_limit);}
  var cap=d.unlimited?"不限":d.total_limit;
  s.innerHTML="合计在途 <b>"+d.total_in_flight+"</b> / 总容量 <b>"+cap+"</b> · 默认上限 "+(d.default_limit>0?d.default_limit:"不限")+" · 更新于 "+esc(d.at)+
    (d.unlimited?"":'<br>建议把主站里 CPA 账号的并发设为 <b>'+d.total_limit+'</b>,超出部分由主站排队,而不是压到这里被拒。');
  if(!S.rows.length){$("rows").innerHTML='<p class="sub">没有找到凭据。</p>';S.keys="";return;}
  var keys=S.rows.map(keyOf).join("\n");
  if(keys!==S.keys){S.keys=keys;build();}
  paint();
}
function selectedRows(){var out=[];for(var j=0;j<S.rows.length;j++){if(S.selected[keyOf(S.rows[j])])out.push(j);}return out;}
function fillSelected(value,label){
  var picked=selectedRows(),list=cards();
  if(!picked.length){setStatus("先勾选要修改的凭据","bad");return;}
  picked.forEach(function(j){S.edits[keyOf(S.rows[j])]=value;list[j].querySelector(".lim").value=value;});
  paint();setStatus("已给 "+picked.length+" 个凭据"+label+",点保存修改后生效","");
}
$("q").oninput=filter;
$("def").oninput=function(){var v=this.value.trim();S.defEdit=v===String(S.def)?null:v;};
$("all").onchange=function(){
  var on=this.checked,list=cards();
  for(var j=0;j<list.length;j++){if(list[j].style.display==="none")continue;S.selected[keyOf(S.rows[j])]=on;list[j].querySelector(".sel").checked=on;}
};
$("applyBulk").onclick=function(){var v=$("bulk").value.trim();if(!valid(v)){setStatus("请填 0–10000 的整数","bad");return;}fillSelected(v,"填入 "+v);};
$("resetBulk").onclick=function(){fillSelected("","设为恢复默认");};
$("save").onclick=function(){
  var keys=Object.keys(S.edits);
  if(!keys.length&&S.defEdit===null){setStatus("没有需要保存的修改","");return;}
  for(var i=0;i<keys.length;i++){if(S.edits[keys[i]]!==""&&!valid(S.edits[keys[i]])){setStatus("有上限不是 0–10000 的整数,请先改正","bad");return;}}
  if(S.defEdit!==null&&!valid(S.defEdit)){setStatus("默认上限请填 0–10000 的整数","bad");return;}
  $("key").value="";$("auth").hidden=false;$("key").focus();
};
$("cancel").onclick=function(){$("key").value="";$("auth").hidden=true;};
$("ok").onclick=function(){var k=$("key").value;$("key").value="";$("auth").hidden=true;if(!k){setStatus("没有输入管理密钥","bad");return;}save(k);};
$("key").onkeydown=function(e){if(e.key==="Enter")$("ok").click();if(e.key==="Escape")$("cancel").click();};
function failure(r){
  return r.json().catch(function(){return {};}).then(function(j){
    var m=(j&&(j.error||j.message))||("HTTP "+r.status);
    if(r.status===401)m="管理密钥不正确(连续错 5 次会被锁 30 分钟)";
    throw new Error(m);
  });
}
function save(k){
  var edits=JSON.parse(JSON.stringify(S.edits)),defEdit=S.defEdit,auth="Bearer "+k;
  setStatus("正在保存…","");
  fetch(CONFIG_API,{headers:{"Authorization":auth},cache:"no-store"}).then(function(r){return r.ok?r.json():failure(r);}).then(function(cfg){
    var cur=cfg&&cfg.credential_concurrency&&typeof cfg.credential_concurrency==="object"?cfg.credential_concurrency:{},next={};
    Object.keys(cur).forEach(function(c){next[c]=cur[c];});
    Object.keys(edits).forEach(function(key){
      Object.keys(next).forEach(function(c){if(c.toLowerCase()===key.toLowerCase())delete next[c];});
      if(edits[key]!=="")next[key]=parseInt(edits[key],10);
    });
    var body={credential_concurrency:next};
    if(defEdit!==null)body.max_concurrency_per_credential=parseInt(defEdit,10);
    return fetch(CONFIG_API,{method:"PATCH",headers:{"Authorization":auth,"Content-Type":"application/json"},body:JSON.stringify(body)});
  }).then(function(r){return r.ok?r:failure(r);}).then(function(){
    Object.keys(edits).forEach(function(key){if(S.edits[key]===edits[key])delete S.edits[key];});
    if(S.defEdit===defEdit)S.defEdit=null;
    setStatus("已保存,立即生效","good");setTimeout(tick,1500);
  }).catch(function(e){setStatus("保存失败:"+e.message,"bad");});
}
function tick(){fetch("concurrency-data",{cache:"no-store"}).then(function(x){return x.json();}).then(render).catch(function(){});}
setInterval(tick,2000);
`

// concurrencyHTML reuses the quota page's frame so both pages look alike.
func concurrencyHTML(body string) map[string]any {
	response := managementHTML(http.StatusOK, body)
	response["Headers"] = map[string][]string{
		"Content-Type":  {"text/html; charset=utf-8"},
		"Cache-Control": {"no-store"},
	}
	return response
}
