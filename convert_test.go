package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

type transport func(*http.Request) (*http.Response, error)

func (f transport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func remoteReply(status int, headers http.Header, data string) *http.Response {
	return &http.Response{StatusCode: status, Header: headers, Body: io.NopCloser(strings.NewReader(data))}
}

func fixture(t *testing.T) (*batch, *int, *int) {
	t.Helper()
	s, e := openState(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(s.close)
	q := newQuark("session=fixture")
	q.interval = 0
	saves, shares := 0, 0
	q.http.Transport = transport(func(r *http.Request) (*http.Response, error) {
		path := strings.TrimPrefix(r.URL.Path, "/1/clouddrive")
		query := r.URL.Query()
		data := ""
		switch path {
		case "/share/mypage/detail":
			data = "{\"list\":[]}"
			if shares > 0 {
				data = "{\"list\":[{\"share_id\":\"own-id\",\"share_url\":\"https://pan.quark.cn/s/ownlink\",\"title\":\"Project\",\"fid_list\":[\"stored\"]}]}"
			}
		case "/share/sharepage/token":
			var input map[string]string
			var raw map[string]json.RawMessage
			if e := json.NewDecoder(r.Body).Decode(&raw); e != nil {
				t.Fatal(e)
			}
			input = map[string]string{"pwd_id": scalar(raw["pwd_id"]), "passcode": scalar(raw["passcode"])}
			if input["pwd_id"] == "original" && input["passcode"] != "A1b2" {
				t.Fatal("提取码没有传入转存流程")
			}
			data = "{\"stoken\":\"fixture-token\",\"title\":\"Project\"}"
		case "/share/sharepage/detail":
			if query.Get("pdir_fid") == "source-dir" {
				data = "{\"list\":[{\"fid\":\"nested-file\",\"file_name\":\"Readme.txt\",\"file\":true,\"size\":17}]}"
			} else {
				switch query.Get("pwd_id") {
				case "original", "ownlink":
					data = "{\"list\":[{\"fid\":\"source-dir\",\"file_name\":\"Project\",\"dir\":true,\"share_fid_token\":\"fixture-fid-token\"}]}"
				case "mixed":
					data = "{\"list\":[{\"fid\":\"one\",\"file_name\":\"A\",\"dir\":true},{\"fid\":\"two\",\"file_name\":\"B\",\"dir\":true}]}"
				case "oversized":
					data = "{\"list\":[{\"fid\":\"big\",\"file_name\":\"Big\",\"file\":true,\"size\":80}]}"
				case "unknown":
					data = "{\"list\":[{\"fid\":\"unknown\",\"file_name\":\"Unknown\",\"file\":true}]}"
				default:
					t.Fatalf("意外的源链接：%s", query.Get("pwd_id"))
				}
			}
		case "/file/sort":
			switch query.Get("pdir_fid") {
			case "0":
				data = "{\"list\":[{\"fid\":\"target\",\"file_name\":\"来自：分享\",\"file_type\":0}]}"
			case "target":
				data = "{\"list\":[]}"
			default:
				t.Fatalf("意外目标目录：%s", query.Get("pdir_fid"))
			}
		case "/share/sharepage/save":
			saves++
			var req struct {
				Target string   `json:"to_pdir_fid"`
				FIDs   []string `json:"fid_list"`
			}
			if e := json.NewDecoder(r.Body).Decode(&req); e != nil {
				t.Fatal(e)
			}
			if req.Target != "target" || !slices.Equal(req.FIDs, []string{"source-dir"}) {
				t.Fatalf("转存请求不匹配：%+v", req)
			}
			data = "{\"task_id\":\"save-task\"}"
		case "/share":
			shares++
			var req struct {
				FIDs []string `json:"fid_list"`
			}
			if e := json.NewDecoder(r.Body).Decode(&req); e != nil {
				t.Fatal(e)
			}
			if !slices.Equal(req.FIDs, []string{"stored"}) {
				t.Fatalf("应分享转存后文件夹：%+v", req)
			}
			data = "{\"task_id\":\"share-task\"}"
		case "/task":
			switch query.Get("task_id") {
			case "save-task":
				data = "{\"status\":2,\"save_as\":{\"save_as_top_fids\":[\"stored\"]}}"
			case "share-task":
				data = "{\"status\":2,\"share_id\":\"own-id\"}"
			default:
				t.Fatal("意外任务 ID")
			}
		case "/share/password":
			data = "{\"share_url\":\"https://pan.quark.cn/s/ownlink\"}"
		default:
			t.Fatalf("意外的调用：%s", r.URL.Path)
		}
		return remoteReply(200, nil, "{\"code\":0,\"data\":"+data+"}"), nil
	})
	return &batch{s: s, q: q, account: "fixture", limit: 17}, &saves, &shares
}

func TestConversionAttachmentsAreOptIn(t *testing.T) {
	for _, tc := range []struct {
		name     string
		attach   bool
		conflict bool
		retry    bool
	}{
		{name: "ordinary conversion"},
		{name: "copies multiple files before sharing", attach: true},
		{name: "leaves a conflicting file alone", attach: true, conflict: true},
		{name: "resumes an interrupted copy without submitting again", attach: true, retry: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, _, shares := fixture(t)
			original := b.q.http.Transport
			copies, templateReads := 0, 0
			polls := 0
			b.q.http.Transport = transport(func(req *http.Request) (*http.Response, error) {
				var data string
				path := strings.TrimPrefix(req.URL.Path, "/1/clouddrive")
				switch {
				case path == "/file/sort" && req.URL.Query().Get("pdir_fid") == "0":
					data = `{"list":[{"fid":"target","file_name":"来自：分享","file_type":0},{"fid":"templates","file_name":"常用附件","file_type":0}]}`
				case path == "/file/sort" && req.URL.Query().Get("pdir_fid") == "templates":
					templateReads++
					data = `{"list":[{"fid":"template1","file_name":"a.pdf","file":true,"size":5},{"fid":"template2","file_name":"b.txt","file":true,"size":2}]}`
				case path == "/file/sort" && req.URL.Query().Get("pdir_fid") == "stored":
					data = `{"list":[]}`
					if tc.conflict {
						data = `{"list":[{"fid":"user-added","file_name":"a.pdf","file":true,"size":8}]}`
					} else if copies > 0 {
						data = `{"list":[{"fid":"added1","file_name":"a.pdf","file":true,"size":5},{"fid":"added2","file_name":"b.txt","file":true,"size":2}]}`
					}
				case path == "/file/copy":
					if *shares != 0 || tc.conflict {
						t.Fatal("copy must precede sharing and avoid conflicts")
					}
					var payload struct {
						Target string   `json:"to_pdir_fid"`
						FIDs   []string `json:"filelist"`
					}
					if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
						t.Fatal(err)
					}
					if payload.Target != "stored" || !slices.Equal(payload.FIDs, []string{"template1", "template2"}) {
						t.Fatalf("unexpected copy request: %+v", payload)
					}
					copies++
					data = `{"task_id":"copy-task"}`
				case path == "/task" && req.URL.Query().Get("task_id") == "copy-task":
					polls++
					if tc.retry && polls == 1 {
						return remoteReply(http.StatusServiceUnavailable, nil, ""), nil
					}
					data = `{"status":2}`
				default:
					return original.RoundTrip(req)
				}
				return remoteReply(200, nil, `{"code":0,"data":`+data+`}`), nil
			})
			src := sources("https://pan.quark.cn/s/original?pwd=A1b2")[0]
			src.Attach = tc.attach
			result := b.one(context.Background(), src, false)
			if tc.retry {
				if result.Status != pending || *shares != 0 {
					t.Fatalf("copy should wait for verification: %+v", result)
				}
				src.Attach = false
				plain := b.one(context.Background(), src, false)
				if plain.Status != pending || polls != 1 || *shares != 0 {
					t.Fatalf("ordinary retry must not resume attachments: %+v", plain)
				}
				src.Attach = true
				result = b.one(context.Background(), src, false)
			}
			switch {
			case tc.conflict:
				if result.Status != failed || !strings.Contains(result.Message, "冲突") || result.Link != "" || copies != 0 || *shares != 0 {
					t.Fatalf("unexpected conflict result: %+v", result)
				}
			case tc.attach:
				if result.Status != created || !result.AttachmentsReady || templateReads != 1 || copies != 1 || *shares != 1 {
					t.Fatalf("unexpected attachment result: %+v", result)
				}
			default:
				if result.Status != created || result.AttachmentsReady || templateReads != 0 || copies != 0 || *shares != 1 {
					t.Fatalf("ordinary conversion attempted attachment work: %+v", result)
				}
			}
		})
	}
}

