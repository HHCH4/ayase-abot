package agent

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"unicode"

	"Abot/internal/document"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
)

const (
	sessionAttachmentsToolName    = "read_session_attachments"
	maxSessionAttachmentResults   = 8
	maxSessionAttachmentReadBytes = 8 << 10
)

// sessionAttachmentsArgs intentionally selects only by a bounded index or
// metadata query. It does not accept an Artifact ID, which prevents a model
// from turning this convenience tool into an arbitrary object-store reader.
type sessionAttachmentsArgs struct {
	Mode  string `json:"mode,omitempty" jsonschema:"summary（默认）只返回元数据；original 返回文本附件原文或解析后的 PDF/Word/Excel 正文"`
	Query string `json:"query,omitempty" jsonschema:"按附件名称、MIME 类型或类型筛选；留空返回全部附件"`
	Index int    `json:"index,omitempty" jsonschema:"1-based 附件序号；留空或 0 返回所有匹配附件"`
}

type sessionAttachmentsResult struct {
	Mode        string                    `json:"mode"`
	Count       int                       `json:"count"`
	Attachments []sessionAttachmentResult `json:"attachments"`
}

type sessionAttachmentResult struct {
	Index            int    `json:"index"`
	Name             string `json:"name,omitempty"`
	Kind             string `json:"kind,omitempty"`
	MIMEType         string `json:"mime_type"`
	Size             int64  `json:"size"`
	Digest           string `json:"digest,omitempty"`
	Summary          string `json:"summary"`
	Content          string `json:"content,omitempty"`
	ContentTruncated bool   `json:"content_truncated,omitempty"`
}

// newSessionAttachmentsTool creates a per-invocation tool so its resolver is
// bound to the Runtime invocation that supplied the current attachments.
func newSessionAttachmentsTool(userID, invocationID string, list AttachmentListResolver, resolver AttachmentResolver) (tool.Tool, error) {
	if list == nil {
		return nil, errors.New("当前会话附件 resolver 未装配")
	}
	return functiontool.New(functiontool.Config{
		Name:        sessionAttachmentsToolName,
		Description: "读取当前 Runtime 任务收到的附件。默认返回附件元数据；需要查看文本附件时使用 mode=original。只能读取当前任务的附件，不能传入任意 Artifact ID。",
	}, func(ctx adkagent.Context, args sessionAttachmentsArgs) (sessionAttachmentsResult, error) {
		return readSessionAttachments(ctx, userID, invocationID, list, resolver, args)
	})
}

func readSessionAttachments(ctx context.Context, userID, invocationID string, list AttachmentListResolver, resolver AttachmentResolver, args sessionAttachmentsArgs) (sessionAttachmentsResult, error) {
	mode := strings.ToLower(strings.TrimSpace(args.Mode))
	if mode == "" {
		mode = "summary"
	}
	if mode != "summary" && mode != "original" {
		return sessionAttachmentsResult{}, errors.New("mode 只能是 summary 或 original")
	}
	if args.Index < 0 {
		return sessionAttachmentsResult{}, errors.New("附件序号不能为负数")
	}
	items, err := list(ctx, strings.TrimSpace(userID), strings.TrimSpace(invocationID))
	if err != nil {
		return sessionAttachmentsResult{}, fmt.Errorf("读取当前任务附件失败: %w", err)
	}
	query := strings.ToLower(strings.TrimSpace(args.Query))
	result := sessionAttachmentsResult{Mode: mode}
	for sourceIndex, attachment := range items {
		if attachment.Ref == nil || len(attachment.Data) > 0 {
			return sessionAttachmentsResult{}, errors.New("当前任务附件不是受保护的 Artifact ref")
		}
		ref := *attachment.Ref
		if err := validateAttachmentRef(ref); err != nil {
			return sessionAttachmentsResult{}, fmt.Errorf("第 %d 个附件 ref 无效", sourceIndex+1)
		}
		name := strings.TrimSpace(attachment.Name)
		if name == "" {
			name = strings.TrimSpace(ref.Name)
		}
		kind := strings.TrimSpace(ref.Kind)
		mimeType := strings.ToLower(strings.TrimSpace(attachment.MIMEType))
		if mimeType == "" {
			mimeType = strings.ToLower(strings.TrimSpace(ref.MIMEType))
		}
		if mimeType == "" {
			mimeType = "application/octet-stream"
		}
		if query != "" && !sessionAttachmentMatches(query, name, kind, mimeType) {
			continue
		}
		if args.Index > 0 && args.Index != sourceIndex+1 {
			continue
		}
		item := sessionAttachmentResult{
			Index: sourceIndex + 1, Name: safeAttachmentLabel(name), Kind: safeAttachmentLabel(kind),
			MIMEType: mimeType, Size: ref.Size, Digest: shortAttachmentDigest(ref.Digest),
			Summary: sessionAttachmentSummary(name, kind, mimeType, ref),
		}
		if mode == "original" && document.IsDocumentAttachment(name, mimeType) {
			content, truncated, readErr := readSessionAttachmentDocument(ctx, strings.TrimSpace(userID), ref, name, mimeType, args.Query, resolver)
			if readErr != nil {
				item.Summary += "；文档正文暂不可解析"
			} else {
				item.Content, item.ContentTruncated = content, truncated
				item.Summary += "；已执行本地文档解析"
			}
		} else if mode == "original" && sessionAttachmentIsText(name, mimeType) {
			content, truncated, readErr := readSessionAttachmentText(ctx, strings.TrimSpace(userID), ref, resolver)
			if readErr != nil {
				item.Summary += "；原文暂不可读取"
			} else {
				item.Content, item.ContentTruncated = content, truncated
			}
		} else if mode == "original" {
			item.Summary += "；二进制附件仅提供摘要"
		}
		result.Attachments = append(result.Attachments, item)
		if len(result.Attachments) >= maxSessionAttachmentResults {
			break
		}
	}
	result.Count = len(result.Attachments)
	return result, nil
}

