package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"Abot/internal/provider"
	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

const (
	maxModalFallbackTextBytes = 16 << 10
	modalFallbackOutputTokens = 1024
)

// applyModalFallback resolves unsupported attachments before model
// requirements are negotiated. Native-capable refs remain refs; unsupported
// media becomes bounded text, so a file that a provider cannot accept never
// turns the whole chat turn into an upstream 400.
func (k *Kernel) applyModalFallback(ctx context.Context, request *ChatRequest, primary provider.ResolvedModel, profile provider.ModelCapabilityProfile, runtime RuntimeOptions) error {
	if request == nil || len(request.Attachments) == 0 {
		return nil
	}
	kept := make([]Attachment, 0, len(request.Attachments))
	var fallbackText []string
	for index, attachment := range request.Attachments {
		mimeType := strings.ToLower(strings.TrimSpace(attachment.MIMEType))
		if mimeType == "" && attachment.Ref != nil {
			mimeType = strings.ToLower(strings.TrimSpace(attachment.Ref.MIMEType))
		}
		if mimeType == "" {
			mimeType = "application/octet-stream"
		}
		if modalCapabilitySupported(mimeType, profile) {
			kept = append(kept, attachment)
			continue
		}

		modality, fallbackModel := modalFallbackTarget(mimeType, runtime)
		text := ""
		fallbackAttempted := runtime.ModalFallbackEnabled && fallbackModel != ""
		if fallbackAttempted {
			text, _ = k.runModalFallback(ctx, request, attachment, index, modality, fallbackModel, primary, runtime)
		}
		fallbackSucceeded := strings.TrimSpace(text) != ""
		if strings.TrimSpace(text) == "" {
			text = modalFallbackExplanation(attachment, mimeType, runtime.ModalFallbackEnabled, fallbackModel)
		}
		k.publishModalFallbackOutput(ctx, request.InvocationID, text, map[string]any{
			"modality": modality, "attachment_index": index + 1,
			"degraded": true, "fallback_attempted": fallbackAttempted, "fallback_succeeded": fallbackSucceeded,
		})
		fallbackText = append(fallbackText, text)
	}
	request.Attachments = kept
	if len(fallbackText) > 0 {
		addition := "附件处理结果：\n- " + strings.Join(fallbackText, "\n- ")
		if strings.TrimSpace(request.Message) == "" {
			request.Message = addition
		} else {
			request.Message = strings.TrimRight(request.Message, " \t\r\n") + "\n\n" + addition
		}
	}
	return nil
}

func modalCapabilitySupported(mimeType string, profile provider.ModelCapabilityProfile) bool {
	switch {
	case strings.HasPrefix(mimeType, "image/"):
		return supportStateUsable(profile.Images.State)
	case strings.HasPrefix(mimeType, "audio/"):
		return supportStateUsable(profile.Audio.State)
	default:
		return supportStateUsable(profile.InputFiles.State)
	}
}

func supportStateUsable(state provider.SupportState) bool {
	return state == provider.SupportSupported || state == provider.SupportDegraded
}

func modalFallbackTarget(mimeType string, runtime RuntimeOptions) (string, string) {
	if strings.HasPrefix(mimeType, "image/") {
		return "image", strings.TrimSpace(runtime.ModalFallbackVisionModel)
	}
	if strings.HasPrefix(mimeType, "audio/") {
		return "audio", strings.TrimSpace(runtime.ModalFallbackAudioModel)
	}
	return "file", ""
}

func modalFallbackExplanation(attachment Attachment, mimeType string, enabled bool, model string) string {
	name := "未命名附件"
	if attachment.Ref != nil && strings.TrimSpace(attachment.Ref.Name) != "" {
		name = attachment.Ref.Name
	} else if strings.TrimSpace(attachment.Name) != "" {
		name = attachment.Name
	}
	reason := "主模型未确认支持该附件类型"
	if !enabled {
		reason = "未启用多模态降级"
	} else if model == "" {
		reason = "未配置对应的转述模型"
	} else {
		reason = "转述模型不可用或未返回可读结果"
	}
	return fmt.Sprintf("%s（%s）：%s。已继续处理本轮文字请求。", safeAttachmentLabel(name), safeAttachmentLabel(mimeType), reason)
}

