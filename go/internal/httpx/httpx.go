// Package httpx holds the JSON response helpers the API handlers share.
//
// The error body is {"detail": "..."} because every shipped client already
// reads that key; changing it would surface as an empty error toast.
package httpx

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
)

// Error is an API error with the status code the client should see.
type Error struct {
	Status int
	Detail string
}

func (e *Error) Error() string { return e.Detail }

// NewError builds an API error.
func NewError(status int, detail string) *Error {
	return &Error{Status: status, Detail: detail}
}

// BadRequest, Unauthorized and friends keep handler code short.
func BadRequest(detail string) *Error   { return NewError(http.StatusBadRequest, detail) }
func Unauthorized(detail string) *Error { return NewError(http.StatusUnauthorized, detail) }
func Forbidden(detail string) *Error    { return NewError(http.StatusForbidden, detail) }
func NotFound(detail string) *Error     { return NewError(http.StatusNotFound, detail) }
func Conflict(detail string) *Error     { return NewError(http.StatusConflict, detail) }

// JSON writes a value with the given status.
func JSON(w http.ResponseWriter, status int, value any) {
	body, err := json.Marshal(value)
	if err != nil {
		slog.Error("encode response", "error", err)
		http.Error(w, `{"detail":"internal error"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// OK writes a 200 JSON body.
func OK(w http.ResponseWriter, value any) { JSON(w, http.StatusOK, value) }

// NoContent finishes a request with an empty 204.
func NoContent(w http.ResponseWriter) { w.WriteHeader(http.StatusNoContent) }

// Fail writes an error body, honouring *Error for the status code.
func Fail(w http.ResponseWriter, err error) {
	var apiErr *Error
	if errors.As(err, &apiErr) {
		JSON(w, apiErr.Status, map[string]string{"detail": apiErr.Detail})
		return
	}
	slog.Error("unhandled handler error", "error", err)
	JSON(w, http.StatusInternalServerError, map[string]string{"detail": "internal error"})
}

// Decode reads a JSON body into dst, rejecting unknown-sized or empty bodies
// the way a client expects: an empty body is a 400, not a silent zero value.
func Decode(r *http.Request, dst any) error {
	if r.Body == nil {
		return BadRequest("request body is required")
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, 8<<20))
	// Unknown fields are ignored on purpose: the shipped clients send a few
	// compatibility keys the server has no use for, and rejecting the whole
	// request over one extra key is a failure the person cannot act on.
	if err := dec.Decode(dst); err != nil {
		if errors.Is(err, io.EOF) {
			return BadRequest("request body is required")
		}
		if isTooLarge(err) {
			return NewError(http.StatusRequestEntityTooLarge, "request body is too large")
		}
		return BadRequest("invalid JSON body: " + err.Error())
	}
	return nil
}

// DecodeOptional behaves like Decode but tolerates a completely empty body so
// endpoints whose fields are all optional accept no payload at all.
func DecodeOptional(r *http.Request, dst any) error {
	if r.Body == nil || r.ContentLength == 0 {
		return nil
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, 8<<20))
	if err := dec.Decode(dst); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		if isTooLarge(err) {
			return NewError(http.StatusRequestEntityTooLarge, "request body is too large")
		}
		return BadRequest("invalid JSON body: " + err.Error())
	}
	return nil
}

// isTooLarge recognises the error http.MaxBytesReader raises, whose text is
// stable but not exported as a type check.
func isTooLarge(err error) bool {
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		return true
	}
	return strings.Contains(err.Error(), "request body too large")
}
