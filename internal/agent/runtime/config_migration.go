package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"Abot/internal/agent"
)

const (
	maxRuntimeConfigMigrationIdempotencyKeyLength = 200
	maxRuntimeConfigMigrationEventDataLength      = 4096
)

var ErrInvalidRuntimeConfigMigration = errors.New("Runtime 配置迁移请求无效")

// RuntimeConfigMigrationEventID derives a stable metadata-only event ID for
// one explicit migration attempt. The old digest, new digest and optional
// idempotency key are all part of the identity, so a retry cannot overwrite a
// different migration with the same Invocation.
func RuntimeConfigMigrationEventID(invocationID, expectedDigest, newDigest, idempotencyKey string) string {
	material := strings.TrimSpace(invocationID) + "\x00" + strings.TrimSpace(expectedDigest) + "\x00" + strings.TrimSpace(newDigest) + "\x00" + strings.TrimSpace(idempotencyKey)
	sum := sha256.Sum256([]byte(material))
	return "runtime-config-migration-" + hex.EncodeToString(sum[:16])
}

func normalizeRuntimeConfigMigrationSnapshot(encoded string) (agent.RuntimeConfigSnapshot, string, string, error) {
	snapshot, err := agent.ParseRuntimeConfigSnapshot(strings.TrimSpace(encoded))
	if err != nil {
		return agent.RuntimeConfigSnapshot{}, "", "", err
	}
	canonicalBytes, err := json.Marshal(snapshot)
	if err != nil {
		return agent.RuntimeConfigSnapshot{}, "", "", fmt.Errorf("%w: 无法编码当前配置快照", ErrInvalidRuntimeConfigMigration)
	}
	canonical := string(canonicalBytes)
	digest := agent.RuntimeConfigSnapshotDigest(canonical)
	if digest == "" {
		return agent.RuntimeConfigSnapshot{}, "", "", fmt.Errorf("%w: 当前配置快照 digest 无效", ErrInvalidRuntimeConfigMigration)
	}
	return snapshot, canonical, digest, nil
}

