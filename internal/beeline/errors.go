package beeline

import "errors"

// ErrNotFound reports that a requested entity does not exist. Every store that
// implements a control-plane repository wraps it — postgres.ErrNotFound and the
// in-memory double's memory.ErrNotFound both do, as does
// control.ErrProviderNotFound — so callers can branch on errors.Is without
// depending on a concrete store package, and without matching on message text.
//
// The HTTP layer's 404 mapping is the caller that matters: it used to test
// strings.Contains(err.Error(), "not found"), which made the status code depend
// on two independent stores happening to word their errors the same way. The
// conformance suite in internal/control/repositorytest pins the errors.Is
// behavior for both.
var ErrNotFound = errors.New("not found")
