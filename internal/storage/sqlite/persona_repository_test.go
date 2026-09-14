package sqlite

import (
	"context"
	"errors"
	"testing"

	configsvc "Abot/internal/config"
)

func TestSQLitePersonaRepositoryPersistsCatalogRevisionsAndBindings(t *testing.T) {
	dataDir := t.TempDir()
	store, err := Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	service, err := configsvc.NewPersonaService(store.PersonaRepository())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := service.EnsureDefault(ctx); err != nil {
		t.Fatal(err)
	}
	item, err := service.SavePersona(ctx, "persona-sqlite", "SQLite 人格", "本地持久化", "第一版指令", nil)
	if err != nil {
		t.Fatal(err)
	}
	item, err = service.SavePersona(ctx, item.ID, item.Name, item.Description, "第二版指令", nil)
	if err != nil || item.Revision != 2 {
		t.Fatalf("更新人格失败: %#v err=%v", item, err)
	}
	if err := service.Bind(ctx, configsvc.BindingBot, "bot-sqlite", item.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	service, err = configsvc.NewPersonaService(store.PersonaRepository())
	if err != nil {
		t.Fatal(err)
	}
	got, err := service.GetPersona(ctx, item.ID)
	if err != nil || got.Revision != 2 || got.Instruction != "第二版指令" || got.Description != "本地持久化" {
		t.Fatalf("重启后人格错误: %#v err=%v", got, err)
	}
	revisions, err := service.ListRevisions(ctx, item.ID)
	if err != nil || len(revisions) != 2 || revisions[0].Number != 2 || revisions[1].Number != 1 {
		t.Fatalf("人格修订没有持久化: %#v err=%v", revisions, err)
	}
	bound, err := service.Binding(ctx, configsvc.BindingBot, "bot-sqlite")
	if err != nil || bound != item.ID {
		t.Fatalf("人格绑定没有持久化: %q err=%v", bound, err)
	}
	if err := service.DeletePersona(ctx, item.ID); !errors.Is(err, configsvc.ErrPersonaInUse) {
		t.Fatalf("仍绑定的人格删除应冲突，得到 %v", err)
	}
	if err := service.Bind(ctx, configsvc.BindingBot, "bot-sqlite", ""); err != nil {
		t.Fatal(err)
	}
	if err := service.SetDefaultPersona(ctx, item.ID); err != nil {
		t.Fatal(err)
	}
	if err := service.DeletePersona(ctx, item.ID); !errors.Is(err, configsvc.ErrDefaultPersona) {
		t.Fatalf("默认人格删除应冲突，得到 %v", err)
	}
}
