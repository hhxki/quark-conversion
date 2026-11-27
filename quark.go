package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const targetName = "来自：分享"
const shareWebBase = "https://drive-h.quark.cn/1/clouddrive"

type paginationError struct {
	minimum int64
	detail  string
}

func (e *paginationError) Error() string {
	return "夸克分页与总数不一致（" + e.detail + "）"
}

var (
	errLogin             = errors.New("夸克会话失效，请重新登录")
	errRate              = errors.New("夸克请求受限，请稍后再试")
	errChallenge         = errors.New("夸克要求验证或拒绝请求，请在官网处理后重试")
	errPossibleChallenge = errors.New("夸克返回了验证页面，请在官网检查后重试")
	errRejected          = errors.New("夸克拒绝请求")
	errTaskFailed        = errors.New("夸克任务失败，请核对官网状态")
	errUncertain         = errors.New("夸克未确认上次操作结果，请先在官网核对")
)

type quark struct {
	http       *http.Client
	base       string
	accountURL string
	cookie     string
	interval   time.Duration
	next       time.Time
	cooldown   time.Time
}

func newQuark(cookie string) *quark {
	return &quark{
		http:       &http.Client{Timeout: 25 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		base:       "https://drive-pc.quark.cn/1/clouddrive",
		accountURL: "https://pan.quark.cn/account/info",
		cookie:     cookie, interval: 650 * time.Millisecond,
	}
}

type response struct {
	Code     json.RawMessage
	Status   json.RawMessage
	Message  string
	Msg      string
	Data     json.RawMessage
	Metadata struct {
		Total *int64 `json:"_total"`
		Size  *int   `json:"_size"`
		Count *int   `json:"_count"`
	}
}

func scalar(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	if raw[0] == '"' {
		var s string
		_ = json.Unmarshal(raw, &s)
		return s
	}
	return string(raw)
}

func waitUntil(ctx context.Context, until time.Time) error {
	if delay := time.Until(until); delay > 0 {
		t := time.NewTimer(delay)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
	return ctx.Err()
}

func retryDelay(value string, attempt int) time.Duration {
	if seconds, err := strconv.ParseInt(value, 10, 32); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(value); err == nil && when.After(time.Now()) {
		return time.Until(when)
	}
	return time.Second * time.Duration(2<<attempt)
}

func (q *quark) call(ctx context.Context, method, path string, params url.Values, payload any) (response, error) {
	return q.callAt(ctx, q.base, method, path, params, payload)
}

func (q *quark) callAt(ctx context.Context, base, method, path string, params url.Values, payload any) (response, error) {
	var result response
	endpoint := base + path
	p := url.Values{"pr": {"ucpro"}, "fr": {"pc"}}
	if path == "/account/info" {
		endpoint = q.accountURL
		p = url.Values{"fr": {"pc"}, "platform": {"pc"}}
	}
	for k, v := range params {
		p[k] = v
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return result, err
	}
	for attempt := 0; attempt < 3; attempt++ {
		if err := waitUntil(ctx, q.next); err != nil {
			return result, err
		}
		var input io.Reader
		if payload != nil {
			input = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, endpoint+"?"+p.Encode(), input)
		if err != nil {
			return result, err
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Origin", "https://pan.quark.cn")
		req.Header.Set("Referer", "https://pan.quark.cn/")
		if base == q.base {
			req.Header.Set("Cookie", q.cookie)
		}
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		q.next = time.Now().Add(q.interval)
		res, err := q.http.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return result, ctx.Err()
			}
			return result, errors.New("连接夸克失败")
		}
		if res.StatusCode == http.StatusTooManyRequests {
			res.Body.Close()
			delay := retryDelay(res.Header.Get("Retry-After"), attempt)
			q.next = time.Now().Add(delay)
			q.cooldown = q.next
			if delay > 30*time.Second {
				return result, fmt.Errorf("%w（服务端要求等待约 %d 秒）", errRate, int64(delay.Round(time.Second)/time.Second))
			}
			continue
		}
		if res.StatusCode == http.StatusUnauthorized {
			res.Body.Close()
			return result, errLogin
		}
		if res.StatusCode == http.StatusForbidden || (res.StatusCode >= 300 && res.StatusCode < 400) {
			res.Body.Close()
			return result, errChallenge
		}
		if res.StatusCode < 200 || res.StatusCode >= 300 {
			res.Body.Close()
			return result, fmt.Errorf("夸克返回 HTTP %d", res.StatusCode)
		}
		raw, readErr := io.ReadAll(io.LimitReader(res.Body, 8<<20))
		res.Body.Close()
		if readErr != nil || json.Unmarshal(raw, &result) != nil {
			preview := strings.ToLower(string(raw[:min(len(raw), 64<<10)]))
			if strings.Contains(preview, "验证码") || strings.Contains(preview, "captcha") {
				return result, errPossibleChallenge
			}
			return result, errors.New("夸克响应格式异常，请在官网检查登录或验证状态")
		}
		code := scalar(result.Code)
		status := scalar(result.Status)
		if code == "0" || (path == "/account/info" && code == "OK") {
			return result, nil
		}
		if code == "401" || status == "401" || code == "UNAUTHORIZED" {
			return result, errLogin
		}
		message := strings.ToLower(result.Message + " " + result.Msg)
		if code == "429" || status == "429" {
			q.next = time.Now().Add(retryDelay("", attempt))
			q.cooldown = q.next
			continue
		}
		for _, word := range []string{"验证码", "风控", "访问频繁", "请求频繁", "captcha", "risk control", "rate limit", "too many requests"} {
			if strings.Contains(message, word) {
				return result, errChallenge
			}
		}
		if code == "403" || status == "403" {
			return result, errChallenge
		}
		return result, fmt.Errorf("%w（状态 %s，代码 %s）", errRejected, safeCode(status), safeCode(code))
	}
	return result, errRate
}

func safeCode(value string) string {
	if len(value) == 0 || len(value) > 24 || strings.ContainsAny(value, " \r\n\t<>{}\"") {
		return "未知"
	}
	return value
}

func getData[T any](res response, err error) (T, error) {
	var data T
	if err != nil {
		return data, err
	}
	if len(res.Data) == 0 || string(res.Data) == "null" || json.Unmarshal(res.Data, &data) != nil {
		return data, errors.New("夸克响应缺少可识别的数据")
	}
	return data, nil
}

func (q *quark) verify(ctx context.Context) (session, error) {
	data, err := getData[map[string]json.RawMessage](q.call(ctx, "GET", "/account/info", nil, nil))
	if err != nil {
		return session{}, err
	}
	var id string
	for _, key := range []string{"user_id", "uid", "qk_uid", "userId", "quark_uid", "member_id"} {
		if id = scalar(data[key]); id != "" && id != "0" {
			break
		}
		id = ""
	}
	if id == "" {
		req := &http.Request{Header: http.Header{"Cookie": {q.cookie}}}
		if c, err := req.Cookie("__uid"); err == nil && c.Value != "" {
			id = "cookie:" + c.Value
		}
	}
	if id == "" {
		return session{}, errors.New("夸克账户信息缺少可识别的账号 ID")
	}
	var avatarURL string
	for _, key := range []string{"avatar_url", "avatar", "avatarUrl"} {
		avatarURL = normalizeAvatarURL(scalar(data[key]))
		if avatarURL != "" {
			break
		}
	}
	return session{Account: id, Nickname: scalar(data["nickname"]), AvatarURL: avatarURL, Cookie: q.cookie}, nil
}

func normalizeAvatarURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "//") {
		raw = "https:" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return ""
	}
	u.Scheme = "https"
	return u.String()
}

