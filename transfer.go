package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"
)

func (b *batch) saveSource(ctx context.Context, src source, r result, remote remoteShare, snapshot contentSnapshot) result {
	if src.Attach {
		if _, err := b.attachmentFiles(ctx); err != nil {
			return problem(r, err)
		}
	}
	var e error
	if b.targetID == "" {
		b.targetID, e = b.q.target(ctx)
		if e != nil {
			return problem(r, e)
		}
	}
	for _, item := range remote.Root {
		if item.Token == "" {
			return problem(r, errors.New("分享详情缺少项目转存令牌"))
		}
	}
	before, e := b.q.driveEntries(ctx, b.targetID)
	if e != nil {
		return problem(r, e)
	}
	expected := slices.Clone(remote.Root)
	for i := range expected {
		expected[i].Token = ""
	}
	rec := record{Phase: "submitting_save", SourceURL: originalSourceURL(src, ""), Title: remote.Title, Target: b.targetID, Before: ids(before), Expected: expected, Content: snapshot}
	if rec.Title == "" {
		rec.Title = remote.Root[0].name()
	}
	if e = b.s.save(b.account, src.ID, rec); e != nil {
		return problem(r, e)
	}
	task, e := b.q.startSave(ctx, remote, b.targetID)
	if e != nil {
		if definitelyRejected(e) {
			if saveErr := b.s.save(b.account, src.ID, record{}); saveErr != nil {
				return problem(r, saveErr)
			}
			return problem(r, e)
		}
		return problem(r, writeProblem(e))
	}
	if task.ID == "" {
		return problem(r, errUncertain)
	}
	rec.Phase, rec.Task = "saving", task.ID
	if e = b.s.save(b.account, src.ID, rec); e != nil {
		return problem(r, e)
	}
	return b.resume(ctx, src, r, rec)
}

func (b *batch) recoverSubmission(ctx context.Context, src source, r result, rec record) result {
	if rec.Phase == "submitting_save" && rec.Target != "" {
		list, e := b.q.driveEntries(ctx, rec.Target)
		if e != nil {
			return problem(r, writeProblem(e))
		}
		fids, e := b.verifyNew(ctx, src, rec, list, false)
		if e != nil {
			return problem(r, e)
		}
		rec.Phase, rec.FIDs = "saved", fids
	} else if rec.Phase == "submitting_share" {
		list, e := b.refreshShares(ctx)
		if e != nil {
			return problem(r, writeProblem(e))
		}
		var found string
		for _, old := range list {
			if old.ID != "" && !expired(old) && equalIDs(old.FIDs, rec.FIDs) {
				if found != "" {
					return problem(r, errUncertain)
				}
				found = old.ID
			}
		}
		if found == "" {
			return problem(r, errUncertain)
		}
		rec.Phase, rec.ShareID = "linking", found
	} else {
		return problem(r, errUncertain)
	}
	if e := b.s.save(b.account, src.ID, rec); e != nil {
		return problem(r, e)
	}
	return b.resume(ctx, src, r, rec)
}

