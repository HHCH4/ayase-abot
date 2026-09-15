package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"iter"
	"strings"
	"testing"

	"Abot/internal/provider"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

func TestDurableAttachmentContentDefersBytesUntilProviderBoundary(t *testing.T) {
	data := []byte("private attachment bytes that must not enter the durable session")
	ref := testAttachmentRef("artifact-1", "notes.txt", "text/plain", data)
	ref.Preview = string(data)
	request := ChatRequest{
		UserID: "user-1", InvocationID: "invocation-1", Message: "请读取附件",
		Attachments: []Attachment{{Name: ref.Name, MIMEType: ref.MIMEType, Ref: &ref}},
	}
	content, err := referenceContentFromChatRequest(request)
	if err != nil {
		t.Fatalf("生成 durable 附件消息失败: %v", err)
	}
	if len(content.Parts) != 2 || content.Parts[1].InlineData != nil {
		t.Fatalf("持久化消息不应包含 inline bytes: %#v", content.Parts)
	}
	if strings.Contains(content.Parts[1].Text, string(data)) {
		t.Fatal("持久化附件描述意外包含附件正文")
	}
	encodedContent, err := json.Marshal(content)
	if err != nil {
		t.Fatalf("序列化 durable 附件消息失败: %v", err)
	}
	if bytes.Contains(encodedContent, data) {
		t.Fatal("Artifact ref 的 Preview 不应把附件正文带入 durable 消息")
	}
	if _, ok := content.Parts[1].PartMetadata[attachmentReferenceMetadataKey]; !ok {
		t.Fatal("持久化消息缺少附件 ref metadata")
	}

	var resolverUser string
	materializer := &attachmentMaterializingLLM{
		delegate: &attachmentTestLLM{}, userID: "user-1",
		resolver: func(_ context.Context, userID string, got AttachmentRef) (io.ReadCloser, error) {
			resolverUser = userID
			if got.ID != ref.ID || got.Digest != ref.Digest {
				return nil, errors.New("收到错误的 Artifact ref")
			}
			return io.NopCloser(bytes.NewReader(data)), nil
		},
	}
	materialized, err := materializer.materialize(context.Background(), &adkmodel.LLMRequest{Contents: []*genai.Content{content}})
	if err != nil {
		t.Fatalf("provider 边界 materialize 失败: %v", err)
	}
	if resolverUser != "user-1" {
		t.Fatalf("附件 resolver user=%q，期望 user-1", resolverUser)
	}
	if content.Parts[1].InlineData != nil {
		t.Fatal("materialize 不应修改 ADK durable 原始消息")
	}
	if len(materialized.Contents) != 1 || len(materialized.Contents[0].Parts) != 3 {
		t.Fatalf("provider 请求 parts=%d，期望描述文本+附件描述+inline data", len(materialized.Contents[0].Parts))
	}
	gotData := materialized.Contents[0].Parts[2].InlineData
	if gotData == nil || !bytes.Equal(gotData.Data, data) || gotData.MIMEType != "text/plain" {
		t.Fatalf("provider 未收到正确的附件 bytes: %#v", gotData)
	}
}

func TestDurableAttachmentMaterializerFailsClosedWithoutResolver(t *testing.T) {
	ref := testAttachmentRef("artifact-without-resolver", "note.txt", "text/plain", []byte("content"))
	content, err := referenceContentFromChatRequest(ChatRequest{
		UserID: "user-1", InvocationID: "invocation-1", Message: "读取",
		Attachments: []Attachment{{Name: ref.Name, MIMEType: ref.MIMEType, Ref: &ref}},
	})
	if err != nil {
		t.Fatalf("构造 durable 附件内容失败: %v", err)
	}
	materializer := &attachmentMaterializingLLM{delegate: &attachmentTestLLM{}}
	if _, err := materializer.materialize(context.Background(), &adkmodel.LLMRequest{Contents: []*genai.Content{content}}); err == nil {
		t.Fatal("没有 Artifact resolver 时不能静默把 ref 当作已 materialize 的附件")
	}
}

