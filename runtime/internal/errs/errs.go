// Package errs defines the error taxonomy shared by every part of the runtime.
// Provider adapters translate their wire-level failures into these kinds so that
// nothing above them depends on a provider's error format.
package errs

import (
	"context"
	"errors"
	"fmt"
	"net/url"
)

type Kind string

const (
	// InvalidConfig means the runtime configuration cannot be used.
	InvalidConfig Kind = "invalid_config"
	// Unsupported means a requirement cannot be met by the selected provider or runtime.
	Unsupported Kind = "unsupported"
	// BadRequest means the caller's request, or one the runtime built, was rejected.
	BadRequest Kind = "bad_request"
	// Auth means credentials were missing or refused.
	Auth Kind = "auth"
	// RateLimited means the provider asked us to slow down.
	RateLimited Kind = "rate_limited"
	// Unavailable means the provider or a tool server could not be reached or failed.
	Unavailable Kind = "unavailable"
	// LimitExceeded means a configured limit, such as tool calls, was reached.
	LimitExceeded Kind = "limit_exceeded"
	// Forbidden means a tool call was not permitted.
	Forbidden Kind = "forbidden"
	// Timeout means a deadline passed.
	Timeout Kind = "timeout"
	// Canceled means the caller went away.
	Canceled Kind = "canceled"
	// Overloaded means the runtime is at its concurrency limit. The request was not started.
	Overloaded Kind = "overloaded"
	// OutcomeUnknown means a tool call may or may not have taken effect. Callers must
	// not assume it failed, and must not repeat it blindly.
	OutcomeUnknown Kind = "outcome_unknown"
	// Internal is anything else.
	Internal Kind = "internal"
)

type Error struct {
	Kind    Kind
	Message string
	Err     error
}

func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %s: %v", e.Kind, e.Message, e.Err)
	}
	return fmt.Sprintf("%s: %s", e.Kind, e.Message)
}

// Detail is the message with any wrapped cause, without the kind prefix.
func (e *Error) Detail() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %v", e.Message, e.Err)
	}
	return e.Message
}

func (e *Error) Unwrap() error { return e.Err }

func New(kind Kind, format string, args ...any) *Error {
	return &Error{Kind: kind, Message: fmt.Sprintf(format, args...)}
}

func Wrap(kind Kind, err error, format string, args ...any) *Error {
	return &Error{Kind: kind, Message: fmt.Sprintf(format, args...), Err: err}
}

// Cause strips the URL that net/http puts in transport errors, which can carry
// credentials in userinfo or query strings.
func Cause(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// KindOf returns the kind of err, mapping context errors and defaulting to Internal.
func KindOf(err error) Kind {
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return Timeout
	case errors.Is(err, context.Canceled):
		return Canceled
	}
	return Internal
}

// FromContext converts a finished context into a typed error, or nil if still live.
func FromContext(ctx context.Context) *Error {
	switch err := ctx.Err(); {
	case errors.Is(err, context.DeadlineExceeded):
		return Wrap(Timeout, err, "deadline exceeded")
	case errors.Is(err, context.Canceled):
		return Wrap(Canceled, err, "request canceled")
	}
	return nil
}