func TestFolderAndHistory(t *testing.T) {
	b, saves, shares := fixture(t)
	ctx := context.Background()
	input := sources("夸克分享 https://pan.quark.cn/s/original\n提取码 A1b2\nhttps://pan.quark.cn/s/original；https://pan.quark.cn/s/mixed\nhttps://pan.quark.cn/s/oversized\nhttps://pan.quark.cn/s/unknown")
	rows := b.convert(ctx, input, false, nil)
	if len(rows) != 4 {
		t.Fatalf("%+v", rows)
	}
	got := []string{rows[0].Status, rows[1].Status, rows[2].Status, rows[3].Status}
	if !slices.Equal(got, []string{created, manual, oversize, unknownSize}) || *saves != 1 || *shares != 1 {
		t.Fatalf("%+v, saves=%d shares=%d", rows, *saves, *shares)
	}
	if rows[0].Link != "https://pan.quark.cn/s/ownlink" || rows[0].Title != "Project" || *rows[0].Size != 17 || !strings.Contains(rows[0].Source, "pwd=A1b2") {
		t.Fatalf("%+v", rows[0])
	}
	b.loaded = false
	remote := b.q.http.Transport
	b.q.http.Transport = transport(func(req *http.Request) (*http.Response, error) {
		if strings.HasSuffix(req.URL.Path, "/share/mypage/detail") {
			t.Fatal("conversion should read the local share index")
		}
		return remote.RoundTrip(req)
	})
	row := b.one(ctx, input[0], false)
	if row.Status != reused || row.Title != rows[0].Title || row.Link != rows[0].Link || *saves != 1 || *shares != 1 {
		t.Fatalf("%+v", row)
	}
}

