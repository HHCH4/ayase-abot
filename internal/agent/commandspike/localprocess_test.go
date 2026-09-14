package commandspike

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func processAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

// TestKillingOnlyShellLeavesGrandchildAlive 是对照实验，用来证明
// COMMAND_RUNTIME.md §9.2 的判断：只终止 shell 进程会留下它派生的子进程。
//
// 它同时固化了第二个事实：孙进程会继承 stdout/stderr 的写端，
// 因此只要孙进程存活，输出的 EOF 就不会到达，"等待输出 drain 完成" 会一直阻塞。
// 这意味着 §9.1 第 9 步必须有上界，不能无条件等待。
func TestKillingOnlyShellLeavesGrandchildAlive(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "grandchild.pid")
	script := writeScript(t, dir, "run.sh", "sleep 30 &\necho $! > \""+pidFile+"\"\nwait\n")

	process, err := startLocalProcess(processSpec{Script: script, GracePeriod: 200 * time.Millisecond})
	if err != nil {
		t.Fatalf("启动进程失败: %v", err)
	}
	grandchild := readPIDFile(t, pidFile)
	defer func() {
		_ = syscall.Kill(grandchild, syscall.SIGKILL)
		waitForProcessExit(t, grandchild)
	}()

	process.KillLeaderOnly()
	if !waitForProcessExit(t, process.pid()) {
		t.Fatalf("只杀 leader 后 shell 进程 %d 未退出", process.pid())
	}
	if !processAlive(grandchild) {
		t.Fatal("孙进程意外消失，无法证明‘只杀 shell 不够’")
	}
	t.Logf("只终止 shell 进程后，孙进程 %d 仍然存活", grandchild)

	// 孙进程仍持有管道写端：drain 不应结束。
	drained := make(chan processResult, 1)
	go func() { drained <- process.Wait() }()
	select {
	case <-drained:
		t.Fatal("孙进程仍持有管道写端时，输出 drain 不应结束")
	case <-time.After(500 * time.Millisecond):
		t.Logf("孙进程存活期间输出 drain 未结束：EOF 取决于整个进程组是否释放管道")
	}

	// 清理孙进程后 drain 才会结束。
	_ = syscall.Kill(grandchild, syscall.SIGKILL)
	select {
	case result := <-drained:
		if result.Outcome != outcomeSignaled {
			t.Fatalf("结果 = %+v，期望 exited/signaled", result)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("孙进程退出后输出 drain 仍未结束")
	}
}

// TestProcessGroupTerminationKillsGrandchildren 验证独立进程组 + 进程组级终止是可行手段。
func TestProcessGroupTerminationKillsGrandchildren(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "grandchild.pid")
	script := writeScript(t, dir, "run.sh", "sleep 30 &\necho $! > \""+pidFile+"\"\nwait\n")

	process, err := startLocalProcess(processSpec{Script: script, GracePeriod: 200 * time.Millisecond})
	if err != nil {
		t.Fatalf("启动进程失败: %v", err)
	}
	grandchild := readPIDFile(t, pidFile)
	if !processAlive(grandchild) {
		t.Fatal("孙进程应处于运行状态")
	}

	result := process.Terminate(outcomeCancelled)
	if result.Status != statusTerminated || result.Outcome != outcomeCancelled {
		t.Fatalf("进程组终止结果 = %+v，期望 terminated/cancelled", result)
	}
	if !waitForProcessExit(t, grandchild) {
		_ = syscall.Kill(grandchild, syscall.SIGKILL)
		t.Fatalf("进程组终止后孙进程 %d 仍然存活", grandchild)
	}
	if processAlive(process.pid()) {
		t.Fatalf("shell 进程 %d 仍然存活", process.pid())
	}
}

// TestStdoutAndStderrAreSeparated 验证两个管道独立采集、各自的 offset 严格有序、
// sequence 全局递增（COMMAND_RUNTIME.md §12.2 / §12.3）。
func TestStdoutAndStderrAreSeparated(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, "run.sh", "echo out-1\necho err-1 1>&2\necho out-2\necho err-2 1>&2\n")

	result, recorder, err := runWithDeadline(context.Background(), processSpec{Script: script})
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if result.Outcome != outcomeSuccess {
		t.Fatalf("执行结果 = %+v，期望 success", result)
	}

	stdout := recorder.text(streamStdout)
	stderr := recorder.text(streamStderr)
	if stdout != "out-1\nout-2\n" {
		t.Fatalf("stdout = %q，期望 out-1/out-2", stdout)
	}
	if stderr != "err-1\nerr-2\n" {
		t.Fatalf("stderr = %q，期望 err-1/err-2", stderr)
	}
	if strings.Contains(stdout, "err-") || strings.Contains(stderr, "out-") {
		t.Fatal("两个 stream 的内容发生了混流")
	}

	chunks, totals, truncated := recorder.snapshot()
	if truncated {
		t.Fatal("小输出不应触发截断")
	}
	if totals[streamStdout] != int64(len(stdout)) || totals[streamStderr] != int64(len(stderr)) {
		t.Fatalf("字节统计不正确: %v", totals)
	}
	lastOffset := map[string]int64{}
	var lastSequence uint64
	for _, item := range chunks {
		if item.Sequence <= lastSequence {
			t.Fatalf("sequence 未严格递增: %d 出现在 %d 之后", item.Sequence, lastSequence)
		}
		lastSequence = item.Sequence
		if item.Offset != lastOffset[item.Stream] {
			t.Fatalf("stream %s 的 offset 不连续: 期望 %d，实际 %d", item.Stream, lastOffset[item.Stream], item.Offset)
		}
		lastOffset[item.Stream] += int64(len(item.Data))
	}
}

