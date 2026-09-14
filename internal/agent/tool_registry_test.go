package agent

import (
	"errors"
	"strings"
	"testing"

	"google.golang.org/genai"
)

type registryTestTool struct {
	name string
	desc string
	long bool
	decl *genai.FunctionDeclaration
}

func (t registryTestTool) Name() string        { return t.name }
func (t registryTestTool) Description() string { return t.desc }
func (t registryTestTool) IsLongRunning() bool { return t.long }
func (t registryTestTool) Declaration() *genai.FunctionDeclaration {
	return t.decl
}

func TestToolRegistryDerivesDescriptorAndStableDigest(t *testing.T) {
	registry := NewToolRegistry()
	tool := registryTestTool{name: "workspace_read_file", desc: "read", decl: &genai.FunctionDeclaration{
		Name: "workspace_read_file", Description: "read", ParametersJsonSchema: map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}},
	}}
	descriptor, err := registry.RegisterRuntimeTool(tool, ToolSourceWorkspace)
	if err != nil {
		t.Fatal(err)
	}
	if descriptor.ID != "workspace.workspace_read_file" || descriptor.SchemaDigest == "" {
		t.Fatalf("descriptor=%#v", descriptor)
	}
	if !stringSliceContainsToolCapability(descriptor.Capabilities, ToolCapabilityReadWorkspace) {
		t.Fatalf("workspace read capability missing: %#v", descriptor.Capabilities)
	}
	if descriptor.InputSchema == nil {
		t.Fatal("function declaration schema should be retained")
	}
	again, err := registry.RegisterRuntimeTool(tool, ToolSourceWorkspace)
	if err != nil {
		t.Fatal(err)
	}
	if descriptor.SchemaDigest != again.SchemaDigest || registry.CatalogRevision() == "" {
		t.Fatalf("same registration should be stable: first=%#v again=%#v", descriptor, again)
	}
}

func TestToolRegistryRejectsConflictingVersionAndName(t *testing.T) {
	registry := NewToolRegistry()
	first := registryTestTool{name: "same_name", desc: "first"}
	if _, err := registry.Register(ToolDescriptor{ID: "builtin.same", ModelName: "same_name", Version: "1.0.0", Source: ToolSourceBuiltin}, first); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Register(ToolDescriptor{ID: "builtin.same", ModelName: "same_name", Version: "1.0.0", Source: ToolSourceBuiltin, InputSchema: []byte(`{"type":"object"}`)}, first); !errors.Is(err, ErrToolConflict) {
		t.Fatalf("schema change should conflict, err=%v", err)
	}
	if _, err := registry.Register(ToolDescriptor{ID: "workspace.other", ModelName: "same_name", Version: "1.0.0", Source: ToolSourceWorkspace}, registryTestTool{name: "same_name"}); !errors.Is(err, ErrToolConflict) {
		t.Fatalf("model name collision should conflict, err=%v", err)
	}
	if _, err := registry.Register(ToolDescriptor{ID: "bad", ModelName: "bad", Version: "1", Source: ToolSourceBuiltin}, first); !errors.Is(err, ErrInvalidToolDescriptor) {
		t.Fatalf("invalid semver should fail, err=%v", err)
	}
}

