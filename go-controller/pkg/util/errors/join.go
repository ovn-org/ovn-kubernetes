package errors

import "strings"

// Join returns an error that wraps the given errors. Any nil error values are
// discarded. Join returns nil if every value in errs is nil. Copied from the
// golang standard library at
// https://github.com/golang/go/blob/a5339da341b8f37c87b77c2fc1318d6ecd2331ff/src/errors/join.go#L19
// Copyright (c) 2009 The Go Authors. All rights reserved.
//
// The difference with the above implementation resides in how this error
// formats. The former uses new lines to concatenate errors which is an
// inconvenience. This implementation formats as the concatenation of the
// strings obtained by calling the Error method of each element of errs,
// recursively unwrapping them if necessary, separated by commas and surrounded
// by brackets.
//
// This is similar as to how the k8s.io apimachinery aggregate error format.
// However this error is simpler and supports the full wrapping semantics, while
// k8s.io apimachinery aggregate error doesn't support the 'errors.As'.
func Join(errs ...error) error {
	n := 0
	for _, err := range errs {
		if err != nil {
			n++
		}
	}
	if n == 0 {
		return nil
	}
	e := &joinError{
		errs: make([]error, 0, n),
	}
	for _, err := range errs {
		if err != nil {
			e.errs = append(e.errs, err)
		}
	}
	return e
}

type joinError struct {
	errs []error
}

func (e *joinError) Error() string {
	// Since Join returns nil if every value in errs is nil,
	// e.errs cannot be empty.
	if len(e.errs) == 1 {
		return e.errs[0].Error()
	}

	var sb strings.Builder
	sb.WriteByte('[')
	for _, err := range e.errs {
		expand(err, &sb)
	}
	sb.WriteByte(']')

	return sb.String()
}

func expand(err error, sb *strings.Builder) {
	if err == nil {
		return
	}
	switch e := err.(type) {
	case interface{ Unwrap() []error }:
		errors := e.Unwrap()
		for _, err := range errors {
			expand(err, sb)
		}
	default:
		// we use '1' here because we start with the opening bracket "["
		if sb.Len() > 1 {
			sb.WriteString(", ")
		}
		sb.WriteString(err.Error())
	}
}

func (e *joinError) Unwrap() []error {
	return e.errs
}
