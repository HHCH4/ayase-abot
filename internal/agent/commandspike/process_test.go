package commandspike

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"testing"
	"time"
)

const (
	streamStdout = "stdout"
	streamStderr = "stderr"

	// 与 COMMAND_RUNTIME.md §7 的状态/结果词汇对齐。
	statusExited     = "exited"
	statusTerminated = "terminated"
	statusStartFail  = "start_failed"
	statusUnknown    = "unknown"

	outcomeSuccess   = "success"
	outcomeNonzero   = "nonzero"
	outcomeSignaled  = "signaled"
	outcomeCancelled = "cancelled"
	outcomeTimedOut  = "timed_out"
	outcomeRuntime   = "runtime_error"
)

// chunk 对齐 COMMAND_RUNTIME.md §12.2：Sequence 在 CommandRun 内统一递增，
// Offset 是所属 stream 的原始字节偏移，chunk 边界不代表行边界或字符边界。
type chunk struct {
	Sequence uint64
	Stream   string
	Offset   int64
	Data     []byte
}

type outputRecorder struct {
	mu        sync.Mutex
	chunks    []chunk
	sequence  uint64
	offsets   map[string]int64
	total     map[string]int64
	stored    map[string]int64
	maxStored int64
	truncated bool
}

func newOutputRecorder(maxStoredPerStream int64) *outputRecorder {
	return &outputRecorder{
		offsets:   map[string]int64{},
		total:     map[string]int64{},
		stored:    map[string]int64{},
		maxStored: maxStoredPerStream,
	}
}

func (r *outputRecorder) record(stream string, data []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	offset := r.offsets[stream]
	r.offsets[stream] = offset + int64(len(data))
	r.total[stream] += int64(len(data))

	stored := data
	if r.maxStored > 0 {
		remaining := r.maxStored - r.stored[stream]
		if remaining <= 0 {
			r.truncated = true
			return
		}
		if int64(len(stored)) > remaining {
			stored = stored[:remaining]
			r.truncated = true
		}
	}
	r.stored[stream] += int64(len(stored))
	r.sequence++
	// 复制数据，避免复用读缓冲区导致的别名问题。
	buffer := make([]byte, len(stored))
	copy(buffer, stored)
	r.chunks = append(r.chunks, chunk{Sequence: r.sequence, Stream: stream, Offset: offset, Data: buffer})
}

func (r *outputRecorder) snapshot() ([]chunk, map[string]int64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	chunks := append([]chunk(nil), r.chunks...)
	total := map[string]int64{}
	for key, value := range r.total {
		total[key] = value
	}
	return chunks, total, r.truncated
}

func (r *outputRecorder) text(stream string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var result []byte
	for _, item := range r.chunks {
		if item.Stream == stream {
			result = append(result, item.Data...)
		}
	}
	return string(result)
}

// drain 持续读取一个 pipe 直到 EOF。它必须一直读到 EOF，否则子进程在写满管道缓冲区后会永久阻塞。
func (r *outputRecorder) drain(stream string, reader io.Reader) {
	buffer := make([]byte, 32*1024)
	for {
		n, err := reader.Read(buffer)
		if n > 0 {
			r.record(stream, buffer[:n])
		}
		if err != nil {
			return
		}
	}
}

type processSpec struct {
	Shell            string
	Script           string
	CWD              string
	Env              []string
	GracePeriod      time.Duration
	MaxStoredPerPipe int64
	Timeout          time.Duration
}

type processResult struct {
	Status   string
	Outcome  string
	ExitCode int
	Signal   syscall.Signal
	Err      error
}

// localProcess 是最小本地进程控制器：独立进程组 + 分离 pipe + 进程组级终止。
type localProcess struct {
	cmd      *exec.Cmd
	pgid     int
	recorder *outputRecorder
	grace    time.Duration

	stdoutRead *os.File
	stderrRead *os.File

	// exited 在 Wait 返回后关闭，waitErr 必须在关闭前写入；
	// 用“只关闭一次”的 channel 而不是传值 channel，避免结果被重复消费。
	exited  chan struct{}
	waitErr error

	finishOnce sync.Once
	drainWG    sync.WaitGroup
}

