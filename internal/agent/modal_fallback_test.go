package agent

import (
	"context"
	"errors"
	"iter"
	"strings"
	"sync"
	"testing"

	"Abot/internal/provider"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

func TestKernelImageFallbackDowngradesBeforePrimaryRequest(t *testing.T) {
	primaryModel := provider.Model{ID: "primary-model", DisplayName: "主模型", Enabled: true, ContextWindow: 8192}
	primaryProfile := provider.DefaultCapabilities(provider.Provider{ID: "primary", Protocol: provider.ProtocolOpenAICompatible}, primaryModel)
	primaryProfile.Images = provider.Support{State: provider.SupportUnsupported, Source: "probe", Reason: "image input rejected"}
	primaryModel.Capabilities = &primaryProfile

	fallbackModel := provider.Model{ID: "vision-model", DisplayName: "视觉模型", Enabled: true, ContextWindow: 8192}
	fallbackProfile := provider.DefaultCapabilities(provider.Provider{ID: "fallback", Protocol: provider.ProtocolOpenAICompatible}, fallbackModel)
	fallbackModel.Capabilities = &fallbackProfile
	repo := &kernelTestRepository{providers: map[string]provider.Provider{
		"primary":  {ID: "primary", Name: "主模型", Protocol: provider.ProtocolOpenAICompatible, Models: []provider.Model{primaryModel}},
		"fallback": {ID: "fallback", Name: "视觉模型", Protocol: provider.ProtocolOpenAICompatible, Models: []provider.Model{fallbackModel}},
	}}
	adapter := &modalFallbackTestAdapter{models: make(map[string]*modalFallbackTestLLM)}
	registry, err := provider.NewRegistry(context.Background(), repo, map[provider.Protocol]provider.Adapter{
		provider.ProtocolOpenAICompatible: adapter,
	})
	if err != nil {
		t.Fatalf("创建 fallback registry 失败: %v", err)
	}
	var usage map[string]any
	var visible string
	kernel, err := NewKernel(Config{
		AppName: "modal-fallback-test", SessionService: session.InMemoryService(), Providers: registry,
		RuntimeConfigResolver: func(context.Context, string, string, string) (RuntimeOptions, error) {
			return RuntimeOptions{
				AIEnabled: true, ProviderID: "primary", ModelID: "primary-model", Instruction: "只回复测试结果。",
				ModalFallbackEnabled: true, ModalFallbackProviderID: "fallback", ModalFallbackVisionModel: "vision-model",
			}, nil
		},
		ModalFallbackUsageObserver:  func(_ context.Context, _ string, metadata map[string]any) { usage = metadata },
		ModalFallbackOutputObserver: func(_ context.Context, _ string, text string, _ map[string]any) { visible = text },
	})
	if err != nil {
		t.Fatalf("创建 fallback Kernel 失败: %v", err)
	}
	data := []byte("fake image bytes")
	result := collectKernelEvents(kernel.Run(context.Background(), ChatRequest{
		UserID: "user-1", InvocationID: "invocation-image-fallback", SessionID: "session-image-fallback",
		Message: "请分析图片", Attachments: []Attachment{{Name: "screen.png", MIMEType: "image/png", Data: data}},
	}))
	if result.err != nil {
		t.Fatalf("图片降级后主模型运行失败: %v", result.err)
	}
	if result.text != "primary reply" {
		t.Fatalf("主模型最终回复=%q", result.text)
	}

	primaryLLM := adapter.model("primary", "primary-model")
	fallbackLLM := adapter.model("fallback", "vision-model")
	primaryRequests := primaryLLM.requestsSnapshot()
	fallbackRequests := fallbackLLM.requestsSnapshot()
	if len(primaryRequests) == 0 || len(fallbackRequests) == 0 {
		t.Fatalf("主模型/fallback 模型调用缺失: primary=%d fallback=%d", len(primaryRequests), len(fallbackRequests))
	}
	if requestHasInlineData(primaryRequests[0]) {
		t.Fatal("主模型不应收到不支持的图片 inline data")
	}
	if !requestHasInlineData(fallbackRequests[0]) || !requestHasData(fallbackRequests[0], data) {
		t.Fatal("fallback 模型应收到图片原始 bytes")
	}
	if !strings.Contains(requestText(primaryRequests[0]), "转述") {
		t.Fatalf("主模型请求缺少 fallback 可见文字: %q", requestText(primaryRequests[0]))
	}
	if usage["scope"] != "modal_fallback" || usage["provider_id"] != "fallback" || usage["model_id"] != "vision-model" || usage["modality"] != "image" {
		t.Fatalf("fallback usage metadata 错误: %#v", usage)
	}
	if _, ok := usage["result_digest"]; !ok {
		t.Fatalf("fallback usage 应包含结果 digest: %#v", usage)
	}
	if !strings.Contains(visible, "转述") || !strings.Contains(visible, "测试截图") {
		t.Fatalf("fallback 结果未通过用户可见输出边界发布: %q", visible)
	}
}

func TestModalFallbackMissingConfigurationKeepsTurnRunnable(t *testing.T) {
	kernel := &Kernel{}
	request := &ChatRequest{Message: "继续处理", Attachments: []Attachment{{Name: "photo.png", MIMEType: "image/png", Data: []byte{1, 2, 3}}}}
	profile := provider.ModelCapabilityProfile{Images: provider.Support{State: provider.SupportUnknown}}
	if err := kernel.applyModalFallback(context.Background(), request, provider.ResolvedModel{}, profile, RuntimeOptions{}); err != nil {
		t.Fatalf("未配置 fallback 不应阻塞 turn: %v", err)
	}
	if len(request.Attachments) != 0 || !strings.Contains(request.Message, "已继续处理本轮文字请求") {
		t.Fatalf("未配置 fallback 的降级结果错误: %#v", request)
	}
}

func TestModalFallbackProviderFailureKeepsTurnRunnable(t *testing.T) {
	primaryModel := provider.Model{ID: "primary-model", DisplayName: "主模型", Enabled: true, ContextWindow: 8192}
	primaryProfile := provider.DefaultCapabilities(provider.Provider{ID: "primary", Protocol: provider.ProtocolOpenAICompatible}, primaryModel)
	primaryProfile.Images = provider.Support{State: provider.SupportUnsupported, Source: "probe"}
	primaryModel.Capabilities = &primaryProfile
	fallbackModel := provider.Model{ID: "vision-model", DisplayName: "视觉模型", Enabled: true, ContextWindow: 8192}
	repo := &kernelTestRepository{providers: map[string]provider.Provider{
		"primary":  {ID: "primary", Name: "主模型", Protocol: provider.ProtocolOpenAICompatible, Models: []provider.Model{primaryModel}},
		"fallback": {ID: "fallback", Name: "视觉模型", Protocol: provider.ProtocolOpenAICompatible, Models: []provider.Model{fallbackModel}},
	}}
	adapter := &modalFallbackTestAdapter{models: make(map[string]*modalFallbackTestLLM), failFallback: true}
	registry, err := provider.NewRegistry(context.Background(), repo, map[provider.Protocol]provider.Adapter{provider.ProtocolOpenAICompatible: adapter})
	if err != nil {
		t.Fatalf("创建失败降级 registry 失败: %v", err)
	}
	kernel, err := NewKernel(Config{
		AppName: "modal-fallback-failure-test", SessionService: session.InMemoryService(), Providers: registry,
		RuntimeConfigResolver: func(context.Context, string, string, string) (RuntimeOptions, error) {
			return RuntimeOptions{
				AIEnabled: true, ProviderID: "primary", ModelID: "primary-model", Instruction: "只回复测试结果。",
				ModalFallbackEnabled: true, ModalFallbackProviderID: "fallback", ModalFallbackVisionModel: "vision-model",
			}, nil
		},
	})
	if err != nil {
		t.Fatalf("创建失败降级 Kernel 失败: %v", err)
	}
	result := collectKernelEvents(kernel.Run(context.Background(), ChatRequest{
		UserID: "user-1", InvocationID: "invocation-image-fallback-failure", SessionID: "session-image-fallback-failure",
		Message: "请分析图片", Attachments: []Attachment{{Name: "broken.png", MIMEType: "image/png", Data: []byte("fake image")}},
	}))
	if result.err != nil || result.text != "primary reply" {
		t.Fatalf("降级 provider 失败时主模型仍应完成本轮: %#v", result)
	}
	primaryRequests := adapter.model("primary", "primary-model").requestsSnapshot()
	fallbackRequests := adapter.model("fallback", "vision-model").requestsSnapshot()
	if len(primaryRequests) != 1 || len(fallbackRequests) != 1 {
		t.Fatalf("降级失败后的调用次数错误: primary=%d fallback=%d", len(primaryRequests), len(fallbackRequests))
	}
	if !strings.Contains(requestText(primaryRequests[0]), "转述模型不可用或未返回可读结果") {
		t.Fatalf("主模型缺少可解释的降级说明: %q", requestText(primaryRequests[0]))
	}
}

func TestKernelNativeImageSupportSkipsFallback(t *testing.T) {
	primaryModel := provider.Model{ID: "primary-model", DisplayName: "主模型", Enabled: true, ContextWindow: 8192}
	primaryProfile := provider.DefaultCapabilities(provider.Provider{ID: "primary", Protocol: provider.ProtocolOpenAICompatible}, primaryModel)
	primaryProfile.Images = provider.Support{State: provider.SupportSupported, Source: "probe"}
	primaryModel.Capabilities = &primaryProfile
	fallbackModel := provider.Model{ID: "vision-model", DisplayName: "视觉模型", Enabled: true, ContextWindow: 8192}
	repo := &kernelTestRepository{providers: map[string]provider.Provider{
		"primary":  {ID: "primary", Name: "主模型", Protocol: provider.ProtocolOpenAICompatible, Models: []provider.Model{primaryModel}},
		"fallback": {ID: "fallback", Name: "视觉模型", Protocol: provider.ProtocolOpenAICompatible, Models: []provider.Model{fallbackModel}},
	}}
	adapter := &modalFallbackTestAdapter{models: make(map[string]*modalFallbackTestLLM)}
	registry, err := provider.NewRegistry(context.Background(), repo, map[provider.Protocol]provider.Adapter{provider.ProtocolOpenAICompatible: adapter})
	if err != nil {
		t.Fatalf("创建原生图片 registry 失败: %v", err)
	}
	kernel, err := NewKernel(Config{
		AppName: "modal-native-image-test", SessionService: session.InMemoryService(), Providers: registry,
		RuntimeConfigResolver: func(context.Context, string, string, string) (RuntimeOptions, error) {
			return RuntimeOptions{
				AIEnabled: true, ProviderID: "primary", ModelID: "primary-model", Instruction: "只回复测试结果。",
				ModalFallbackEnabled: true, ModalFallbackProviderID: "fallback", ModalFallbackVisionModel: "vision-model",
			}, nil
		},
	})
	if err != nil {
		t.Fatalf("创建原生图片 Kernel 失败: %v", err)
	}
	result := collectKernelEvents(kernel.Run(context.Background(), ChatRequest{
		UserID: "user-1", SessionID: "session-native-image", Message: "请分析图片",
		Attachments: []Attachment{{Name: "screen.png", MIMEType: "image/png", Data: []byte("fake image")}},
	}))
	if result.err != nil || result.text != "primary reply" {
		t.Fatalf("原生图片路径运行失败: %#v", result)
	}
	primaryRequests := adapter.model("primary", "primary-model").requestsSnapshot()
	fallbackRequests := adapter.model("fallback", "vision-model").requestsSnapshot()
	if len(primaryRequests) != 1 || len(fallbackRequests) != 0 {
		t.Fatalf("原生图片不应调用 fallback: primary=%d fallback=%d", len(primaryRequests), len(fallbackRequests))
	}
	if !requestHasData(primaryRequests[0], []byte("fake image")) {
		t.Fatal("原生图片路径的主模型应收到图片 bytes")
	}
}

func TestModelRequirementsClassifiesAttachmentModalities(t *testing.T) {
	tests := []struct {
		name       string
		attachment Attachment
		images     bool
		audio      bool
		inputFiles bool
	}{
		{name: "image", attachment: Attachment{MIMEType: "image/png"}, images: true},
		{name: "audio", attachment: Attachment{MIMEType: "audio/ogg"}, audio: true},
		{name: "text", attachment: Attachment{MIMEType: "text/plain"}, inputFiles: true},
		{name: "pdf", attachment: Attachment{MIMEType: "application/pdf"}, inputFiles: true},
		{name: "empty mime", attachment: Attachment{Name: "unknown.bin"}, inputFiles: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := modelRequirements(nil, []Attachment{test.attachment}, 0)
			if got.RequiresImages != test.images || got.RequiresAudio != test.audio || got.RequiresFiles != test.inputFiles {
				t.Fatalf("附件能力归类=%#v", got)
			}
		})
	}
}

