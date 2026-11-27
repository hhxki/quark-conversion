package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

const (
	created     = "created"
	reused      = "reused"
	manual      = "manual_selection"
	oversize    = "over_limit"
	unknownSize = "size_unknown"
	paused      = "paused"
	pending     = "pending_confirmation"
	failed      = "failed"
)

type candidate struct {
	Title string `json:"title"`
	Link  string `json:"link"`
}

type result struct {
	Source           string      `json:"source"`
	Status           string      `json:"status"`
	Title            string      `json:"title,omitempty"`
	RecordedAt       int64       `json:"recorded_at,omitempty"`
	Link             string      `json:"link,omitempty"`
	Message          string      `json:"message,omitempty"`
	Size             *int64      `json:"size_bytes,omitempty"`
	SizeReported     bool        `json:"size_reported,omitempty"`
	AttachmentsReady bool        `json:"attachments_ready,omitempty"`
	Limit            int64       `json:"limit_bytes,omitempty"`
	Candidates       []candidate `json:"candidates,omitempty"`
}

type batch struct {
	s           *state
	q           *quark
	account     string
	limit       int64
	targetID    string
	shares      []shareInfo
	loaded      bool
	retest      bool
	attachments []entry
}

func (b *batch) mine(ctx context.Context) ([]shareInfo, error) {
	if b.loaded {
		return b.shares, nil
	}
	var synced bool
	if err := b.s.setting("share-index:"+b.account, &synced); err != nil {
		return nil, err
	}
	if !synced {
		return b.refreshShares(ctx)
	}
	var err error
	b.shares, err = b.s.indexedShares(b.account)
	b.loaded = err == nil
	return b.shares, err
}

func (b *batch) asLink(ctx context.Context, s shareInfo) (string, error) {
	if link := s.link(); link != "" {
		return link, nil
	}
	if s.ID == "" {
		return "", nil
	}
	full, e := b.q.link(ctx, s.ID)
	if e != nil {
		return "", e
	}
	return full.link(), nil
}

func accountPaused(e error) bool {
	return errors.Is(e, errRate) || errors.Is(e, errLogin) || errors.Is(e, errChallenge) || errors.Is(e, errPossibleChallenge)
}

func problem(r result, e error) result {
	r.Message = e.Error()
	switch {
	case accountPaused(e):
		r.Status = paused
	case errors.Is(e, errUncertain):
		r.Status = pending
	default:
		r.Status = failed
	}
	return r
}

func (b *batch) convert(ctx context.Context, input []source, fresh bool, progress func(result)) []result {
	rows := make([]result, 0, len(input))
	var pauseMessage string
	for _, src := range input {
		if ctx.Err() != nil && pauseMessage == "" {
			pauseMessage = "转换已中断，重新提交可继续检查已有任务"
		}
		r := result{Source: src.address(), Status: paused, Message: pauseMessage}
		if pauseMessage == "" {
			r = b.one(ctx, src, fresh)
			if r.Status == paused {
				pauseMessage = r.Message
			}
		}
		rows = append(rows, r)
		if progress != nil {
			progress(r)
		}
	}
	return rows
}