func TestKernelDurableAttachmentKeepsSessionMetadataOnly(t *testing.T) {
	data := []byte("session must never persist these private attachment bytes")
	ref := testAttachmentRef("artifact-kernel", "note.txt", "text/plain", data)
	model := provider.Model{ID: "demo-model", Enabled: true, ContextWindow: 8192}
	profile := provider.DefaultCapabilities(provider.Provider{ID: "demo", Protocol: provider.ProtocolOpenAICompatible}, model)
	profile.Images = provider.Support{State: provider.SupportSupported, Source: "test"}
	profile.InputFiles = provider.Support{State: provider.SupportSupported, Source: "test"}
	model.Capabilities = &profile
	repo := &kernelTestRepository{providers: map[string]provider.Provider{
		"demo": {ID: "demo", Name: "演示供应商", Protocol: provider.ProtocolOpenAICompatible, Models: []provider.Model{model}},
	}}
	adapter := &kernelTestAdapter{}
	registry, err := provider.NewRegistry(context.Background(), repo, map[provider.Protocol]provider.Adapter{provider.ProtocolOpenAICompatible: adapter})
	if err != nil {
		t.Fatalf("创建 durable attachment registry 失败: %v", err)
	}
	sessions := session.InMemoryService()
	resolverCalls := 0
	kernel, err := NewKernel(Config{
		AppName: "durable-attachment-kernel", SessionService: sessions, Providers: registry,
		AttachmentResolver: func(_ context.Context, userID string, got AttachmentRef) (io.ReadCloser, error) {
			resolverCalls++
			if userID != "user-1" || got.ID != ref.ID {
				return nil, errors.New("附件 resolver 收到错误边界")
			}
			return io.NopCloser(bytes.NewReader(data)), nil
		},
	})
	if err != nil {
		t.Fatalf("创建 durable attachment Kernel 失败: %v", err)
	}
	result := collectKernelEvents(kernel.Run(context.Background(), ChatRequest{
		UserID: "user-1", InvocationID: "invocation-kernel-attachment", SessionID: "session-kernel-attachment",
		ProviderID: "demo", ModelID: "demo-model", Message: "请读取附件", Attachments: []Attachment{{Name: ref.Name, MIMEType: ref.MIMEType, Ref: &ref}},
	}))
	if result.err != nil || result.text != "测试回复" {
		t.Fatalf("durable attachment Kernel 运行失败: %#v", result)
	}
	if resolverCalls == 0 || adapter.model == nil || adapter.model.request == nil || !requestContainsAttachmentBytes(adapter.model.request, data) {
		t.Fatalf("provider 边界未 materialize 原始附件: resolver_calls=%d model=%#v", resolverCalls, adapter.model)
	}
	stored, err := sessions.Get(context.Background(), &session.GetRequest{AppName: "durable-attachment-kernel", UserID: "user-1", SessionID: "session-kernel-attachment"})
	if err != nil {
		t.Fatalf("读取 durable attachment 会话失败: %v", err)
	}
	for event := range stored.Session.Events().All() {
		encoded, marshalErr := json.Marshal(event)
		if marshalErr != nil {
			t.Fatalf("序列化 durable attachment 会话事件失败: %v", marshalErr)
		}
		if bytes.Contains(encoded, data) {
			t.Fatalf("Session/Event 写入了附件原始 bytes: %s", encoded)
		}
	}
}