type entry struct {
	ID       string `json:"fid"`
	Name     string `json:"file_name"`
	Filename string
	Dir      *bool
	File     *bool
	Type     json.RawMessage `json:"file_type"`
	Size     *int64
	Token    string `json:"share_fid_token"`
	MD5      string
	SHA1     string
}

func (e entry) name() string {
	if e.Name != "" {
		return e.Name
	}
	return e.Filename
}

func (e entry) directory() (bool, bool) {
	if e.Dir != nil {
		return *e.Dir, true
	}
	if e.File != nil {
		return !*e.File, true
	}
	switch scalar(e.Type) {
	case "0":
		return true, true
	case "1":
		return false, true
	}
	return false, false
}

type shareInfo struct {
	ID        string `json:"share_id"`
	PwdID     string `json:"pwd_id"`
	Title     string
	URL       string `json:"share_url"`
	Passcode  string
	Status    json.RawMessage
	CreatedAt int64    `json:"created_at"`
	ExpiredAt int64    `json:"expired_at"`
	URLType   int      `json:"url_type"`
	FIDs      []string `json:"fid_list"`
}

func (q *quark) cancelShares(ctx context.Context, ids []string) error {
	_, err := q.call(ctx, "POST", "/share/delete", nil, map[string][]string{"share_ids": ids})
	return err
}