func startLocalProcess(spec processSpec) (*localProcess, error) {
	if spec.Shell == "" {
		spec.Shell = "/bin/sh"
	}
	if spec.GracePeriod <= 0 {
		spec.GracePeriod = 3 * time.Second
	}

	// 使用显式 os.Pipe 而不是 cmd.StdoutPipe()：后者要求在所有读取完成前不能调用 Wait，
	// 而 Wait 会在进程退出时关闭管道，容易在大输出场景下截断数据。
	stdoutRead, stdoutWrite, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	stderrRead, stderrWrite, err := os.Pipe()
	if err != nil {
		stdoutRead.Close()
		stdoutWrite.Close()
		return nil, err
	}

	command := exec.Command(spec.Shell, "-c", spec.Script)
	command.Dir = spec.CWD
	if len(spec.Env) > 0 {
		command.Env = spec.Env
	}
	command.Stdout = stdoutWrite
	command.Stderr = stderrWrite
	// 独立进程组：只有这样才能连同 shell 派生的子进程一起终止。
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := command.Start(); err != nil {
		stdoutRead.Close()
		stdoutWrite.Close()
		stderrRead.Close()
		stderrWrite.Close()
		return nil, err
	}
	// 父进程必须关闭写端，否则子进程退出后读取端永远等不到 EOF。
	stdoutWrite.Close()
	stderrWrite.Close()

	process := &localProcess{
		cmd:        command,
		pgid:       command.Process.Pid,
		recorder:   newOutputRecorder(spec.MaxStoredPerPipe),
		grace:      spec.GracePeriod,
		stdoutRead: stdoutRead,
		stderrRead: stderrRead,
		exited:     make(chan struct{}),
	}
	process.drainWG.Add(2)
	go func() {
		defer process.drainWG.Done()
		process.recorder.drain(streamStdout, stdoutRead)
	}()
	go func() {
		defer process.drainWG.Done()
		process.recorder.drain(streamStderr, stderrRead)
	}()
	go func() {
		process.waitErr = command.Wait()
		close(process.exited)
	}()
	return process, nil
}

func (p *localProcess) pid() int { return p.cmd.Process.Pid }

// Wait 等待进程自然退出。
func (p *localProcess) Wait() processResult {
	<-p.exited
	p.finish()
	return classify(p.waitErr, "")
}

// finish 在进程退出后等待两个 pipe 读完，顺序对齐 COMMAND_RUNTIME.md §9.1 的第 8、9 步。
func (p *localProcess) finish() {
	p.finishOnce.Do(func() {
		p.drainWG.Wait()
		p.stdoutRead.Close()
		p.stderrRead.Close()
	})
}

// Terminate 复刻 COMMAND_RUNTIME.md §9.2 的终止顺序：
// SIGTERM 进程组 → grace → SIGKILL 进程组 → Wait。
func (p *localProcess) Terminate(outcome string) processResult {
	if err := syscall.Kill(-p.pgid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return processResult{Status: statusUnknown, Outcome: outcomeRuntime, Err: err}
	}
	select {
	case <-p.exited:
	case <-time.After(p.grace):
		if err := syscall.Kill(-p.pgid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			return processResult{Status: statusUnknown, Outcome: outcomeRuntime, Err: err}
		}
		<-p.exited
	}
	p.finish()
	return classify(p.waitErr, outcome)
}

// KillLeaderOnly 只终止直接子进程，用于对照实验：证明“只杀 shell 会留下孙进程”。
// 它不是生产路径，仅供 spike 取证。
func (p *localProcess) KillLeaderOnly() {
	_ = syscall.Kill(p.pid(), syscall.SIGKILL)
}

func classify(waitErr error, trigger string) processResult {
	if waitErr == nil {
		return processResult{Status: statusExited, Outcome: outcomeSuccess}
	}
	var exitErr *exec.ExitError
	if !errors.As(waitErr, &exitErr) {
		return processResult{Status: statusUnknown, Outcome: outcomeRuntime, Err: waitErr}
	}
	status, ok := exitErr.Sys().(syscall.WaitStatus)
	if !ok {
		return processResult{Status: statusUnknown, Outcome: outcomeRuntime, Err: waitErr}
	}
	if trigger != "" {
		return processResult{Status: statusTerminated, Outcome: trigger, Signal: status.Signal(), Err: waitErr}
	}
	if status.Signaled() {
		return processResult{Status: statusExited, Outcome: outcomeSignaled, Signal: status.Signal(), Err: waitErr}
	}
	return processResult{Status: statusExited, Outcome: outcomeNonzero, ExitCode: status.ExitStatus(), Err: waitErr}
}

// runWithDeadline 复刻 Runtime 的用法：进程生命周期独立于调用方，超时或 ctx 取消走同一条终止路径。
func runWithDeadline(ctx context.Context, spec processSpec) (processResult, *outputRecorder, error) {
	process, err := startLocalProcess(spec)
	if err != nil {
		return processResult{Status: statusStartFail, Outcome: outcomeRuntime, Err: err}, nil, err
	}
	var timeout <-chan time.Time
	if spec.Timeout > 0 {
		timer := time.NewTimer(spec.Timeout)
		defer timer.Stop()
		timeout = timer.C
	}
	select {
	case <-process.exited:
		return process.Wait(), process.recorder, nil
	case <-ctx.Done():
		return process.Terminate(outcomeCancelled), process.recorder, nil
	case <-timeout:
		return process.Terminate(outcomeTimedOut), process.recorder, nil
	}
}

func writeScript(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := dir + "/" + name
	if err := os.WriteFile(path, []byte(content), 0o700); err != nil {
		t.Fatalf("写入脚本失败: %v", err)
	}
	return path
}

// waitForProcessExit 轮询确认某个 PID 是否已经消失（signal 0 探测）。
func waitForProcessExit(t *testing.T, pid int) bool {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func readPIDFile(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(path)
		if err == nil {
			var pid int
			if _, scanErr := fmt.Sscanf(string(raw), "%d", &pid); scanErr == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("未能从 %s 读到子进程 PID", path)
	return 0
}
