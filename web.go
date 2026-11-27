package main

import (
	"context"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

//go:embed web/*
var assets embed.FS

func respond(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func respondError(w http.ResponseWriter, status int, e error) {
	respond(w, status, map[string]string{"error": e.Error()})
}

func body[T any](w http.ResponseWriter, r *http.Request) (T, error) {
	var value T
	if media := strings.SplitN(r.Header.Get("Content-Type"), ";", 2)[0]; media != "application/json" {
		return value, errors.New("请使用 application/json")
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxInput*3)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if e := d.Decode(&value); e != nil {
		return value, errors.New("请求参数不正确或超出大小限制")
	}
	if e := d.Decode(new(any)); e != io.EOF {
		return value, errors.New("请求应包含一个 JSON 对象")
	}
	return value, nil
}

type convertRequest struct {
	Text   string
	Limit  string
	Fresh  bool
	Retest bool
	Attach bool
}

type ownShareRow struct {
	Key        string `json:"key"`
	ID         string `json:"id,omitempty"`
	Title      string `json:"title"`
	Link       string `json:"link,omitempty"`
	Status     string `json:"status"`
	RecordedAt int64  `json:"recorded_at,omitempty"`
}

func syncLoginShares(ctx context.Context, s *state, account session) map[string]any {
	result := map[string]any{"logged_in": true, "nickname": account.Nickname, "avatar_url": account.AvatarURL}
	if _, err := s.syncShares(ctx); err != nil {
		result["share_sync_error"] = err.Error()
	}
	var updated session
	if s.setting("session", &updated) == nil && updated.Account == account.Account && updated.Cookie == account.Cookie {
		result["avatar_url"] = updated.AvatarURL
	}
	return result
}

func web(s *state, address string, updates *officialUpdates) http.Handler {
	sub, _ := fs.Sub(assets, "web")
	mux := http.NewServeMux()
	var loginMutex sync.Mutex
	var activeQR *qrLogin
	var qrRetry time.Time
	updates.register(mux, s)
	mux.Handle("/", http.FileServer(http.FS(sub)))
	mux.HandleFunc("GET /api/v1/settings", func(w http.ResponseWriter, r *http.Request) {
		account, limit, e := s.config()
		if e != nil {
			respondError(w, 500, e)
			return
		}
		respond(w, 200, map[string]any{"logged_in": account.Cookie != "", "nickname": account.Nickname, "avatar_url": normalizeAvatarURL(account.AvatarURL), "limit": limit})
	})
	mux.HandleFunc("POST /api/v1/settings", func(w http.ResponseWriter, r *http.Request) {
		p, e := body[struct{ Limit string }](w, r)
		if e == nil {
			_, e = parseLimit(p.Limit)
		}
		if e != nil {
			respondError(w, 400, e)
			return
		}
		if e = s.set("limit", strings.TrimSpace(p.Limit)); e != nil {
			respondError(w, 500, e)
			return
		}
		respond(w, 200, map[string]string{"limit": strings.TrimSpace(p.Limit)})
	})
	mux.HandleFunc("POST /api/v1/login", func(w http.ResponseWriter, r *http.Request) {
		p, e := body[struct{ Cookie string }](w, r)
		if e != nil {
			respondError(w, 400, e)
			return
		}
		account, e := s.login(r.Context(), p.Cookie)
		if e != nil {
			respondError(w, 400, e)
			return
		}
		respond(w, 200, syncLoginShares(r.Context(), s, account))
	})
	mux.HandleFunc("POST /api/v1/login/qr", func(w http.ResponseWriter, r *http.Request) {
		if _, e := body[struct{}](w, r); e != nil {
			respondError(w, 400, e)
			return
		}
		loginMutex.Lock()
		defer loginMutex.Unlock()
		if time.Now().Before(qrRetry) {
			respondError(w, 429, errRate)
			return
		}
		if activeQR == nil || time.Now().After(activeQR.expires) {
			var e error
			activeQR, e = newQR(r.Context())
			if e != nil {
				qrRetry = time.Now().Add(10 * time.Second)
				respondError(w, 502, e)
				return
			}
		}
		respond(w, 200, map[string]string{"image": "data:image/png;base64," + base64.StdEncoding.EncodeToString(activeQR.png)})
	})
	mux.HandleFunc("POST /api/v1/login/qr/check", func(w http.ResponseWriter, r *http.Request) {
		if _, e := body[struct{}](w, r); e != nil {
			respondError(w, 400, e)
			return
		}
		loginMutex.Lock()
		defer loginMutex.Unlock()
		if activeQR == nil {
			respondError(w, 400, errors.New("请先获取登录二维码"))
			return
		}
		cookie, e := activeQR.check(r.Context())
		if e != nil {
			activeQR = nil
			respondError(w, 400, e)
			return
		}
		if cookie == "" {
			respond(w, 200, map[string]bool{"logged_in": false})
			return
		}
		activeQR = nil
		account, e := s.login(r.Context(), cookie)
		if e != nil {
			respondError(w, 400, e)
			return
		}
		respond(w, 200, syncLoginShares(r.Context(), s, account))
	})
	mux.HandleFunc("POST /api/v1/logout", func(w http.ResponseWriter, r *http.Request) {
		if _, e := body[struct{}](w, r); e != nil {
			respondError(w, 400, e)
			return
		}
		release, e := s.acquire(r.Context())
		if e != nil {
			respondError(w, 409, e)
			return
		}
		defer release()
		if e = s.set("session", session{}); e != nil {
			respondError(w, 500, e)
			return
		}
		respond(w, 200, map[string]bool{"logged_in": false})
	})
	mux.HandleFunc("POST /api/v1/preview", func(w http.ResponseWriter, r *http.Request) {
		p, e := body[struct{ Text string }](w, r)
		if e == nil && len(p.Text) > maxInput {
			e = errors.New("输入不能超过 2 MiB")
		}
		if e != nil {
			respondError(w, 400, e)
			return
		}
		links := make([]string, 0)
		for _, src := range sources(p.Text) {
			links = append(links, src.address())
		}
		respond(w, 200, links)
	})
	mux.HandleFunc("POST /api/v1/convert", func(w http.ResponseWriter, r *http.Request) {
		p, e := body[convertRequest](w, r)
		if e == nil && len(p.Text) > maxInput {
			e = errors.New("输入不能超过 2 MiB")
		}
		if e != nil {
			respondError(w, 400, e)
			return
		}
		if r.URL.Query().Get("stream") == "1" {
			w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
			encoder := json.NewEncoder(w)
			flush := http.NewResponseController(w)
			_ = flush.Flush()
			_, e = s.convert(r.Context(), p.Text, p.Limit, p.Fresh, p.Retest, p.Attach, func(row result) {
				_ = encoder.Encode(row)
				_ = flush.Flush()
			})
			if e != nil {
				_ = encoder.Encode(map[string]string{"error": e.Error()})
			}
			return
		}
		items, e := s.convert(r.Context(), p.Text, p.Limit, p.Fresh, p.Retest, p.Attach, nil)
		if e != nil {
			respondError(w, 400, e)
			return
		}
		respond(w, 200, items)
	})
	mux.HandleFunc("POST /api/v1/confirm", func(w http.ResponseWriter, r *http.Request) {
		p, e := body[struct{ Source, Link string }](w, r)
		if e != nil {
			respondError(w, 400, e)
			return
		}
		b, release, e := s.startBatch(r.Context(), "")
		if e != nil {
			respondError(w, 400, e)
			return
		}
		defer release()
		row, e := b.confirm(r.Context(), p.Source, p.Link)
		if e != nil {
			respondError(w, 400, e)
			return
		}
		respond(w, 200, row)
	})
	mux.HandleFunc("GET /api/v1/history", func(w http.ResponseWriter, r *http.Request) {
		account, _, e := s.config()
		if e != nil {
			respondError(w, 500, e)
			return
		}
		rows, e := s.history(account.Account)
		if e != nil {
			respondError(w, 500, e)
			return
		}
		respond(w, 200, rows)
	})
	mux.HandleFunc("POST /api/v1/history/remove", func(w http.ResponseWriter, r *http.Request) {
		p, e := body[struct {
			Source  string
			Sources []string
		}](w, r)
		if e != nil {
			respondError(w, 400, e)
			return
		}
		if p.Source != "" {
			p.Sources = append(p.Sources, p.Source)
		}
		if len(p.Sources) == 0 {
			respondError(w, 400, errors.New("请选择要移除的本地记录"))
			return
		}
		ids := make([]string, 0, len(p.Sources))
		for _, address := range p.Sources {
			input := sources(address)
			if len(input) != 1 || !validShareURL(address) {
				respondError(w, 400, errors.New("有无法识别的源分享链接"))
				return
			}
			ids = append(ids, input[0].ID)
		}
		release, e := s.acquire(r.Context())
		if e != nil {
			respondError(w, 409, e)
			return
		}
		defer release()
		account, _, e := s.config()
		if e != nil {
			respondError(w, 500, e)
			return
		}
		if account.Account == "" || account.Cookie == "" {
			respondError(w, 401, errors.New("请先登录夸克"))
			return
		}
		if e = s.removeHistory(r.Context(), account.Account, ids); e != nil {
			respondError(w, 409, e)
			return
		}
		respond(w, 200, map[string]bool{"removed": true})
	})
	mux.HandleFunc("GET /api/v1/shares", func(w http.ResponseWriter, r *http.Request) {
		b, release, e := s.startBatch(r.Context(), "")
		if e != nil {
			respondError(w, 400, e)
			return
		}
		defer release()
		var shares []shareInfo
		if r.URL.Query().Get("refresh") == "1" {
			shares, e = b.refreshShares(r.Context())
		} else {
			shares, e = b.mine(r.Context())
		}
		if e != nil {
			respondError(w, 502, e)
			return
		}
		items := make([]ownShareRow, 0, len(shares))
		for _, old := range shares {
			status := "share_active"
			if expired(old) {
				status = "share_inactive"
			}
			// The management list can offer the page URL even if a private share requires manual passcode entry.
			old.URLType = 0
			items = append(items, ownShareRow{Key: shareIndexKey(old), ID: old.ID, Title: old.Title, Link: old.link(), Status: status, RecordedAt: unixSeconds(old.CreatedAt)})
		}
		respond(w, 200, items)
	})
	cancelShareHandler := func(w http.ResponseWriter, r *http.Request) {
		p, err := body[struct{ IDs []string }](w, r)
		if err != nil {
			respondError(w, 400, err)
			return
		}
		if !uniqueIDs(p.IDs) {
			respondError(w, 400, errors.New("请选择要取消的分享"))
			return
		}
		b, release, err := s.startBatch(r.Context(), "")
		if err != nil {
			respondError(w, 400, err)
			return
		}
		defer release()
		if r.URL.Path == "/api/v1/shares/cancel-and-delete" {
			cancelled, err := b.cancelAndDeleteShares(r.Context(), p.IDs)
			if !cancelled && err != nil {
				respondError(w, 502, err)
				return
			}
			result := map[string]any{"cancelled": cancelled, "deleted": err == nil}
			if err != nil {
				result["delete_error"] = err.Error()
			}
			respond(w, 200, result)
			return
		}
		if err = b.cancelShares(r.Context(), p.IDs); err != nil {
			respondError(w, 502, err)
			return
		}
		respond(w, 200, map[string]bool{"cancelled": true})
	}
	mux.HandleFunc("POST /api/v1/shares/cancel", cancelShareHandler)
	mux.HandleFunc("POST /api/v1/shares/cancel-and-delete", cancelShareHandler)
	listenHost, port, _ := net.SplitHostPort(address)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data: https:; frame-ancestors 'none'; form-action 'self'; base-uri 'none'")
		localAddr, _ := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
		requestHost := r.Host
		if port == "80" && !strings.Contains(requestHost, ":") {
			requestHost += ":80"
		}
		allowedHost := requestHost == address || requestHost == "localhost:"+port
		if listenHost == "0.0.0.0" && localAddr != nil {
			allowedHost = allowedHost || requestHost == localAddr.String()
		}
		if !allowedHost {
			respondError(w, 403, errors.New("请使用此电脑的 IP 地址访问"))
			return
		}
		origin := r.Header.Get("Origin")
		if (origin != "" && origin != "http://"+r.Host) || r.Header.Get("Sec-Fetch-Site") == "cross-site" {
			respondError(w, 403, errors.New("请求来源不受支持"))
			return
		}
		mux.ServeHTTP(w, r)
	})
}