func TestToolRegistrySelectionPolicyCompatibilityAndBudget(t *testing.T) {
	registry := NewToolRegistry()
	for _, item := range []struct {
		id    string
		name  string
		score float64
		caps  []ToolCapability
	}{
		{id: "builtin.required", name: "required", score: 1, caps: []ToolCapability{ToolCapabilityReadWorkspace}},
		{id: "builtin.high", name: "high", score: 10, caps: []ToolCapability{ToolCapabilityReadWorkspace}},
		{id: "builtin.low", name: "low", score: 1, caps: []ToolCapability{ToolCapabilityReadWorkspace}},
	} {
		if _, err := registry.Register(ToolDescriptor{ID: item.id, ModelName: item.name, Version: "1.0.0", Source: ToolSourceBuiltin, Capabilities: item.caps}, registryTestTool{name: item.name}); err != nil {
			t.Fatal(err)
		}
	}
	selection, err := registry.Select(ToolSelectionRequest{
		InvocationID: "inv-tools", PolicyRevision: "policy-1", ModelProfile: "profile-1", MaxSchemaTokens: 100,
		RequiredTools: []string{"required"}, Scores: map[string]float64{"builtin.high": 10, "builtin.low": 1},
		Compatible: func(descriptor ToolDescriptor) (bool, string) {
			if descriptor.ModelName == "low" {
				return false, "model_unsupported"
			}
			return true, ""
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(selection.Tools) != 2 || selection.SchemaTokens <= 0 {
		t.Fatalf("selection=%#v", selection)
	}
	if len(selection.Snapshot.Tools) != 2 || selection.Snapshot.InvocationID != "inv-tools" {
		t.Fatalf("snapshot=%#v", selection.Snapshot)
	}
	foundUnsupported := false
	for _, excluded := range selection.Excluded {
		if excluded.ID == "builtin.low" && excluded.Reason == "model_unsupported" {
			foundUnsupported = true
		}
	}
	if !foundUnsupported {
		t.Fatalf("compatibility exclusion missing: %#v", selection.Excluded)
	}
	if err := selection.Snapshot.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestToolRegistryRejectsMissingRequiredTool(t *testing.T) {
	registry := NewToolRegistry()
	if _, err := registry.Register(ToolDescriptor{ID: "builtin.required", ModelName: "required", Version: "1.0.0", Source: ToolSourceBuiltin}, registryTestTool{name: "required"}); err != nil {
		t.Fatal(err)
	}
	selection, err := registry.Select(ToolSelectionRequest{InvocationID: "inv-required", RequiredTools: []string{"required"}, MaxSchemaTokens: 1})
	if !errors.Is(err, ErrRequiredToolUnavailable) {
		t.Fatalf("必需工具超预算必须硬失败，selection=%#v err=%v", selection, err)
	}
	if len(selection.MissingRequired) != 1 || selection.MissingRequired[0] != "builtin.required" {
		t.Fatalf("缺失必需工具信息不完整: %#v", selection.MissingRequired)
	}
}

func TestToolRegistrySearchOmitsSchema(t *testing.T) {
	registry := NewToolRegistry()
	if _, err := registry.RegisterRuntimeTool(registryTestTool{name: "calculate", desc: "精确计算", decl: &genai.FunctionDeclaration{ParametersJsonSchema: map[string]any{"type": "object"}}}, ToolSourceBuiltin); err != nil {
		t.Fatal(err)
	}
	items := registry.Search("计算", nil, ToolSourceBuiltin, "", 10)
	if len(items) != 1 || items[0].InputSchema != nil || items[0].OutputSchema != nil {
		t.Fatalf("search result should be bounded descriptor: %#v", items)
	}
}

func TestToolSetSnapshotIDIsScopedToInvocation(t *testing.T) {
	registry := NewToolRegistry()
	if _, err := registry.Register(ToolDescriptor{ID: "builtin.one", ModelName: "one", Version: "1.0.0", Source: ToolSourceBuiltin}, registryTestTool{name: "one"}); err != nil {
		t.Fatal(err)
	}
	selectFor := func(invocationID string) ToolSetSnapshot {
		t.Helper()
		selection, err := registry.Select(ToolSelectionRequest{InvocationID: invocationID, MaxSchemaTokens: 100})
		if err != nil {
			t.Fatal(err)
		}
		return selection.Snapshot
	}
	first := selectFor("inv-first")
	second := selectFor("inv-second")

	// 同一份工具集在两次调用里的内容摘要相同，但快照行按 invocation 存储
	// （InvocationID 是唯一索引）。如果 ID 只由 digest 派生，第二个
	// Invocation 就会撞主键并整轮失败。
	if first.Digest != second.Digest {
		t.Fatalf("相同工具集应得到相同 digest: %s vs %s", first.Digest, second.Digest)
	}
	if first.ID == second.ID {
		t.Fatalf("不同 invocation 的快照 ID 必须不同: %s", first.ID)
	}
	for _, item := range []ToolSetSnapshot{first, second} {
		if !strings.Contains(item.ID, item.InvocationID) {
			t.Fatalf("快照 ID 必须绑定 invocation: id=%s invocation=%s", item.ID, item.InvocationID)
		}
	}
}
