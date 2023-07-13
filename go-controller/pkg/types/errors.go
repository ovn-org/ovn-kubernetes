package types

import (
	"errors"
	"fmt"
)

type SuppressedError struct {
	Inner error
}

func (e *SuppressedError) Error() string {
	return fmt.Sprintf("suppressed error logged: %v", e.Inner.Error())
}

func (e *SuppressedError) Unwrap() error {
	return e.Inner
}

func NewSuppressedError(err error) error {
	return &SuppressedError{
		Inner: err,
	}
}

func IsSuppressedError(err error) bool {
	var suppressedError *SuppressedError
	return errors.As(err, &suppressedError)
}
