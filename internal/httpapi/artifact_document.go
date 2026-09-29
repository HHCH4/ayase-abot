package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"unicode/utf8"

	"Abot/internal/artifact"
	"Abot/internal/document"
)

const maxDocumentViewBytes int64 = 10 << 20

const (
	maxDocumentViewBlocks    = 512
	maxDocumentViewBlockText = 8 << 10
)

// documentArtifactResponse 是解析层的只读投影。它只返回有界文本块和图片
// 元数据，不把原始二进制或临时图片数据暴露给 WebUI，避免预览接口变成
// 第二条附件下载和内存放大路径。
type documentArtifactResponse struct {
	Artifact  artifact.ArtifactRef            `json:"artifact"`
	Name      string                          `json:"name"`
	MIMEType  string                          `json:"mime_type"`
	Kind      string                          `json:"kind"`
	Parsed    bool                            `json:"parsed"`
	PageCount int                             `json:"page_count,omitempty"`
	Truncated bool                            `json:"truncated"`
	Rendered  string                          `json:"rendered,omitempty"`
	Blocks    []documentArtifactBlockResponse `json:"blocks,omitempty"`
	Images    []documentArtifactImageResponse `json:"images,omitempty"`
	Warnings  []string                        `json:"warnings,omitempty"`
}

type documentArtifactBlockResponse struct {
	Locator string `json:"locator"`
	Text    string `json:"text"`
}

type documentArtifactImageResponse struct {
	Locator  string `json:"locator"`
	Name     string `json:"name,omitempty"`
	MIMEType string `json:"mime_type"`
	Size     int    `json:"size"`
	Digest   string `json:"digest"`
}

func (s *Server) getArtifactDocument(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireArtifacts()
	if err != nil {
		writeError(writer, err)
		return
	}
	userID, userErr := artifactUserID(request, "")
	if userErr != nil {
		writeError(writer, userErr)
		return
	}
	reader, _, item, err := service.Open(request.Context(), userID, request.PathValue("id"), artifact.ByteRange{})
	if err != nil {
		writeError(writer, err)
		return
	}
	defer reader.Close()

	// Parser 自身也有上限；这里在读入内存前再次限制，确保 HTTP 预览不会
	// 因为一个合法但很大的 Artifact 绕过解析层边界。
	data, err := io.ReadAll(io.LimitReader(reader, maxDocumentViewBytes+1))
	if err != nil {
		writeError(writer, fmt.Errorf("读取文档 Artifact 失败: %w", err))
		return
	}
	if int64(len(data)) > maxDocumentViewBytes || item.Size > maxDocumentViewBytes {
		writeError(writer, fmt.Errorf("%w: 文档结构化预览上限为 %d bytes", artifact.ErrTooLarge, maxDocumentViewBytes))
		return
	}
	parsed, err := document.Parse(request.Context(), item.Name, item.MIMEType, data)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			writeError(writer, err)
			return
		}
		writeError(writer, fmt.Errorf("解析文档 Artifact 失败: %w", err))
		return
	}

	response := documentArtifactResponse{
		Artifact:  item.Ref(),
		Name:      parsed.Name,
		MIMEType:  parsed.MIMEType,
		Kind:      parsed.Kind,
		Parsed:    parsed.Parsed,
		PageCount: parsed.PageCount,
		Truncated: parsed.Truncated,
		Warnings:  append([]string(nil), parsed.Warnings...),
	}
	response.Rendered, response.Truncated = parsed.Render("", document.DefaultRenderBytes)
	if len(parsed.Blocks) > maxDocumentViewBlocks {
		parsed.Blocks = parsed.Blocks[:maxDocumentViewBlocks]
		response.Truncated = true
	}
	response.Blocks = make([]documentArtifactBlockResponse, 0, len(parsed.Blocks))
	for _, block := range parsed.Blocks {
		text, truncated := limitDocumentViewText(block.Text, maxDocumentViewBlockText)
		response.Blocks = append(response.Blocks, documentArtifactBlockResponse{Locator: block.Locator, Text: text})
		response.Truncated = response.Truncated || truncated
	}
	response.Images = make([]documentArtifactImageResponse, 0, len(parsed.Images))
	for _, image := range parsed.Images {
		digest := sha256.Sum256(image.Data)
		response.Images = append(response.Images, documentArtifactImageResponse{
			Locator: image.Locator, Name: image.Name, MIMEType: image.MIMEType,
			Size: len(image.Data), Digest: "sha256:" + hex.EncodeToString(digest[:]),
		})
	}
	writeJSON(writer, http.StatusOK, response)
}

func limitDocumentViewText(value string, maxBytes int) (string, bool) {
	if maxBytes <= 0 || len([]byte(value)) <= maxBytes {
		return value, false
	}
	data := []byte(value[:maxBytes])
	for len(data) > 0 && !utf8.Valid(data) {
		data = data[:len(data)-1]
	}
	return string(data) + "\n[内容已截断]", true
}
