package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

type session struct {
	Account   string
	Nickname  string
	AvatarURL string
	Cookie    string
}

type record struct {
	SourceURL       string `json:",omitempty"`
	Target          string
	Before          []string
	Expected        []entry
	Content         contentSnapshot
	Phase           string
	Link            string
	ShareID         string
	Task            string
	FIDs            []string
	Title           string
	RecordedAt      int64
	AttachmentsDone bool            `json:",omitempty"`
	AttachmentCopy  *attachmentCopy `json:",omitempty"`
	Attached        []attachedFile  `json:",omitempty"`
}

type state struct {
	db    *sql.DB
	locks *sql.DB
}

func openState(dir string) (*state, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	for _, name := range []string{"quark.db", "run.db"} {
		p := filepath.Join(dir, name)
		f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err == nil {
			if err = f.Close(); err != nil {
				return nil, err
			}
		} else if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "quark.db"))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	for _, stmt := range []string{
		"PRAGMA busy_timeout=5000",
		"CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY, value TEXT NOT NULL)",
		"CREATE TABLE IF NOT EXISTS records (account TEXT NOT NULL, source TEXT NOT NULL, value TEXT NOT NULL, PRIMARY KEY (account,source))",
		"CREATE TABLE IF NOT EXISTS share_index (account TEXT NOT NULL, share_key TEXT NOT NULL, value TEXT NOT NULL, PRIMARY KEY (account,share_key))",
		"CREATE TABLE IF NOT EXISTS source_links (account TEXT NOT NULL, source TEXT NOT NULL, source_url TEXT NOT NULL, share_id TEXT NOT NULL, link TEXT NOT NULL, PRIMARY KEY (account,source))",
	} {
		if _, err = db.Exec(stmt); err != nil {
			db.Close()
			return nil, err
		}
	}
	locks, err := sql.Open("sqlite", filepath.Join(dir, "run.db"))
	if err != nil {
		db.Close()
		return nil, err
	}
	locks.SetMaxOpenConns(1)
	return &state{db: db, locks: locks}, nil
}

func (s *state) close() {
	s.locks.Close()
	s.db.Close()
}

func (s *state) acquire(ctx context.Context) (func(), error) {
	conn, err := s.locks.Conn(ctx)
	if err != nil {
		return nil, err
	}
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		conn.Close()
		return nil, errors.New("另一个转换任务正在使用该数据目录")
	}
	return func() {
		conn.ExecContext(context.Background(), "ROLLBACK")
		conn.Close()
	}, nil
}

func (s *state) setting(key string, into any) error {
	var data string
	err := s.db.QueryRow("SELECT value FROM settings WHERE key=?", key).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal([]byte(data), into)
}

func (s *state) set(key string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = s.db.Exec("INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", key, string(data))
	return err
}

func (s *state) get(account, id string) (record, error) {
	var r record
	var data string
	err := s.db.QueryRow("SELECT value FROM records WHERE account=? AND source=?", account, id).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return r, nil
	}
	if err != nil {
		return r, err
	}
	return r, json.Unmarshal([]byte(data), &r)
}

func (s *state) save(account, id string, r record) error {
	return saveRecord(s.db, account, id, r)
}

type sqlWriter interface {
	Exec(string, ...any) (sql.Result, error)
}

func (s *state) sourceLink(account, id string) (record, error) {
	var rec record
	err := s.db.QueryRow("SELECT source_url, share_id, link FROM source_links WHERE account=? AND source=?", account, id).
		Scan(&rec.SourceURL, &rec.ShareID, &rec.Link)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return rec, err
}

func rememberSource(db sqlWriter, account, id string, rec record) error {
	if rec.Phase != "complete" || (rec.ShareID == "" && rec.Link == "") {
		return nil
	}
	if rec.SourceURL == "" {
		rec.SourceURL = "https://pan.quark.cn/s/" + id
	}
	_, err := db.Exec(`INSERT INTO source_links(account,source,source_url,share_id,link) VALUES(?,?,?,?,?)
		ON CONFLICT(account,source) DO UPDATE SET source_url=excluded.source_url, share_id=excluded.share_id, link=excluded.link`,
		account, id, rec.SourceURL, rec.ShareID, rec.Link)
	return err
}

func saveRecord(db sqlWriter, account, id string, r record) error {
	if r.RecordedAt == 0 && r.Phase != "" {
		r.RecordedAt = time.Now().Unix()
	}
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, err = db.Exec("INSERT INTO records(account,source,value) VALUES(?,?,?) ON CONFLICT(account,source) DO UPDATE SET value=excluded.value", account, id, string(data))
	return err
}

func (s *state) history(account string) ([]result, error) {
	out := make([]result, 0)
	if account == "" {
		return out, nil
	}
	rows, e := s.db.Query(`SELECT r.source, r.value, COALESCE(json_extract(i.value, '$.Title'), '')
		FROM records r LEFT JOIN share_index i ON i.account=r.account AND i.share_key='id:' || json_extract(r.value, '$.ShareID')
		WHERE r.account=? ORDER BY r.rowid DESC`, account)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	for rows.Next() {
		var id, data, indexedTitle string
		if e = rows.Scan(&id, &data, &indexedTitle); e != nil {
			return nil, e
		}
		var rec record
		if e = json.Unmarshal([]byte(data), &rec); e != nil {
			return nil, e
		}
		if rec.Phase == "" {
			continue
		}
		title := rec.Title
		if title == "" {
			title = indexedTitle
		}
		sourceURL := rec.SourceURL
		if sourceURL == "" {
			sourceURL = "https://pan.quark.cn/s/" + id
		}
		row := result{Source: sourceURL, Link: rec.Link, Title: title, Status: pending, RecordedAt: rec.RecordedAt}
		if rec.Phase == "complete" {
			row.Status = reused
		} else {
			row.Message = "有未完成的任务，请重新输入源分享继续检查"
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func (s *state) removeHistory(ctx context.Context, account string, sourceIDs []string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, id := range sourceIDs {
		var data string
		err = tx.QueryRowContext(ctx, "SELECT value FROM records WHERE account=? AND source=?", account, id).Scan(&data)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		var rec record
		if err = json.Unmarshal([]byte(data), &rec); err != nil {
			return err
		}
		if rec.Phase == "" {
			continue
		}
		if rec.Phase != "complete" {
			return errors.New("所选记录中有未完成的转存任务，请先处理该任务")
		}
		if err = rememberSource(tx, account, id, rec); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "DELETE FROM records WHERE account=? AND source=?", account, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}
