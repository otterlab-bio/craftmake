package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

const (
	ExitSuccess         = 0
	ExitUsage           = 2
	ExitConfiguration   = 3
	ExitCompilation     = 4
	ExitTaskFailure     = 5
	ExitBackendFailure  = 6
	ExitStateFailure    = 7
	ExitCancelled       = 8
	ExitInternalFailure = 9
)

type ExitError struct {
	code int
	err  error
}

func (exitError *ExitError) Error() string {
	return exitError.err.Error()
}

func (exitError *ExitError) Unwrap() error {
	return exitError.err
}

func WithExitCode(code int, err error) error {
	if err == nil {
		return nil
	}
	var existingExitError *ExitError
	if errors.As(err, &existingExitError) {
		return err
	}
	return &ExitError{code: code, err: err}
}

func ExitCode(err error) int {
	if err == nil {
		return ExitSuccess
	}
	var exitError *ExitError
	if errors.As(err, &exitError) {
		return exitError.code
	}
	if errors.Is(err, context.Canceled) {
		return ExitCancelled
	}
	if isCobraUsageError(err) {
		return ExitUsage
	}
	return ExitInternalFailure
}

func usageError(format string, arguments ...any) error {
	return WithExitCode(ExitUsage, fmt.Errorf(format, arguments...))
}

func configurationError(err error) error {
	return WithExitCode(ExitConfiguration, err)
}

func compilationError(err error) error {
	return WithExitCode(ExitCompilation, err)
}

func taskFailureError(err error) error {
	if errors.Is(err, context.Canceled) {
		return WithExitCode(ExitCancelled, err)
	}
	return WithExitCode(ExitTaskFailure, err)
}

func backendFailureError(err error) error {
	return WithExitCode(ExitBackendFailure, err)
}

func stateFailureError(err error) error {
	return WithExitCode(ExitStateFailure, err)
}

func internalFailureError(err error) error {
	return WithExitCode(ExitInternalFailure, err)
}

func isCobraUsageError(err error) bool {
	errorMessage := err.Error()
	for _, prefix := range []string{
		"unknown command ",
		"unknown flag: ",
		"flag needs an argument: ",
		"required flag(s) ",
		"accepts ",
	} {
		if strings.HasPrefix(errorMessage, prefix) {
			return true
		}
	}
	return false
}