func (q *quark) recycleFiles(ctx context.Context, fids []string) error {
	task, err := getData[taskData](q.call(ctx, "POST", "/file/delete", nil, map[string]any{
		"action_type": 2, "filelist": fids, "exclude_fids": []string{},
	}))
	if err != nil {
		return err
	}
	if task.ID == "" {
		return errUncertain
	}
	_, err = q.task(ctx, task.ID)
	return err
}

func (s shareInfo) link() string {
	link := s.URL
	if link == "" && s.PwdID != "" {
		link = "https://pan.quark.cn/s/" + s.PwdID
	}
	if !validShareURL(link) {
		return ""
	}
	u, _ := url.Parse(link)
	if s.Passcode != "" {
		query := u.Query()
		query.Set("pwd", s.Passcode)
		u.RawQuery = query.Encode()
	} else if s.URLType == 2 && u.Query().Get("pwd") == "" {
		return ""
	}
	return u.String()
}

func (q *quark) pages(ctx context.Context, base, path string, params url.Values, consume func(response) (int, error)) error {
	const size = 50
	pageSize := size
	var received int64
	var total *int64
	for page := 1; page <= 2000; page++ {
		params.Set("_page", strconv.Itoa(page))
		params.Set("_size", strconv.Itoa(size))
		params.Set("_fetch_total", "1")
		res, err := q.callAt(ctx, base, "GET", path, params, nil)
		if err != nil {
			return err
		}
		count, err := consume(res)
		if err != nil {
			return err
		}
		if res.Metadata.Size != nil && *res.Metadata.Size > 0 {
			pageSize = *res.Metadata.Size
		}
		received += int64(count)
		if res.Metadata.Total != nil {
			if total != nil && *total != *res.Metadata.Total {
				return &paginationError{max(received, *total, *res.Metadata.Total), fmt.Sprintf("第 %d 页的接口总数从 %d 变成 %d 项", page, *total, *res.Metadata.Total)}
			}
			total = res.Metadata.Total
		}
		if total != nil {
			if received == *total {
				return nil
			}
			if received > *total || count == 0 {
				return &paginationError{max(received, *total), fmt.Sprintf("第 %d 页返回 %d 项，累计 %d 项，接口总数 %d 项", page, count, received, *total)}
			}
			continue
		}
		if res.Metadata.Count != nil && count != *res.Metadata.Count {
			minimum := received - int64(count) + int64(max(count, *res.Metadata.Count))
			return &paginationError{minimum, fmt.Sprintf("第 %d 页返回 %d 项，接口标记为 %d 项，未给出总数", page, count, *res.Metadata.Count)}
		}
		if count < pageSize {
			return nil
		}
	}
	return errors.New("夸克列表超过分页检查范围")
}

func (q *quark) mine(ctx context.Context) ([]shareInfo, error) {
	list := make([]shareInfo, 0)
	err := q.pages(ctx, q.base, "/share/mypage/detail", url.Values{"_order_field": {"created_at"}, "_order_type": {"desc"}}, func(r response) (int, error) {
		d, e := getData[struct{ List []shareInfo }](r, nil)
		if e != nil {
			return 0, e
		}
		if d.List == nil {
			return 0, errors.New("未识别到我的分享列表")
		}
		list = append(list, d.List...)
		return len(d.List), nil
	})
	return list, err
}

type remoteShare struct {
	ID        string
	Token     string
	Title     string
	Root      []entry
	TotalSize int64
	Owner     bool
	ShareID   string
}

type shareListing struct {
	IsOwner json.RawMessage `json:"is_owner"`
	List    []entry
	Files   []entry
	Share   struct {
		ID    string `json:"share_id"`
		Title string
		Size  json.RawMessage
	}
	TokenInfo struct{ Title string } `json:"token_info"`
}

func (d shareListing) totalSize() int64 {
	size, err := strconv.ParseInt(scalar(d.Share.Size), 10, 64)
	if err != nil || size <= 0 {
		return 0
	}
	return size
}

type shareToken struct {
	Token  string `json:"stoken"`
	Title  string
	Author struct {
		AvatarURL json.RawMessage `json:"avatar_url"`
	}
}

