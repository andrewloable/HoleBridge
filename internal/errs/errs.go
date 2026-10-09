// Package errs is the error catalog: every error the host prints carries a stable code from
// spec/errors.json, with its problem, cause and fix. Catalog is generated from that file by
// go generate (see gen/).
package errs

import (
	"fmt"
	"strings"
)

//go:generate go run ./gen

// Entry is one code's entry in spec/errors.json.
type Entry struct {
	Code, Title, Problem, Cause, Fix string
	Where                            []string
}

// Error is an error with a catalog code. Detail adds specifics; Err is the wrapped cause.
type Error struct {
	Code, Detail string
	Err          error
}

// E returns an Error for code. It panics if code is not in Catalog.
func E(code, detail string, err error) *Error {
	if _, ok := Catalog[code]; !ok {
		panic("errs: unknown code " + code)
	}
	return &Error{Code: code, Detail: detail, Err: err}
}

// Error returns "CODE: problem", plus " (detail)" when Detail is set, as in docs/cli.md. With no
// problem text the detail follows the code: "CODE (detail)".
func (e *Error) Error() string {
	s := e.Code
	if p := Catalog[e.Code].Problem; p != "" {
		s += ": " + p
	}
	if e.Detail != "" {
		s += " (" + e.Detail + ")"
	}
	return s
}

// Unwrap returns the wrapped error, so errors.Is and errors.As reach it.
func (e *Error) Unwrap() error {
	return e.Err
}

// Format renders e as the four lines of docs/cli.md: the error line, the cause, the fix and a
// link to the code's entry under docsURL.
func Format(e *Error, docsURL string) string {
	c := Catalog[e.Code]
	return fmt.Sprintf("error %s\n  %s\n  Fix: %s\n  More: %s#%s",
		e.Error(), c.Cause, c.Fix, docsURL, strings.ToLower(e.Code))
}