func TestCLIShareTemplate(t *testing.T) {
	const message = "夸克网盘给你分享了「{{.Title}}」\n链接：{{.URL}}"
	path := filepath.Join(t.TempDir(), "share.txt")
	if err := os.WriteFile(path, []byte("\ufeff"+message+"\r\n"), 0600); err != nil {
		t.Fatal(err)
	}
	row := result{Title: "党委会的工作方法.pdf", Link: "https://pan.quark.cn/s/example?pwd=A1b2", Status: reused}
	for _, filename := range []string{"", path} {
		text, ending := message, "\n"
		if filename != "" {
			text, ending = "", "\r\n"
		}
		tpl, err := loadShareTemplate(text, filename, false)
		if err != nil {
			t.Fatal(err)
		}
		var output strings.Builder
		for range 2 {
			if err = writeShareText(&output, tpl, row); err != nil {
				t.Fatal(err)
			}
		}
		want := "夸克网盘给你分享了「" + row.Title + "」\n链接：" + row.Link + ending
		if output.String() != want+want {
			t.Fatalf("unexpected template output %q", output.String())
		}
	}
	if _, err := loadShareTemplate("{{.Typo}}", "", false); err == nil {
		t.Fatal("unknown field should be rejected before conversion")
	}
	if _, err := loadShareTemplate(message, "", true); err == nil {
		t.Fatal("JSON and template flags should be mutually exclusive")
	}
	var output strings.Builder
	if err := writeShareText(&output, nil, row); err != nil || output.String() != row.Link+"\n" {
		t.Fatalf("default output changed: %q, %v", output.String(), err)
	}
}

func TestShareIndexKeepsLastCompleteSnapshot(t *testing.T) {
	b, _, _ := fixture(t)
	ctx := context.Background()
	seed := shareInfo{ID: "seed", URL: "https://pan.quark.cn/s/seed", FIDs: []string{"stored"}}
	if err := b.completeShare(ctx, "source", record{Phase: "complete", ShareID: seed.ID}, seed); err != nil {
		t.Fatal(err)
	}
	b.q.http.Transport = transport(func(req *http.Request) (*http.Response, error) {
		if req.URL.Query().Get("_page") == "1" {
			return remoteReply(200, nil, `{"code":0,"metadata":{"_total":1},"data":{"list":[{"share_id":"seed","title":"Synced"}]}}`), nil
		}
		t.Fatalf("unexpected page request: %s", req.URL.RawQuery)
		return nil, nil
	})
	list, err := b.refreshShares(ctx)
	if err != nil || len(list) != 1 || !slices.Equal(list[0].FIDs, seed.FIDs) {
		t.Fatalf("unexpected snapshot: %+v, %v", list, err)
	}
	b.q.http.Transport = transport(func(req *http.Request) (*http.Response, error) {
		if req.URL.Query().Get("_page") == "1" {
			return remoteReply(200, nil, `{"code":0,"metadata":{"_total":2},"data":{"list":[{"share_id":"partial"}]}}`), nil
		}
		return remoteReply(500, nil, ""), nil
	})
	if _, err = b.refreshShares(ctx); err == nil {
		t.Fatal("partial sync must fail")
	}
	b.loaded = false
	list, err = b.mine(ctx)
	if err != nil || len(list) != 1 || list[0].ID != seed.ID || list[0].Title != "Synced" {
		t.Fatalf("previous snapshot lost: %+v, %v", list, err)
	}
	other, err := b.s.indexedShares("some-other-account")
	if err != nil || len(other) != 0 {
		t.Fatalf("unexpected cross-account entries: %+v, %v", other, err)
	}
}

