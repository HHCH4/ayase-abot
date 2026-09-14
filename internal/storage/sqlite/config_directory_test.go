package sqlite

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	agentruntime "Abot/internal/agent/runtime"
	"Abot/internal/config"
)

func TestSQLiteRuntimeConfigDirectoryPersistsMonotonicBodyAcrossRestart(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	first := sqliteDirectoryProfile(t, "profile-sqlite", 1, "one")
	firstEnvelope := sqliteDirectorySigned(t, first, "", now)
	repo := store.RuntimeRepository().(agentruntime.RuntimeConfigDirectoryRepository)
	if duplicate, err := repo.AcceptRuntimeConfigDirectory(ctx, firstEnvelope, first); err != nil || duplicate {
		t.Fatalf("SQLite 首次接收失败 duplicate=%v err=%v", duplicate, err)
	}
	retry := firstEnvelope
	retry.IssuedAt = retry.IssuedAt.Add(time.Second)
	retry.Signature = ""
	retry, _ = agentruntime.SignRuntimeConfigDirectoryEnvelope(retry, []byte(configDirectorySQLiteSecret))
	if duplicate, err := repo.AcceptRuntimeConfigDirectory(ctx, retry, first); err != nil || !duplicate {
		t.Fatalf("SQLite 重试应幂等 duplicate=%v err=%v", duplicate, err)
	}
	updated := first
	updated.Revision = 2
	updated.Values = map[string]any{"safe.value": "two"}
	updatedEnvelope := sqliteDirectorySigned(t, updated, firstEnvelope.BodyDigest, now.Add(2*time.Second))
	if duplicate, err := repo.AcceptRuntimeConfigDirectory(ctx, updatedEnvelope, updated); err != nil || duplicate {
		t.Fatalf("SQLite 连续 revision 接收失败 duplicate=%v err=%v", duplicate, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	repo = reopened.RuntimeRepository().(agentruntime.RuntimeConfigDirectoryRepository)
	stored, err := repo.GetRuntimeConfigDirectory(ctx, agentruntime.RuntimeConfigDirectoryKindProfile, first.ID)
	if err != nil || stored.Envelope.Revision != 2 || stored.Entry.Values["safe.value"] != "two" {
		t.Fatalf("SQLite close/reopen 后正文丢失: %#v err=%v", stored, err)
	}
	if _, err := repo.AcceptRuntimeConfigDirectory(ctx, firstEnvelope, first); !errors.Is(err, agentruntime.ErrRuntimeConfigDirectoryStale) {
		t.Fatalf("SQLite 旧 revision 应 stale，实际=%v", err)
	}
	wrong := updated
	wrong.Revision = 3
	wrongEnvelope := sqliteDirectorySigned(t, wrong, "sha256:0000000000000000000000000000000000000000000000000000000000000000", now.Add(3*time.Second))
	if _, err := repo.AcceptRuntimeConfigDirectory(ctx, wrongEnvelope, wrong); !errors.Is(err, agentruntime.ErrRuntimeConfigDirectoryConflict) {
		t.Fatalf("SQLite 错误 previous 应 conflict，实际=%v", err)
	}
}

func TestSQLiteRuntimeConfigDirectoryOutboxSurvivesRestartAndLeaseTakeover(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	now := time.Now().UTC().Truncate(time.Microsecond)
	entry := sqliteDirectoryProfile(t, "profile-outbox-sqlite", 1, "durable")
	item, err := agentruntime.NewRuntimeConfigDirectoryOutbox("runtime-a", "runtime-b", entry, "", "sqlite-outbox", "sqlite-outbox-key", now)
	if err != nil {
		t.Fatal(err)
	}
	store, err := Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	outbox := store.RuntimeRepository().(agentruntime.RuntimeConfigDirectoryOutboxRepository)
	saved, err := outbox.EnqueueRuntimeConfigDirectoryOutbox(ctx, item)
	if err != nil || saved.ID != item.ID {
		t.Fatalf("SQLite outbox 首次入队失败: %#v err=%v", saved, err)
	}
	claimed, ok, err := outbox.ClaimRuntimeConfigDirectoryOutbox(ctx, "sqlite-worker-a", now, 30*time.Second)
	if err != nil || !ok || claimed.Attempt != 1 || claimed.Status != agentruntime.RuntimeConfigDirectoryOutboxProcessing {
		t.Fatalf("SQLite outbox 首次 claim 失败: %#v ok=%v err=%v", claimed, ok, err)
	}
	if completed, err := outbox.CompleteRuntimeConfigDirectoryOutbox(ctx, item.ID, "wrong-worker", now.Add(time.Second)); err != nil || completed {
		t.Fatalf("SQLite 错误 owner 不得完成: completed=%v err=%v", completed, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	outbox = reopened.RuntimeRepository().(agentruntime.RuntimeConfigDirectoryOutboxRepository)
	stored, err := outbox.GetRuntimeConfigDirectoryOutbox(ctx, item.ID)
	if err != nil || stored.Entry.Values["safe.value"] != "durable" || stored.Attempt != 1 || stored.LeaseOwner != "sqlite-worker-a" {
		t.Fatalf("SQLite 重启后 outbox 正文/lease 丢失: %#v err=%v", stored, err)
	}
	claimed, ok, err = outbox.ClaimRuntimeConfigDirectoryOutbox(ctx, "sqlite-worker-b", now.Add(31*time.Second), 5*time.Second)
	if err != nil || !ok || claimed.Attempt != 2 || claimed.LeaseOwner != "sqlite-worker-b" {
		t.Fatalf("SQLite 应接管过期 lease: %#v ok=%v err=%v", claimed, ok, err)
	}
	if retried, err := outbox.RetryRuntimeConfigDirectoryOutbox(ctx, item.ID, "sqlite-worker-b", now.Add(32*time.Second), "secret=sqlite-secret"); err != nil || !retried {
		t.Fatalf("SQLite retry 失败: retried=%v err=%v", retried, err)
	}
	stored, err = outbox.GetRuntimeConfigDirectoryOutbox(ctx, item.ID)
	if err != nil || stored.Status != agentruntime.RuntimeConfigDirectoryOutboxQueued || !stored.AvailableAt.Equal(now.Add(34*time.Second)) || strings.Contains(stored.LastError, "sqlite-secret") {
		t.Fatalf("SQLite retry 状态/脱敏错误: %#v err=%v", stored, err)
	}
	claimed, ok, err = outbox.ClaimRuntimeConfigDirectoryOutbox(ctx, "sqlite-worker-c", now.Add(34*time.Second), 5*time.Second)
	if err != nil || !ok || claimed.Attempt != 3 {
		t.Fatalf("SQLite 退避后 claim 失败: %#v ok=%v err=%v", claimed, ok, err)
	}
	if completed, err := outbox.CompleteRuntimeConfigDirectoryOutbox(ctx, item.ID, "sqlite-worker-c", now.Add(35*time.Second)); err != nil || !completed {
		t.Fatalf("SQLite 有效 lease 完成失败: completed=%v err=%v", completed, err)
	}
	items, err := outbox.ListRuntimeConfigDirectoryOutbox(ctx, agentruntime.RuntimeConfigDirectoryKindProfile, entry.ID, agentruntime.RuntimeConfigDirectoryOutboxCompleted, 10)
	if err != nil || len(items) != 1 || items[0].Status != agentruntime.RuntimeConfigDirectoryOutboxCompleted {
		t.Fatalf("SQLite completed 列表错误: %#v err=%v", items, err)
	}
	if duplicate, err := outbox.EnqueueRuntimeConfigDirectoryOutbox(ctx, item); err != nil || duplicate.ID != item.ID || duplicate.Status != agentruntime.RuntimeConfigDirectoryOutboxCompleted {
		t.Fatalf("SQLite 终态 cursor 应幂等返回: %#v err=%v", duplicate, err)
	}
}

func TestSQLiteRuntimeConfigDirectoryMaterializationUsesRevisionCASAndPreservesSelection(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	configService, err := config.NewService(store.ConfigRepository(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := configService.EnsureDefault(ctx); err != nil {
		t.Fatal(err)
	}
	personaService, err := config.NewPersonaService(store.PersonaRepository())
	if err != nil {
		t.Fatal(err)
	}
	if err := personaService.EnsureDefault(ctx); err != nil {
		t.Fatal(err)
	}
	defaultProfile, err := configService.GetProfile(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	defaultPersonaID, err := personaService.DefaultPersonaID(ctx)
	if err != nil {
		t.Fatal(err)
	}

	profileEntry, err := agentruntime.NewRuntimeConfigDirectoryProfileEntry(config.Profile{ID: "remote-profile", Name: "远程配置", Revision: 1, Values: config.Values{"ai.temperature": 0.25}})
	if err != nil {
		t.Fatal(err)
	}
	profileEnvelope := sqliteDirectorySigned(t, profileEntry, "", time.Now().UTC())
	profileRecord := agentruntime.RuntimeConfigDirectoryRecord{Envelope: profileEnvelope, Entry: profileEntry, ReceivedAt: time.Now().UTC()}
	materializer := store.RuntimeConfigDirectoryMaterializer()
	if materializer == nil {
		t.Fatal("SQLite 必须暴露显式 directory materializer")
	}
	if duplicate, err := materializer.MaterializeRuntimeConfigDirectory(ctx, profileRecord); err != nil || duplicate {
		t.Fatalf("profile 首次 materialize 失败 duplicate=%v err=%v", duplicate, err)
	}
	profile, err := configService.GetProfile(ctx, profileEntry.ID)
	if err != nil || profile.Revision != 1 || profile.Values["ai.temperature"] != 0.25 || profile.IsDefault {
		t.Fatalf("profile materialize 内容/默认标记错误: %#v err=%v", profile, err)
	}
	unchangedDefault, err := configService.GetProfile(ctx, defaultProfile.ID)
	if err != nil || !unchangedDefault.IsDefault {
		t.Fatalf("materialize 不得改变默认 profile: %#v err=%v", unchangedDefault, err)
	}
	if duplicate, err := materializer.MaterializeRuntimeConfigDirectory(ctx, profileRecord); err != nil || !duplicate {
		t.Fatalf("profile 重复 materialize 应幂等 duplicate=%v err=%v", duplicate, err)
	}
	profileV2 := profileEntry
	profileV2.Revision = 2
	profileV2.Values = config.Values{"ai.temperature": 0.4}
	profileV2Envelope := sqliteDirectorySigned(t, profileV2, profileEnvelope.BodyDigest, time.Now().UTC())
	if duplicate, err := materializer.MaterializeRuntimeConfigDirectory(ctx, agentruntime.RuntimeConfigDirectoryRecord{Envelope: profileV2Envelope, Entry: profileV2, ReceivedAt: time.Now().UTC()}); err != nil || duplicate {
		t.Fatalf("profile revision 2 materialize 失败 duplicate=%v err=%v", duplicate, err)
	}
	profile, err = configService.GetProfile(ctx, profileEntry.ID)
	if err != nil || profile.Revision != 2 || profile.Values["ai.temperature"] != 0.4 {
		t.Fatalf("profile revision 2 未落库: %#v err=%v", profile, err)
	}
	profileV3 := profileV2
	profileV3.Revision = 3
	profileV3.Values = config.Values{"ai.temperature": 0.5}
	wrongProfileEnvelope := sqliteDirectorySigned(t, profileV3, "sha256:0000000000000000000000000000000000000000000000000000000000000000", time.Now().UTC())
	if _, err := materializer.MaterializeRuntimeConfigDirectory(ctx, agentruntime.RuntimeConfigDirectoryRecord{Envelope: wrongProfileEnvelope, Entry: profileV3, ReceivedAt: time.Now().UTC()}); !errors.Is(err, agentruntime.ErrRuntimeConfigDirectoryConflict) {
		t.Fatalf("profile 错误 previous digest 应 conflict，实际=%v", err)
	}
	unknownEntry, err := agentruntime.NewRuntimeConfigDirectoryProfileEntry(config.Profile{ID: "remote-unknown", Name: "未知配置", Revision: 1, Values: config.Values{"not_in_schema": true}})
	if err != nil {
		t.Fatal(err)
	}
	unknownEnvelope := sqliteDirectorySigned(t, unknownEntry, "", time.Now().UTC())
	if _, err := materializer.MaterializeRuntimeConfigDirectory(ctx, agentruntime.RuntimeConfigDirectoryRecord{Envelope: unknownEnvelope, Entry: unknownEntry, ReceivedAt: time.Now().UTC()}); !errors.Is(err, agentruntime.ErrInvalidRuntimeConfigDirectoryMaterialization) {
		t.Fatalf("未知 profile 字段应 fail-closed，实际=%v", err)
	}

	personaEntry, err := agentruntime.NewRuntimeConfigDirectoryPersonaEntry(config.Persona{ID: "remote-persona", Name: "远程人格", Description: "远程描述", Instruction: "保持简洁", Revision: 1, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	personaEnvelope := sqliteDirectorySigned(t, personaEntry, "", time.Now().UTC())
	if duplicate, err := materializer.MaterializeRuntimeConfigDirectory(ctx, agentruntime.RuntimeConfigDirectoryRecord{Envelope: personaEnvelope, Entry: personaEntry, ReceivedAt: time.Now().UTC()}); err != nil || duplicate {
		t.Fatalf("persona 首次 materialize 失败 duplicate=%v err=%v", duplicate, err)
	}
	persona, err := personaService.GetPersona(ctx, personaEntry.ID)
	if err != nil || persona.Revision != 1 || persona.Instruction != "保持简洁" || persona.IsDefault {
		t.Fatalf("persona materialize 内容/默认标记错误: %#v err=%v", persona, err)
	}
	if got, err := personaService.DefaultPersonaID(ctx); err != nil || got != defaultPersonaID {
		t.Fatalf("materialize 不得改变默认 persona: got=%q err=%v", got, err)
	}
	personaV2 := personaEntry
	personaV2.Revision = 2
	personaV2.Instruction = "保持准确且简洁"
	personaV2Envelope := sqliteDirectorySigned(t, personaV2, personaEnvelope.BodyDigest, time.Now().UTC())
	if duplicate, err := materializer.MaterializeRuntimeConfigDirectory(ctx, agentruntime.RuntimeConfigDirectoryRecord{Envelope: personaV2Envelope, Entry: personaV2, ReceivedAt: time.Now().UTC()}); err != nil || duplicate {
		t.Fatalf("persona revision 2 materialize 失败 duplicate=%v err=%v", duplicate, err)
	}
	persona, err = personaService.GetPersona(ctx, personaEntry.ID)
	if err != nil || persona.Revision != 2 || persona.Instruction != "保持准确且简洁" {
		t.Fatalf("persona revision 2 未落库: %#v err=%v", persona, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	reloadedConfig, err := config.NewService(reopened.ConfigRepository(), nil)
	if err != nil {
		t.Fatal(err)
	}
	reloadedProfile, err := reloadedConfig.GetProfile(ctx, profileEntry.ID)
	if err != nil || reloadedProfile.Revision != 2 || reloadedProfile.Values["ai.temperature"] != 0.4 {
		t.Fatalf("重启后 materialized profile 丢失: %#v err=%v", reloadedProfile, err)
	}
	reloadedPersonaService, err := config.NewPersonaService(reopened.PersonaRepository())
	if err != nil {
		t.Fatal(err)
	}
	reloadedPersona, err := reloadedPersonaService.GetPersona(ctx, personaEntry.ID)
	if err != nil || reloadedPersona.Revision != 2 || reloadedPersona.Instruction != "保持准确且简洁" {
		t.Fatalf("重启后 materialized persona 丢失: %#v err=%v", reloadedPersona, err)
	}
}

func TestSQLiteRuntimeConfigDirectoryFanoutIsAtomicAndSurvivesReconcile(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	entry := sqliteDirectoryProfile(t, "profile-fanout-sqlite", 1, "fanout")
	plan, children, err := agentruntime.NewRuntimeConfigDirectoryFanoutPlan("runtime-a", []string{"runtime-c", "runtime-a", "runtime-b"}, entry, "", "sqlite-fanout", "sqlite-fanout-key", now)
	if err != nil {
		t.Fatal(err)
	}
	repo := store.RuntimeRepository().(agentruntime.RuntimeConfigDirectoryFanoutRepository)
	saved, err := repo.EnqueueRuntimeConfigDirectoryFanout(ctx, plan, children)
	if err != nil || saved.ID != plan.ID || saved.PendingCount != 3 {
		t.Fatalf("SQLite fanout 原子登记失败: %#v err=%v", saved, err)
	}
	if duplicate, err := repo.EnqueueRuntimeConfigDirectoryFanout(ctx, plan, children); err != nil || duplicate.ID != plan.ID {
		t.Fatalf("SQLite fanout 重复登记应幂等: %#v err=%v", duplicate, err)
	}
	childRepo := store.RuntimeRepository().(agentruntime.RuntimeConfigDirectoryOutboxRepository)
	claimed, ok, err := childRepo.ClaimRuntimeConfigDirectoryOutbox(ctx, "fanout-worker-a", now, time.Minute)
	if err != nil || !ok {
		t.Fatalf("SQLite fanout claim 首个 child 失败: %#v ok=%v err=%v", claimed, ok, err)
	}
	if _, err := childRepo.CompleteRuntimeConfigDirectoryOutbox(ctx, claimed.ID, "fanout-worker-a", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	partial, err := repo.ReconcileRuntimeConfigDirectoryFanout(ctx, plan.ID, now.Add(time.Second))
	if err != nil || partial.Status != agentruntime.RuntimeConfigDirectoryFanoutPartial || partial.CompletedCount != 1 || partial.PendingCount != 2 {
		t.Fatalf("SQLite fanout partial 摘要错误: %#v err=%v", partial, err)
	}
	for index := 0; index < 2; index++ {
		claimed, ok, err = childRepo.ClaimRuntimeConfigDirectoryOutbox(ctx, "fanout-worker-b", now.Add(time.Duration(index+2)*time.Second), time.Minute)
		if err != nil || !ok {
			t.Fatalf("SQLite fanout claim child %d 失败: %#v ok=%v err=%v", index, claimed, ok, err)
		}
		if _, err := childRepo.CompleteRuntimeConfigDirectoryOutbox(ctx, claimed.ID, "fanout-worker-b", now.Add(time.Duration(index+3)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	completed, err := repo.ReconcileRuntimeConfigDirectoryFanout(ctx, plan.ID, now.Add(5*time.Second))
	if err != nil || completed.Status != agentruntime.RuntimeConfigDirectoryFanoutCompleted || completed.CompletedCount != 3 || completed.PendingCount != 0 {
		t.Fatalf("SQLite fanout completed 摘要错误: %#v err=%v", completed, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	repo = reopened.RuntimeRepository().(agentruntime.RuntimeConfigDirectoryFanoutRepository)
	reloaded, err := repo.GetRuntimeConfigDirectoryFanout(ctx, plan.ID)
	if err != nil || reloaded.Status != agentruntime.RuntimeConfigDirectoryFanoutCompleted || len(reloaded.Destinations) != 3 || len(reloaded.OutboxIDs) != 3 {
		t.Fatalf("SQLite 重启后 fanout plan 丢失: %#v err=%v", reloaded, err)
	}
}

func TestSQLiteRuntimeConfigDirectoryFanoutConcurrentEnqueueIsIdempotent(t *testing.T) {
	ctx := context.Background()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC().Truncate(time.Microsecond)
	entry := sqliteDirectoryProfile(t, "profile-fanout-concurrent", 1, "concurrent")
	plan, children, err := agentruntime.NewRuntimeConfigDirectoryFanoutPlan("runtime-source", []string{"runtime-a", "runtime-b", "runtime-c"}, entry, "", "concurrent-correlation", "concurrent-key", now)
	if err != nil {
		t.Fatal(err)
	}
	repo := store.RuntimeRepository().(agentruntime.RuntimeConfigDirectoryFanoutRepository)
	var wait sync.WaitGroup
	errCh := make(chan error, 8)
	for index := 0; index < 8; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if _, err := repo.EnqueueRuntimeConfigDirectoryFanout(ctx, plan, children); err != nil {
				errCh <- err
			}
		}()
	}
	wait.Wait()
	close(errCh)
	for err := range errCh {
		t.Errorf("并发 fanout enqueue 不应失败: %v", err)
	}
	items, err := repo.ListRuntimeConfigDirectoryFanouts(ctx, "runtime-source", "", 10)
	if err != nil || len(items) != 1 || items[0].ID != plan.ID {
		t.Fatalf("并发 fanout enqueue 应只保留一个 parent: items=%#v err=%v", items, err)
	}
}

func TestSQLiteRuntimeConfigDirectoryFanoutDoesNotReuseChildAcrossPlans(t *testing.T) {
	ctx := context.Background()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC().Truncate(time.Microsecond)
	entry := sqliteDirectoryProfile(t, "profile-fanout-owner-sqlite", 1, "owner")
	runtimeRepo := store.RuntimeRepository()
	outboxRepo := runtimeRepo.(agentruntime.RuntimeConfigDirectoryOutboxRepository)
	fanoutRepo := runtimeRepo.(agentruntime.RuntimeConfigDirectoryFanoutRepository)
	direct, err := agentruntime.NewRuntimeConfigDirectoryOutbox("runtime-source", "runtime-a", entry, "", "owner-correlation-sqlite", "owner-key-sqlite", now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := outboxRepo.EnqueueRuntimeConfigDirectoryOutbox(ctx, direct); err != nil {
		t.Fatal(err)
	}
	firstPlan, firstChildren, err := agentruntime.NewRuntimeConfigDirectoryFanoutPlan("runtime-source", []string{"runtime-a", "runtime-b"}, entry, "", "owner-correlation-sqlite", "owner-key-sqlite", now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fanoutRepo.EnqueueRuntimeConfigDirectoryFanout(ctx, firstPlan, firstChildren); err != nil {
		t.Fatalf("SQLite fanout 应可接管同 identity child: %v", err)
	}
	owned, err := outboxRepo.GetRuntimeConfigDirectoryOutbox(ctx, firstChildren[0].ID)
	if err != nil || owned.FanoutID != firstPlan.ID {
		t.Fatalf("SQLite child 未持久化 fanout owner: %#v err=%v", owned, err)
	}
	secondPlan, secondChildren, err := agentruntime.NewRuntimeConfigDirectoryFanoutPlan("runtime-source", []string{"runtime-a"}, entry, "", "owner-correlation-sqlite", "owner-key-sqlite", now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fanoutRepo.EnqueueRuntimeConfigDirectoryFanout(ctx, secondPlan, secondChildren); err == nil {
		t.Fatal("SQLite child 不得被两个不同 fanout parent 复用")
	}
}

const configDirectorySQLiteSecret = "config-directory-sqlite-secret-32-bytes"

func sqliteDirectoryProfile(t *testing.T, id string, revision int, value any) agentruntime.RuntimeConfigDirectoryEntry {
	t.Helper()
	entry, err := agentruntime.NewRuntimeConfigDirectoryProfileEntry(config.Profile{ID: id, Name: "profile-" + id, Revision: revision, Values: config.Values{"safe.value": value}})
	if err != nil {
		t.Fatal(err)
	}
	return entry
}

func sqliteDirectorySigned(t *testing.T, entry agentruntime.RuntimeConfigDirectoryEntry, previous string, at time.Time) agentruntime.RuntimeConfigDirectoryEnvelope {
	t.Helper()
	envelope, err := agentruntime.NewRuntimeConfigDirectoryEnvelope("runtime-a", "runtime-b", entry, previous, "sqlite-correlation", "sqlite-"+entry.ID, at)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err = agentruntime.SignRuntimeConfigDirectoryEnvelope(envelope, []byte(configDirectorySQLiteSecret))
	if err != nil {
		t.Fatal(err)
	}
	return envelope
}
