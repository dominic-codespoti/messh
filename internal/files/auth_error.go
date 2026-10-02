package files

import (
	"errors"
	"fmt"
)

var ErrCapabilityDenied = errors.New("capability denied")

type CapabilityDeniedError struct{ Code string }

func (e *CapabilityDeniedError) Error() string {
	return fmt.Sprintf("%v: %s", ErrCapabilityDenied, e.Code)
}
func (e *CapabilityDeniedError) Unwrap() error     { return ErrCapabilityDenied }
func (e *CapabilityDeniedError) ErrorCode() string { return e.Code }