func requestHasInlineData(request *adkmodel.LLMRequest) bool {
	if request == nil {
		return false
	}
	for _, content := range request.Contents {
		if content == nil {
			continue
		}
		for _, part := range content.Parts {
			if part != nil && part.InlineData != nil {
				return true
			}
		}
	}
	return false
}

func requestHasData(request *adkmodel.LLMRequest, expected []byte) bool {
	if request == nil {
		return false
	}
	for _, content := range request.Contents {
		if content == nil {
			continue
		}
		for _, part := range content.Parts {
			if part != nil && part.InlineData != nil && string(part.InlineData.Data) == string(expected) {
				return true
			}
		}
	}
	return false
}

func requestText(request *adkmodel.LLMRequest) string {
	if request == nil {
		return ""
	}
	var builder strings.Builder
	for _, content := range request.Contents {
		if content == nil {
			continue
		}
		for _, part := range content.Parts {
			if part != nil {
				builder.WriteString(part.Text)
			}
		}
	}
	return builder.String()
}

type modalFallbackTestAdapter struct {
	mu           sync.Mutex
	models       map[string]*modalFallbackTestLLM
	failFallback bool
}

func (a *modalFallbackTestAdapter) BuildModel(_ context.Context, p provider.Provider, modelID string) (adkmodel.LLM, error) {
	return a.model(p.ID, modelID), nil
}

