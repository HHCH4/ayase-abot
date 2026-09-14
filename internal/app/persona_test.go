package app

import (
	"context"
	"testing"

	configsvc "Abot/internal/config"
	"Abot/internal/storage/sqlite"
)

func TestResolvePersonaRuntimeFollowsCatalogDefaultAndLegacyFallback(t *testing.T) {
	store, err := sqlite.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	service, err := configsvc.NewPersonaService(store.PersonaRepository())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := service.EnsureDefault(ctx); err != nil {
		t.Fatal(err)
	}
	custom, err := service.SavePersona(ctx, "persona-app", "应用人格", "", "应用指令", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.SetDefaultPersona(ctx, custom.ID); err != nil {
		t.Fatal(err)
	}
	runtime, err := resolvePersonaRuntime(ctx, service, "", "", configsvc.Runtime{Instruction: configsvc.DefaultPersonaInstruction})
	if err != nil || runtime.PersonaID != custom.ID || runtime.Instruction != custom.Instruction {
		t.Fatalf("目录默认人格没有进入运行时: %#v err=%v", runtime, err)
	}
	legacy, err := resolvePersonaRuntime(ctx, service, "", "", configsvc.Runtime{Instruction: "旧配置指令"})
	if err != nil || legacy.PersonaID != "" || legacy.Instruction != "旧配置指令" {
		t.Fatalf("旧 profile fallback 被错误覆盖: %#v err=%v", legacy, err)
	}
}
