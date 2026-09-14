//go:build windows

package workspace

import (
	"context"
	"time"
)

func startLocalPTYCommand(context.Context, string, string, TTYSpec, time.Duration, string, *Service) (CommandResult, error) {
	return CommandResult{ExitCode: -1, StartFailed: true}, ErrUnsupported
}

func ptySupported() bool { return false }
