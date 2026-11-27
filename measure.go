package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
)

var errOversize = errors.New("超出大小上限")
var md5Pattern = regexp.MustCompile("(?i)^[0-9a-f]{32}$")
var sha1Pattern = regexp.MustCompile("(?i)^[0-9a-f]{40}$")

type fileMark struct {
	Dir  bool
	Size int64
	MD5  string
	SHA1 string
}
type contentSnapshot struct {
	Size       int64
	Strong     bool
	ExactPaths bool
	Items      map[string]fileMark
}

func (a contentSnapshot) same(b contentSnapshot) bool {
	if !a.Strong || !b.Strong || a.Size != b.Size || len(a.Items) != len(b.Items) {
		return false
	}
	for path, first := range a.Items {
		second, ok := b.Items[path]
		if !ok || first.Dir != second.Dir || first.Size != second.Size {
			return false
		}
		if first.Dir {
			continue
		}
		switch {
		case first.SHA1 != "" && second.SHA1 != "":
			if first.SHA1 != second.SHA1 {
				return false
			}
		case first.MD5 != "" && second.MD5 != "":
			if first.MD5 != second.MD5 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func (b *batch) measure(ctx context.Context, r remoteShare, limit int64) (contentSnapshot, error) {
	return measure(r.Root, limit, func(parent string) ([]entry, error) {
		return b.q.sourceEntries(ctx, r, parent)
	})
}

func (a contentSnapshot) sameLayout(b contentSnapshot) bool {
	if !a.ExactPaths || !b.ExactPaths || a.Size != b.Size || len(a.Items) != len(b.Items) {
		return false
	}
	for path, first := range a.Items {
		second, ok := b.Items[path]
		if !ok || first.Dir != second.Dir || first.Size != second.Size {
			return false
		}
		if first.SHA1 != "" && second.SHA1 != "" && first.SHA1 != second.SHA1 {
			return false
		}
		if first.MD5 != "" && second.MD5 != "" && first.MD5 != second.MD5 {
			return false
		}
	}
	return true
}

func measure(root []entry, limit int64, children func(string) ([]entry, error)) (contentSnapshot, error) {
	type item struct {
		entry
		parent string
		depth  int
	}
	queue := make([]item, 0, len(root))
	for _, e := range root {
		queue = append(queue, item{entry: e})
	}
	snapshot := contentSnapshot{Strong: true, ExactPaths: true, Items: map[string]fileMark{}}
	visited := map[string]bool{}
	files := 0
	for i := 0; i < len(queue); i++ {
		x := queue[i]
		if i >= 100000 || x.depth >= 64 || visited[x.ID] {
			return snapshot, errors.New("分享目录过深、过大或文件 ID 重复")
		}
		visited[x.ID] = true
		path := x.parent + "/" + x.name()
		if x.name() == "" || strings.Contains(x.name(), "/") {
			snapshot.ExactPaths = false
		}
		if _, exists := snapshot.Items[path]; exists {
			snapshot.ExactPaths = false
		}
		dir, ok := x.directory()
		if !ok {
			return snapshot, errors.New("目录包含未知文件类型")
		}
		if dir {
			snapshot.Items[path] = fileMark{Dir: true}
			list, e := children(x.ID)
			if e != nil {
				var mismatch *paginationError
				if errors.As(e, &mismatch) {
					return snapshot, fmt.Errorf("目录 %s：%w", strings.TrimPrefix(path, "/"), e)
				}
				return snapshot, e
			}
			for _, child := range list {
				queue = append(queue, item{entry: child, parent: path, depth: x.depth + 1})
			}
			continue
		}
		if x.Size == nil || *x.Size < 0 {
			return snapshot, errors.New("分享中有文件未提供有效大小")
		}
		size := *x.Size
		if size > limit || snapshot.Size > limit-size {
			if snapshot.Size <= math.MaxInt64-size {
				snapshot.Size += size
			}
			return snapshot, errOversize
		}
		snapshot.Size += size
		files++
		md5, sha1 := "", ""
		if md5Pattern.MatchString(x.MD5) {
			md5 = strings.ToLower(x.MD5)
		}
		if sha1Pattern.MatchString(x.SHA1) {
			sha1 = strings.ToLower(x.SHA1)
		}
		if md5 == "" && sha1 == "" {
			snapshot.Strong = false
		}
		snapshot.Items[path] = fileMark{Size: size, MD5: md5, SHA1: sha1}
	}
	snapshot.Strong = snapshot.Strong && snapshot.ExactPaths && files > 0
	return snapshot, nil
}