func (q *quark) shareToken(ctx context.Context, s source) (shareToken, error) {
	return getData[shareToken](q.call(ctx, "POST", "/share/sharepage/token", nil, map[string]any{"pwd_id": s.ID, "passcode": s.Code, "support_visit_limit_private_share": true}))
}

func (q *quark) ownShareAvatar(ctx context.Context, shares []shareInfo) string {
	attempts := 0
	for _, share := range shares {
		if share.ExpiredAt > 0 && share.ExpiredAt <= time.Now().UnixMilli() {
			continue
		}
		input := sources(share.link())
		if len(input) != 1 {
			continue
		}
		if attempts == 3 || ctx.Err() != nil {
			break
		}
		attempts++
		data, err := q.shareToken(ctx, input[0])
		if errors.Is(err, errLogin) || errors.Is(err, errRate) || errors.Is(err, errChallenge) {
			break
		}
		if err == nil {
			if avatar := normalizeAvatarURL(scalar(data.Author.AvatarURL)); avatar != "" {
				return avatar
			}
		}
	}
	return ""
}

func (q *quark) inspect(ctx context.Context, s source) (remoteShare, error) {
	r := remoteShare{ID: s.ID}
	d, e := q.shareToken(ctx, s)
	if e != nil {
		return r, e
	}
	if d.Token == "" {
		return r, errors.New("源分享不可访问或需要正确的提取码")
	}
	r.Token, r.Title = d.Token, d.Title
	r.Root, e = q.readSourceEntries(ctx, r, "0", &r)
	return r, e
}

func (q *quark) sourceEntries(ctx context.Context, r remoteShare, parent string) ([]entry, error) {
	return q.readSourceEntries(ctx, r, parent, nil)
}

func (q *quark) readSourceEntries(ctx context.Context, r remoteShare, parent string, summary *remoteShare) ([]entry, error) {
	const path = "/share/sharepage/detail"
	params := sourceParams(r, parent)
	list, err := q.entries(ctx, q.base, path, params, summary)
	var mismatch *paginationError
	if !errors.As(err, &mismatch) {
		return list, err
	}

	// The public share page uses a different host for its read-only directory listing.
	retry, err := q.entries(ctx, shareWebBase, path, params, summary)
	if err != nil {
		var alternateMismatch *paginationError
		if errors.As(err, &alternateMismatch) || (accountPaused(err) && !errors.Is(err, errLogin)) {
			return retry, err
		}
		return retry, fmt.Errorf("%w，夸克网页版目录也未能完成核对：%v", mismatch, err)
	}
	if int64(len(retry)) < mismatch.minimum {
		return retry, &paginationError{mismatch.minimum, fmt.Sprintf("重新读取后仍只有 %d 项，先前分页报告至少 %d 项", len(retry), mismatch.minimum)}
	}
	seen := make(map[string]bool, len(retry))
	for _, item := range retry {
		seen[item.ID] = true
	}
	for _, item := range list {
		if !seen[item.ID] {
			return retry, &paginationError{mismatch.minimum, "两次目录读取返回的文件不一致"}
		}
	}
	return retry, nil
}

func (q *quark) webShareSize(ctx context.Context, r remoteShare) (int64, error) {
	params := sourceParams(r, "0")
	params.Set("_page", "1")
	params.Set("_size", "50")
	params.Set("_fetch_total", "1")
	resp, err := q.callAt(ctx, shareWebBase, "GET", "/share/sharepage/detail", params, nil)
	d, err := getData[shareListing](resp, err)
	if err != nil {
		return 0, err
	}
	if d.List == nil {
		d.List = d.Files
	}
	if len(d.List) != 1 || d.List[0].ID != r.Root[0].ID || (resp.Metadata.Total != nil && *resp.Metadata.Total != 1) {
		return 0, nil
	}
	return d.totalSize(), nil
}

func sourceParams(r remoteShare, parent string) url.Values {
	metadata := "0"
	if parent == "0" {
		metadata = "1"
	}
	return url.Values{
		"pwd_id": {r.ID}, "stoken": {r.Token}, "pdir_fid": {parent}, "force": {"0"}, "ver": {"2"},
		"uc_param_str": {""}, "_sort": {"file_type:asc,file_name:asc"},
		"_fetch_share": {metadata}, "_fetch_banner": {metadata}, "fetch_relate_conversation": {metadata},
	}
}

func (q *quark) driveEntries(ctx context.Context, parent string) ([]entry, error) {
	return q.entries(ctx, q.base, "/file/sort", url.Values{"pdir_fid": {parent}}, nil)
}