func sessionAttachmentMatches(query, name, kind, mimeType string) bool {
	for _, value := range []string{name, kind, mimeType} {
		if strings.Contains(strings.ToLower(value), query) {
			return true
		}
	}
	return false
}

func sessionAttachmentSummary(name, kind, mimeType string, ref AttachmentRef) string {
	label := attachmentDescription(name, mimeType, ref.Size)
	if kind != "" {
		label += "，类型=" + safeAttachmentLabel(kind)
	}
	if digest := shortAttachmentDigest(ref.Digest); digest != "" {
		label += "，digest=" + digest
	}
	return label
}

func shortAttachmentDigest(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= 24 {
		return value
	}
	return value[:24] + "…"
}

func sessionAttachmentIsText(name, mimeType string) bool {
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(mimeType)), "text/") {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(mimeType)) {
	case "application/json", "application/xml", "application/yaml", "application/x-yaml", "application/javascript", "application/x-javascript", "text/csv":
		return true
	}
	switch strings.ToLower(filepath.Ext(name)) {
	case ".txt", ".md", ".markdown", ".json", ".yaml", ".yml", ".xml", ".csv", ".log", ".go", ".js", ".ts", ".tsx", ".py", ".java", ".rs", ".sql", ".html", ".css":
		return true
	default:
		return false
	}
}

func readSessionAttachmentText(ctx context.Context, userID string, ref AttachmentRef, resolver AttachmentResolver) (string, bool, error) {
	if resolver == nil {
		return "", false, errors.New("附件 resolver 未装配")
	}
	data, err := readAttachmentRef(ctx, userID, ref, resolver)
	if err != nil {
		return "", false, err
	}
	truncated := len(data) > maxSessionAttachmentReadBytes
	if truncated {
		data = data[:maxSessionAttachmentReadBytes]
	}
	// NUL is not useful in a model-facing text preview and can confuse some
	// downstream serializers; other UTF-8/control content remains user data.
	data = []byte(strings.Map(func(r rune) rune {
		if r == 0 || unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t' {
			return -1
		}
		return r
	}, string(data)))
	return string(data), truncated, nil
}

// readSessionAttachmentDocument 为模型提供带来源信息的文档正文，同时继续受工具输出上限约束。
func readSessionAttachmentDocument(ctx context.Context, userID string, ref AttachmentRef, name, mimeType, query string, resolver AttachmentResolver) (string, bool, error) {
	if resolver == nil {
		return "", false, errors.New("附件 resolver 未装配")
	}
	data, err := readAttachmentRef(ctx, userID, ref, resolver)
	if err != nil {
		return "", false, err
	}
	parsed, err := document.Parse(ctx, name, mimeType, data)
	if err != nil {
		return "", false, err
	}
	content, truncated := parsed.Render(query, maxSessionAttachmentReadBytes)
	return content, truncated, nil
}
