package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestManualShareUpdate(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("Node.js is required for the CLI fixture")
	}
	dir := t.TempDir()
	s, err := openState(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.close)
	script := filepath.Join(dir, "quark-drive.cjs")
	const fixture = `
const fs = require('node:fs');
const action = process.argv[2];
const state = __filename + '.applied';
const result = (data, msg = '') => console.log(JSON.stringify({code: 0, type: 'result', action, data, msg}));
switch (action) {
  case 'get-share-update-list':
    console.log(JSON.stringify({code: 0, type: 'list', action, data: {
      share_url: 'https://pan.quark.cn/s/example?pwd=ABCD', passcode: 'ABCD', title: 'Share'
    }}));
    result({total: 1, has_next_page: false});
    break;
  case 'get-share-update-files':
    result(fs.existsSync(state) ? {total: 0, files: []} : {total: 1, files: [{title: 'new.txt', size: 50, file_type: 'txt'}]});
    break;
  case 'get-share-saved-dir':
    result({pdir_fid: 'saved-folder', pdir_name: 'Destination'});
    break;
  case 'saveas-update':
    fs.writeFileSync(state, 'applied', {flag: 'wx'});
    result({task_id: 'save-task', status: 2}, 'Saved');
    break;
  default: process.exitCode = 1;
}
`
	if err := os.WriteFile(script, []byte(fixture), 0600); err != nil {
		t.Fatal(err)
	}
	updates, err := openOfficialUpdates(script)
	if err != nil {
		t.Fatal(err)
	}
	handler := web(s, "127.0.0.1:8765", updates)
	send := func(operation, payload string, status int) map[string]any {
		t.Helper()
		req := httptest.NewRequest("POST", "http://127.0.0.1:8765/api/v1/updates/"+operation, strings.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		if res.Code != status || strings.Contains(res.Body.String(), "ABCD") {
			t.Fatalf("%s: HTTP %d, %s", operation, res.Code, res.Body.String())
		}
		var data map[string]any
		if err := json.Unmarshal(res.Body.Bytes(), &data); err != nil {
			t.Fatal(err)
		}
		return data
	}
	list := send("list", `{"page":1}`, http.StatusOK)
	if len(list["items"].([]any)) != 1 {
		t.Fatalf("unexpected update list: %v", list)
	}
	const source = `"url":"https://pan.quark.cn/s/example"`
	preview := send("preview", "{"+source+"}", http.StatusOK)
	if preview["pdir_fid"] != "saved-folder" || preview["total"] != float64(1) {
		t.Fatalf("unexpected update preview: %v", preview)
	}
	send("apply", "{"+source+`,"dir_id":"different"}`, http.StatusConflict)
	const target = `,"dir_id":"saved-folder"}`
	if saved := send("apply", "{"+source+target, http.StatusOK); saved["message"] != "Saved" {
		t.Fatalf("unexpected save result: %v", saved)
	}
	if repeat := send("apply", "{"+source+target, http.StatusOK); repeat["total"] != float64(0) {
		t.Fatalf("unexpected retry result: %v", repeat)
	}
}
