package types

import (
	"errors"
	"fmt"

	kerrors "k8s.io/apimachinery/pkg/util/errors"
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
	// errors.As() is not supported with Aggregate type error. Aggregate.Errors() converts an
	// Aggregate type error into a slice of builtin error and then errors.As() can be used
	if agg, ok := err.(kerrors.Aggregate); ok && err != nil {
		suppress := false
		for _, err := range agg.Errors() {
			if errors.As(err, &suppressedError) {
				suppress = true
			} else {
				return false
			}
		}
		return suppress
	}
	return errors.As(err, &suppressedError)
}
