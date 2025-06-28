// Package testutils provides minimal assertion and requirement helpers for Go tests without external dependencies.
// It offers commonly used functions similar to testify's assert and require packages.
package testutils

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
// If format is provided, it will be used as a printf-style format string with the given args.
func (a *Assert) True(condition bool, format string, args ...any) {
	a.t.Helper()
	if !condition {
		var message string
		if format != "" {
			message = fmt.Sprintf(format, args...)
		}
		a.t.Errorf("Expected condition to be true but was false. %s", message)
	}
}

// False asserts that the condition is false.
// Logs an error using t.Errorf if the condition is true and continues execution.
// If format is provided, it will be used as a printf-style format string with the given args.
func (a *Assert) False(condition bool, format string, args ...any) {
	a.t.Helper()
	if condition {
		var message string
		if format != "" {
			message = fmt.Sprintf(format, args...)
		}
		a.t.Errorf("Expected condition to be false but was true. %s", message)
	}
}

// Len asserts that the provided object has the expected length.
// Supports arrays, slices, maps, channels, and strings.
// Logs an error using t.Errorf if lengths do not match and continues execution.
// If format is provided, it will be used as a printf-style format string with the given args.
func (a *Assert) Len(object any, expected int, format string, args ...any) {
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
		var message string
		if format != "" {
			message = fmt.Sprintf(format, args...)
		}
		a.t.Errorf("Expected length %d but got %d. %s", expected, sz, message)
	}
}

// NoError asserts that err is nil.
// Logs an error using t.Errorf if err is not nil.
// If format is provided, it will be used as a printf-style format string with the given args.
func (a *Assert) NoError(err error, format string, args ...any) {
	a.t.Helper()
	if err != nil {
		var message string
		if format != "" {
			message = fmt.Sprintf(format, args...)
		}
		a.t.Errorf("Expected no error but got: %v. %s", err, message)
	}
}

// Error asserts that err is not nil.
// Logs an error using t.Errorf if err is nil.
// If format is provided, it will be used as a printf-style format string with the given args.
func (a *Assert) Error(err error, format string, args ...any) {
	a.t.Helper()
	if err == nil {
		var message string
		if format != "" {
			message = fmt.Sprintf(format, args...)
		}
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
// If format is provided, it will be used as a printf-style format string with the given args.
func (r *Require) True(condition bool, format string, args ...any) {
	r.t.Helper()
	if !condition {
		var message string
		if format != "" {
			message = fmt.Sprintf(format, args...)
		}
		r.t.Fatalf("Required condition to be true but was false. %s", message)
	}
}

// False requires that the condition is false.
// Stops test execution using t.Fatalf if the condition is true.
// If format is provided, it will be used as a printf-style format string with the given args.
func (r *Require) False(condition bool, format string, args ...any) {
	r.t.Helper()
	if condition {
		var message string
		if format != "" {
			message = fmt.Sprintf(format, args...)
		}
		r.t.Fatalf("Required condition to be false but was true. %s", message)
	}
}

// Len requires that the provided object has the expected length.
// Stops test execution using t.Fatalf if lengths do not match.
// If format is provided, it will be used as a printf-style format string with the given args.
func (r *Require) Len(object any, expected int, format string, args ...any) {
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
		var message string
		if format != "" {
			message = fmt.Sprintf(format, args...)
		}
		r.t.Fatalf("Required length %d but got %d. %s", expected, sz, message)
	}
}

// NoError requires that err is nil.
// Stops test execution using t.Fatalf if err is not nil.
// If format is provided, it will be used as a printf-style format string with the given args.
func (r *Require) NoError(err error, format string, args ...any) {
	r.t.Helper()
	if err != nil {
		var message string
		if format != "" {
			message = fmt.Sprintf(format, args...)
		}
		r.t.Fatalf("Expected no error but got: %v. %s", err, message)
	}
}

// Error requires that err is not nil.
// Stops test execution using t.Fatalf if err is nil.
// If format is provided, it will be used as a printf-style format string with the given args.
func (r *Require) Error(err error, format string, args ...any) {
	r.t.Helper()
	if err == nil {
		var message string
		if format != "" {
			message = fmt.Sprintf(format, args...)
		}
		r.t.Fatalf("Expected error but got nil. %s", message)
	}
}
