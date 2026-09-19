package openai

import (
	"bytes"
	"image"
	_ "image/gif"
	"image/jpeg"
	"image/png"
	"log/slog"
	"path/filepath"
	"strings"
)

const (
	// DeepSeek 的内联单图上限是 32 MiB；Abot 入口目前把单个附件限制在 10 MiB。
	// 这里按 Abot 入口上限的 80% 提前处理，给 Base64 和请求 JSON 预留余量。
	imageCompressionTriggerBytes int64 = 8 << 20
	// 压缩目标取入口上限的 70%，避免压缩后仍贴着限制发送。
	imageCompressionTargetBytes int64 = 7 << 20
	// 解码图片前先限制像素数量，避免小体积高压缩比图片造成过大的内存分配。
	imageCompressionMaxPixels int64 = 32 << 20
)

// preparedImage 是发送给 OpenAI 兼容线路前的图片数据；它不会回写 Artifact 或用户原始附件。
type preparedImage struct {
	data     []byte
	mimeType string
	name     string
}

// prepareInlineImage 统一识别实际图片格式，并在接近附件大小上限时压缩副本。
func prepareInlineImage(name, declaredMIME string, data []byte) preparedImage {
	prepared := preparedImage{data: data, mimeType: strings.TrimSpace(declaredMIME), name: strings.TrimSpace(name)}
	if len(data) == 0 {
		return prepared
	}

	// DeepSeek 按文件内容识别格式；若能从文件头识别出标准图片类型，就优先使用实际类型。
	if detected := detectSupportedImageMIME(data); detected != "" {
		prepared.mimeType = detected
	}
	if !isImageMIME(prepared.mimeType) || int64(len(data)) < imageCompressionTriggerBytes {
		return prepared
	}

	compressed, compressedMIME, compressedName, ok := compressInlineImage(prepared.name, prepared.mimeType, data)
	if !ok || len(compressed) >= len(data) {
		return prepared
	}
	prepared.data = compressed
	prepared.mimeType = compressedMIME
	prepared.name = compressedName
	slog.Info("模型发送前压缩图片", "原始字节数", len(data), "发送字节数", len(compressed), "原始MIME", declaredMIME, "发送MIME", compressedMIME, "文件名", compressedName)
	return prepared
}

// detectSupportedImageMIME 根据文件签名识别 DeepSeek 文档列出的四种图片格式。
func detectSupportedImageMIME(data []byte) string {
	if len(data) >= 8 && bytes.Equal(data[:8], []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}) {
		return "image/png"
	}
	if len(data) >= 3 && data[0] == 0xff && data[1] == 0xd8 && data[2] == 0xff {
		return "image/jpeg"
	}
	if len(data) >= 6 && (bytes.Equal(data[:6], []byte("GIF87a")) || bytes.Equal(data[:6], []byte("GIF89a"))) {
		return "image/gif"
	}
	if len(data) >= 12 && bytes.Equal(data[:4], []byte("RIFF")) && bytes.Equal(data[8:12], []byte("WEBP")) {
		return "image/webp"
	}
	return ""
}

// compressInlineImage 只压缩可安全解码的静态图片；GIF/WebP 在标准库中没有完整编码器时保持原样。
func compressInlineImage(name, mimeType string, data []byte) ([]byte, string, string, bool) {
	if mimeType == "image/gif" || mimeType == "image/webp" {
		return nil, "", "", false
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || config.Width <= 0 || config.Height <= 0 || int64(config.Width)*int64(config.Height) > imageCompressionMaxPixels {
		return nil, "", "", false
	}
	decoded, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, "", "", false
	}

	// 先尝试无损 PNG 重编码，避免透明图片和截图文字因为有损转换变差。
	originalSize := len(data)
	if format == "png" {
		if encoded := encodePNG(decoded); len(encoded) > 0 && len(encoded) < len(data) {
			data = encoded
		}
		if imageHasTransparency(decoded) {
			if len(data) < originalSize {
				return data, "image/png", name, true
			}
			return nil, "", "", false
		}
	}

	// 不含透明通道的图片可以转成高质量 JPEG；逐级降低质量直到达到目标或不再变小。
	qualities := []int{92, 86, 80, 74, 68}
	best := []byte(nil)
	for _, quality := range qualities {
		encoded := encodeJPEG(decoded, quality)
		if len(encoded) == 0 || len(encoded) >= len(data) {
			continue
		}
		if len(best) == 0 || len(encoded) < len(best) {
			best = encoded
		}
		if int64(len(encoded)) <= imageCompressionTargetBytes {
			break
		}
	}
	if len(best) == 0 {
		if format == "png" && len(data) < originalSize {
			return data, "image/png", name, true
		}
		return nil, "", "", false
	}
	return best, "image/jpeg", jpegDisplayName(name), true
}

// encodePNG 用最高无损压缩等级重编码图片，减少未优化 PNG 的体积。
func encodePNG(value image.Image) []byte {
	var buffer bytes.Buffer
	encoder := png.Encoder{CompressionLevel: png.BestCompression}
	if err := encoder.Encode(&buffer, value); err != nil {
		return nil
	}
	return buffer.Bytes()
}

// encodeJPEG 用指定质量编码图片，保持较高 OCR 和视觉细节质量。
func encodeJPEG(value image.Image, quality int) []byte {
	var buffer bytes.Buffer
	if err := jpeg.Encode(&buffer, value, &jpeg.Options{Quality: quality}); err != nil {
		return nil
	}
	return buffer.Bytes()
}

// imageHasTransparency 检查是否存在透明像素，透明图片不能直接转 JPEG。
func imageHasTransparency(value image.Image) bool {
	bounds := value.Bounds()
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			_, _, _, alpha := value.At(x, y).RGBA()
			if alpha < 0xffff {
				return true
			}
		}
	}
	return false
}

// jpegDisplayName 同步更新展示文件名，避免上游把 JPEG 当成 PNG 继续处理。
func jpegDisplayName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "attachment.jpg"
	}
	ext := filepath.Ext(name)
	if ext == "" {
		return name + ".jpg"
	}
	return strings.TrimSuffix(name, ext) + ".jpg"
}