func TestCancelSharesValidatesOwnershipAndUpdatesIndex(t *testing.T) {
	b, _, _ := fixture(t)
	ctx := context.Background()
	accounts := map[string][]string{b.account: {"first", "second"}, "other": {"third"}}
	for account, ids := range accounts {
		for _, id := range ids {
			if err := storeShare(b.s.db, account, shareInfo{ID: id}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := b.s.set("share-index:"+b.account, true); err != nil {
		t.Fatal(err)
	}
	requested := []string{"first", "second"}
	requests := 0
	denied := true
	b.q.http.Transport = transport(func(req *http.Request) (*http.Response, error) {
		requests++
		var body struct {
			IDs []string `json:"share_ids"`
		}
		if req.Method != "POST" || !strings.HasSuffix(req.URL.Path, "/share/delete") || json.NewDecoder(req.Body).Decode(&body) != nil || !slices.Equal(body.IDs, requested) {
			t.Fatalf("unexpected cancellation request: %s %v", req.URL.Path, body.IDs)
		}
		if denied {
			return remoteReply(200, nil, `{"code":123,"status":400}`), nil
		}
		return remoteReply(200, nil, `{"code":0}`), nil
	})
	if err := b.cancelShares(ctx, []string{"first", "third"}); err == nil || requests != 0 {
		t.Fatalf("other account's share reached remote cancellation: %v", err)
	}
	if err := b.cancelShares(ctx, requested); !errors.Is(err, errRejected) {
		t.Fatalf("expected a rejected cancellation, got %v", err)
	}
	if shares, err := b.s.indexedShares(b.account); err != nil || len(shares) != 2 {
		t.Fatalf("failed cancellation modified the index: %+v, %v", shares, err)
	}
	denied = false
	if err := b.cancelShares(ctx, requested); err != nil {
		t.Fatal(err)
	}
	if shares, err := b.s.indexedShares(b.account); err != nil || len(shares) != 0 {
		t.Fatalf("cancelled shares remained indexed: %+v, %v", shares, err)
	}
	if shares, err := b.s.indexedShares("other"); err != nil || len(shares) != 1 || shares[0].ID != "third" {
		t.Fatalf("another account's index changed: %+v, %v", shares, err)
	}
}

func TestCancelAndDeleteUsesOnlyVerifiedSourceFiles(t *testing.T) {
	for _, tc := range []struct {
		name, owner, taskStatus string
		cancelled, deleted      bool
	}{
		{"success", "1", "2", true, true},
		{"not owned", "0", "2", false, false},
		{"delete task failed", "1", "3", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			b, _, _ := fixture(t)
			b.loaded = true
			b.shares = []shareInfo{
				{ID: "first", FIDs: []string{"owned-file"}},
				{ID: "second", URL: "https://pan.quark.cn/s/ownedshare"},
			}
			if err := b.s.save(b.account, "history", record{Phase: "complete", FIDs: []string{"owned-file"}}); err != nil {
				t.Fatal(err)
			}
			var calls []string
			b.q.http.Transport = transport(func(req *http.Request) (*http.Response, error) {
				path := strings.TrimPrefix(req.URL.Path, "/1/clouddrive")
				calls = append(calls, path)
				data := "{}"
				switch path {
				case "/share/sharepage/token":
					data = `{"stoken":"fixture"}`
				case "/share/sharepage/detail":
					data = `{"is_owner":` + tc.owner + `,"share":{"share_id":"second"},"list":[{"fid":"owned-file"},{"fid":"another-owned-file"}]}`
				case "/share/delete":
				case "/file/delete":
					var body struct {
						Type int      `json:"action_type"`
						IDs  []string `json:"filelist"`
					}
					if err := json.NewDecoder(req.Body).Decode(&body); err != nil || body.Type != 2 || !slices.Equal(body.IDs, []string{"owned-file", "another-owned-file"}) {
						t.Fatalf("unexpected recycle request: %+v, %v", body, err)
					}
					data = `{"task_id":"recycle-task"}`
				case "/task":
					data = `{"status":` + tc.taskStatus + `}`
				default:
					t.Fatalf("unexpected request: %s", path)
				}
				return remoteReply(200, nil, `{"code":0,"data":`+data+`}`), nil
			})
			cancelled, err := b.cancelAndDeleteShares(ctx, []string{"first", "second"})
			if cancelled != tc.cancelled || (err == nil) != tc.deleted {
				t.Fatalf("unexpected outcome: cancelled=%v, err=%v", cancelled, err)
			}
			want := []string{"/share/sharepage/token", "/share/sharepage/detail"}
			if tc.cancelled {
				want = append(want, "/share/delete", "/file/delete", "/task")
			}
			if !slices.Equal(calls, want) {
				t.Fatalf("unexpected request order: %v", calls)
			}
			history, err := b.s.get(b.account, "history")
			if err != nil || history.Phase != "complete" || (len(history.FIDs) == 0) != tc.deleted {
				t.Fatalf("unexpected history after cancellation: %+v, %v", history, err)
			}
		})
	}
}

func TestSourceMappingSurvivesHistoryRemovalAndRefresh(t *testing.T) {
	ctx := context.Background()
	b, saves, shares := fixture(t)
	src := sources("https://pan.quark.cn/s/original?pwd=A1b2")[0]
	created := b.one(ctx, src, false)
	if created.Link == "" {
		t.Fatalf("initial conversion failed: %+v", created)
	}
	if err := b.s.removeHistory(ctx, b.account, []string{src.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.refreshShares(ctx); err != nil {
		t.Fatal(err)
	}
	rec, err := b.s.get(b.account, src.ID)
	if err != nil || rec.Phase != "" {
		t.Fatalf("history removal did not remove the detailed record: %+v, %v", rec, err)
	}
	transportBefore := b.q.http.Transport
	b.q.http.Transport = transport(func(req *http.Request) (*http.Response, error) {
		if strings.HasSuffix(req.URL.Path, "/share/sharepage/token") {
			var body struct {
				ID string `json:"pwd_id"`
			}
			raw, err := io.ReadAll(req.Body)
			if err != nil || json.Unmarshal(raw, &body) != nil || body.ID != "ownlink" {
				t.Fatal("known source should verify the indexed output without opening the original")
			}
			req.Body = io.NopCloser(strings.NewReader(string(raw)))
		}
		return transportBefore.RoundTrip(req)
	})
	row := b.one(ctx, sources("https://pan.quark.cn/s/original")[0], false)
	if row.Status != reused || row.Link != created.Link || *saves != 1 || *shares != 1 {
		t.Fatalf("expected exact reuse without new writes: %+v", row)
	}
	history, err := b.s.history(b.account)
	if err != nil || len(history) != 1 || history[0].Source != src.address() {
		t.Fatalf("original address was lost: %+v, %v", history, err)
	}
}

func TestReusesVerifiedCachedShareWithoutSourceHistory(t *testing.T) {
	b, saves, shares := fixture(t)
	remote := b.q.http.Transport
	b.q.http.Transport = transport(func(req *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(req.URL.Path, "/share/mypage/detail"):
			return remoteReply(200, nil, `{"code":0,"data":{"list":[{"share_id":"cached","title":"Project","share_url":"https://pan.quark.cn/s/ownlink"}]}}`), nil
		case strings.HasSuffix(req.URL.Path, "/share/sharepage/detail") && req.URL.Query().Get("pdir_fid") == "0":
			return remoteReply(200, nil, `{"code":0,"data":{"is_owner":true,"share":{"share_id":"cached","size":17},"list":[{"fid":"source-dir","file_name":"Project","dir":true}]}}`), nil
		}
		return remote.RoundTrip(req)
	})
	row := b.one(context.Background(), sources("https://pan.quark.cn/s/original?pwd=A1b2")[0], false)
	if row.Status != reused || row.Link != "https://pan.quark.cn/s/ownlink" || *saves != 0 || *shares != 0 {
		t.Fatalf("expected reuse using verified cached files: %+v", row)
	}
}

func TestHistoryRemovalAndRetest(t *testing.T) {
	b, saves, shares := fixture(t)
	ctx := context.Background()
	src := sources("https://pan.quark.cn/s/original?pwd=A1b2")[0]
	row := b.one(ctx, src, false)
	if row.Status != created {
		t.Fatalf("expected a completed conversion, got %+v", row)
	}
	history, err := b.s.history(b.account)
	if err != nil || len(history) != 1 || history[0].Title != "Project" || history[0].Link != row.Link || history[0].RecordedAt == 0 {
		t.Fatalf("unexpected history: %+v, %v", history, err)
	}
	if err = b.s.set("session", session{Account: b.account, Cookie: "session=fixture"}); err != nil {
		t.Fatal(err)
	}
	handler := web(b.s, "127.0.0.1:8765", nil)
	remove := func(addresses ...string) int {
		data, _ := json.Marshal(map[string][]string{"sources": addresses})
		request := httptest.NewRequest("POST", "http://127.0.0.1:8765/api/v1/history/remove", strings.NewReader(string(data)))
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response.Code
	}
	if err = b.s.save(b.account, "pending", record{Phase: "saving"}); err != nil {
		t.Fatal(err)
	}
	if code := remove(history[0].Source, "https://pan.quark.cn/s/pending"); code != 409 {
		t.Fatalf("expected pending operation to be protected, got %d", code)
	}
	if remaining, err := b.s.get(b.account, src.ID); err != nil || remaining.Phase != "complete" {
		t.Fatalf("batch removal was not rolled back: %+v, %v", remaining, err)
	}
	if err = b.s.save(b.account, "second", record{Phase: "complete"}); err != nil {
		t.Fatal(err)
	}
	if code := remove(history[0].Source, "https://pan.quark.cn/s/second"); code != 200 {
		t.Fatalf("expected successful removal, got %d", code)
	}
	if remaining, err := b.s.history(b.account); err != nil || len(remaining) != 1 || remaining[0].Status != pending {
		t.Fatalf("unexpected remaining history: %+v, %v", remaining, err)
	}
	indexed, err := b.s.indexedShares(b.account)
	if err != nil || len(indexed) != 1 || *saves != 1 || *shares != 1 {
		t.Fatalf("removal changed shares: %+v, %v, %d saves / %d shares", indexed, err, *saves, *shares)
	}
	b.retest = true
	row = b.one(ctx, src, false)
	if row.Status != created || *saves != 2 || *shares != 2 {
		t.Fatalf("retest should generate a new share: %+v, %d saves / %d shares", row, *saves, *shares)
	}
}

func TestResumeStillUsesSavedFiles(t *testing.T) {
	b, saves, shares := fixture(t)
	ctx := context.Background()
	if e := b.s.save(b.account, "original", record{Phase: "saved", Title: "Project", FIDs: []string{"stored"}}); e != nil {
		t.Fatal(e)
	}
	row := b.one(ctx, sources("https://pan.quark.cn/s/original")[0], false)
	if row.Status != created || *saves != 0 || *shares != 1 {
		t.Fatalf("%+v", row)
	}
}

func TestMeasureUsesActualPageSize(t *testing.T) {
	for _, tc := range []struct {
		name, secondMetadata string
		wantError            bool
	}{
		{name: "complete", secondMetadata: `{"_size":1,"_count":1}`},
		{name: "incomplete", secondMetadata: `{"_size":1,"_count":0}`, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := newQuark("session=fixture")
			q.interval = 0
			pages := 0
			q.http.Transport = transport(func(r *http.Request) (*http.Response, error) {
				pages++
				page := (pages-1)%2 + 1
				if r.URL.Query().Get("_page") != strconv.Itoa(page) || r.URL.Query().Get("pdir_fid") != "folder" {
					t.Fatalf("unexpected directory page: %s", r.URL.RawQuery)
				}
				if page == 1 {
					return remoteReply(200, nil, `{"code":0,"metadata":{"_size":1,"_count":2,"_total":2},"data":{"list":[{"fid":"a","file":true,"size":10737418240}]}}`), nil
				}
				list := `[{"fid":"b","file":true,"size":16106127360}]`
				if tc.wantError {
					list = `[]`
				}
				return remoteReply(200, nil, `{"code":0,"metadata":`+tc.secondMetadata+`,"data":{"list":`+list+`}}`), nil
			})
			isDir := true
			b := &batch{q: q}
			snapshot, err := b.measure(context.Background(), remoteShare{Root: []entry{{ID: "folder", Name: "sample", Dir: &isDir}}}, 100<<30)
			if tc.wantError {
				if err == nil || !strings.Contains(err.Error(), "累计 1 项") || !strings.Contains(err.Error(), "sample") {
					t.Fatalf("expected incomplete directory error, got %v", err)
				}
			} else if err != nil || snapshot.Size != 25<<30 {
				t.Fatalf("expected 25 GiB, got %d bytes, error %v", snapshot.Size, err)
			}
			wantPages := 2
			if tc.wantError {
				wantPages = 4
			}
			if pages != wantPages {
				t.Fatalf("expected %d page requests, got %d", wantPages, pages)
			}
		})
	}
}

func TestUsesShareSummaryWithoutReadingChildDirectories(t *testing.T) {
	const exactSize = 27427364475
	for _, tc := range []struct {
		name, summaryHost, status string
		limit                     int64
		candidate                 bool
	}{
		{name: "size supplied with root", summaryHost: "drive-pc.quark.cn", limit: exactSize, status: created},
		{name: "size supplied by web root", summaryHost: "drive-h.quark.cn", limit: exactSize, status: manual, candidate: true},
		{name: "over the byte limit", summaryHost: "drive-pc.quark.cn", limit: exactSize - 1, status: oversize},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, saves, _ := fixture(t)
			b.limit = tc.limit
			remote := b.q.http.Transport
			b.q.http.Transport = transport(func(req *http.Request) (*http.Response, error) {
				if tc.candidate && strings.HasSuffix(req.URL.Path, "/share/mypage/detail") {
					return remoteReply(200, nil, `{"code":0,"data":{"list":[{"share_id":"old","title":"Project","share_url":"https://pan.quark.cn/s/ownlink"}]}}`), nil
				}
				if strings.HasSuffix(req.URL.Path, "/share/sharepage/detail") {
					if req.URL.Query().Get("pdir_fid") != "0" {
						t.Fatal("share size should not require reading any child directories")
					}
					if req.URL.Host == tc.summaryHost {
						return remoteReply(200, nil, `{"code":0,"metadata":{"_total":1},"data":{"share":{"size":`+strconv.FormatInt(exactSize, 10)+`},"list":[{"fid":"source-dir","file_name":"Project","dir":true,"size":0,"share_fid_token":"fixture-fid-token"}]}}`), nil
					}
				}
				return remote.RoundTrip(req)
			})
			row := b.one(context.Background(), sources("https://pan.quark.cn/s/original?pwd=A1b2")[0], false)
			if row.Status != tc.status || !row.SizeReported || row.Size == nil || *row.Size != exactSize {
				t.Fatalf("unexpected conversion: %+v", row)
			}
			if (row.Status == created) != (*saves == 1) || tc.candidate && len(row.Candidates) != 1 {
				t.Fatalf("unexpected save or candidate: %+v, %d saves", row, *saves)
			}
		})
	}
}