func (a *modalFallbackTestAdapter) TestConnection(context.Context, provider.Provider) (provider.ProbeResult, error) {
	return provider.ProbeResult{Message: "ok"}, nil
}

func (a *modalFallbackTestAdapter) DiscoverModels(context.Context, provider.Provider) ([]provider.Model, error) {
	return nil, nil
}

func (a *modalFallbackTestAdapter) model(providerID, modelID string) *modalFallbackTestLLM {
	a.mu.Lock()
	defer a.mu.Unlock()
	key := providerID + "/" + modelID
	if value := a.models[key]; value != nil {
		return value
	}
	value := &modalFallbackTestLLM{name: key, fail: a.failFallback && strings.HasPrefix(key, "fallback/")}
	a.models[key] = value
	return value
}

type modalFallbackTestLLM struct {
	mu       sync.Mutex
	name     string
	fail     bool
	requests []*adkmodel.LLMRequest
}

func (m *modalFallbackTestLLM) Name() string { return m.name }

func (m *modalFallbackTestLLM) GenerateContent(_ context.Context, request *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	m.mu.Lock()
	m.requests = append(m.requests, request)
	m.mu.Unlock()
	text := "primary reply"
	if strings.HasPrefix(m.name, "fallback/") {
		text = "图片中包含一张测试截图"
	}
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		if m.fail {
			yield(nil, errors.New("fallback upstream unavailable"))
			return
		}
		yield(&adkmodel.LLMResponse{Content: genai.NewContentFromText(text, genai.RoleModel)}, nil)
	}
}

func (m *modalFallbackTestLLM) requestsSnapshot() []*adkmodel.LLMRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]*adkmodel.LLMRequest(nil), m.requests...)
}
