package workspace

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
)

const maxPTYInputBytes = 64 * 1024

var (
	ErrPTYUnavailable   = errors.New("当前命令没有可用的 PTY")
	ErrPTYWriterHeld    = errors.New("命令 PTY 写入权已被占用")
	ErrPTYWriterDenied  = errors.New("命令 PTY 写入权校验失败")
	ErrPTYInvalidSignal = errors.New("PTY signal 无效")
)

// commandPTYHandle is the narrow control boundary exposed by a running local
// PTY. Implementations must never persist stdin bytes or execute a second
// command when a handle is reused.
type commandPTYHandle interface {
	Write([]byte) (int, error)
	Resize(uint16, uint16) error
	Signal(string) error
	Close() error
}

type ptyWriterLease struct {
	token string
}

// registerPTYSession attaches a live control handle to one immutable
// CommandRun. The handle is removed when the command exits; it is not a
// durable reattach mechanism.
func (s *Service) registerPTYSession(id string, handle commandPTYHandle) error {
	if s == nil || handle == nil || strings.TrimSpace(id) == "" {
		return ErrPTYUnavailable
	}
	s.ptyMu.Lock()
	defer s.ptyMu.Unlock()
	if s.ptySessions == nil {
		s.ptySessions = make(map[string]commandPTYHandle)
	}
	if s.ptySubscribers == nil {
		s.ptySubscribers = make(map[string]map[chan CommandChunk]struct{})
	}
	if _, exists := s.ptySessions[id]; exists {
		return fmt.Errorf("%w: command_run_id=%s", ErrCommandRunConflict, id)
	}
	s.ptySessions[id] = handle
	return nil
}

func (s *Service) unregisterPTYSession(id string, handle commandPTYHandle) {
	if s == nil {
		return
	}
	s.ptyMu.Lock()
	if current, ok := s.ptySessions[strings.TrimSpace(id)]; ok && (handle == nil || current == handle) {
		delete(s.ptySessions, strings.TrimSpace(id))
	}
	for subscriber := range s.ptySubscribers[strings.TrimSpace(id)] {
		close(subscriber)
	}
	delete(s.ptySubscribers, strings.TrimSpace(id))
	delete(s.ptyWriterLeases, strings.TrimSpace(id))
	s.ptyMu.Unlock()
}

// subscribePTYOutput returns a bounded, best-effort live stream. It is only
// an in-process view; durable replay remains the CommandOutputRepository
// contract and is used to close gaps after a subscriber reconnects.
func (s *Service) subscribePTYOutput(id string) (<-chan CommandChunk, func(), error) {
	if s == nil {
		return nil, nil, ErrPTYUnavailable
	}
	s.ptyMu.Lock()
	defer s.ptyMu.Unlock()
	if s.ptySessions[strings.TrimSpace(id)] == nil {
		return nil, nil, ErrPTYUnavailable
	}
	if s.ptySubscribers == nil {
		s.ptySubscribers = make(map[string]map[chan CommandChunk]struct{})
	}
	channel := make(chan CommandChunk, 256)
	key := strings.TrimSpace(id)
	if s.ptySubscribers[key] == nil {
		s.ptySubscribers[key] = make(map[chan CommandChunk]struct{})
	}
	s.ptySubscribers[key][channel] = struct{}{}
	var once sync.Once
	release := func() {
		once.Do(func() {
			s.ptyMu.Lock()
			if subscribers := s.ptySubscribers[key]; subscribers != nil {
				if _, ok := subscribers[channel]; ok {
					delete(subscribers, channel)
					close(channel)
				}
				if len(subscribers) == 0 {
					delete(s.ptySubscribers, key)
				}
			}
			s.ptyMu.Unlock()
		})
	}
	return channel, release, nil
}

func (s *Service) publishPTYChunk(id string, chunk CommandChunk) {
	if s == nil || strings.TrimSpace(id) == "" {
		return
	}
	s.ptyMu.Lock()
	defer s.ptyMu.Unlock()
	key := strings.TrimSpace(id)
	for subscriber := range s.ptySubscribers[key] {
		select {
		case subscriber <- chunk:
		default:
			// A slow client must never block the command reader. Closing this
			// subscriber forces it to reconnect and replay durable chunks.
			delete(s.ptySubscribers[key], subscriber)
			close(subscriber)
		}
	}
	if len(s.ptySubscribers[key]) == 0 {
		delete(s.ptySubscribers, key)
	}
}

func (s *Service) ptyHandle(id string) (commandPTYHandle, error) {
	if s == nil {
		return nil, ErrPTYUnavailable
	}
	s.ptyMu.RLock()
	handle := s.ptySessions[strings.TrimSpace(id)]
	s.ptyMu.RUnlock()
	if handle == nil {
		return nil, ErrPTYUnavailable
	}
	return handle, nil
}