func TestShareDirectoryRetriesMismatchedCount(t *testing.T) {
	const five = `[{"fid":"a"},{"fid":"b"},{"fid":"c"},{"fid":"d"},{"fid":"e"}]`
	const six = `[{"fid":"a"},{"fid":"b"},{"fid":"c"},{"fid":"d"},{"fid":"e"},{"fid":"f"}]`
	for _, tc := range []struct {
		name, retry string
		wantError   bool
	}{
		{name: "recovered", retry: six},
		{name: "still missing", retry: five, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := newQuark("session=fixture")
			q.interval = 0
			requests := 0
			q.http.Transport = transport(func(r *http.Request) (*http.Response, error) {
				requests++
				query := r.URL.Query()
				if query.Get("_page") != "1" || query.Get("_size") != "50" || query.Get("_fetch_share") != "0" || query.Get("_sort") != "file_type:asc,file_name:asc" {
					t.Fatalf("unexpected page: %s", r.URL.RawQuery)
				}
				if requests == 1 && r.URL.Host == "drive-pc.quark.cn" {
					return remoteReply(200, nil, `{"code":0,"metadata":{"_count":6},"data":{"list":`+five+`}}`), nil
				}
				if requests == 2 && r.URL.Host == "drive-h.quark.cn" && r.Header.Get("Cookie") == "" {
					return remoteReply(200, nil, `{"code":0,"data":{"list":`+tc.retry+`}}`), nil
				}
				t.Fatalf("unexpected request: %s", r.URL.RawQuery)
				return nil, nil
			})
			list, err := q.sourceEntries(context.Background(), remoteShare{ID: "original", Token: "token"}, "folder")
			if (err != nil) != tc.wantError || requests != 2 || !tc.wantError && len(list) != 6 {
				t.Fatalf("got %d entries, %d requests, error %v", len(list), requests, err)
			}
		})
	}
}

