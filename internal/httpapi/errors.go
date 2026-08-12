package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	httpx "github.com/primandproper/platform-go/v10/errors/http"
	"github.com/primandproper/platform-go/v10/routing"
)

// errorBody is this API's error shape: a flat {"error": "..."} object. Every
// error the router renders — a handler's, or one of its own binding failures —
// is encoded as one of these by encodeError.
type errorBody struct {
	Error string `json:"error"`
}

// serverError is a failure the service owns rather than the caller: a store
// read that failed, an engine that errored. Handlers return one instead of
// writing through the escape hatch, so the router logs it, attaches it to the
// request span, and records it on the operation.
//
// Client errors deliberately do NOT use this — see the convention documented on
// Register.
//
// It keeps the operator's text and the caller's text apart, because they were
// never the same: handlers logged a description ("computing estimate") and sent
// the bare cause. Error() is what the router logs and traces, so it carries the
// description; clientMessage is what encodeError sends, so it stays the bare
// cause it has always been.
type serverError struct {
	cause         error
	description   string
	clientMessage string
}

func (e *serverError) Error() string {
	if e.cause == nil {
		return e.description
	}

	return fmt.Sprintf("%s: %v", e.description, e.cause)
}

// Unwrap keeps errors.Is/As working through the wrapper, so a caller testing
// for a sentinel cause still finds it.
func (e *serverError) Unwrap() error { return e.cause }

// internalError builds the error a handler returns when its own work failed.
// description is for the log and the span; the caller keeps receiving cause's
// own text, exactly as it did when handlers wrote 500s through fail.
func internalError(description string, cause error) error {
	return &serverError{description: description, cause: cause, clientMessage: cause.Error()}
}

// internalMessage is internalError for a failure with no underlying error to
// report — an invariant this package violated on its own.
func internalMessage(message string) error {
	return &serverError{description: message, clientMessage: message}
}

// encodeError renders every error the router handles into this API's flat body.
//
// It exists because the router's default rendering is the platform APIError
// envelope — {"error": {"message": ..., "code": ...}, "details": {...}} — whose
// "error" is an object where this API has always sent a string. Handlers avoid
// it by writing their own bytes (see wire.go), but the router's own binding
// failures never reach a handler, so before this encoder a malformed request
// body answered in the envelope shape while every other 400 answered flat. The
// console and the follower client both read .error as a string.
//
// A serverError carries the status the handler chose. Everything else —
// binding failures above all — keeps the status and message the platform would
// have used, re-shaped: DefaultErrorBody is the source of both, so a decode
// failure still reports "could not decode request body" with the status its
// code maps to, and only the envelope around it changes.
func encodeError(ctx context.Context, err error) (status int, body any) {
	if svcErr, ok := errors.AsType[*serverError](err); ok {
		return http.StatusInternalServerError, errorBody{Error: svcErr.clientMessage}
	}

	status, body = routing.DefaultErrorBody(ctx, err)

	return status, errorBody{Error: platformMessage(body, err)}
}

// platformMessage pulls the client-facing message out of the platform envelope
// DefaultErrorBody built, falling back to the error's own text. The envelope's
// message is preferred because for a binding failure it is the sanitized one —
// the wrapped cause is for the operation record, not for the caller.
func platformMessage(body any, err error) string {
	if resp, ok := body.(*httpx.APIResponse[any]); ok && resp.Error != nil && resp.Error.Message != "" {
		return resp.Error.Message
	}

	return err.Error()
}