func (b *batch) resume(ctx context.Context, src source, r result, rec record) result {
	if rec.Phase == "saving" {
		d, e := b.q.task(ctx, rec.Task)
		if e != nil {
			return problem(r, writeProblem(e))
		}
		fids := d.Save.FIDs
		if len(fids) != len(rec.Expected) || !uniqueIDs(fids) || intersects(rec.Before, fids) {
			list, e := b.q.driveEntries(ctx, rec.Target)
			if e != nil {
				return problem(r, writeProblem(e))
			}
			fids, e = b.verifyNew(ctx, src, rec, list, true)
			if e != nil {
				return problem(r, e)
			}
		}
		rec.Phase, rec.Task, rec.FIDs = "saved", "", fids
		if e = b.s.save(b.account, src.ID, rec); e != nil {
			return problem(r, e)
		}
	}
	if rec.Phase == "saved" {
		if !uniqueIDs(rec.FIDs) {
			return problem(r, errUncertain)
		}
		if rec.AttachmentCopy != nil && !src.Attach {
			return problem(r, fmt.Errorf("%w：该链接已有附件复制任务，请带上 --attach 或勾选「常用附件」核对", errUncertain))
		}
		if src.Attach {
			if len(rec.FIDs) != 1 {
				return problem(r, errors.New("添加附件需要转存结果为单个文件夹"))
			}
			dir, known := false, false
			if len(rec.Expected) == 1 {
				dir, known = rec.Expected[0].directory()
			} else if rec.Target != "" {
				items, err := b.q.driveEntries(ctx, rec.Target)
				if err != nil {
					return problem(r, err)
				}
				for _, item := range items {
					if item.ID == rec.FIDs[0] {
						dir, known = item.directory()
						break
					}
				}
			}
			if !known || !dir {
				return problem(r, errors.New("无法确认转存文件夹，未添加附件"))
			}
			if err := b.copyAttachments(ctx, src, &rec, rec.FIDs[0]); err != nil {
				return problem(r, err)
			}
		}
		rec.Phase = "submitting_share"
		if e := b.s.save(b.account, src.ID, rec); e != nil {
			return problem(r, e)
		}
		d, e := b.q.startShare(ctx, rec.FIDs, rec.Title)
		if e != nil {
			if definitelyRejected(e) {
				rec.Phase = "saved"
				if saveErr := b.s.save(b.account, src.ID, rec); saveErr != nil {
					return problem(r, saveErr)
				}
				return problem(r, e)
			}
			return problem(r, writeProblem(e))
		}
		switch {
		case d.ShareID != "":
			rec.Phase, rec.ShareID = "linking", d.ShareID
		case d.ID != "":
			rec.Phase, rec.Task = "sharing", d.ID
		default:
			return problem(r, errUncertain)
		}
		if e = b.s.save(b.account, src.ID, rec); e != nil {
			return problem(r, e)
		}
	}
	if rec.Phase == "sharing" {
		d, e := b.q.task(ctx, rec.Task)
		if e != nil {
			if errors.Is(e, errTaskFailed) {
				rec.Phase, rec.Task = "saved", ""
				if saveErr := b.s.save(b.account, src.ID, rec); saveErr != nil {
					return problem(r, saveErr)
				}
				return problem(r, e)
			}
			return problem(r, writeProblem(e))
		}
		id := d.ShareID
		if id == "" {
			id = d.Share.ID
		}
		if id == "" {
			return problem(r, errUncertain)
		}
		rec.Phase, rec.Task, rec.ShareID = "linking", "", id
		if e = b.s.save(b.account, src.ID, rec); e != nil {
			return problem(r, e)
		}
	}
	if rec.Phase != "linking" {
		return problem(r, errUncertain)
	}
	d, e := b.q.link(ctx, rec.ShareID)
	if e != nil {
		return problem(r, e)
	}
	link := d.link()
	if link == "" {
		return problem(r, errUncertain)
	}
	rec.Phase, rec.Link, rec.Before, rec.Expected = "complete", link, nil, nil
	rec.SourceURL = originalSourceURL(src, rec.SourceURL)
	rec.Content = contentSnapshot{}
	if rec.Title == "" {
		rec.Title = d.Title
	}
	d.ID, d.FIDs, d.Title = rec.ShareID, rec.FIDs, rec.Title
	if d.CreatedAt == 0 {
		d.CreatedAt = time.Now().Unix()
	}
	if e = b.completeShare(ctx, src.ID, rec, d); e != nil {
		return problem(r, e)
	}
	r.Link, r.Status, r.Title = link, created, rec.Title
	r.AttachmentsReady = src.Attach && rec.AttachmentsDone
	return r
}

func uniqueIDs(values []string) bool {
	if len(values) == 0 {
		return false
	}
	seen := map[string]bool{}
	for _, id := range values {
		if id == "" || seen[id] {
			return false
		}
		seen[id] = true
	}
	return true
}

func intersects(a, b []string) bool {
	before := map[string]bool{}
	for _, id := range a {
		before[id] = true
	}
	for _, id := range b {
		if before[id] {
			return true
		}
	}
	return false
}

func (b *batch) verifyNew(ctx context.Context, src source, rec record, entries []entry, taskCompleted bool) ([]string, error) {
	fids, e := identifyNew(rec.Before, entries, rec.Expected)
	if e != nil {
		return nil, e
	}
	sourceContent := rec.Content
	if sourceContent.Items == nil && sourceContent.Size > 0 {
		// A size summary is sufficient for the limit, but recovery still needs the file layout.
		remote, err := b.q.inspect(ctx, src)
		if accountPaused(err) {
			return nil, err
		}
		if err != nil || !equalIDs(ids(remote.Root), ids(rec.Expected)) {
			return nil, errUncertain
		}
		sourceContent, err = b.measure(ctx, remote, sourceContent.Size)
		if accountPaused(err) {
			return nil, err
		}
		if err != nil || sourceContent.Size != rec.Content.Size {
			return nil, errUncertain
		}
	}
	root := make([]entry, 0, len(fids))
	for _, entry := range entries {
		if slices.Contains(fids, entry.ID) {
			root = append(root, entry)
		}
	}
	snapshot, e := measure(root, rec.Content.Size, func(id string) ([]entry, error) { return b.q.driveEntries(ctx, id) })
	if accountPaused(e) {
		return nil, e
	}
	if e != nil {
		return nil, errUncertain
	}
	if sourceContent.same(snapshot) || (taskCompleted && sourceContent.sameLayout(snapshot)) {
		return fids, nil
	}
	return nil, errUncertain
}

func identifyNew(before []string, current, expected []entry) ([]string, error) {
	old := map[string]bool{}
	for _, id := range before {
		old[id] = true
	}
	var added []entry
	for _, e := range current {
		if !old[e.ID] {
			added = append(added, e)
		}
	}
	if len(added) != len(expected) || len(added) == 0 {
		return nil, errUncertain
	}
	fids := make([]string, 0, len(added))
	used := map[string]bool{}
	for _, want := range expected {
		matches := 0
		var id string
		wd, wk := want.directory()
		for _, found := range added {
			fd, fk := found.directory()
			if !wk || !fk || wd != fd || want.name() == "" || want.name() != found.name() {
				continue
			}
			if !wd && (want.Size == nil || found.Size == nil || *want.Size != *found.Size) {
				continue
			}
			id = found.ID
			matches++
		}
		if matches != 1 || used[id] {
			return nil, errUncertain
		}
		used[id] = true
		fids = append(fids, id)
	}
	return fids, nil
}
