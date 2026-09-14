package config

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"
)

type memoryPersonaRepository struct {
	mu        sync.Mutex
	items     map[string]Persona
	revisions map[string][]PersonaRevision
	bindings  map[string]string
}

func newMemoryPersonaRepository() *memoryPersonaRepository {
	return &memoryPersonaRepository{items: map[string]Persona{}, revisions: map[string][]PersonaRevision{}, bindings: map[string]string{}}
}

func (r *memoryPersonaRepository) ListPersonas(context.Context) ([]Persona, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	items := make([]Persona, 0, len(r.items))
	for _, item := range r.items {
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	return items, nil
}

func (r *memoryPersonaRepository) GetPersona(_ context.Context, id string) (Persona, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	item, ok := r.items[id]
	if !ok {
		return Persona{}, ErrNotFound
	}
	return item, nil
}

func (r *memoryPersonaRepository) ListPersonaRevisions(_ context.Context, id string) ([]PersonaRevision, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	items := append([]PersonaRevision(nil), r.revisions[id]...)
	return items, nil
}

func (r *memoryPersonaRepository) SavePersona(_ context.Context, item Persona, revision PersonaRevision) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.items[item.ID]; exists {
		old := r.items[item.ID]
		item.CreatedAt = old.CreatedAt
	}
	if item.IsDefault {
		for id, current := range r.items {
			current.IsDefault = id == item.ID
			r.items[id] = current
		}
	}
	r.items[item.ID] = item
	r.revisions[item.ID] = append(r.revisions[item.ID], revision)
	return nil
}

func (r *memoryPersonaRepository) DeletePersona(_ context.Context, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.items[id]; !ok {
		return ErrNotFound
	}
	for _, bound := range r.bindings {
		if bound == id {
			return ErrPersonaInUse
		}
	}
	delete(r.items, id)
	delete(r.revisions, id)
	return nil
}

func (r *memoryPersonaRepository) SetDefaultPersona(_ context.Context, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	item, ok := r.items[id]
	if !ok {
		return ErrNotFound
	}
	if !item.Enabled {
		return ErrInvalidRequest
	}
	for currentID, current := range r.items {
		current.IsDefault = currentID == id
		r.items[currentID] = current
	}
	return nil
}

func (r *memoryPersonaRepository) DefaultPersonaID(_ context.Context) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, item := range r.items {
		if item.IsDefault {
			return id, nil
		}
	}
	return "", ErrNotFound
}

func personaBindingKey(scope BindingScope, target string) string {
	return string(scope) + "\x00" + target
}

func (r *memoryPersonaRepository) BindPersona(_ context.Context, scope BindingScope, targetID, personaID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := personaBindingKey(scope, targetID)
	if personaID == "" {
		delete(r.bindings, key)
	} else {
		r.bindings[key] = personaID
	}
	return nil
}

func (r *memoryPersonaRepository) GetPersonaBinding(_ context.Context, scope BindingScope, targetID string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.bindings[personaBindingKey(scope, targetID)], nil
}

func TestPersonaServiceCatalogRevisionAndLifecycle(t *testing.T) {
	repository := newMemoryPersonaRepository()
	service, err := NewPersonaService(repository)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := service.EnsureDefault(ctx); err != nil {
		t.Fatal(err)
	}
	defaultPersona, err := service.GetPersona(ctx, DefaultPersonaID)
	if err != nil || !defaultPersona.IsDefault || !defaultPersona.Enabled || defaultPersona.Revision != 1 {
		t.Fatalf("默认人格错误: %#v err=%v", defaultPersona, err)
	}
	disabled := false
	if _, err := service.SavePersona(ctx, defaultPersona.ID, defaultPersona.Name, defaultPersona.Description, defaultPersona.Instruction, &disabled); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("默认人格禁用应被拒绝，得到 %v", err)
	}
	custom, err := service.SavePersona(ctx, "persona-reviewer", "审查者", "只关注风险", "你是严格的代码审查者。", nil)
	if err != nil {
		t.Fatal(err)
	}
	if custom.Revision != 1 || !custom.Enabled {
		t.Fatalf("新建人格状态错误: %#v", custom)
	}
	disabled = false
	custom, err = service.SavePersona(ctx, custom.ID, custom.Name, custom.Description, "第二版指令", &disabled)
	if err != nil || custom.Revision != 2 || custom.Enabled {
		t.Fatalf("更新人格没有递增/禁用: %#v err=%v", custom, err)
	}
	revisions, err := service.ListRevisions(ctx, custom.ID)
	if err != nil || len(revisions) != 2 || revisions[0].Number != 2 || revisions[0].Instruction != "第二版指令" {
		t.Fatalf("人格修订历史错误: %#v err=%v", revisions, err)
	}
	if err := service.SetDefaultPersona(ctx, custom.ID); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("禁用人格设为默认应失败，得到 %v", err)
	}
	if err := service.DeletePersona(ctx, DefaultPersonaID); !errors.Is(err, ErrDefaultPersona) {
		t.Fatalf("删除默认人格应失败，得到 %v", err)
	}
	if _, err := service.SavePersona(ctx, "persona-reviewer-2", "审查者", "", "另一个指令", nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("重复人格名称应冲突，得到 %v", err)
	}
	if _, err := service.SavePersona(ctx, "Bad-ID", "错误", "", "指令", nil); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("非法人格 ID 应被拒绝，得到 %v", err)
	}
}