func TestRateLimitAndUnknownWrite(t *testing.T) {
	q := newQuark("session=fixture")
	q.interval = 0
	q.http.Transport = transport(func(r *http.Request) (*http.Response, error) {
		return remoteReply(429, http.Header{"Retry-After": {"90"}}, ""), nil
	})
	_, err := q.call(context.Background(), "GET", "/file/sort", nil, nil)
	if !errors.Is(err, errRate) || time.Until(q.cooldown) < 80*time.Second {
		t.Fatalf("%v / %v", err, q.cooldown)
	}
	q.next = time.Time{}
	q.http.Transport = transport(func(r *http.Request) (*http.Response, error) {
		return remoteReply(200, nil, "<html>unexpected</html>"), nil
	})
	_, err = q.call(context.Background(), "POST", "/share", nil, nil)
	if err == nil || definitelyRejected(err) {
		t.Fatalf("未知写入结果不可标为明确拒绝：%v", err)
	}
}

func TestAccountAvatarInSettings(t *testing.T) {
	q := newQuark("session=example")
	q.http.Transport = transport(func(req *http.Request) (*http.Response, error) {
		return remoteReply(200, nil, `{"code":"OK","data":{"user_id":"example","nickname":"测试账号","avatar_url":"//img.example.com/avatar.png"}}`), nil
	})
	account, err := q.verify(context.Background())
	if err != nil || account.AvatarURL != "https://img.example.com/avatar.png" {
		t.Fatalf("unexpected avatar: %q, err=%v", account.AvatarURL, err)
	}
	s, err := openState(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.close)
	if err := s.set("session", account); err != nil {
		t.Fatal(err)
	}
	res := httptest.NewRecorder()
	web(s, "127.0.0.1:8765", nil).ServeHTTP(res, httptest.NewRequest("GET", "http://127.0.0.1:8765/api/v1/settings", nil))
	var settings map[string]any
	if err := json.Unmarshal(res.Body.Bytes(), &settings); err != nil {
		t.Fatal(err)
	}
	if res.Code != 200 || settings["avatar_url"] != account.AvatarURL || settings["nickname"] != account.Nickname || strings.Contains(res.Body.String(), q.cookie) {
		t.Fatalf("unexpected account settings, HTTP %d", res.Code)
	}
}

