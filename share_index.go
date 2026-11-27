package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"
)

func shareIndexKey(share shareInfo) string {
	if share.ID != "" {
		return "id:" + share.ID
	}
	if share.PwdID != "" {
		return "slug:" + share.PwdID
	}
	if share.URL != "" {
		return "url:" + share.URL
	}
	return ""
}

func storeShare(db sqlWriter, account string, share shareInfo) error {
	key := shareIndexKey(share)
	if key == "" {
		return errors.New("我的分享列表中有项目缺少分享 ID 和链接")
	}
	data, err := json.Marshal(share)
	if err != nil {
		return err
	}
	_, err = db.Exec("INSERT INTO share_index(account,share_key,value) VALUES(?,?,?) ON CONFLICT(account,share_key) DO UPDATE SET value=excluded.value", account, key, string(data))
	return err
}

func (s *state) indexedShares(account string) ([]shareInfo, error) {
	rows, err := s.db.Query("SELECT value FROM share_index WHERE account=? ORDER BY rowid DESC", account)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var shares []shareInfo
	for rows.Next() {
		var value string
		var share shareInfo
		if err = rows.Scan(&value); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(value), &share); err != nil {
			return nil, err
		}
		shares = append(shares, share)
	}
	return shares, rows.Err()
}

func (b *batch) refreshShares(ctx context.Context) ([]shareInfo, error) {
	shares, err := b.q.mine(ctx)
	if err != nil {
		return nil, err
	}
	previous, err := b.s.indexedShares(b.account)
	if err != nil {
		return nil, err
	}
	knownFiles := make(map[string][]string, len(previous))
	for _, share := range previous {
		knownFiles[shareIndexKey(share)] = share.FIDs
	}
	seen := make(map[string]bool, len(shares))
	for i := range shares {
		key := shareIndexKey(shares[i])
		if key == "" || seen[key] {
			return nil, errors.New("我的分享分页包含缺失或重复的分享标识")
		}
		seen[key] = true
		if len(shares[i].FIDs) == 0 {
			shares[i].FIDs = knownFiles[key]
		}
	}
	tx, err := b.s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("DELETE FROM share_index WHERE account=?", b.account); err != nil {
		return nil, err
	}
	for _, share := range slices.Backward(shares) {
		if err = storeShare(tx, b.account, share); err != nil {
			return nil, err
		}
	}
	if _, err = tx.Exec("INSERT INTO settings(key,value) VALUES(?, 'true') ON CONFLICT(key) DO UPDATE SET value=excluded.value", "share-index:"+b.account); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	b.shares, b.loaded = shares, true
	var account session
	if len(shares) != 0 && b.s.setting("session", &account) == nil &&
		account.Account == b.account && account.Cookie == b.q.cookie && account.AvatarURL == "" {
		avatarCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		if avatar := b.q.ownShareAvatar(avatarCtx, shares); avatar != "" {
			account.AvatarURL = avatar
			_ = b.s.set("session", account)
		}
	}
	return shares, nil
}

