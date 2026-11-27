package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const attachmentFolder = "常用附件"

type attachmentCopy struct {
	Target string
	Files  []entry
	Task   string
	Done   bool
}

type attachedFile struct {
	Source entry
	DestID string
}

func (b *batch) attachmentFiles(ctx context.Context) ([]entry, error) {
	if b.attachments != nil {
		return b.attachments, nil
	}
	root, err := b.q.driveEntries(ctx, "0")
	if err != nil {
		return nil, err
	}
	var id string
	for _, item := range root {
		if item.name() == attachmentFolder {
			dir, known := item.directory()
			if !known || !dir || id != "" {
				return nil, errors.New("网盘根目录的「常用附件」必须是唯一的文件夹")
			}
			id = item.ID
		}
	}
	if id == "" {
		return nil, errors.New("请在网盘根目录创建「常用附件」，并放入要复制的文件")
	}
	files, err := b.q.driveEntries(ctx, id)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, errors.New("「常用附件」里没有文件")
	}
	names := make(map[string]bool, len(files))
	for _, item := range files {
		dir, known := item.directory()
		if !known || dir || item.name() == "" || item.Size == nil || names[item.name()] {
			return nil, errors.New("「常用附件」仅支持名称唯一、大小已知的普通文件")
		}
		names[item.name()] = true
	}
	b.attachments = files
	return files, nil
}

func attachmentMatches(source, target entry, copyConfirmed bool) bool {
	dir, known := target.directory()
	if !known || dir || source.Size == nil || target.Size == nil || *source.Size != *target.Size {
		return false
	}
	if source.SHA1 != "" && target.SHA1 != "" {
		return strings.EqualFold(source.SHA1, target.SHA1)
	}
	if source.MD5 != "" && target.MD5 != "" {
		return strings.EqualFold(source.MD5, target.MD5)
	}
	return copyConfirmed
}

func (q *quark) startAttachmentCopy(ctx context.Context, target string, files []entry) (taskData, error) {
	reply, err := q.call(ctx, "POST", "/file/copy", nil, map[string]any{
		"action_type": 1, "to_pdir_fid": target, "filelist": ids(files), "exclude_fids": []string{},
	})
	var task taskData
	if err == nil && len(reply.Data) != 0 && json.Unmarshal(reply.Data, &task) != nil {
		err = errUncertain
	}
	if err == nil && task.ID == "" && task.Status != nil && *task.Status != 2 {
		err = errUncertain
	}
	return task, err
}

func (b *batch) copyAttachments(ctx context.Context, src source, rec *record, target string) error {
	if rec.AttachmentCopy == nil {
		files, err := b.attachmentFiles(ctx)
		if err != nil {
			return err
		}
		items, err := b.q.driveEntries(ctx, target)
		if err != nil {
			return err
		}
		var missing []entry
		for _, file := range files {
			found := false
			for _, item := range items {
				if item.name() != file.name() {
					continue
				}
				confirmed := false
				for _, saved := range rec.Attached {
					if saved.Source.ID == file.ID && saved.Source.name() == file.name() && saved.DestID == item.ID &&
						attachmentMatches(file, saved.Source, true) {
						confirmed = true
						break
					}
				}
				if found || !attachmentMatches(file, item, confirmed) {
					return fmt.Errorf("附件 %q 与目标目录中的同名项目冲突，请先核对", file.name())
				}
				found = true
			}
			if !found {
				missing = append(missing, file)
			}
		}
		if len(missing) == 0 {
			if !rec.AttachmentsDone {
				rec.AttachmentsDone = true
				return b.s.save(b.account, src.ID, *rec)
			}
			return nil
		}
		rec.AttachmentCopy = &attachmentCopy{Target: target, Files: missing}
		if err := b.s.save(b.account, src.ID, *rec); err != nil {
			return err
		}
		task, err := b.q.startAttachmentCopy(ctx, target, missing)
		if err != nil {
			if definitelyRejected(err) {
				rec.AttachmentCopy = nil
				if storeErr := b.s.save(b.account, src.ID, *rec); storeErr != nil {
					return storeErr
				}
				return err
			}
			return writeProblem(err)
		}
		rec.AttachmentCopy.Task, rec.AttachmentCopy.Done = task.ID, task.ID == ""
		if err := b.s.save(b.account, src.ID, *rec); err != nil {
			return err
		}
	}
	copy := rec.AttachmentCopy
	if copy.Target != target {
		return fmt.Errorf("%w：附件目标目录已变更", errUncertain)
	}
	if copy.Task != "" && !copy.Done {
		if _, err := b.q.task(ctx, copy.Task); err != nil {
			if errors.Is(err, errTaskFailed) {
				rec.AttachmentCopy = nil
				if storeErr := b.s.save(b.account, src.ID, *rec); storeErr != nil {
					return storeErr
				}
				return err
			}
			return writeProblem(err)
		}
		copy.Done = true
		if err := b.s.save(b.account, src.ID, *rec); err != nil {
			return err
		}
	}
	items, err := b.q.driveEntries(ctx, target)
	if err != nil {
		return err
	}
	var attached []attachedFile
	for _, file := range copy.Files {
		var matches []entry
		for _, item := range items {
			if item.name() == file.name() {
				matches = append(matches, item)
			}
		}
		if len(matches) != 1 || !attachmentMatches(file, matches[0], copy.Done) {
			return fmt.Errorf("%w：请检查目标目录里的附件 %q", errUncertain, file.name())
		}
		attached = append(attached, attachedFile{Source: file, DestID: matches[0].ID})
	}
	rec.AttachmentCopy = nil
	rec.AttachmentsDone = true
	rec.Attached = append(rec.Attached, attached...)
	return b.s.save(b.account, src.ID, *rec)
}
