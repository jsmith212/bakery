package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jsmith212/bakery/internal/db"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestAcquireBootLockWithWaitRetriesThenSucceeds proves the poll: a holder
// that releases partway through the budget is picked up on a LATER attempt,
// not just the first one -- this is the node-drain / slow-terminating-pod
// case BootLockWait exists for.
func TestAcquireBootLockWithWaitRetriesThenSucceeds(t *testing.T) {
	t.Parallel()

	var attempts int

	try := func(_ context.Context) (*db.BootLock, error) {
		attempts++
		if attempts < 3 {
			return nil, db.ErrLocked
		}

		return &db.BootLock{}, nil
	}

	lock, err := acquireBootLockWithWait(t.Context(), time.Second, time.Millisecond, discardLogger(), try)
	if err != nil {
		t.Fatalf("acquireBootLockWithWait() error = %v, want success on the 3rd attempt", err)
	}

	if lock == nil {
		t.Fatal("acquireBootLockWithWait() returned a nil lock on success")
	}

	if attempts != 3 {
		t.Errorf("attempts = %d, want 3 (1 initial + 2 retries)", attempts)
	}
}

// TestAcquireBootLockWithWaitReturnsCtxErrOnCancel proves a SIGTERM during the
// wait is a clean exit, not a hang: the poll must select on ctx.Done(), never
// block on the ticker alone.
func TestAcquireBootLockWithWaitReturnsCtxErrOnCancel(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())

	var attempts int

	try := func(_ context.Context) (*db.BootLock, error) {
		attempts++
		if attempts == 2 {
			cancel() // cancel mid-wait, right after the 2nd failed attempt
		}

		return nil, db.ErrLocked
	}

	_, err := acquireBootLockWithWait(ctx, time.Minute, 20*time.Millisecond, discardLogger(), try)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("acquireBootLockWithWait() error = %v, want context.Canceled", err)
	}
}

// TestAcquireBootLockWithWaitExhaustsBudgetAndNamesRollingUpdate: BootLockWait
// cannot rescue a RollingUpdate deployment -- see acquireBootLockWithWait's
// doc -- so the failure at budget exhaustion must say so plainly, not merely
// time out silently.
func TestAcquireBootLockWithWaitExhaustsBudgetAndNamesRollingUpdate(t *testing.T) {
	t.Parallel()

	try := func(_ context.Context) (*db.BootLock, error) { return nil, db.ErrLocked }

	_, err := acquireBootLockWithWait(t.Context(), 50*time.Millisecond, 10*time.Millisecond, discardLogger(), try)
	if !errors.Is(err, db.ErrLocked) {
		t.Fatalf("acquireBootLockWithWait() error = %v, want it to wrap db.ErrLocked", err)
	}

	if !strings.Contains(err.Error(), "RollingUpdate") {
		t.Errorf("error = %q, want it to name RollingUpdate as the likely cause", err.Error())
	}
}

// TestAcquireBootLockWithWaitZeroIsFailFast: the default (0s) must behave
// EXACTLY like today -- one attempt, no retry loop, no WARN log -- so an
// operator who never opts in sees no behavior change at all.
func TestAcquireBootLockWithWaitZeroIsFailFast(t *testing.T) {
	t.Parallel()

	var attempts int

	try := func(_ context.Context) (*db.BootLock, error) {
		attempts++

		return nil, db.ErrLocked
	}

	_, err := acquireBootLockWithWait(t.Context(), 0, time.Millisecond, discardLogger(), try)
	if !errors.Is(err, db.ErrLocked) {
		t.Fatalf("acquireBootLockWithWait() error = %v, want db.ErrLocked", err)
	}

	if attempts != 1 {
		t.Errorf("attempts = %d, want 1 (BootLockWait=0 must not retry)", attempts)
	}
}