// TestNonzeroExitIsNotRuntimeFailure 固化 COMMAND_RUNTIME.md §3.5：
// 非零退出码是可用的执行结果，不是 Runtime 故障。
func TestNonzeroExitIsNotRuntimeFailure(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, "run.sh", "echo before-failure\nexit 3\n")

	result, _, err := runWithDeadline(context.Background(), processSpec{Script: script})
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if result.Status != statusExited || result.Outcome != outcomeNonzero || result.ExitCode != 3 {
		t.Fatalf("结果 = %+v，期望 exited/nonzero/3", result)
	}
}

// TestSelfSignaledProcessReportsSignal 区分“进程自己被信号终止”和“被 Runtime 取消”。
func TestSelfSignaledProcessReportsSignal(t *testing.T) {
	// 让 startLocalProcess 直接启动的 shell 给自己发信号。如果改为先启动
	// 脚本子进程，dash 会将子进程的 SIGTERM 折算为正常退出码 143，
	// 无法验证被观察进程的 WaitStatus.Signaled 语义。
	result, _, err := runWithDeadline(context.Background(), processSpec{Script: "kill -TERM $$"})
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if result.Status != statusExited || result.Outcome != outcomeSignaled || result.Signal != syscall.SIGTERM {
		t.Fatalf("结果 = %+v，期望 exited/signaled/SIGTERM", result)
	}
}

// TestLargeOutputIsDrainedWithoutDeadlock 验证大输出场景：必须持续读到 EOF，
// 同时保存量必须有界，且截断不能让进程卡死（COMMAND_RUNTIME.md §12 的有界持久化前提）。
func TestLargeOutputIsDrainedWithoutDeadlock(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, "run.sh", "dd if=/dev/zero bs=1048576 count=2 2>/dev/null | tr '\\0' 'x'\n")

	const cap = 64 * 1024
	done := make(chan struct{})
	var result processResult
	var recorder *outputRecorder
	go func() {
		defer close(done)
		result, recorder, _ = runWithDeadline(context.Background(), processSpec{
			Script:           script,
			MaxStoredPerPipe: cap,
		})
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("大输出场景发生死锁：30 秒内没有结束")
	}

	if result.Outcome != outcomeSuccess {
		t.Fatalf("结果 = %+v，期望 success", result)
	}
	const expected = 2 * 1024 * 1024
	_, totals, truncated := recorder.snapshot()
	if totals[streamStdout] != expected {
		t.Fatalf("stdout 总字节 = %d，期望 %d（说明没有读到 EOF）", totals[streamStdout], expected)
	}
	if !truncated {
		t.Fatal("超过上限的输出应被标记为截断")
	}
	stored := int64(len(recorder.text(streamStdout)))
	if stored > cap {
		t.Fatalf("保存字节 = %d，超过上限 %d", stored, cap)
	}
}

// TestTimeoutTerminatesWholeProcessGroup 验证超时与取消走同一条终止路径。
func TestTimeoutTerminatesWholeProcessGroup(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "grandchild.pid")
	script := writeScript(t, dir, "run.sh", "sleep 30 &\necho $! > \""+pidFile+"\"\nwait\n")

	result, _, err := runWithDeadline(context.Background(), processSpec{
		Script:      script,
		GracePeriod: 200 * time.Millisecond,
		Timeout:     300 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if result.Status != statusTerminated || result.Outcome != outcomeTimedOut {
		t.Fatalf("结果 = %+v，期望 terminated/timed_out", result)
	}
	grandchild := readPIDFile(t, pidFile)
	if !waitForProcessExit(t, grandchild) {
		_ = syscall.Kill(grandchild, syscall.SIGKILL)
		t.Fatalf("超时终止后孙进程 %d 仍然存活", grandchild)
	}
}

// TestContextCancellationTerminatesProcessGroup 验证调用方 ctx 取消同样终止整个进程组。
func TestContextCancellationTerminatesProcessGroup(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "grandchild.pid")
	script := writeScript(t, dir, "run.sh", "sleep 30 &\necho $! > \""+pidFile+"\"\nwait\n")

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()
	result, _, err := runWithDeadline(ctx, processSpec{Script: script, GracePeriod: 200 * time.Millisecond})
	cancel()
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if result.Status != statusTerminated || result.Outcome != outcomeCancelled {
		t.Fatalf("结果 = %+v，期望 terminated/cancelled", result)
	}
	grandchild := readPIDFile(t, pidFile)
	if !waitForProcessExit(t, grandchild) {
		_ = syscall.Kill(grandchild, syscall.SIGKILL)
		t.Fatalf("ctx 取消后孙进程 %d 仍然存活", grandchild)
	}
}

// TestStartFailureIsReported 验证启动失败必须与执行失败区分（start_failed 而不是 nonzero）。
func TestStartFailureIsReported(t *testing.T) {
	result, _, err := runWithDeadline(context.Background(), processSpec{
		Shell:  filepath.Join(t.TempDir(), "不存在的解释器"),
		Script: "echo never",
	})
	if err == nil {
		t.Fatal("启动不存在的解释器应当报错")
	}
	if result.Status != statusStartFail {
		t.Fatalf("结果 = %+v，期望 start_failed", result)
	}
	if !errors.Is(err, syscall.ENOENT) && !strings.Contains(err.Error(), "no such file") {
		t.Logf("启动失败错误（仅记录，不做断言）: %v", err)
	}
}
