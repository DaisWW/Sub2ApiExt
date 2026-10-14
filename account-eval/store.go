package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	_ "modernc.org/sqlite"
)

type Store struct{ db *sql.DB }

func openStore(ctx context.Context, path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	_, err = db.ExecContext(ctx, `PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000; PRAGMA foreign_keys=ON;
	CREATE TABLE IF NOT EXISTS suites(id TEXT PRIMARY KEY,data TEXT NOT NULL);
	CREATE TABLE IF NOT EXISTS runs(seq INTEGER PRIMARY KEY AUTOINCREMENT,id TEXT UNIQUE NOT NULL,status TEXT NOT NULL,data TEXT NOT NULL);
	CREATE TABLE IF NOT EXISTS results(id INTEGER PRIMARY KEY AUTOINCREMENT,run_id TEXT NOT NULL REFERENCES runs(id),account_id INTEGER NOT NULL,question INTEGER NOT NULL,status TEXT NOT NULL,data TEXT NOT NULL DEFAULT '{}',mark TEXT NOT NULL DEFAULT '',UNIQUE(run_id,account_id,question));
	CREATE INDEX IF NOT EXISTS result_run ON results(run_id);
	UPDATE results SET status='interrupted' WHERE status IN ('queued','running') AND run_id IN (SELECT id FROM runs WHERE status IN ('running','cancelling'));
	UPDATE runs SET status='interrupted' WHERE status IN ('running','cancelling');`)
	if err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) suites(ctx context.Context) ([]Suite, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT data FROM suites ORDER BY rowid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Suite{}
	for rows.Next() {
		var raw string
		var v Suite
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(raw), &v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *Store) saveSuite(ctx context.Context, v Suite) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO suites(id,data) VALUES(?,?) ON CONFLICT(id) DO UPDATE SET data=excluded.data`, v.ID, string(b))
	return err
}

func (s *Store) createRun(ctx context.Context, r Run, cells []Result) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `INSERT INTO runs(id,status,data) VALUES(?,'queued',?)`, r.ID, string(b)); err != nil {
		return err
	}
	for _, c := range cells {
		if _, err = tx.ExecContext(ctx, `INSERT INTO results(run_id,account_id,question,status) VALUES(?,?,?,'queued')`, r.ID, c.AccountID, c.Question); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) runs(ctx context.Context) ([]Run, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT r.data,r.status,COUNT(x.id),COALESCE(SUM(x.status NOT IN ('queued','running')),0) FROM runs r LEFT JOIN results x ON x.run_id=r.id GROUP BY r.id ORDER BY r.seq DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Run{}
	for rows.Next() {
		var raw, status string
		var r Run
		var total, done int
		if err := rows.Scan(&raw, &status, &total, &done); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(raw), &r); err != nil {
			return nil, err
		}
		r.Status = status
		r.Total = total
		r.Done = done
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) run(ctx context.Context, id string) (Run, error) {
	var r Run
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT data,status FROM runs WHERE id=?`, id).Scan(&raw, &r.Status)
	if err != nil {
		return r, err
	}
	status := r.Status
	err = json.Unmarshal([]byte(raw), &r)
	r.Status = status
	return r, err
}

func (s *Store) results(ctx context.Context, id string, full bool) ([]Result, error) {
	projection := "json_remove(data,'$.answer','$.html')"
	if full {
		projection = "data"
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,account_id,question,status,`+projection+`,mark FROM results WHERE run_id=? ORDER BY account_id,question`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Result{}
	for rows.Next() {
		var r Result
		var raw, status, mark string
		var rid, aid int64
		var q int
		if err := rows.Scan(&rid, &aid, &q, &status, &raw, &mark); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(raw), &r); err != nil {
			return nil, err
		}
		r.ID = rid
		r.RunID = id
		r.AccountID = aid
		r.Question = q
		r.Status = status
		r.Mark = mark
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) result(ctx context.Context, id int64) (Result, error) {
	var r Result
	var raw, status, mark string
	err := s.db.QueryRowContext(ctx, `SELECT run_id,account_id,question,status,data,mark FROM results WHERE id=?`, id).Scan(&r.RunID, &r.AccountID, &r.Question, &status, &raw, &mark)
	if err != nil {
		return r, err
	}
	run, aid, q := r.RunID, r.AccountID, r.Question
	err = json.Unmarshal([]byte(raw), &r)
	r.ID = id
	r.RunID = run
	r.AccountID = aid
	r.Question = q
	r.Status = status
	r.Mark = mark
	return r, err
}

func (s *Store) finish(ctx context.Context, r Result) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `UPDATE results SET status=?,data=? WHERE id=? AND status='running'`, r.Status, string(b), r.ID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err == nil && n != 1 {
		return fmt.Errorf("结果状态已变化")
	}
	return err
}