func (b *batch) one(ctx context.Context, src source, fresh bool) result {
	r := result{Source: src.address()}
	rec, e := b.s.get(b.account, src.ID)
	if e != nil {
		return problem(r, e)
	}
	if src.Attach && (rec.Phase == "submitting_share" || rec.Phase == "sharing" || rec.Phase == "linking") && !rec.AttachmentsDone {
		return problem(r, errors.New("请先完成这条未结束的分享，再勾选添加附件"))
	}
	switch rec.Phase {
	case "submitting_save", "submitting_share":
		return b.recoverSubmission(ctx, src, r, rec)
	case "saving", "saved", "sharing", "linking":
		return b.resume(ctx, src, r, rec)
	}
	var list []shareInfo
	if b.retest {
		if rec.Phase == "complete" {
			return problem(r, errors.New("请先从本地历史移除这条已完成的记录，再测试新转存"))
		}
	} else {
		list, e = b.mine(ctx)
		if e != nil {
			return problem(r, e)
		}
	}
	if !b.retest {
		if matched, ok := b.matchRecordedShare(ctx, src, r, rec, list); ok {
			return matched
		}
		saved, err := b.s.sourceLink(b.account, src.ID)
		if err != nil {
			return problem(r, err)
		}
		if saved.ShareID != rec.ShareID || saved.Link != rec.Link {
			if matched, ok := b.matchRecordedShare(ctx, src, r, saved, list); ok {
				return matched
			}
		}
	}
	for _, old := range list {
		if own := old.link(); sameLink(src.address(), own) && !expired(old) {
			return b.reuse(ctx, src, r, old, own, old.FIDs)
		}
	}
	if (rec.Link != "" || rec.ShareID != "") && len(rec.FIDs) > 0 {
		rec.Phase, rec.Link, rec.ShareID = "saved", "", ""
		if e := b.s.save(b.account, src.ID, rec); e != nil {
			return problem(r, e)
		}
		return b.resume(ctx, src, r, rec)
	}
	if src.Ambiguous {
		r.Status, r.Message = manual, "有多个可能的提取码，请手动补充"
		return r
	}
	remote, e := b.q.inspect(ctx, src)
	if e != nil {
		return problem(r, e)
	}
	if len(remote.Root) == 0 {
		return problem(r, errors.New("源分享为空或已失效"))
	}
	named := matchingShares(remote, list)
	for _, old := range named {
		if equalIDs(ids(remote.Root), old.FIDs) {
			link, e := b.asLink(ctx, old)
			if e != nil {
				return problem(r, e)
			}
			if link != "" {
				return b.reuse(ctx, src, r, old, link, old.FIDs)
			}
		}
	}
	containsFolder := false
	for _, item := range remote.Root {
		dir, ok := item.directory()
		if !ok {
			return problem(r, errors.New("源分享包含无法识别的项目类型"))
		}
		containsFolder = containsFolder || dir
	}
	if containsFolder && len(remote.Root) > 1 {
		r.Status, r.Message = manual, "多个顶层文件夹或文件夹与散文件混合，请手动筛选"
		return r
	}
	if src.Attach && !containsFolder {
		r.Status, r.Message = manual, "添加附件需要源分享只有一个顶层文件夹，请先将源文件整理进一个文件夹"
		return r
	}
	if containsFolder && remote.TotalSize == 0 {
		remote.TotalSize, e = b.q.webShareSize(ctx, remote)
		if accountPaused(e) && !errors.Is(e, errLogin) {
			return problem(r, e)
		}
	}
	var snapshot contentSnapshot
	if remote.TotalSize > 0 {
		snapshot.Size, r.SizeReported = remote.TotalSize, true
		e = nil
		if snapshot.Size > b.limit {
			e = errOversize
		}
	} else {
		snapshot, e = b.measure(ctx, remote, b.limit)
	}
	if e != nil {
		switch {
		case errors.Is(e, errOversize):
			r.Status, r.Message, r.Size, r.Limit = oversize, e.Error(), &snapshot.Size, b.limit
		case accountPaused(e):
			return problem(r, e)
		default:
			r.Status, r.Message = unknownSize, e.Error()
		}
		return r
	}
	r.Size, r.Limit = &snapshot.Size, b.limit
	if !fresh && !src.Attach {
		for i, old := range named {
			if i >= 20 {
				break
			}
			link, e := b.asLink(ctx, old)
			if e != nil {
				return problem(r, e)
			}
			if link == "" {
				continue
			}
			r.Candidates = append(r.Candidates, candidate{Title: old.Title, Link: link})
			if i >= 3 {
				continue
			}
			input := sources(link)
			if len(input) != 1 {
				continue
			}
			copy, e := b.q.inspect(ctx, input[0])
			if accountPaused(e) {
				return problem(r, e)
			}
			if e != nil {
				continue
			}
			if old.ID != "" && copy.Owner && copy.ShareID == old.ID && equalIDs(ids(remote.Root), ids(copy.Root)) {
				return b.reuseChecked(ctx, src, r, old, link, ids(copy.Root), copy)
			}
			if !snapshot.Strong {
				continue
			}
			other, e := b.measure(ctx, copy, snapshot.Size)
			if accountPaused(e) {
				return problem(r, e)
			}
			if e == nil && snapshot.same(other) {
				return b.reuseChecked(ctx, src, r, old, link, old.FIDs, copy)
			}
		}
		if len(r.Candidates) > 0 {
			r.Status, r.Message = manual, "找到可能相同的旧分享，请确认后复用，或选择继续转存"
			return r
		}
	}
	return b.saveSource(ctx, src, r, remote, snapshot)
}

func (b *batch) matchRecordedShare(ctx context.Context, src source, r result, rec record, list []shareInfo) (result, bool) {
	for _, old := range list {
		if expired(old) || ((rec.ShareID == "" || rec.ShareID != old.ID) && !sameLink(rec.Link, old.link())) {
			continue
		}
		link, err := b.asLink(ctx, old)
		if err != nil {
			return problem(r, err), true
		}
		if link != "" {
			return b.reuse(ctx, src, r, old, link, rec.FIDs), true
		}
	}
	return r, false
}

func ids(entries []entry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.ID)
	}
	return out
}

func equalIDs(left, right []string) bool {
	if len(left) == 0 || len(left) != len(right) {
		return false
	}
	a, b := slices.Clone(left), slices.Clone(right)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}

func unixSeconds(timestamp int64) int64 {
	if timestamp > 1000000000000000 {
		timestamp /= 1000
	}
	if timestamp > 100000000000 {
		timestamp /= 1000
	}
	return timestamp
}

