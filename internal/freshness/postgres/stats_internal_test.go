package postgres

import (
	"errors"
	"fmt"
	"testing"

	platformerrors "github.com/primandproper/primitives-go/v2/errors"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestUnwrapScan pins that the stats memo stays invisible in failures.
//
// cache/memory wraps whatever a loader returns with the key it was loading, so
// without unwrapScan a database outage answered /_ops_/freshness with
// `loading "all": freshness: reading debt: …` — naming an internal memo entry
// in a body a client reads. The wrapper is reproduced here exactly as the cache
// applies it (platform errors.Wrapf), so this fails if that ever stops being
// unwrappable.
func TestUnwrapScan(t *testing.T) {
	t.Parallel()

	t.Run("strips the loader wrapper from a scan error", func(t *testing.T) {
		t.Parallel()

		scanErr := fmt.Errorf("freshness: reading debt: %w", errors.New("connection refused"))
		wrapped := platformerrors.Wrapf(&scanError{err: scanErr}, "loading %q", debtAllKey)

		require.Contains(t, wrapped.Error(), "loading", "precondition: the cache wraps with the key")

		got := unwrapScan(wrapped)
		assert.NotContains(t, got.Error(), "loading", "the memo key must not reach the caller")
		assert.Equal(t, scanErr.Error(), got.Error())
		assert.ErrorIs(t, got, scanErr, "the scan's own error survives, wrappers and all")
	})

	t.Run("leaves an error that is not a scan error alone", func(t *testing.T) {
		t.Parallel()

		// A context cancellation or a loader type mismatch is the cache's own
		// error, not a scan's, and there is nothing to recover from it.
		other := errors.New("context canceled")

		assert.Equal(t, other, unwrapScan(other))
	})
}