func TestShareSyncBackfillsAccountAvatar(t *testing.T) {
	ctx := context.Background()
	s, err := openState(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.close)
	account := session{Account: "owner", Nickname: "Owner", Cookie: "session=example"}
	if err := s.set("session", account); err != nil {
		t.Fatal(err)
	}
	q := newQuark(account.Cookie)
	q.interval = 0
	q.http.Transport = transport(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/1/clouddrive/share/mypage/detail":
			return remoteReply(200, nil, `{"code":0,"metadata":{"_total":2,"_size":100},"data":{"list":[{"share_id":"first","pwd_id":"missing"},{"share_id":"second","pwd_id":"mine","passcode":"AB12"}]}}`), nil
		case "/1/clouddrive/share/sharepage/token":
			var payload struct {
				ID   string `json:"pwd_id"`
				Code string `json:"passcode"`
			}
			if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			switch {
			case payload.ID == "missing":
				return remoteReply(200, nil, `{"code":0,"data":{"stoken":"first","author":{}}}`), nil
			case payload.ID == "mine" && payload.Code == "AB12":
				return remoteReply(200, nil, `{"code":0,"data":{"stoken":"second","author":{"avatar_url":"http://image.quark.cn/path/avatar.jpg"}}}`), nil
			}
		}
		t.Fatalf("unexpected request: %s %s", req.Method, req.URL.Path)
		return nil, nil
	})
	b := batch{s: s, q: q, account: account.Account}
	if _, err := b.refreshShares(ctx); err != nil {
		t.Fatal(err)
	}
	res := httptest.NewRecorder()
	web(s, "127.0.0.1:8765", nil).ServeHTTP(res, httptest.NewRequest("GET", "http://127.0.0.1:8765/api/v1/settings", nil))
	var settings struct {
		Nickname  string
		AvatarURL string `json:"avatar_url"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &settings); err != nil {
		t.Fatal(err)
	}
	if res.Code != http.StatusOK || settings.AvatarURL != "https://image.quark.cn/path/avatar.jpg" || settings.Nickname != "Owner" {
		t.Fatalf("unexpected settings: %s", res.Body.String())
	}
}

func TestLANWebRequests(t *testing.T) {
	s, err := openState(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.close)
	handler := web(s, "0.0.0.0:8765", nil)
	const local = "http://192.168.1.20:8765"
	for _, tc := range []struct {
		host, origin string
		status       int
	}{
		{local, local, http.StatusOK},
		{local, "http://other.example", http.StatusForbidden},
		{"http://other.example:8765", "", http.StatusForbidden},
	} {
		req := httptest.NewRequest("POST", tc.host+"/api/v1/settings", strings.NewReader(`{"limit":"100G"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", tc.origin)
		ctx := context.WithValue(req.Context(), http.LocalAddrContextKey, &net.TCPAddr{IP: net.IPv4(192, 168, 1, 20), Port: 8765})
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req.WithContext(ctx))
		if res.Code != tc.status {
			t.Errorf("host %s origin %s: HTTP %d, want %d", tc.host, tc.origin, res.Code, tc.status)
		}
	}
}