func (k *Kernel) runModalFallback(ctx context.Context, request *ChatRequest, attachment Attachment, index int, modality, modelID string, primary provider.ResolvedModel, runtime RuntimeOptions) (string, error) {
	providerID := strings.TrimSpace(runtime.ModalFallbackProviderID)
	if providerID == "" {
		providerID = strings.TrimSpace(primary.Provider.ID)
	}
	if k == nil || k.providers == nil {
		return "", errors.New("fallback provider registry 未装配")
	}
	resolved, err := k.providers.Resolve(ctx, providerID, modelID)
	if err != nil {
		return "", err
	}
	if resolved.LLM == nil {
		return "", errors.New("fallback 模型未装配")
	}
	data, err := k.materializeFallbackAttachment(ctx, request.UserID, attachment)
	if err != nil {
		return "", err
	}
	prompt := "请把这个附件转述成简洁、准确的中文文字，供另一个不支持该模态的对话模型继续回答。"
	if modality == "audio" {
		prompt = "请准确转写并概括这个音频附件的可辨识内容，输出简洁中文文字，供另一个不支持音频的对话模型继续回答。"
	} else if modality == "image" {
		prompt = "请描述这个图片附件中与用户问题可能相关的内容，输出简洁、准确的中文文字，供另一个不支持图片的对话模型继续回答。"
	}
	name := attachment.Name
	if attachment.Ref != nil && strings.TrimSpace(name) == "" {
		name = attachment.Ref.Name
	}
	content := genai.NewContentFromParts([]*genai.Part{
		{Text: prompt + "\n附件名称：" + safeAttachmentLabel(name)},
		{InlineData: &genai.Blob{Data: data, MIMEType: fallbackMIME(attachment), DisplayName: safeAttachmentLabel(name)}},
	}, genai.RoleUser)
	maxOutput := int32(modalFallbackOutputTokens)
	if resolved.Model.MaxOutputTokens > 0 && resolved.Model.MaxOutputTokens < int(maxOutput) {
		maxOutput = int32(resolved.Model.MaxOutputTokens)
	}
	llmRequest := &adkmodel.LLMRequest{Model: resolved.Model.ID, Config: &genai.GenerateContentConfig{MaxOutputTokens: maxOutput}, Contents: []*genai.Content{content}}
	var builder strings.Builder
	var usage map[string]any
	for response, responseErr := range resolved.LLM.GenerateContent(ctx, llmRequest, false) {
		if responseErr != nil {
			return "", responseErr
		}
		if response == nil {
			continue
		}
		if response.Content != nil {
			builder.WriteString(TextFromContent(response.Content))
		}
		if response.InputTranscription != nil && response.InputTranscription.Text != "" {
			builder.WriteString(response.InputTranscription.Text)
		}
		if response.OutputTranscription != nil && response.OutputTranscription.Text != "" {
			builder.WriteString(response.OutputTranscription.Text)
		}
		if response.UsageMetadata != nil {
			encoded, marshalErr := json.Marshal(response.UsageMetadata)
			if marshalErr == nil {
				_ = json.Unmarshal(encoded, &usage)
			}
		}
	}
	result := trimModalFallbackText(builder.String())
	if observer := k.modalFallbackObserver(); observer != nil {
		metadata := map[string]any{
			"scope": "modal_fallback", "provider_id": resolved.Provider.ID, "model_id": resolved.Model.ID,
			"modality": modality, "attachment_index": index + 1, "degraded": true, "result_bytes": len([]byte(result)),
		}
		if result != "" {
			sum := sha256.Sum256([]byte(result))
			metadata["result_digest"] = "sha256:" + hex.EncodeToString(sum[:])
		}
		if usage != nil {
			metadata["usage"] = usage
		}
		observer(ctx, request.InvocationID, metadata)
	}
	if result == "" {
		return "", errors.New("fallback 模型未返回文字")
	}
	return fmt.Sprintf("%s 转述：%s", safeAttachmentLabel(name), result), nil
}

func (k *Kernel) modalFallbackObserver() ModalFallbackUsageObserver {
	if k == nil {
		return nil
	}
	k.locksMu.Lock()
	defer k.locksMu.Unlock()
	return k.modalFallbackUsageObserver
}

func (k *Kernel) publishModalFallbackOutput(ctx context.Context, invocationID, text string, metadata map[string]any) {
	if k == nil || strings.TrimSpace(text) == "" {
		return
	}
	k.locksMu.Lock()
	observer := k.modalFallbackOutputObserver
	k.locksMu.Unlock()
	if observer == nil {
		return
	}
	observer(ctx, strings.TrimSpace(invocationID), trimModalFallbackText(text), metadata)
}

func (k *Kernel) materializeFallbackAttachment(ctx context.Context, userID string, attachment Attachment) ([]byte, error) {
	if attachment.Ref != nil {
		if k.attachmentResolver == nil {
			return nil, errors.New("fallback attachment resolver 未装配")
		}
		return readAttachmentRef(ctx, userID, *attachment.Ref, k.attachmentResolver)
	}
	if len(attachment.Data) == 0 || int64(len(attachment.Data)) > maxAttachmentMaterializeBytes {
		return nil, errors.New("fallback 附件内容为空或超出上限")
	}
	return append([]byte(nil), attachment.Data...), nil
}

func fallbackMIME(attachment Attachment) string {
	mimeType := strings.TrimSpace(attachment.MIMEType)
	if mimeType == "" && attachment.Ref != nil {
		mimeType = strings.TrimSpace(attachment.Ref.MIMEType)
	}
	if mimeType == "" {
		return "application/octet-stream"
	}
	return mimeType
}

func trimModalFallbackText(value string) string {
	value = strings.TrimSpace(value)
	if len([]byte(value)) <= maxModalFallbackTextBytes {
		return value
	}
	data := []byte(value[:maxModalFallbackTextBytes])
	for len(data) > 0 && !utf8.Valid(data) {
		data = data[:len(data)-1]
	}
	return strings.TrimSpace(string(data)) + "（转述已截断）"
}
