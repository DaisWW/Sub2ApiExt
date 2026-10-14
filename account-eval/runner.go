package main

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"time"
)

type Engine struct {
	store    *Store
	load     func(context.Context, int64) (CredentialAccount, error)
	evaluate func(context.Context, CredentialAccount, Settings, Question) Result
	mu       sync.Mutex
	cancel   context.CancelFunc
	current  string
	wake     chan struct{}
}

func (e *Engine) notify() {
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

func (e *Engine) loop(ctx context.Context) error {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return nil
		}
		var id string
		err := e.store.db.QueryRowContext(ctx, `SELECT id FROM runs WHERE status='queued' ORDER BY seq LIMIT 1`).Scan(&id)
		if err != nil && err != sql.ErrNoRows {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		if err == nil {
			e.mu.Lock()
			res, claimErr := e.store.db.ExecContext(ctx, `UPDATE runs SET status='running' WHERE id=? AND status='queued'`, id)
			var n int64
			if claimErr == nil {
				n, claimErr = res.RowsAffected()
			}
			batchCtx, cancel := context.WithCancel(ctx)
			if claimErr == nil && n == 1 {
				e.current = id
				e.cancel = cancel
			}
			e.mu.Unlock()
			if claimErr != nil {
				cancel()
				return claimErr
			}
			if n != 1 {
				cancel()
				continue
			}
			err = e.runBatch(batchCtx, id)
			cancel()
			e.mu.Lock()
			e.current = ""
			e.cancel = nil
			e.mu.Unlock()
			if err != nil {
				return err
			}
			continue
		}
		select {
		case <-ctx.Done():
			return nil
		case <-e.wake:
		case <-ticker.C:
		}
	}
}

func (e *Engine) cancelRun(ctx context.Context, id string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	r, err := e.store.run(ctx, id)
	if err != nil {
		return err
	}
	if r.Status != "queued" && r.Status != "running" && r.Status != "cancelling" {
		return fmt.Errorf("任务已结束")
	}
	tx, err := e.store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	status := "cancelled"
	if id == e.current {
		status = "cancelling"
	}
	if _, err = tx.ExecContext(ctx, `UPDATE runs SET status=? WHERE id=?`, status, id); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE results SET status='cancelled' WHERE run_id=? AND status='queued'`, id); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	if id == e.current && e.cancel != nil {
		e.cancel()
	}
	return nil
}

func (e *Engine) runBatch(ctx context.Context, id string) error {
	initCtx, initCancel := context.WithTimeout(context.Background(), 5*time.Second)
	r, err := e.store.run(initCtx, id)
	if err != nil {
		initCancel()
		return err
	}
	cells, err := e.store.results(initCtx, id, false)
	initCancel()
	if err != nil {
		return err
	}
	byAccount := map[int64][]Result{}
	for _, c := range cells {
		if c.Status == "queued" {
			byAccount[c.AccountID] = append(byAccount[c.AccountID], c)
		}
	}
	work := make(chan []Result, len(r.Accounts))
	for _, a := range r.Accounts {
		if c := byAccount[a.ID]; len(c) > 0 {
			work <- c
		}
	}
	close(work)
	workerCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	failures := make(chan error, 1)
	failed := func(err error) {
		select {
		case failures <- err:
		default:
		}
		cancel()
	}
	var wg sync.WaitGroup
	for i := 0; i < r.Settings.Concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for list := range work {
				for _, cell := range list {
					if workerCtx.Err() != nil {
						return
					}
					res, err := e.store.db.ExecContext(workerCtx, `UPDATE results SET status='running' WHERE id=? AND status='queued'`, cell.ID)
					if err != nil {
						if workerCtx.Err() == nil {
							failed(err)
						}
						return
					}
					n, err := res.RowsAffected()
					if err != nil {
						failed(err)
						return
					}
					if n != 1 {
						continue
					}
					requestCtx, requestCancel := context.WithTimeout(workerCtx, time.Duration(r.Settings.Timeout)*time.Second)
					a, loadErr := e.load(requestCtx, cell.AccountID)
					out := Result{Status: "failed", Error: "无法读取账户凭据"}
					if loadErr == nil {
						out = e.evaluate(requestCtx, a, r.Settings, r.Suite.Questions[cell.Question])
					}
					requestCancel()
					out.ID = cell.ID
					out.RunID = id
					out.AccountID = cell.AccountID
					out.Question = cell.Question
					if workerCtx.Err() != nil {
						out.Status = "interrupted"
						out.Error = "服务停止或任务被取消"
					}
					persistCtx, persistCancel := context.WithTimeout(context.Background(), 5*time.Second)
					if err := e.store.finish(persistCtx, out); err != nil {
						persistCancel()
						failed(err)
						return
					}
					persistCancel()
				}
			}
		}()
	}
	wg.Wait()
	select {
	case err := <-failures:
		return err
	default:
	}
	persistCtx, persistCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer persistCancel()
	// Finalization and cancellation must not leave a finished run in cancelling.
	e.mu.Lock()
	defer e.mu.Unlock()
	tx, err := e.store.db.BeginTx(persistCtx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var status string
	if err = tx.QueryRowContext(persistCtx, `SELECT status FROM runs WHERE id=?`, id).Scan(&status); err != nil {
		return err
	}
	if status == "cancelling" {
		status = "cancelled"
		_, err = tx.ExecContext(persistCtx, `UPDATE results SET status='cancelled' WHERE run_id=? AND status IN ('queued','interrupted')`, id)
	} else if ctx.Err() != nil {
		status = "interrupted"
		_, err = tx.ExecContext(persistCtx, `UPDATE results SET status='interrupted' WHERE run_id=? AND status='queued'`, id)
	} else {
		var count int
		err = tx.QueryRowContext(persistCtx, `SELECT COUNT(*) FROM results WHERE run_id=? AND status<>'completed'`, id).Scan(&count)
		status = "completed"
		if count > 0 {
			status = "completed_with_errors"
		}
	}
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(persistCtx, `UPDATE runs SET status=? WHERE id=?`, status, id); err != nil {
		return err
	}
	return tx.Commit()
}
