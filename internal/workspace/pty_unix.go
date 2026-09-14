//go:build !windows

package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
)

type localPTYSession struct {
	file   *os.File
	cmd    *exec.Cmd
	mu     sync.Mutex
	closed bool
}

func (session *localPTYSession) Write(data []byte) (int, error) {
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.closed || session.file == nil {
		return 0, ErrPTYUnavailable
	}
	return session.file.Write(data)
}

func (session *localPTYSession) Resize(rows, cols uint16) error {
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.closed || session.file == nil {
		return ErrPTYUnavailable
	}
	return pty.Setsize(session.file, &pty.Winsize{Rows: rows, Cols: cols})
}

func (session *localPTYSession) Signal(value string) error {
	signal, ok := ptySignal(strings.TrimSpace(value))
	if !ok {
		return ErrPTYInvalidSignal
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.closed || session.cmd == nil || session.cmd.Process == nil {
		return ErrPTYUnavailable
	}
	// StartWithSize creates a new session, so the command PID is also the
	// process-group ID. Signalling the group includes shell descendants while
	// avoiding a broad kill of the host process.
	if err := syscall.Kill(-session.cmd.Process.Pid, signal); err != nil {
		return fmt.Errorf("PTY signal 发送失败: %w", err)
	}
	return nil
}

func (session *localPTYSession) Close() error {
	session.mu.Lock()
	if session.closed {
		session.mu.Unlock()
		return nil
	}
	session.closed = true
	file := session.file
	session.file = nil
	session.mu.Unlock()
	if file != nil {
		return file.Close()
	}
	return nil
}

func ptySignal(value string) (syscall.Signal, bool) {
	switch strings.ToUpper(strings.TrimSpace(value)) {
	case "HUP", "SIGHUP":
		return syscall.SIGHUP, true
	case "INT", "SIGINT", "CTRL_C":
		return syscall.SIGINT, true
	case "QUIT", "SIGQUIT":
		return syscall.SIGQUIT, true
	case "TERM", "SIGTERM":
		return syscall.SIGTERM, true
	case "KILL", "SIGKILL":
		return syscall.SIGKILL, true
	case "USR1", "SIGUSR1":
		return syscall.SIGUSR1, true
	case "USR2", "SIGUSR2":
		return syscall.SIGUSR2, true
	default:
		return 0, false
	}
}

func startLocalPTYCommand(ctx context.Context, directory, command string, spec TTYSpec, timeout time.Duration, runID string, service *Service) (CommandResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := spec.Validate(); err != nil {
		return CommandResult{ExitCode: -1, StartFailed: true}, err
	}
	started := time.Now()
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.Command("sh", "-lc", command)
	cmd.Dir = directory
	cmd.Env = withPTYEnvironment(spec)
	terminal, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: spec.Rows, Cols: spec.Cols})
	if err != nil {
		return CommandResult{ExitCode: -1, StartFailed: true, DurationMS: time.Since(started).Milliseconds()}, fmt.Errorf("启动 PTY 命令失败: %w", err)
	}
	session := &localPTYSession{file: terminal, cmd: cmd}
	if service != nil && strings.TrimSpace(runID) != "" {
		if registerErr := service.registerPTYSession(runID, session); registerErr != nil {
			_ = session.Signal("KILL")
			_ = session.Close()
			return CommandResult{ExitCode: -1, StartFailed: true, DurationMS: time.Since(started).Milliseconds()}, registerErr
		}
		defer service.unregisterPTYSession(runID, session)
	}
	defer session.Close()
	outputContext := ctx
	if service != nil && strings.TrimSpace(runID) != "" {
		outputContext = WithCommandObserver(outputContext, func(chunk CommandChunk) {
			if observer := commandObserver(ctx); observer != nil {
				observer(chunk)
			}
			service.publishPTYChunk(runID, chunk)
		})
	}
	output := newObservedBuffer(outputContext, "terminal")
	var drain sync.WaitGroup
	drain.Add(1)
	go drainCommandPipe(terminal, output, &drain)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var waitErr error
	timedOut := false
	unknown := false
	select {
	case waitErr = <-done:
	case <-runCtx.Done():
		timedOut = errors.Is(runCtx.Err(), context.DeadlineExceeded)
		_ = session.Signal("KILL")
		select {
		case waitErr = <-done:
		case <-time.After(2 * time.Second):
			unknown = true
		}
	}
	if !unknown {
		drained := make(chan struct{})
		go func() {
			drain.Wait()
			close(drained)
		}()
		select {
		case <-drained:
		case <-time.After(2 * time.Second):
			unknown = true
		}
	}
	terminalText := output.String()
	terminalBytes := output.BytesWritten()
	result := CommandResult{Output: terminalText, Terminal: terminalText, ExitCode: exitCode(waitErr), Truncated: output.Truncated(), TimedOut: timedOut, Unknown: unknown, Started: true, DurationMS: time.Since(started).Milliseconds(), StdoutBytes: terminalBytes, TerminalBytes: terminalBytes}
	if unknown {
		return result, fmt.Errorf("PTY 命令结束状态未知")
	}
	if runCtx.Err() != nil {
		if timedOut {
			return result, fmt.Errorf("PTY 命令执行超时: %w", runCtx.Err())
		}
		return result, fmt.Errorf("PTY 命令执行已取消: %w", runCtx.Err())
	}
	if waitErr != nil {
		return result, fmt.Errorf("PTY 命令执行失败（退出码 %d）: %s", result.ExitCode, strings.TrimSpace(result.Output))
	}
	return result, nil
}

func withPTYEnvironment(spec TTYSpec) []string {
	env := workspaceCommandEnvironment()
	term := spec.Term
	if strings.TrimSpace(term) == "" {
		term = defaultTTYTerm
	}
	for index, value := range env {
		if strings.HasPrefix(value, "TERM=") {
			env[index] = "TERM=" + term
			return env
		}
	}
	return append(env, "TERM="+term)
}

func ptySupported() bool { return true }
