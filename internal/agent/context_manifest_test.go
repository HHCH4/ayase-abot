package agent

import (
	"errors"
	"strings"
	"testing"
	"time"

	"Abot/internal/provider"
	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

func TestContextManifestIsDeterministicAndClassifiesBudget(t *testing.T) {
	request := &adkmodel.LLMRequest{
		Model:    "demo-model",
		Contents: []*genai.Content{genai.NewContentFromText("历史消息", genai.RoleUser), genai.NewContentFromText("模型回复", genai.RoleModel)},
		Config: &genai.GenerateContentConfig{
			SystemInstruction: genai.NewContentFromText("系统约束", genai.RoleUser),
			Tools:             []*genai.Tool{{FunctionDeclarations: []*genai.FunctionDeclaration{{Name: "read_file", Description: "读取文件"}}}},
		},
	}
	runtime := RuntimeOptions{CompactionSafetyTokens: 16, AIMaxOutputTokens: 64}
	model := provider.Model{ID: "demo-model", ContextWindow: 80, MaxOutputTokens: 32}
	contract := &TaskContractProjection{Version: 2, TaskType: "inspect", Goal: "检查代码", SourceMessageIDs: []string{"message-1"}}
	first := buildContextManifest(request, "inv-1", "inv-1:model:1", model, runtime, []ProjectInstruction{{Path: "AGENTS.md", Content: "规则"}}, contract)
	second := buildContextManifest(request, "inv-1", "different:model:9", model, runtime, []ProjectInstruction{{Path: "AGENTS.md", Content: "规则"}}, contract)
	if first.Digest == "" || first.Digest != second.Digest {
		t.Fatalf("相同上下文选择应生成稳定 digest: first=%s second=%s", first.Digest, second.Digest)
	}
	if first.EstimatedInput <= 0 || len(first.Included) < 4 {
		t.Fatalf("manifest 未记录系统/历史/工具/契约/指令: %#v", first)
	}
	if len(first.Excluded) == 0 || len(first.Warnings) == 0 {
		t.Fatalf("超出硬窗口时应有 warning 和 excluded: %#v", first)
	}
	if err := first.Validate(); err != nil {
		t.Fatal(err)
	}
	if EstimateTextTokens("中文项目指令") <= 0 || EstimateContentTokens(nil) != 0 {
		t.Fatal("token estimator 边界不正确")
	}
	if first.CreatedAt.Before(time.Now().Add(-time.Minute)) {
		t.Fatal("manifest created_at 不应过旧")
	}
}

func TestContextManifestIncludesFreshWorkingSetAndExcludesStale(t *testing.T) {
	request := &adkmodel.LLMRequest{Config: &genai.GenerateContentConfig{SystemInstruction: genai.NewContentFromText("system", genai.RoleModel)}}
	manifest := buildContextManifestWithWorkingSet(request, "inv-working", "call-1", provider.Model{ID: "demo", ContextWindow: 1000}, RuntimeOptions{}, nil, nil, nil, []WorkingSetItem{
		{ID: "fresh", Kind: "file_slice", SourceRef: "file:a", Priority: WorkingSetPriorityP1, TokenEstimate: 3, ContentDigest: "a", Freshness: WorkingSetFresh},
		{ID: "stale", Kind: "file_slice", SourceRef: "file:b", Priority: WorkingSetPriorityP1, TokenEstimate: 3, ContentDigest: "b", Freshness: WorkingSetStale},
	})
	var fresh, stale bool
	for _, item := range manifest.Included {
		fresh = fresh || item.ID == "working_set:fresh"
	}
	for _, item := range manifest.Excluded {
		stale = stale || item.ID == "stale" && item.Reason == WorkingSetStale
	}
	if !fresh || !stale {
		t.Fatalf("working set manifest=%#v", manifest)
	}
}

func TestContextManifestUsesBoundedTokenizerCalibration(t *testing.T) {
	request := &adkmodel.LLMRequest{Contents: []*genai.Content{genai.NewContentFromText("calibrated context", genai.RoleUser)}}
	runtime := RuntimeOptions{CompactionSafetyTokens: 8, AIMaxOutputTokens: 16}
	baseModel := provider.Model{ID: "demo", ContextWindow: 1000}
	base := buildContextManifest(request, "inv-calibration", "call-base", baseModel, runtime, nil, nil)
	profile := provider.DefaultCapabilities(provider.Provider{ID: "demo-provider", Protocol: provider.ProtocolOpenAICompatible}, baseModel)
	profile.Tokenizer.Calibration.Samples = provider.MinTokenizerCalibrationSamples
	profile.Tokenizer.Calibration.SafetyMultiplier = 1.5
	baseModel.Capabilities = &profile
	calibrated := buildContextManifest(request, "inv-calibration", "call-calibrated", baseModel, runtime, nil, nil)
	if calibrated.EstimateQuality != "calibrated" || calibrated.EstimateMultiplier != 1.5 || calibrated.EstimatorName == "" {
		t.Fatalf("manifest 未记录 tokenizer 校准元数据: %#v", calibrated)
	}
	if calibrated.EstimatedInput <= base.EstimatedInput {
		t.Fatalf("达到样本门槛后应保守放大估算: base=%d calibrated=%d", base.EstimatedInput, calibrated.EstimatedInput)
	}
	if calibrated.Digest == base.Digest {
		t.Fatal("估算 multiplier 改变后 manifest digest 不应保持不变")
	}
	if got := ScaleTokenEstimate(10, 0.1); got != 10 {
		t.Fatalf("低于 1 的 multiplier 不应缩小估算: %d", got)
	}
	if got := ScaleTokenEstimate(10, provider.MaxTokenizerSafetyMultiplier*10); got != 20 {
		t.Fatalf("multiplier 应封顶: %d", got)
	}
}

func TestContextTokenEstimatesSaturateAtIntLimit(t *testing.T) {
	maxInt := int(^uint(0) >> 1)
	if got := ScaleTokenEstimate(maxInt, provider.MaxTokenizerSafetyMultiplier); got != maxInt {
		t.Fatalf("超大 token estimate 不应溢出: got=%d max=%d", got, maxInt)
	}
	content := &genai.Content{Parts: []*genai.Part{{InlineData: &genai.Blob{Data: []byte{1}}}}}
	if got := EstimateContentTokens(content); got <= 0 || got > maxInt {
		t.Fatalf("content token estimate 边界不正确: %d", got)
	}
	if !budgetExceedsWindow(maxInt, 1, 1, maxInt) {
		t.Fatal("预算求和溢出时应判定为超窗")
	}
}

func TestContextTokenEstimatesUseBoundedModalityCosts(t *testing.T) {
	smallImage := &genai.Content{Parts: []*genai.Part{{InlineData: &genai.Blob{MIMEType: "image/png", Data: []byte{1}}}}}
	largeImage := &genai.Content{Parts: []*genai.Part{{InlineData: &genai.Blob{MIMEType: "image/png", Data: make([]byte, 2*imageModalityChunkBytes)}}}}
	if got := EstimateContentTokens(smallImage); got != imageModalityBaseTokens+imageModalityChunkTokens {
		t.Fatalf("小图片应包含固定基线和首个 bounded chunk: got=%d", got)
	}
	if got := EstimateContentTokens(largeImage); got <= EstimateContentTokens(smallImage) || got > imageModalityMaxTokens {
		t.Fatalf("大图片估算应单调且封顶: got=%d small=%d", got, EstimateContentTokens(smallImage))
	}
	if got := EstimateContentTokens(&genai.Content{Parts: []*genai.Part{{InlineData: &genai.Blob{MIMEType: "application/octet-stream", Data: make([]byte, 64*1024)}}}}); got > unknownModalityMaxTokens {
		t.Fatalf("未知二进制估算不应超过上限: %d", got)
	}
	if got := EstimateContentTokens(&genai.Content{Parts: []*genai.Part{{FileData: &genai.FileData{MIMEType: "application/pdf", FileURI: "gs://bucket/file.pdf"}}}}); got != remoteModalityTokens {
		t.Fatalf("远程 FileData 应使用固定保守成本: %d", got)
	}
}

func TestContextTokenEstimatesCombineTextModalityAndFunctionParts(t *testing.T) {
	content := &genai.Content{Parts: []*genai.Part{
		genai.NewPartFromText("hello"),
		{InlineData: &genai.Blob{MIMEType: "image/png", Data: []byte{1}}},
		{FunctionCall: &genai.FunctionCall{Name: "read_file", Args: map[string]any{"path": "/tmp/a"}}},
	}}
	want := EstimateTextTokens("hello") + imageModalityBaseTokens + imageModalityChunkTokens + 256
	if got := EstimateContentTokens(content); got != want {
		t.Fatalf("多模态/文本/函数分项应可解释相加: got=%d want=%d", got, want)
	}
}

func TestDynamicContextBudgetAndBudgetErrorAreExplicit(t *testing.T) {
	if got := DynamicContextBudget(1000, 200, 100, 500); got != 200 {
		t.Fatalf("dynamic budget=%d", got)
	}
	if got := DynamicContextBudget(1000, 200, 100, 800); got != -100 {
		t.Fatalf("P0 超窗时 dynamic budget=%d", got)
	}
	err := &ContextBudgetError{Model: "demo", ContextWindow: 100, Estimated: 90, OutputReserve: 20, SafetyReserve: 10}
	if !errors.Is(err, ErrContextBudgetExceeded) || !strings.Contains(err.Error(), "context_window=100") {
		t.Fatalf("budget error=%v", err)
	}
}

func TestTrimContextHistoryPreservesStructuralMessages(t *testing.T) {
	request := &adkmodel.LLMRequest{Contents: []*genai.Content{
		genai.NewContentFromText(strings.Repeat("old ", 80), genai.RoleModel),
		genai.NewContentFromText(strings.Repeat("recent ", 40), genai.RoleModel),
		{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{Name: "read_file", ID: "call-1"}}}},
		genai.NewContentFromText("keep this user request", genai.RoleUser),
	}}
	manifest := ContextManifest{ContextWindow: 100, OutputReserve: 10, SafetyReserve: 10, EstimatedInput: 200}
	removed := TrimContextHistory(request, manifest)
	if len(removed) == 0 || len(request.Contents) >= 4 {
		t.Fatalf("应裁剪旧历史: removed=%#v contents=%d", removed, len(request.Contents))
	}
	foundFunction := false
	foundUser := false
	for _, content := range request.Contents {
		if content == nil {
			continue
		}
		if content.Parts != nil && content.Parts[0] != nil && content.Parts[0].FunctionCall != nil {
			foundFunction = true
		}
		if content.Role == genai.RoleUser && TextFromContent(content) != "keep this user request" {
			t.Fatal("最新用户消息不能被裁剪")
		}
		if content.Role == genai.RoleUser {
			foundUser = true
		}
	}
	if !foundFunction || !foundUser {
		t.Fatalf("结构化消息未保留: function=%v user=%v", foundFunction, foundUser)
	}
}
