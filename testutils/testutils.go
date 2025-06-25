// Package testutil provides minimal assertion and requirement helpers for Go tests without external dependencies.
// It offers commonly used functions similar to testify's assert and require packages.
package testutil

import (
	"fmt"
	"reflect"
	"testing"
)

// Assert provides assertion methods for testing that log failures but allow continuation.
// It wraps a *testing.T instance and offers methods similar to testify's assert package.
type Assert struct {
	t *testing.T
}

// NewAssert creates a new Assert object bound to the provided testing.T instance.
func NewAssert(t *testing.T) *Assert {
	return &Assert{t: t}
}

// True asserts that the condition is true.
// Logs an error using t.Errorf if the condition is false and continues execution.
func (a *Assert) True(condition bool, msgAndArgs ...any) {
	a.t.Helper()
	if !condition {
		message := messageFrom(msgAndArgs...)
		a.t.Errorf("Expected condition to be true but was false. %s", message)
	}
}

// False asserts that the condition is false.
// Logs an error using t.Errorf if the condition is true and continues execution.
func (a *Assert) False(condition bool, msgAndArgs ...any) {
	a.t.Helper()
	if condition {
		message := messageFrom(msgAndArgs...)
		a.t.Errorf("Expected condition to be false but was true. %s", message)
	}
}

// Len asserts that the provided object has the expected length.
// Supports arrays, slices, maps, channels, and strings.
// Logs an error using t.Errorf if lengths do not match and continues execution.
func (a *Assert) Len(object any, expected int, msgAndArgs ...any) {
	a.t.Helper()
	rv := reflect.ValueOf(object)
	sz := -1

	switch rv.Kind() {
	case reflect.Array, reflect.Slice, reflect.Map, reflect.Chan, reflect.String:
		sz = rv.Len()
	default:
		a.t.Errorf("Len assertion requires array, slice, map, channel, or string but got %T", object)
		return
	}

	if sz != expected {
		message := messageFrom(msgAndArgs...)
		a.t.Errorf("Expected length %d but got %d. %s", expected, sz, message)
	}
}

// NoError asserts that err is nil.
// Logs an error using t.Errorf if err is not nil.
func (a *Assert) NoError(err error, msgAndArgs ...any) {
	a.t.Helper()
	if err != nil {
		message := messageFrom(msgAndArgs...)
		a.t.Errorf("Expected no error but got: %v. %s", err, message)
	}
}

// Error asserts that err is not nil.
// Logs an error using t.Errorf if err is nil.
func (a *Assert) Error(err error, msgAndArgs ...any) {
	a.t.Helper()
	if err == nil {
		message := messageFrom(msgAndArgs...)
		a.t.Errorf("Expected error but got nil. %s", message)
	}
}

// Require provides requirement methods for testing that stop execution on failure.
// It wraps a *testing.T instance and offers methods similar to testify's require package.
type Require struct {
	t *testing.T
}

// NewRequire creates a new Require object bound to the provided testing.T instance.
func NewRequire(t *testing.T) *Require {
	return &Require{t: t}
}

// True requires that the condition is true.
// Stops test execution using t.Fatalf if the condition is false.
func (r *Require) True(condition bool, msgAndArgs ...any) {
	r.t.Helper()
	if !condition {
		message := messageFrom(msgAndArgs...)
		r.t.Fatalf("Required condition to be true but was false. %s", message)
	}
}

// False requires that the condition is false.
// Stops test execution using t.Fatalf if the condition is true.
func (r *Require) False(condition bool, msgAndArgs ...any) {
	r.t.Helper()
	if condition {
		message := messageFrom(msgAndArgs...)
		r.t.Fatalf("Required condition to be false but was true. %s", message)
	}
}

// Len requires that the provided object has the expected length.
// Stops test execution using t.Fatalf if lengths do not match.
func (r *Require) Len(object any, expected int, msgAndArgs ...any) {
	r.t.Helper()
	rv := reflect.ValueOf(object)
	sz := -1

	switch rv.Kind() {
	case reflect.Array, reflect.Slice, reflect.Map, reflect.Chan, reflect.String:
		sz = rv.Len()
	default:
		r.t.Fatalf("Len requirement requires array, slice, map, channel, or string but got %T", object)
		return
	}

	if sz != expected {
		message := messageFrom(msgAndArgs...)
		r.t.Fatalf("Required length %d but got %d. %s", expected, sz, message)
	}
}

// NoError requires that err is nil.
// Stops test execution using t.Fatalf if err is not nil.
func (r *Require) NoError(err error, msgAndArgs ...any) {
	r.t.Helper()
	if err != nil {
		message := messageFrom(msgAndArgs...)
		r.t.Fatalf("Expected no error but got: %v. %s", err, message)
	}
}

// Error requires that err is not nil.
// Stops test execution using t.Fatalf if err is nil.
func (r *Require) Error(err error, msgAndArgs ...any) {
	r.t.Helper()
	if err == nil {
		message := messageFrom(msgAndArgs...)
		r.t.Fatalf("Expected error but got nil. %s", message)
	}
}

// messageFrom constructs a message string from variadic arguments.
// If no arguments are provided, returns an empty string.
// If the first argument is a string and there are additional args, it is treated as a format string.
// Otherwise all args are sprinted.
func messageFrom(msgAndArgs ...any) string {
	if len(msgAndArgs) == 0 {
		return ""
	}
	if format, ok := msgAndArgs[0].(string); ok && len(msgAndArgs) > 1 {
		// First element is a format string with further args
		return fmt.Sprintf(format, msgAndArgs[1:]...)
	}
	// Fallback to sprinting all args
	return fmt.Sprint(msgAndArgs...)
}