func expired(s shareInfo) bool {
	expiration := unixSeconds(s.ExpiredAt)
	if expiration > 0 && time.Now().Unix() >= expiration {
		return true
	}
	switch strings.ToLower(scalar(s.Status)) {
	case "expired", "revoked", "cancelled", "canceled", "invalid", "disabled":
		return true
	}
	return false
}

func matchingShares(remote remoteShare, shares []shareInfo) []shareInfo {
	names := map[string]bool{}
	if title := normalizeName(remote.Title); title != "" {
		names[title] = true
	}
	remoteIDs := ids(remote.Root)
	for _, item := range remote.Root {
		if name := normalizeName(item.name()); name != "" {
			names[name] = true
		}
	}
	var matches []shareInfo
	for _, old := range shares {
		if !expired(old) && (equalIDs(remoteIDs, old.FIDs) || names[normalizeName(old.Title)]) {
			matches = append(matches, old)
		}
	}
	return matches
}

func normalizeName(s string) string { return strings.ToLower(strings.Join(strings.Fields(s), " ")) }

func sameLink(a, b string) bool {
	if !validShareURL(a) || !validShareURL(b) {
		return false
	}
	left, right := sources(a), sources(b)
	return len(left) == 1 && len(right) == 1 && left[0].ID == right[0].ID
}

func originalSourceURL(src source, previous string) string {
	if src.Code == "" && previous != "" {
		return previous
	}
	address := src.address()
	if strings.HasPrefix(address, "http://") {
		return "https://" + strings.TrimPrefix(address, "http://")
	}
	return address
}

func writeProblem(e error) error {
	if accountPaused(e) {
		return e
	}
	return fmt.Errorf("%w：%v", errUncertain, e)
}

func (b *batch) reuse(ctx context.Context, src source, r result, old shareInfo, link string, fids []string) result {
	input := sources(link)
	if len(input) != 1 {
		return problem(r, errors.New("无法识别旧分享链接"))
	}
	detail, e := b.q.inspect(ctx, input[0])
	if e != nil {
		return problem(r, e)
	}
	return b.reuseChecked(ctx, src, r, old, link, fids, detail)
}

func (b *batch) reuseChecked(ctx context.Context, src source, r result, old shareInfo, link string, fids []string, detail remoteShare) result {
	if len(detail.Root) == 0 {
		return problem(r, errors.New("旧分享已失效或内容为空"))
	}
	title := old.Title
	if title == "" {
		title = detail.Title
	}
	if title == "" {
		title = detail.Root[0].name()
	}
	rec, e := b.s.get(b.account, src.ID)
	if e != nil {
		return problem(r, e)
	}
	previousURL := rec.SourceURL
	if previousURL == "" {
		saved, err := b.s.sourceLink(b.account, src.ID)
		if err != nil {
			return problem(r, err)
		}
		previousURL = saved.SourceURL
	}
	if rec.ShareID != old.ID || !sameLink(rec.Link, link) {
		rec = record{Phase: "complete", ShareID: old.ID, Link: link, FIDs: fids, Title: title}
	}
	rec.SourceURL = originalSourceURL(src, previousURL)
	if len(rec.FIDs) == 0 {
		rec.FIDs = fids
	}
	if src.Attach {
		if old.ID == "" || !detail.Owner || detail.ShareID != old.ID || len(detail.Root) != 1 {
			return problem(r, errors.New("无法确认旧分享是当前账号的单文件夹分享，未添加附件"))
		}
		dir, known := detail.Root[0].directory()
		if !known || !dir {
			return problem(r, errors.New("旧分享没有可添加附件的文件夹"))
		}
		if e := b.copyAttachments(ctx, src, &rec, detail.Root[0].ID); e != nil {
			return problem(r, e)
		}
	}
	if e := b.completeShare(ctx, src.ID, rec, shareInfo{}); e != nil {
		return problem(r, e)
	}
	r.Link, r.Status, r.Title = link, reused, title
	r.AttachmentsReady = src.Attach && rec.AttachmentsDone
	return r
}

func (b *batch) confirm(ctx context.Context, sourceURL, link string) (result, error) {
	items := sources(sourceURL)
	if len(items) != 1 || !validShareURL(link) {
		return result{}, errors.New("请提交单个源分享和有效的旧链接")
	}
	r := result{Source: items[0].address()}
	existing, e := b.s.get(b.account, items[0].ID)
	if e != nil {
		return r, e
	}
	if existing.Phase != "" && existing.Phase != "complete" {
		return r, errUncertain
	}
	list, e := b.mine(ctx)
	if e != nil {
		return r, e
	}
	for _, old := range list {
		if expired(old) || (!sameLink(link, old.link()) && !sameLink(link, "https://pan.quark.cn/s/"+old.PwdID)) {
			continue
		}
		address, e := b.asLink(ctx, old)
		if e != nil {
			return r, e
		}
		if address != "" {
			return b.reuse(ctx, items[0], r, old, address, old.FIDs), nil
		}
	}
	return r, errors.New("该链接未在当前账号的分享中找到")
}