func TestReadSessionAttachmentsIsScopedAndBounded(t *testing.T) {
	note := []byte("第一行\n第二行")
	image := []byte{1, 2, 3, 4}
	noteRef := testAttachmentRef("note", "notes.txt", "text/plain", note)
	imageRef := testAttachmentRef("image", "screen.png", "image/png", image)
	items := []Attachment{
		{Name: noteRef.Name, MIMEType: noteRef.MIMEType, Ref: &noteRef},
		{Name: imageRef.Name, MIMEType: imageRef.MIMEType, Ref: &imageRef},
	}
	var gotUser, gotInvocation string
	list := func(_ context.Context, userID, invocationID string) ([]Attachment, error) {
		gotUser, gotInvocation = userID, invocationID
		return items, nil
	}
	resolverCalls := 0
	resolver := func(_ context.Context, _ string, ref AttachmentRef) (io.ReadCloser, error) {
		resolverCalls++
		switch ref.ID {
		case noteRef.ID:
			return io.NopCloser(bytes.NewReader(note)), nil
		case imageRef.ID:
			return io.NopCloser(bytes.NewReader(image)), nil
		default:
			return nil, errors.New("unknown ref")
		}
	}

	summary, err := readSessionAttachments(context.Background(), "user-1", "invocation-1", list, resolver, sessionAttachmentsArgs{})
	if err != nil {
		t.Fatalf("读取附件摘要失败: %v", err)
	}
	if gotUser != "user-1" || gotInvocation != "invocation-1" || summary.Count != 2 || resolverCalls != 0 {
		t.Fatalf("附件摘要边界错误: user=%q invocation=%q result=%#v resolver_calls=%d", gotUser, gotInvocation, summary, resolverCalls)
	}
	if summary.Attachments[0].Content != "" || !strings.Contains(summary.Attachments[0].Summary, "notes.txt") {
		t.Fatalf("summary 模式不应返回正文: %#v", summary.Attachments[0])
	}

	original, err := readSessionAttachments(context.Background(), "user-1", "invocation-1", list, resolver, sessionAttachmentsArgs{Mode: "original", Query: "notes"})
	if err != nil {
		t.Fatalf("读取文本附件原文失败: %v", err)
	}
	if original.Count != 1 || original.Attachments[0].Content != string(note) || original.Attachments[0].ContentTruncated || resolverCalls != 1 {
		t.Fatalf("original 模式结果错误: %#v resolver_calls=%d", original, resolverCalls)
	}

	imageOriginal, err := readSessionAttachments(context.Background(), "user-1", "invocation-1", list, resolver, sessionAttachmentsArgs{Mode: "original", Index: 2})
	if err != nil {
		t.Fatalf("读取二进制附件摘要失败: %v", err)
	}
	if imageOriginal.Count != 1 || imageOriginal.Attachments[0].Content != "" || !strings.Contains(imageOriginal.Attachments[0].Summary, "二进制附件仅提供摘要") {
		t.Fatalf("二进制附件不应伪装为原文: %#v", imageOriginal)
	}

	if _, err := readSessionAttachments(context.Background(), "user-1", "invocation-1", func(context.Context, string, string) ([]Attachment, error) {
		return []Attachment{{Data: []byte("inline")}}, nil
	}, resolver, sessionAttachmentsArgs{}); err == nil {
		t.Fatal("会话附件工具不应接受带 inline bytes 的 Runtime 附件")
	}
	tool, err := newSessionAttachmentsTool("user-1", "invocation-1", list, resolver)
	if err != nil || tool == nil || tool.Name() != sessionAttachmentsToolName {
		t.Fatalf("创建会话附件工具失败: tool=%v err=%v", tool, err)
	}
}

func testAttachmentRef(id, name, mimeType string, data []byte) AttachmentRef {
	sum := sha256.Sum256(data)
	return AttachmentRef{
		ID: id, Version: 1, Digest: "sha256:" + hex.EncodeToString(sum[:]), Kind: "input_attachment",
		MIMEType: mimeType, Size: int64(len(data)), Name: name,
	}
}

func requestContainsAttachmentBytes(request *adkmodel.LLMRequest, expected []byte) bool {
	if request == nil {
		return false
	}
	for _, content := range request.Contents {
		if content == nil {
			continue
		}
		for _, part := range content.Parts {
			if part != nil && part.InlineData != nil && bytes.Equal(part.InlineData.Data, expected) {
				return true
			}
		}
	}
	return false
}

type attachmentTestLLM struct{}

func (m *attachmentTestLLM) Name() string { return "attachment-test" }

func (m *attachmentTestLLM) GenerateContent(_ context.Context, _ *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		yield(&adkmodel.LLMResponse{Content: genai.NewContentFromText("ok", genai.RoleModel)}, nil)
	}
}
