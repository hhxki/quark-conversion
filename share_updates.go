package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type officialUpdates struct {
	node, script string
}

func openOfficialUpdates(script string) (*officialUpdates, error) {
	if script == "" {
		return nil, nil
	}
	path, err := filepath.Abs(script)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("--quark-cli 必须指向夸克官方的 quark-drive.cjs 文件")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		return nil, errors.New("使用 --quark-cli 需要安装 Node.js，并将 node 加入 PATH")
	}
	return &officialUpdates{node: node, script: path}, nil
}

type updateShare struct {
	URL       string `json:"share_url"`
	Title     string `json:"title"`
	UpdatedAt string `json:"updated_at"`
}

type updateFile struct {
	Title string `json:"title"`
	Size  int64  `json:"size"`
	Type  string `json:"file_type"`
}

type updateResult struct {
	Total *int         `json:"total"`
	Next  bool         `json:"has_next_page"`
	Files []updateFile `json:"files"`
	DirID string       `json:"pdir_fid"`
	Dir   string       `json:"pdir_name"`
	Task  string       `json:"task_id"`
	State int          `json:"status"`
}

type updateReply struct {
	Code    *int            `json:"code"`
	Type    string          `json:"type"`
	Action  string          `json:"action"`
	Message string          `json:"msg"`
	Data    json.RawMessage `json:"data"`
}

func (u *officialUpdates) call(ctx context.Context, command string, args ...string) ([]updateShare, updateReply, error) {
	timeout := 30 * time.Second
	if command == "saveas-update" {
		timeout = 16 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	raw, runErr := exec.CommandContext(ctx, u.node, append([]string{u.script, command}, args...)...).Output()
	rows, result, err := parseUpdateReply(raw, command)
	if err != nil {
		return nil, result, err
	}
	if runErr != nil {
		return nil, result, errors.New("夸克官方 CLI 执行中断")
	}
	return rows, result, nil
}

func parseUpdateReply(raw []byte, command string) ([]updateShare, updateReply, error) {
	rows := make([]updateShare, 0)
	var result updateReply
	lines := bufio.NewScanner(bytes.NewReader(raw))
	lines.Buffer(make([]byte, 4096), 256<<10)
	for lines.Scan() {
		var reply updateReply
		if json.Unmarshal(lines.Bytes(), &reply) != nil || reply.Code == nil {
			continue
		}
		switch {
		case reply.Type == "result" && (reply.Action == command || *reply.Code != 0):
			result = reply
		case reply.Type == "list" && reply.Action == command && *reply.Code == 0:
			var row updateShare
			if err := json.Unmarshal(reply.Data, &row); err != nil {
				return nil, result, errors.New("夸克更新列表格式异常")
			}
			if src := updateSource(row.URL); src != "" {
				row.URL = src
				rows = append(rows, row)
			}
		}
	}
	if lines.Err() != nil || result.Code == nil {
		return nil, result, errors.New("夸克官方 CLI 未返回可识别的结果，请检查版本或授权状态")
	}
	if *result.Code == 41043 {
		return nil, result, errors.New("夸克未识别到该链接的历史转存记录")
	}
	if *result.Code != 0 {
		return nil, result, fmt.Errorf("夸克官方 CLI 返回 %d · %s", *result.Code, result.Message)
	}
	return rows, result, nil
}

func updateSource(raw string) string {
	if !validShareURL(raw) {
		return ""
	}
	items := sources(raw)
	if len(items) != 1 {
		return ""
	}
	return "https://pan.quark.cn/s/" + items[0].ID
}

func (u *officialUpdates) query(ctx context.Context, command string, args ...string) ([]updateShare, updateResult, error) {
	rows, reply, err := u.call(ctx, command, args...)
	var data updateResult
	if err == nil {
		err = json.Unmarshal(reply.Data, &data)
	}
	return rows, data, err
}

func (u *officialUpdates) register(mux *http.ServeMux, s *state) {
	mux.HandleFunc("GET /api/v1/updates/status", func(w http.ResponseWriter, r *http.Request) {
		respond(w, http.StatusOK, map[string]bool{"configured": u != nil})
	})
	mux.HandleFunc("POST /api/v1/updates/{operation}", func(w http.ResponseWriter, r *http.Request) {
		if u == nil {
			respondError(w, http.StatusServiceUnavailable, errors.New("启动服务时请用 --quark-cli 指定夸克官方 CLI"))
			return
		}
		p, err := body[struct {
			URL   string
			Page  int
			DirID string `json:"dir_id"`
		}](w, r)
		if err != nil {
			respondError(w, http.StatusBadRequest, err)
			return
		}
		page := max(p.Page, 1)
		operation := r.PathValue("operation")
		source := updateSource(p.URL)
		if operation != "list" && source == "" {
			respondError(w, http.StatusBadRequest, errors.New("请选择有效的夸克源分享链接"))
			return
		}
		ctx := r.Context()
		// Fail fast while another operation holds the shared data-directory lock.
		lockCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
		release, err := s.acquire(lockCtx)
		cancel()
		if err != nil {
			respondError(w, http.StatusConflict, errors.New("另一个转换或更新任务正在执行，请稍后再试"))
			return
		}
		defer release()
		switch operation {
		case "list":
			rows, data, err := u.query(ctx, "get-share-update-list", "--page", strconv.Itoa(page))
			if err != nil {
				respondError(w, http.StatusBadGateway, err)
				return
			}
			respond(w, http.StatusOK, map[string]any{"items": rows, "has_next_page": data.Next})
		case "preview", "apply":
			data, err := u.preview(ctx, source, page)
			if err != nil {
				respondError(w, http.StatusBadGateway, err)
				return
			}
			if operation == "preview" || *data.Total == 0 {
				respond(w, http.StatusOK, data)
				return
			}
			if p.DirID != data.DirID {
				respondError(w, http.StatusConflict, errors.New("夸克记录的转存目录已变更，请重新查看更新后再确认"))
				return
			}
			_, reply, err := u.call(ctx, "saveas-update", "--url", source)
			var saved updateResult
			if err != nil || json.Unmarshal(reply.Data, &saved) != nil || saved.State != 2 || saved.Task == "" {
				message := "夸克未确认转存成功，请先到网盘核对，再重新检查更新"
				if err != nil {
					message += " · " + err.Error()
				}
				respondError(w, http.StatusBadGateway, errors.New(message))
				return
			}
			if reply.Message == "" {
				reply.Message = "新增文件已转存"
			}
			respond(w, http.StatusOK, map[string]string{"message": reply.Message})
		default:
			respondError(w, http.StatusNotFound, errors.New("未知更新操作"))
		}
	})
}

func (u *officialUpdates) preview(ctx context.Context, source string, page int) (updateResult, error) {
	_, data, err := u.query(ctx, "get-share-update-files", "--url", source, "--page", strconv.Itoa(page))
	if err != nil {
		return data, err
	}
	if data.Total == nil || *data.Total < 0 {
		return data, errors.New("夸克未返回有效的更新数量")
	}
	if *data.Total == 0 {
		return data, nil
	}
	_, dir, err := u.query(ctx, "get-share-saved-dir", "--url", source)
	if err != nil {
		return data, err
	}
	if strings.TrimSpace(dir.DirID) == "" {
		return data, errors.New("夸克未返回上次转存目录")
	}
	data.Dir, data.DirID = dir.Dir, dir.DirID
	return data, nil
}