func (b *batch) completeShare(ctx context.Context, sourceID string, rec record, share shareInfo) error {
	tx, err := b.s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = saveRecord(tx, b.account, sourceID, rec); err != nil {
		return err
	}
	if err = rememberSource(tx, b.account, sourceID, rec); err != nil {
		return err
	}
	if shareIndexKey(share) != "" {
		if err = storeShare(tx, b.account, share); err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	if b.loaded && shareIndexKey(share) != "" {
		b.shares = slices.DeleteFunc(b.shares, func(old shareInfo) bool { return shareIndexKey(old) == shareIndexKey(share) })
		b.shares = slices.Insert(b.shares, 0, share)
	}
	return nil
}

func (b *batch) cancelShares(ctx context.Context, ids []string) error {
	if _, err := b.ownedShares(ctx, ids); err != nil {
		return err
	}
	_, err := b.cancelOwnedShares(ctx, ids)
	return err
}

func (b *batch) ownedShares(ctx context.Context, ids []string) ([]shareInfo, error) {
	shares, err := b.mine(ctx)
	if err != nil {
		return nil, err
	}
	owned := make(map[string]shareInfo, len(shares))
	for _, share := range shares {
		owned[share.ID] = share
	}
	selected := make([]shareInfo, 0, len(ids))
	for _, id := range ids {
		share, ok := owned[id]
		if id == "" || !ok {
			return nil, errors.New("有分享不在当前账号的列表中，请刷新后重试")
		}
		selected = append(selected, share)
	}
	return selected, nil
}

func (b *batch) cancelOwnedShares(ctx context.Context, ids []string) (bool, error) {
	if err := b.q.cancelShares(ctx, ids); err != nil {
		if definitelyRejected(err) {
			return false, err
		}
		_ = b.s.set("share-index:"+b.account, false)
		return false, errors.New("夸克未确认取消结果，请先刷新我的分享")
	}
	if err := b.s.removeShareIndex(ctx, b.account, ids); err != nil {
		_ = b.s.set("share-index:"+b.account, false)
		return true, errors.New("夸克已取消分享，本地列表更新失败，请刷新我的分享")
	}
	return true, nil
}

func (b *batch) shareFiles(ctx context.Context, share shareInfo) ([]string, error) {
	if len(share.FIDs) > 0 {
		return share.FIDs, nil
	}
	shareID := share.ID
	if share.link() == "" {
		detail, err := b.q.link(ctx, share.ID)
		if err != nil {
			return nil, err
		}
		if detail.ID != "" && detail.ID != share.ID {
			return nil, errUncertain
		}
		if len(detail.FIDs) > 0 {
			return detail.FIDs, nil
		}
		share = detail
	}
	input := sources(share.link())
	if len(input) != 1 {
		return nil, errUncertain
	}
	detail, err := b.q.inspect(ctx, input[0])
	if err != nil {
		return nil, err
	}
	if !detail.Owner || detail.ShareID != shareID {
		return nil, errUncertain
	}
	return ids(detail.Root), nil
}

func (b *batch) cancelAndDeleteShares(ctx context.Context, shareIDs []string) (bool, error) {
	selected, err := b.ownedShares(ctx, shareIDs)
	if err != nil {
		return false, err
	}
	seen := make(map[string]bool)
	var fids []string
	for _, share := range selected {
		files, err := b.shareFiles(ctx, share)
		if err != nil {
			return false, fmt.Errorf("无法确认 %q 的源文件：%w", share.Title, err)
		}
		if !uniqueIDs(files) || slices.Contains(files, "0") {
			return false, fmt.Errorf("无法确认 %q 的源文件", share.Title)
		}
		for _, id := range files {
			if !seen[id] {
				seen[id] = true
				fids = append(fids, id)
			}
		}
	}
	cancelled, err := b.cancelOwnedShares(ctx, shareIDs)
	if !cancelled {
		return false, err
	}
	if err != nil {
		return true, fmt.Errorf("源文件未提交删除：%w", err)
	}
	if err = b.q.recycleFiles(ctx, fids); err != nil {
		_ = b.s.set("share-index:"+b.account, false)
		if definitelyRejected(err) || errors.Is(err, errTaskFailed) {
			return true, fmt.Errorf("源文件删除失败：%w", err)
		}
		return true, errors.New("源文件删除结果未确认，请到夸克网盘或回收站核对")
	}
	_ = b.s.set("share-index:"+b.account, false)
	if err := b.s.clearDeletedFileReferences(ctx, b.account, fids); err != nil {
		return true, errors.New("源文件已移入回收站，本地历史更新失败")
	}
	return true, nil
}

func (s *state) clearDeletedFileReferences(ctx context.Context, account string, fids []string) error {
	encoded, err := json.Marshal(fids)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `UPDATE records SET value=json_set(value, '$.FIDs', json('[]'))
		WHERE account=? AND json_extract(value, '$.Phase')='complete'
		AND EXISTS (SELECT 1 FROM json_each(records.value, '$.FIDs') AS saved
			JOIN json_each(?) AS deleted ON saved.value=deleted.value)`, account, string(encoded))
	return err
}

func (s *state) removeShareIndex(ctx context.Context, account string, ids []string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, id := range ids {
		if _, err = tx.ExecContext(ctx, "DELETE FROM share_index WHERE account=? AND share_key=?", account, "id:"+id); err != nil {
			return err
		}
	}
	return tx.Commit()
}