func TestPersonaServiceSelectionPrecedenceAndBindingSafety(t *testing.T) {
	repository := newMemoryPersonaRepository()
	service, err := NewPersonaService(repository)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := service.EnsureDefault(ctx); err != nil {
		t.Fatal(err)
	}
	botPersona, err := service.SavePersona(ctx, "persona-bot", "机器人", "", "机器人指令", nil)
	if err != nil {
		t.Fatal(err)
	}
	conversationPersona, err := service.SavePersona(ctx, "persona-conv", "对话", "", "对话指令", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Bind(ctx, BindingBot, "bot-a", botPersona.ID); err != nil {
		t.Fatal(err)
	}
	if err := service.Bind(ctx, BindingConversation, "conversation-a", conversationPersona.ID); err != nil {
		t.Fatal(err)
	}
	selected, ok, err := service.ResolveSelection(ctx, "bot-a", "conversation-a", DefaultPersonaID)
	if err != nil || !ok || selected.ID != conversationPersona.ID {
		t.Fatalf("对话绑定没有覆盖机器人绑定: %#v ok=%v err=%v", selected, ok, err)
	}
	if err := service.Bind(ctx, BindingConversation, "conversation-a", ""); err != nil {
		t.Fatal(err)
	}
	selected, ok, err = service.ResolveSelection(ctx, "bot-a", "conversation-a", DefaultPersonaID)
	if err != nil || !ok || selected.ID != botPersona.ID {
		t.Fatalf("机器人绑定没有生效: %#v ok=%v err=%v", selected, ok, err)
	}
	if err := service.Bind(ctx, BindingBot, "bot-a", ""); err != nil {
		t.Fatal(err)
	}
	selected, ok, err = service.ResolveSelection(ctx, "bot-a", "conversation-a", DefaultPersonaID)
	if err != nil || !ok || selected.ID != DefaultPersonaID {
		t.Fatalf("配置 fallback 没有生效: %#v ok=%v err=%v", selected, ok, err)
	}
	disabled := false
	if _, err := service.SavePersona(ctx, botPersona.ID, botPersona.Name, botPersona.Description, botPersona.Instruction, &disabled); err != nil {
		t.Fatal(err)
	}
	if err := service.Bind(ctx, BindingBot, "bot-b", botPersona.ID); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("禁用人格不应允许绑定，得到 %v", err)
	}
	if _, ok, err := service.ResolveSelection(ctx, "", "", botPersona.ID); !errors.Is(err, ErrInvalidRequest) || ok {
		t.Fatalf("禁用 fallback 应失败且不返回选中，ok=%v err=%v", ok, err)
	}
	if err := service.Bind(ctx, BindingScope("invalid"), "target", DefaultPersonaID); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("非法绑定范围应被拒绝，得到 %v", err)
	}
	if err := service.Bind(ctx, BindingBot, strings.Repeat("x", 0), DefaultPersonaID); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("空绑定目标应被拒绝，得到 %v", err)
	}
	if err := service.Bind(ctx, BindingBot, strings.Repeat("x", 201), DefaultPersonaID); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("超长绑定目标应被拒绝，得到 %v", err)
	}
}

func TestPersonaServiceConcurrentNameConflictHasSingleWinner(t *testing.T) {
	repository := newMemoryPersonaRepository()
	service, err := NewPersonaService(repository)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := service.EnsureDefault(ctx); err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 8)
	var wait sync.WaitGroup
	for index := 0; index < 8; index++ {
		index := index
		wait.Add(1)
		go func() {
			defer wait.Done()
			id := "persona-concurrent-" + string(rune('a'+index))
			_, saveErr := service.SavePersona(ctx, id, "同名人格", "", "并发指令", nil)
			results <- saveErr
		}()
	}
	wait.Wait()
	close(results)
	winners := 0
	conflicts := 0
	for saveErr := range results {
		if saveErr == nil {
			winners++
		} else if errors.Is(saveErr, ErrConflict) {
			conflicts++
		} else {
			t.Fatalf("并发保存返回意外错误: %v", saveErr)
		}
	}
	if winners != 1 || conflicts != 7 {
		t.Fatalf("并发名称冲突 winner=%d conflicts=%d", winners, conflicts)
	}
}
