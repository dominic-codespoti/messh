package recipes

import "errors"

type CodedError struct {
	Code    string
	Message string
	Cause   error
}

func (e *CodedError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	if e.Cause != nil {
		return e.Cause.Error()
	}
	return e.Code
}
func (e *CodedError) Unwrap() error     { return e.Cause }
func (e *CodedError) ErrorCode() string { return e.Code }

var ErrNotFound = errors.New("recipe not found")

func coded(code, msg string, err error) error {
	return &CodedError{Code: code, Message: msg, Cause: err}
}
func invalid(err error) error {
	return coded("invalid_recipe_definition", "invalid recipe definition: "+err.Error(), err)
}
