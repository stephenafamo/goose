package lock

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/sethvargo/go-retry"
)

// NewPostgresSessionLocker returns a SessionLocker that utilizes PostgreSQL's exclusive
// session-level advisory lock mechanism.
//
// This function creates a SessionLocker that can be used to acquire and release locks for
// synchronization purposes. The lock acquisition is retried until it is successfully acquired or
// until the maximum duration is reached. The default lock duration is set to 60 minutes, and the
// default unlock duration is set to 1 minute.
//
// See [SessionLockerOption] for options that can be used to configure the SessionLocker.
func NewPostgresSessionLocker(opts ...SessionLockerOption) (SessionLocker, error) {
	cfg := sessionLockerConfig{
		lockID:        DefaultLockID,
		lockTimeout:   DefaultLockTimeout,
		unlockTimeout: DefaultUnlockTimeout,
	}
	for _, opt := range opts {
		if err := opt.apply(&cfg); err != nil {
			return nil, err
		}
	}
	return &postgresSessionLocker{
		lockID: cfg.lockID,
		retryLock: retry.WithMaxDuration(
			cfg.lockTimeout,
			retry.NewConstant(2*time.Second),
		),
		retryUnlock: retry.WithMaxDuration(
			cfg.unlockTimeout,
			retry.NewConstant(2*time.Second),
		),
	}, nil
}

type postgresSessionLocker struct {
	lockID      int64
	retryLock   retry.Backoff
	retryUnlock retry.Backoff
}

var _ SessionLocker = (*postgresSessionLocker)(nil)

func (l *postgresSessionLocker) SessionLock(ctx context.Context, conn *sql.Conn) error {
	return retry.Do(ctx, l.retryLock, func(ctx context.Context) error {
		row := conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1)", l.lockID)
		var locked bool
		if err := row.Scan(&locked); err != nil {
			return fmt.Errorf("failed to execute pg_try_advisory_lock: %w", err)
		}
		if locked {
			// A session-level advisory lock was acquired.
			return nil
		}
		// A session-level advisory lock could not be acquired. This is likely because another
		// process has already acquired the lock. We will continue retrying until the lock is
		// acquired or the maximum number of retries is reached.
		return retry.RetryableError(errors.New("failed to acquire lock"))
	})
}

func (l *postgresSessionLocker) SessionUnlock(ctx context.Context, conn *sql.Conn) error {
	return retry.Do(ctx, l.retryUnlock, func(ctx context.Context) error {
		var unlocked bool
		row := conn.QueryRowContext(ctx, "SELECT pg_advisory_unlock($1)", l.lockID)
		if err := row.Scan(&unlocked); err != nil {
			return fmt.Errorf("failed to execute pg_advisory_unlock: %w", err)
		}
		if unlocked {
			// A session-level advisory lock was released.
			return nil
		}
		/*
			TODO(mf): provide users with some documentation on how they can unlock the session
			manually.

			This is probably not an issue for 99.99% of users since pg_advisory_unlock_all() will
			release all session level advisory locks held by the current session. This function is
			implicitly invoked at session end, even if the client disconnects ungracefully.

			Here is output from a session that has a lock held:

			SELECT pid,granted,((classid::bigint<<32)|objid::bigint)AS goose_lock_id FROM pg_locks
			WHERE locktype='advisory';

			| pid | granted | goose_lock_id       |
			|-----|---------|---------------------|
			| 191 | t       | 5887940537704921958 |

			A forceful way to unlock the session is to terminate the backend with SIGTERM:

			SELECT pg_terminate_backend(191);

			Subsequent commands on the same connection will fail with:

			Query 1 ERROR: FATAL: terminating connection due to administrator command
		*/
		return retry.RetryableError(errors.New("failed to unlock session"))
	})
}
