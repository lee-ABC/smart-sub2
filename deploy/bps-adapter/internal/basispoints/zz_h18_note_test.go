package basispoints

import (
	"encoding/json"
	"testing"
)

// h18：在面板上给一个号写的备注，必须能回到卡片上。
//
// 面板把备注存进凭据文件，卡片上的备注来自管理列表，列表读的是记录的 Metadata。
// 宿主自己解析的文件，宿主会把这个键从文件抄进记录；插件认领的文件不会——记录的
// Metadata 就是这里交回去的那一份。2026-10-06 的现象正是这样：保存返回 200，文件里
// 有 "note"，卡片上永远没有。

func noteMetadata(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	c, err := parseCredential(raw)
	if err != nil {
		t.Fatal(err)
	}
	metadata, ok := authData(raw, "a.json", c, Provider)["Metadata"].(map[string]any)
	if !ok {
		t.Fatal("记录里没有 Metadata")
	}
	return metadata
}

func TestAnOperatorNoteReachesTheRecord(t *testing.T) {
	raw := []byte(`{"note":"  长效TEAM ","access_token":"x","account_id":"acct","email":"a@example.com"}`)
	got, ok := noteMetadata(t, raw)["note"].(string)
	if !ok {
		t.Fatal("文件里有备注，交回宿主的记录里却没有 note 这个键，卡片上就不会显示")
	}
	if got != "长效TEAM" {
		t.Errorf("note = %q，应当是去掉首尾空白后的 %q", got, "长效TEAM")
	}
}

// 文件里从来没有这个键，记录里就不要它：null、类型不对都算没有，文件损坏也不能编出一个来。
func TestNoNoteMeansNoKey(t *testing.T) {
	cases := map[string][]byte{
		"根本没这个字段": []byte(`{"access_token":"x","account_id":"a"}`),
		"是 null":  []byte(`{"note":null,"access_token":"x","account_id":"a"}`),
		"类型不对":    []byte(`{"note":42,"access_token":"x","account_id":"a"}`),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if value, has := noteMetadata(t, raw)["note"]; has {
				t.Errorf("没有备注的号多出了 note = %#v", value)
			}
		})
	}
	if note, has := credentialNote([]byte(`{{{`)); has || note != "" {
		t.Errorf("损坏的文件读出了备注 %q（has=%v）", note, has)
	}
}

// 清空过的备注要原样交回去一个空串，不能把键省掉。
//
// 续期结束时带回来的是它开始那一刻的文件。宿主存盘时拿记录的 Metadata 盖在这份旧文件
// 上，只有 Metadata 里在的键才盖得住：键省掉了，续期途中刚清空的备注就原样回来——隔离
// 实例里实测如此（清空「待清空」，续期一结束，文件和卡片上又是「待清空」）。
func TestAClearedNoteIsPassedOnAsEmpty(t *testing.T) {
	cases := map[string][]byte{
		"空串":   []byte(`{"note":"","access_token":"x","account_id":"a"}`),
		"只有空白": []byte(`{"note":"   ","access_token":"x","account_id":"a"}`),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			value, has := noteMetadata(t, raw)["note"]
			if !has {
				t.Fatal("清空过的备注没有交回去，续期会把旧备注带回来")
			}
			if value != "" {
				t.Errorf("note = %#v，应当是空串", value)
			}
		})
	}
}

// 加这个键不能动到别的：同一个号带不带备注，交回去的记录只差 note 这一个键。
// 没人写过备注的号因此和以前逐键相同——扩展导入时的去重靠的就是列表里这些条目的形状。
func TestTheNoteIsTheOnlyDifference(t *testing.T) {
	plain := noteMetadata(t, []byte(`{"access_token":"x","account_id":"a","email":"a@example.com"}`))
	noted := noteMetadata(t, []byte(`{"note":"n","access_token":"x","account_id":"a","email":"a@example.com"}`))
	if len(noted) != len(plain)+1 {
		t.Errorf("带备注的记录有 %d 个键，不带的有 %d 个，应当恰好多一个", len(noted), len(plain))
	}
	for key, value := range plain {
		if string(jsonBytes(value)) != string(jsonBytes(noted[key])) {
			t.Errorf("带上备注之后 %q 变了：%s -> %s", key, jsonBytes(value), jsonBytes(noted[key]))
		}
	}
}