func normalizeRuntimeConfigMigrationCommit(commit RuntimeConfigMigrationCommit, now time.Time) (RuntimeConfigMigrationCommit, agent.RuntimeConfigSnapshot, string, error) {
	commit.InvocationID = strings.TrimSpace(commit.InvocationID)
	commit.ExpectedDigest = strings.TrimSpace(strings.ToLower(commit.ExpectedDigest))
	commit.IdempotencyKey = strings.TrimSpace(commit.IdempotencyKey)
	if commit.InvocationID == "" || commit.ExpectedDigest == "" || strings.TrimSpace(commit.NewSnapshot) == "" {
		return RuntimeConfigMigrationCommit{}, agent.RuntimeConfigSnapshot{}, "", fmt.Errorf("%w: invocation_id、expected_digest 和 new_snapshot 不能为空", ErrInvalidRuntimeConfigMigration)
	}
	if len(commit.IdempotencyKey) > maxRuntimeConfigMigrationIdempotencyKeyLength {
		return RuntimeConfigMigrationCommit{}, agent.RuntimeConfigSnapshot{}, "", fmt.Errorf("%w: idempotency_key 超出长度限制", ErrInvalidRuntimeConfigMigration)
	}
	snapshot, canonical, newDigest, err := normalizeRuntimeConfigMigrationSnapshot(commit.NewSnapshot)
	if err != nil {
		return RuntimeConfigMigrationCommit{}, agent.RuntimeConfigSnapshot{}, "", err
	}
	commit.NewSnapshot = canonical
	if commit.Event.InvocationID != "" && strings.TrimSpace(commit.Event.InvocationID) != commit.InvocationID {
		return RuntimeConfigMigrationCommit{}, agent.RuntimeConfigSnapshot{}, "", fmt.Errorf("%w: event invocation_id 不一致", ErrInvalidRuntimeConfigMigration)
	}
	commit.Event.InvocationID = commit.InvocationID
	commit.Event.Type = strings.TrimSpace(commit.Event.Type)
	if commit.Event.Type == "" {
		commit.Event.Type = EventRuntimeConfigMigrated
	}
	if commit.Event.Type != EventRuntimeConfigMigrated {
		return RuntimeConfigMigrationCommit{}, agent.RuntimeConfigSnapshot{}, "", fmt.Errorf("%w: event type 不受支持", ErrInvalidRuntimeConfigMigration)
	}
	commit.Event.ID = strings.TrimSpace(commit.Event.ID)
	expectedEventID := RuntimeConfigMigrationEventID(commit.InvocationID, commit.ExpectedDigest, newDigest, commit.IdempotencyKey)
	if commit.Event.ID == "" {
		commit.Event.ID = expectedEventID
	}
	if commit.Event.ID != expectedEventID {
		return RuntimeConfigMigrationCommit{}, agent.RuntimeConfigSnapshot{}, "", fmt.Errorf("%w: event id 不匹配迁移身份", ErrInvalidRuntimeConfigMigration)
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	if commit.Event.Timestamp.IsZero() {
		commit.Event.Timestamp = now
	} else {
		commit.Event.Timestamp = commit.Event.Timestamp.UTC()
	}
	return commit, snapshot, newDigest, nil
}

// NormalizeRuntimeConfigMigrationCommit validates and canonicalizes a
// migration payload for storage implementations outside this package.
func NormalizeRuntimeConfigMigrationCommit(commit RuntimeConfigMigrationCommit, now time.Time) (RuntimeConfigMigrationCommit, agent.RuntimeConfigSnapshot, string, error) {
	return normalizeRuntimeConfigMigrationCommit(commit, now)
}

func runtimeConfigMigrationEventData(invocationID, expectedDigest, newDigest, idempotencyKey string, previous, current agent.RuntimeConfigSnapshot) (map[string]any, error) {
	data := map[string]any{
		"invocation_id":              strings.TrimSpace(invocationID),
		"from_digest":                strings.TrimSpace(expectedDigest),
		"to_digest":                  strings.TrimSpace(newDigest),
		"from_version":               previous.Version,
		"to_version":                 current.Version,
		"from_provider":              previous.ProviderID,
		"to_provider":                current.ProviderID,
		"from_model":                 previous.ModelID,
		"to_model":                   current.ModelID,
		"from_persona":               previous.PersonaID,
		"to_persona":                 current.PersonaID,
		"from_tool_catalog_revision": previous.ToolCatalogRevision,
		"to_tool_catalog_revision":   current.ToolCatalogRevision,
	}
	if strings.TrimSpace(idempotencyKey) != "" {
		data["idempotency_key"] = strings.TrimSpace(idempotencyKey)
	}
	encoded, err := json.Marshal(data)
	if err != nil || len(encoded) > maxRuntimeConfigMigrationEventDataLength {
		return nil, fmt.Errorf("%w: 迁移事件 metadata 超出边界", ErrInvalidRuntimeConfigMigration)
	}
	return data, nil
}

// RuntimeConfigMigrationEventData builds the bounded metadata-only audit
// payload used by Memory/SQLite commit implementations.
func RuntimeConfigMigrationEventData(invocationID, expectedDigest, newDigest, idempotencyKey string, previous, current agent.RuntimeConfigSnapshot) (map[string]any, error) {
	return runtimeConfigMigrationEventData(invocationID, expectedDigest, newDigest, idempotencyKey, previous, current)
}

func runtimeConfigMigrationWaitingStatus(status InvocationStatus) bool {
	switch status {
	case InvocationWaitingApproval, InvocationWaitingTool, InvocationWaitingUser:
		return true
	default:
		return false
	}
}

// IsRuntimeConfigMigrationWaitingStatus reports whether an Invocation is at a
// resumable waiting boundary where an explicit config migration is allowed.
func IsRuntimeConfigMigrationWaitingStatus(status InvocationStatus) bool {
	return runtimeConfigMigrationWaitingStatus(status)
}

func (c *Coordinator) currentRuntimeConfigSnapshot(ctx context.Context, invocation Invocation) (agent.RuntimeConfigSnapshot, string, error) {
	if c == nil || c.kernel == nil {
		return agent.RuntimeConfigSnapshot{}, "", fmt.Errorf("%w: Kernel 未装配", ErrInvalidRuntimeConfigMigration)
	}
	var workspaceID *string
	if strings.TrimSpace(invocation.WorkspaceID) != "" {
		value := strings.TrimSpace(invocation.WorkspaceID)
		workspaceID = &value
	}
	_, encoded, err := c.kernel.ResolveRuntimeConfigSnapshot(ctx, agent.ChatRequest{
		UserID: invocation.UserID, BotID: invocation.BotID, ConversationID: invocation.ConversationID,
		SessionID: invocation.SessionID, ProviderID: invocation.ProviderID, ModelID: invocation.ModelID,
		WorkspaceID: workspaceID, TargetPath: invocation.TargetPath,
	})
	if err != nil {
		return agent.RuntimeConfigSnapshot{}, "", fmt.Errorf("%w: 当前 Runtime 配置无法解析", ErrConflict)
	}
	snapshot, canonical, _, err := normalizeRuntimeConfigMigrationSnapshot(encoded)
	if err != nil {
		return agent.RuntimeConfigSnapshot{}, "", err
	}
	return snapshot, canonical, nil
}

// MigrateRuntimeConfig explicitly accepts the currently resolved Runtime
// configuration for a waiting Invocation. The operation is deliberately
// separate from ResumeInvocation/ResolveApproval: merely observing a changed
// config cannot mutate the trust boundary, and callers must provide the
// digest they saw when the migration was requested.
func (c *Coordinator) MigrateRuntimeConfig(ctx context.Context, invocationID, expectedDigest, idempotencyKey string) (Invocation, error) {
	if c == nil || c.repo == nil {
		return Invocation{}, ErrInvalidRuntimeConfigMigration
	}
	c.approvalMu.Lock()
	defer c.approvalMu.Unlock()

	invocationID = strings.TrimSpace(invocationID)
	expectedDigest = strings.TrimSpace(strings.ToLower(expectedDigest))
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if invocationID == "" || expectedDigest == "" {
		return Invocation{}, fmt.Errorf("%w: invocation_id 和 expected_config_snapshot_digest 不能为空", ErrInvalidRuntimeConfigMigration)
	}
	if len(idempotencyKey) > maxRuntimeConfigMigrationIdempotencyKeyLength {
		return Invocation{}, fmt.Errorf("%w: idempotency_key 超出长度限制", ErrInvalidRuntimeConfigMigration)
	}
	invocation, err := c.repo.GetInvocation(ctx, invocationID)
	if err != nil {
		return Invocation{}, err
	}
	if invocation.Status.Terminal() {
		return Invocation{}, fmt.Errorf("%w: terminal invocation 不允许迁移配置", ErrConflict)
	}
	if !runtimeConfigMigrationWaitingStatus(invocation.Status) {
		return Invocation{}, fmt.Errorf("%w: 只有等待边界可以迁移配置", ErrConflict)
	}
	storedSnapshot, _, _, storedErr := normalizeRuntimeConfigMigrationSnapshot(invocation.ConfigSnapshot)
	if storedErr != nil {
		return Invocation{}, fmt.Errorf("%w: 已保存配置快照无效", ErrConflict)
	}
	storedDigest := agent.RuntimeConfigSnapshotDigest(invocation.ConfigSnapshot)
	if storedDigest == "" {
		return Invocation{}, fmt.Errorf("%w: Invocation 缺少有效配置快照", ErrConflict)
	}
	if storedDigest != expectedDigest {
		if idempotencyKey != "" && c.runtimeConfigMigrationAlreadyApplied(ctx, invocation, expectedDigest, idempotencyKey) {
			return invocation, nil
		}
		return Invocation{}, fmt.Errorf("%w: expected_config_snapshot_digest 已过期", ErrConflict)
	}
	currentSnapshot, currentEncoded, err := c.currentRuntimeConfigSnapshot(ctx, invocation)
	if err != nil {
		return Invocation{}, err
	}
	currentDigest := agent.RuntimeConfigSnapshotDigest(currentEncoded)
	if currentDigest == "" {
		return Invocation{}, fmt.Errorf("%w: 当前配置快照 digest 无效", ErrInvalidRuntimeConfigMigration)
	}
	if currentDigest == storedDigest {
		return invocation, nil
	}
	commit := RuntimeConfigMigrationCommit{
		InvocationID: invocation.ID, ExpectedDigest: storedDigest, NewSnapshot: currentEncoded, IdempotencyKey: idempotencyKey,
		Event: AgentEvent{ID: RuntimeConfigMigrationEventID(invocation.ID, storedDigest, currentDigest, idempotencyKey), InvocationID: invocation.ID, Type: EventRuntimeConfigMigrated, Timestamp: time.Now().UTC()},
	}
	data, dataErr := runtimeConfigMigrationEventData(invocation.ID, storedDigest, currentDigest, idempotencyKey, storedSnapshot, currentSnapshot)
	if dataErr != nil {
		return Invocation{}, dataErr
	}
	commit.Event.Data = data
	configSource, configDestination, _, configEnabled := c.runtimeConfigDeliveryConfig()
	if configEnabled {
		if _, ok := c.repo.(RuntimeConfigDeliveryOutboxRepository); !ok {
			return Invocation{}, fmt.Errorf("%w: Runtime 未启用 config delivery outbox", ErrConflict)
		}
		deliveryRepo, ok := c.repo.(RuntimeConfigMigrationDeliveryCommitRepository)
		if !ok {
			return Invocation{}, fmt.Errorf("%w: Repository 未提供配置迁移与 delivery 原子提交", ErrConflict)
		}
		committed, event, _, commitErr := deliveryRepo.CommitRuntimeConfigMigrationWithDelivery(ctx, commit, configSource, configDestination)
		if commitErr != nil {
			return Invocation{}, commitErr
		}
		c.publishStoredEvent(event)
		return committed, nil
	}
	repository, ok := c.repo.(RuntimeConfigMigrationCommitRepository)
	if !ok {
		return Invocation{}, fmt.Errorf("%w: Repository 未提供配置迁移原子提交", ErrConflict)
	}
	committed, event, commitErr := repository.CommitRuntimeConfigMigration(ctx, commit)
	if commitErr != nil {
		return Invocation{}, commitErr
	}
	c.publishStoredEvent(event)
	return committed, nil
}

func (c *Coordinator) runtimeConfigMigrationAlreadyApplied(ctx context.Context, invocation Invocation, expectedDigest, idempotencyKey string) bool {
	if strings.TrimSpace(invocation.ConfigSnapshotDigest) == "" {
		return false
	}
	events, err := c.listAllInvocationEvents(ctx, invocation.ID, maxRuntimeEventReplay)
	if err != nil {
		return false
	}
	for _, event := range events {
		if event.Type != EventRuntimeConfigMigrated || event.Data == nil {
			continue
		}
		from, _ := event.Data["from_digest"].(string)
		to, _ := event.Data["to_digest"].(string)
		key, _ := event.Data["idempotency_key"].(string)
		if strings.TrimSpace(from) == expectedDigest && strings.TrimSpace(key) == idempotencyKey && strings.TrimSpace(to) == invocation.ConfigSnapshotDigest {
			return true
		}
	}
	return false
}

// RuntimeConfigMigrationEventMatches validates the immutable digest and
// idempotency metadata of a previously persisted migration event.
func RuntimeConfigMigrationEventMatches(event AgentEvent, expectedDigest, newDigest, idempotencyKey string) bool {
	return runtimeConfigMigrationEventMatches(event, expectedDigest, newDigest, idempotencyKey)
}