func (q *quark) entries(ctx context.Context, base, path string, p url.Values, summary *remoteShare) ([]entry, error) {
	var out []entry
	seen := map[string]bool{}
	err := q.pages(ctx, base, path, p, func(r response) (int, error) {
		d, e := getData[shareListing](r, nil)
		if e != nil {
			return 0, e
		}
		if d.List == nil {
			d.List = d.Files
		}
		if d.List == nil {
			return 0, errors.New("未识别到夸克文件列表")
		}
		if summary != nil {
			if base == q.base && d.Share.ID != "" {
				summary.ShareID = d.Share.ID
				owner := scalar(d.IsOwner)
				summary.Owner = owner == "1" || owner == "true"
			}
			if summary.Title == "" {
				summary.Title = d.Share.Title
				if summary.Title == "" {
					summary.Title = d.TokenInfo.Title
				}
			}
			if summary.TotalSize == 0 {
				summary.TotalSize = d.totalSize()
			}
		}
		for _, f := range d.List {
			if f.ID == "" || seen[f.ID] {
				return 0, errors.New("夸克文件分页包含缺失或重复的 ID")
			}
			seen[f.ID] = true
		}
		out = append(out, d.List...)
		return len(d.List), nil
	})
	return out, err
}

func (q *quark) target(ctx context.Context) (string, error) {
	root, e := q.driveEntries(ctx, "0")
	if e != nil {
		return "", e
	}
	var id string
	for _, x := range root {
		if x.name() != targetName {
			continue
		}
		isDir, known := x.directory()
		if !known || !isDir || id != "" {
			return "", errors.New("网盘根目录里的「来自：分享」存在歧义")
		}
		id = x.ID
	}
	if id != "" {
		return id, nil
	}
	d, e := getData[entry](q.call(ctx, "POST", "/file", nil, map[string]any{"pdir_fid": "0", "file_name": targetName, "dir_init_lock": false}))
	if e != nil {
		return "", e
	}
	if d.ID == "" {
		return "", errUncertain
	}
	return d.ID, nil
}

type taskData struct {
	ID      string `json:"task_id"`
	Status  *int
	ShareID string `json:"share_id"`
	Share   struct {
		ID string `json:"share_id"`
	}
	Save struct {
		FIDs []string `json:"save_as_top_fids"`
	} `json:"save_as"`
}

func (q *quark) startSave(ctx context.Context, r remoteShare, target string) (taskData, error) {
	fids, tokens := make([]string, 0, len(r.Root)), make([]string, 0, len(r.Root))
	for _, x := range r.Root {
		if x.Token == "" {
			return taskData{}, errors.New("分享详情缺少项目转存令牌")
		}
		fids = append(fids, x.ID)
		tokens = append(tokens, x.Token)
	}
	return getData[taskData](q.call(ctx, "POST", "/share/sharepage/save", nil, map[string]any{
		"fid_list": fids, "fid_token_list": tokens, "to_pdir_fid": target, "pwd_id": r.ID, "stoken": r.Token, "pdir_fid": "0", "scene": "link", "pdir_save_all": false, "exclude_fids": []string{},
	}))
}

func (q *quark) startShare(ctx context.Context, fids []string, title string) (taskData, error) {
	return getData[taskData](q.call(ctx, "POST", "/share", nil, map[string]any{"fid_list": fids, "title": title, "url_type": 1, "expired_type": 1}))
}

func (q *quark) task(ctx context.Context, id string) (taskData, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	for i := 0; ; i++ {
		d, e := getData[taskData](q.call(ctx, "GET", "/task", url.Values{"task_id": {id}, "retry_index": {strconv.Itoa(i)}}, nil))
		if e != nil {
			return d, e
		}
		if d.Status == nil {
			return d, errUncertain
		}
		switch *d.Status {
		case 2:
			return d, nil
		case 3:
			return d, errTaskFailed
		case 0, 1:
		default:
			return d, errUncertain
		}
		if e := waitUntil(ctx, time.Now().Add(time.Second)); e != nil {
			return d, e
		}
	}
}

func (q *quark) link(ctx context.Context, id string) (shareInfo, error) {
	return getData[shareInfo](q.call(ctx, "POST", "/share/password", nil, map[string]string{"share_id": id}))
}

func definitelyRejected(e error) bool {
	return errors.Is(e, errRate) || errors.Is(e, errLogin) || errors.Is(e, errChallenge) || errors.Is(e, errRejected)
}