// 续期之后备注要还在：记录里有，宿主接下来写回文件的那一份里也有，而且令牌确实换成了新的。
func TestANoteSurvivesATokenRefresh(t *testing.T) {
	service := NewService()
	service.SetHost(func(method string, payload any, out any) error {
		if method != "host.http.do" {
			return nil
		}
		if response, ok := out.(*upstreamResponse); ok {
			response.StatusCode = 200
			response.Body = []byte(`{"access_token":"eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiJmaXh0dXJlLXVzZXIiLCJjbGllbnRfaWQiOiJhcHBfRU1vYW1FRVo3M2YwQ2tYYVhwN2hyYW5uIiwiZXhwIjo0MTAyNDQ0ODAwLCJ2ZXJzaW9uIjoyLCJodHRwczovL2FwaS5vcGVuYWkuY29tL2F1dGgiOnsiY2hhdGdwdF9hY2NvdW50X2lkIjoiYWNjdCIsInVzZXJfaWQiOiJmaXh0dXJlLXVzZXIiLCJjaGF0Z3B0X3VzZXJfaWQiOiJmaXh0dXJlLXVzZXIiLCJjaGF0Z3B0X3BsYW5fdHlwZSI6InRlYW0ifX0.fixture-signature","refresh_token":"rt.2.fake","expires_in":864000}`)
		}
		return nil
	})
	raw, _ := json.Marshal(authRefreshRequest{
		AuthID:      "bp-a",
		StorageJSON: []byte(`{"note":"长效TEAM","access_token":"eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiJmaXh0dXJlLXVzZXIiLCJjbGllbnRfaWQiOiJhcHBfRU1vYW1FRVo3M2YwQ2tYYVhwN2hyYW5uIiwiZXhwIjo0MTAyNDQ0ODAwLCJ2ZXJzaW9uIjoxLCJodHRwczovL2FwaS5vcGVuYWkuY29tL2F1dGgiOnsiY2hhdGdwdF9hY2NvdW50X2lkIjoiYWNjdCIsInVzZXJfaWQiOiJmaXh0dXJlLXVzZXIiLCJjaGF0Z3B0X3VzZXJfaWQiOiJmaXh0dXJlLXVzZXIiLCJjaGF0Z3B0X3BsYW5fdHlwZSI6InRlYW0ifX0.fixture-signature","refresh_token":"rt.1.fake","account_id":"acct","email":"a@example.com"}`),
		Metadata:    map[string]any{"file_name": "a.json"},
	})
	response, err := service.authRefresh(raw)
	if err != nil {
		t.Fatal(err)
	}
	auth := objectValue(response["Auth"])
	if got := objectValue(auth["Metadata"])["note"]; got != "长效TEAM" {
		t.Errorf("续期之后记录里 note = %#v", got)
	}
	storage, _ := auth["StorageJSON"].([]byte)
	var stored map[string]any
	if err := json.Unmarshal(storage, &stored); err != nil {
		t.Fatalf("续期交回的 StorageJSON 不是 JSON：%v", err)
	}
	if stored["note"] != "长效TEAM" {
		t.Errorf("续期之后要写回文件的那一份里 note = %#v", stored["note"])
	}
	if stored["access_token"] != "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiJmaXh0dXJlLXVzZXIiLCJjbGllbnRfaWQiOiJhcHBfRU1vYW1FRVo3M2YwQ2tYYVhwN2hyYW5uIiwiZXhwIjo0MTAyNDQ0ODAwLCJ2ZXJzaW9uIjoyLCJodHRwczovL2FwaS5vcGVuYWkuY29tL2F1dGgiOnsiY2hhdGdwdF9hY2NvdW50X2lkIjoiYWNjdCIsInVzZXJfaWQiOiJmaXh0dXJlLXVzZXIiLCJjaGF0Z3B0X3VzZXJfaWQiOiJmaXh0dXJlLXVzZXIiLCJjaGF0Z3B0X3BsYW5fdHlwZSI6InRlYW0ifX0.fixture-signature" || stored["refresh_token"] != "rt.2.fake" {
		t.Errorf("这次续期没有真的换令牌（access=%v refresh=%v），上面两条等于没测", stored["access_token"], stored["refresh_token"])
	}
}
