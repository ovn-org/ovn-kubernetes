package errors

import (
	"errors"
	"reflect"
	"testing"
)

// Copied from
// https://github.com/golang/go/blob/a5339da341b8f37c87b77c2fc1318d6ecd2331ff/src/errors/join_test.go#L13
// Copyright (c) 2009 The Go Authors. All rights reserved.
func TestJoinReturnsNil(t *testing.T) {
	if err := Join(); err != nil {
		t.Errorf("errors.Join() = %v, want nil", err)
	}
	if err := Join(nil); err != nil {
		t.Errorf("errors.Join(nil) = %v, want nil", err)
	}
	if err := Join(nil, nil); err != nil {
		t.Errorf("errors.Join(nil, nil) = %v, want nil", err)
	}
}

// Copied from
// https://github.com/golang/go/blob/a5339da341b8f37c87b77c2fc1318d6ecd2331ff/src/errors/join_test.go#L25
// Copyright (c) 2009 The Go Authors. All rights reserved.
func TestJoin(t *testing.T) {
	err1 := errors.New("err1")
	err2 := errors.New("err2")
	for _, test := range []struct {
		errs []error
		want []error
	}{{
		errs: []error{err1},
		want: []error{err1},
	}, {
		errs: []error{err1, err2},
		want: []error{err1, err2},
	}, {
		errs: []error{err1, nil, err2},
		want: []error{err1, err2},
	}} {
		got := Join(test.errs...).(interface{ Unwrap() []error }).Unwrap()
		if !reflect.DeepEqual(got, test.want) {
			t.Errorf("Join(%v) = %v; want %v", test.errs, got, test.want)
		}
		if len(got) != cap(got) {
			t.Errorf("Join(%v) returns errors with len=%v, cap=%v; want len==cap", test.errs, len(got), cap(got))
		}
	}
}

// Copied from
// https://github.com/golang/go/blob/a5339da341b8f37c87b77c2fc1318d6ecd2331ff/src/errors/join_test.go#L51
// Copyright (c) 2009 The Go Authors. All rights reserved.
func TestJoinErrorMethod(t *testing.T) {
	err1 := errors.New("err1")
	err2 := errors.New("err2")
	for _, test := range []struct {
		errs []error
		want string
	}{{
		errs: []error{err1},
		want: "err1",
	}, {
		errs: []error{err1, err2},
		want: "[err1, err2]",
	}, {
		errs: []error{err1, nil, err2},
		want: "[err1, err2]",
	}} {
		got := Join(test.errs...).Error()
		if got != test.want {
			t.Errorf("Join(%v).Error() = %q; want %q", test.errs, got, test.want)
		}
	}
}

func TestJoinErrorMethodRecursive(t *testing.T) {
	err1 := errors.New("err1")
	err2 := errors.New("err2")
	err3 := errors.New("err3")
	err4 := errors.New("err4")
	for _, test := range []struct {
		errs []error
		want string
	}{{
		errs: []error{err1, Join(err2), err3},
		want: "[err1, err2, err3]",
	}, {
		errs: []error{err1, Join(err2, err3), err4},
		want: "[err1, err2, err3, err4]",
	}, {
		errs: []error{err1, Join(err2, nil, err3), err4},
		want: "[err1, err2, err3, err4]",
	}} {
		got := Join(test.errs...).Error()
		if got != test.want {
			t.Errorf("Join(%v).Error() = %q; want %q", test.errs, got, test.want)
		}
	}
}
