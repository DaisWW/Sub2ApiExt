package store

import (
	"context"
	"database/sql"
	"time"

	"github.com/redis/go-redis/v9"
)

type Store struct {
	db                 *sql.DB
	redis              *redis.Client
	concurrencySlotTTL time.Duration
}

func New(db *sql.DB, redisClient ...*redis.Client) *Store {
	store := &Store{db: db}
	if len(redisClient) > 0 {
		store.redis = redisClient[0]
	}
	return store
}

func (s *Store) SetConcurrencySlotTTL(value time.Duration) {
	s.concurrencySlotTTL = value
}

const cycleAdvisoryLockID int64 = 734205318

func (s *Store) AcquireCycleLease(ctx context.Context) (release func(), acquired bool, err error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, false, err
	}
	var locked bool
	if err := conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, cycleAdvisoryLockID).Scan(&locked); err != nil {
		_ = conn.Close()
		return nil, false, err
	}
	if !locked {
		_ = conn.Close()
		return nil, false, nil
	}
	return func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_, _ = conn.ExecContext(unlockCtx, `SELECT pg_advisory_unlock($1)`, cycleAdvisoryLockID)
		_ = conn.Close()
	}, true, nil
}