func TestLocalPreviewAndSettings(t *testing.T) {
	s, e := openState(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(s.close)
	h := web(s, "127.0.0.1:8765", nil)
	send := func(path, origin, jsonBody string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "http://127.0.0.1:8765"+path, strings.NewReader(jsonBody))
		req.Header.Set("Content-Type", "application/json")
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		res := httptest.NewRecorder()
		h.ServeHTTP(res, req)
		return res
	}
	if r := send("/api/v1/preview", "http://localhost:9999", "{}"); r.Code != 403 {
		t.Fatal(r.Code)
	}
	if r := send("/api/v1/settings", "", "{\"limit\":\"0.5T\"}"); r.Code != 200 {
		t.Fatal(r.Body.String())
	}
	if _, limit, e := s.config(); e != nil || limit != "0.5T" {
		t.Fatalf("%s %v", limit, e)
	}
	if bytes, e := parseLimit("0.5T"); e != nil || bytes != 1<<39 {
		t.Fatalf("%d %v", bytes, e)
	}
	r := send("/api/v1/preview", "", "{\"text\":\"链接：https://pan.quark.cn/s/example，https://pan.quark.cn/s/example\"}")
	if r.Code != 200 || r.Body.String() != "[\"https://pan.quark.cn/s/example\"]\n" {
		t.Fatal(r.Body.String())
	}
}