// AcquireCommandPTYWriter grants a short-lived, in-memory writer token. An
// empty requested token creates a new opaque token; callers must send it back
// on every stdin/control operation. Only one writer is allowed per run.
func (s *Service) AcquireCommandPTYWriter(ctx context.Context, id, requested string) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	run, err := s.GetCommandRun(ctx, id)
	if err != nil {
		return "", err
	}
	if run.TTY == nil || !run.TTY.Enabled || run.Mode != "pty" || run.Status != CommandRunRunning {
		return "", ErrPTYUnavailable
	}
	if _, err := s.ptyHandle(id); err != nil {
		return "", err
	}
	token := strings.TrimSpace(requested)
	if token == "" {
		randomBytes := make([]byte, 16)
		if _, err := rand.Read(randomBytes); err != nil {
			return "", fmt.Errorf("生成 PTY writer token 失败: %w", err)
		}
		token = "ptyw_" + hex.EncodeToString(randomBytes)
	}
	s.ptyMu.Lock()
	defer s.ptyMu.Unlock()
	if current := s.ptyWriterLeases[strings.TrimSpace(id)]; current != nil {
		if current.token != token {
			return "", ErrPTYWriterHeld
		}
		return token, nil
	}
	if s.ptyWriterLeases == nil {
		s.ptyWriterLeases = make(map[string]*ptyWriterLease)
	}
	s.ptyWriterLeases[strings.TrimSpace(id)] = &ptyWriterLease{token: token}
	return token, nil
}

func (s *Service) releaseCommandPTYWriter(id, token string) {
	if s == nil {
		return
	}
	s.ptyMu.Lock()
	if current := s.ptyWriterLeases[strings.TrimSpace(id)]; current != nil && current.token == strings.TrimSpace(token) {
		delete(s.ptyWriterLeases, strings.TrimSpace(id))
	}
	s.ptyMu.Unlock()
}

// ReleaseCommandPTYWriter releases a lease when an attach connection closes.
// It is idempotent and intentionally carries no process-cancellation side
// effect.
func (s *Service) ReleaseCommandPTYWriter(id, token string) {
	s.releaseCommandPTYWriter(id, token)
}

// SubscribeCommandPTYOutput exposes the bounded live stream to transport
// adapters such as the HTTP WebSocket handler. The returned release function
// is idempotent.
func (s *Service) SubscribeCommandPTYOutput(id string) (<-chan CommandChunk, func(), error) {
	return s.subscribePTYOutput(id)
}

func (s *Service) validatePTYWriter(id, token string) error {
	token = strings.TrimSpace(token)
	if token == "" {
		return ErrPTYWriterDenied
	}
	s.ptyMu.RLock()
	current := s.ptyWriterLeases[strings.TrimSpace(id)]
	s.ptyMu.RUnlock()
	if current == nil || current.token != token {
		return ErrPTYWriterDenied
	}
	return nil
}

// WriteCommandPTY sends one bounded stdin frame. The bytes are deliberately
// not routed through CommandChunk/AgentEvent or the durable output store.
func (s *Service) WriteCommandPTY(ctx context.Context, id, token string, data []byte) (int, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if len(data) == 0 || len(data) > maxPTYInputBytes {
		return 0, fmt.Errorf("%w: stdin 单帧必须在 1 到 %d 字节之间", ErrInvalidRequest, maxPTYInputBytes)
	}
	if err := s.validatePTYWriter(id, token); err != nil {
		return 0, err
	}
	handle, err := s.ptyHandle(id)
	if err != nil {
		return 0, err
	}
	return handle.Write(data)
}

func (s *Service) ResizeCommandPTY(ctx context.Context, id string, rows, cols uint16) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if rows < 1 || rows > maxTTYDimension || cols < 1 || cols > maxTTYDimension {
		return fmt.Errorf("%w: PTY rows/cols 必须在 1 到 %d 之间", ErrInvalidRequest, maxTTYDimension)
	}
	handle, err := s.ptyHandle(id)
	if err != nil {
		return err
	}
	return handle.Resize(rows, cols)
}

func (s *Service) SignalCommandPTY(ctx context.Context, id, token, signal string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.validatePTYWriter(id, token); err != nil {
		return err
	}
	handle, err := s.ptyHandle(id)
	if err != nil {
		return err
	}
	return handle.Signal(signal)
}

// ClosePTYSessions is used by an embedding host during graceful shutdown. It
// only closes local control handles; CommandRun's terminal checkpoint remains
// responsible for deciding whether the outcome is known.
func (s *Service) ClosePTYSessions() {
	if s == nil {
		return
	}
	s.ptyMu.Lock()
	handles := make([]commandPTYHandle, 0, len(s.ptySessions))
	for _, handle := range s.ptySessions {
		handles = append(handles, handle)
	}
	s.ptyMu.Unlock()
	for _, handle := range handles {
		_ = handle.Close()
	}
}
