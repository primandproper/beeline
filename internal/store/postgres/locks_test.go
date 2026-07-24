package postgres_test

import (
	"context"
	"sync"
	"testing"

	"github.com/primandproper/beeline/internal/store/postgres"
	"github.com/primandproper/beeline/internal/store/postgres/pgtest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestTryJanitorLockElectsOneWinnerAndFailsOver pins the per-tick election
// semantics: while one head's sweep holds the lock every other head's try
// loses, and the moment the holder releases (or dies — its connection closes)
// the next try wins.
func TestTryJanitorLockElectsOneWinnerAndFailsOver(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	locker := postgres.NewAdvisoryLocker(pgtest.Open(t))

	holding := make(chan struct{})
	release := make(chan struct{})

	var wg sync.WaitGroup
	wg.Go(func() {
		won, err := locker.TryJanitorLock(ctx, func(context.Context) error {
			close(holding)
			<-release
			return nil
		})
		assert.NoError(t, err)
		assert.True(t, won, "the first head wins the uncontested lock")
	})

	<-holding
	won, err := locker.TryJanitorLock(ctx, func(context.Context) error {
		t.Error("the loser's sweep must not run")
		return nil
	})
	require.NoError(t, err)
	assert.False(t, won, "a second head loses while the first holds the lock")

	close(release)
	wg.Wait()

	won, err = locker.TryJanitorLock(ctx, func(context.Context) error { return nil })
	require.NoError(t, err)
	assert.True(t, won, "after the holder releases, the next tick's try wins (failover)")
}

// TestWithAreaLockSerializesAcrossCallers verifies the blocking variant used
// by area lifecycle mutations: the second caller waits for the first.
func TestWithAreaLockSerializesAcrossCallers(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	locker := postgres.NewAdvisoryLocker(pgtest.Open(t))

	var order []string
	var mu sync.Mutex
	appendStep := func(step string) {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, step)
	}

	inside := make(chan struct{})
	proceed := make(chan struct{})

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		err := locker.WithAreaLock(ctx, 7, func(context.Context) error {
			appendStep("first-start")
			close(inside)
			<-proceed
			appendStep("first-end")
			return nil
		})
		assert.NoError(t, err)
	}()

	<-inside
	go func() {
		defer wg.Done()
		err := locker.WithAreaLock(ctx, 7, func(context.Context) error {
			appendStep("second")
			return nil
		})
		assert.NoError(t, err)
	}()

	close(proceed)
	wg.Wait()

	assert.Equal(t, []string{"first-start", "first-end", "second"}, order,
		"the second mutation of the same area waits out the first")
}
