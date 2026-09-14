package artifact

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode/utf8"
)

// This file implements bounded, metadata-only extraction. Every extractor reads
// a capped number of bytes from the immutable object, never writes to disk and
// never inflates an archive beyond a fixed budget, so a hostile attachment
// cannot turn a preview request into a decompression bomb.
//
// Extraction deliberately does not parse document bodies (PDF text, image
// pixels, archive members). Those need heavyweight parsers whose attack surface
// is not justified by a single-user local deployment; the bounded metadata below
// is enough to tell a user what an attachment is and whether it is readable.

const (
	// MaxExtractionBytes bounds the persisted JSON projection. It also keeps the
	// result inside the artifact metadata budget.
	MaxExtractionBytes = 12 << 10
	// maxExtractionReadBytes caps how much of one object an extractor may read.
	maxExtractionReadBytes = 8 << 20
	// maxArchiveEntries bounds the reported member list of an archive.
	maxArchiveEntries = 200
	// maxArchiveNameBytes bounds one reported member name.
	maxArchiveNameBytes = 512
	// maxArchiveNameBudget bounds the total bytes of reported member names. Without it a
	// hostile archive could push the projection past MaxExtractionBytes and turn a
	// preview request into an error instead of a truncated listing.
	maxArchiveNameBudget = 4 << 10
	// maxArchiveInMemoryBytes is the largest object a zip central directory may
	// be read from. Larger objects keep metadata-only extraction.
	maxArchiveInMemoryBytes = 8 << 20
	// maxGzipInflateBytes caps a trial inflate of a gzip member.
	maxGzipInflateBytes = 1 << 20
	// maxExpansionRatio flags a suspicious uncompressed/compressed ratio.
	maxExpansionRatio = 1000
	// maxPDFScanBytes caps the PDF header scan.
	maxPDFScanBytes = 1 << 20
	// maxTextPreviewBytes keeps the text preview inside MaxExtractionBytes even
	// after JSON escaping, which can expand newlines and non-ASCII text.
	maxTextPreviewBytes = 6 << 10
)

// ExtractionKind identifies which bounded projection was produced.
type ExtractionKind string

const (
	// ExtractionMetadata is the honest fallback: identity only, no preview.
	ExtractionMetadata ExtractionKind = "metadata"
	ExtractionText     ExtractionKind = "text"
	ExtractionImage    ExtractionKind = "image"
	ExtractionPDF      ExtractionKind = "pdf"
	ExtractionArchive  ExtractionKind = "archive"
)

// ExtractionVersion is the extractor contract version. It travels with every
// stored projection so a future change can be detected instead of silently
// reusing a result produced by an older parser.
const ExtractionVersion = "1"

// ExtractionMetadataKey is the artifact metadata key that stores the bounded
// projection. The value never contains object keys, credentials or raw bytes.
const ExtractionMetadataKey = "extraction"

type ImageExtraction struct {
	Format string `json:"format"`
	Width  int    `json:"width,omitempty"`
	Height int    `json:"height,omitempty"`
}

type PDFExtraction struct {
	Version   string `json:"version,omitempty"`
	Pages     int    `json:"pages,omitempty"`
	Encrypted bool   `json:"encrypted,omitempty"`
}

type ArchiveExtraction struct {
	Format string `json:"format"`
	// Entries is the number of members observed inside the read budget. It is
	// not a claim about the whole archive when Truncated is set.
	Entries           int      `json:"entries"`
	TotalUncompressed int64    `json:"total_uncompressed,omitempty"`
	Truncated         bool     `json:"truncated,omitempty"`
	Names             []string `json:"names,omitempty"`
	// SuspiciousPaths lists members whose names escape the archive root, which
	// is what a zip-slip payload looks like. Nothing is ever extracted, so this
	// is disclosure rather than protection.
	SuspiciousPaths []string `json:"suspicious_paths,omitempty"`
}

// Extraction is the persisted, bounded projection of one artifact.
type Extraction struct {
	Kind         ExtractionKind     `json:"kind"`
	Extractor    string             `json:"extractor"`
	Version      string             `json:"version"`
	SourceDigest string             `json:"source_digest"`
	SourceSize   int64              `json:"source_size"`
	Preview      string             `json:"preview,omitempty"`
	Truncated    bool               `json:"truncated,omitempty"`
	Image        *ImageExtraction   `json:"image,omitempty"`
	PDF          *PDFExtraction     `json:"pdf,omitempty"`
	Archive      *ArchiveExtraction `json:"archive,omitempty"`
	Warnings     []string           `json:"warnings,omitempty"`
	ExtractedAt  time.Time          `json:"extracted_at"`
}

// StoredExtraction returns the persisted projection together with whether it is
// still valid for the current object identity.
func (item Artifact) StoredExtraction() (Extraction, bool) {
	if len(item.Metadata) == 0 {
		return Extraction{}, false
	}
	raw, ok := item.Metadata[ExtractionMetadataKey]
	if !ok {
		return Extraction{}, false
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return Extraction{}, false
	}
	var extraction Extraction
	if err := json.Unmarshal(encoded, &extraction); err != nil {
		return Extraction{}, false
	}
	if strings.TrimSpace(extraction.Extractor) == "" || extraction.Version != ExtractionVersion {
		return Extraction{}, false
	}
	if extraction.SourceDigest != item.Digest || extraction.SourceSize != item.Size {
		return Extraction{}, false
	}
	return extraction, true
}

// Extract produces (and persists) the bounded projection of one artifact. It is
// idempotent: a stored projection made by the current extractor version for the
// same object identity is returned as-is.
func (s *Service) Extract(ctx context.Context, userID, id string) (Extraction, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	item, err := s.Get(ctx, userID, id)
	if err != nil {
		return Extraction{}, err
	}
	if item.Status == StatusQuarantined {
		return Extraction{}, ErrQuarantined
	}
	if item.Status != StatusReady {
		return Extraction{}, fmt.Errorf("%w: artifact 当前状态为 %s", ErrConflict, item.Status)
	}
	if existing, ok := item.StoredExtraction(); ok {
		return existing, nil
	}
	extraction, err := s.buildExtraction(ctx, item)
	if err != nil {
		return Extraction{}, err
	}
	stored, err := s.persistExtraction(ctx, item, extraction)
	if err != nil {
		return Extraction{}, err
	}
	return stored, nil
}

func (s *Service) buildExtraction(ctx context.Context, item Artifact) (Extraction, error) {
	extraction := Extraction{
		Kind: ExtractionMetadata, Extractor: "bounded-metadata", Version: ExtractionVersion,
		SourceDigest: item.Digest, SourceSize: item.Size, ExtractedAt: s.now().UTC(),
	}
	if item.Size <= 0 {
		extraction.Warnings = append(extraction.Warnings, "对象为空，没有可提取的内容")
		return extraction, nil
	}
	switch {
	case isTextMIMEType(item.MIMEType):
		return s.extractText(ctx, item, extraction)
	case isImageMIMEType(item.MIMEType):
		return s.extractImage(ctx, item, extraction)
	case isPDFMIMEType(item.MIMEType):
		return s.extractPDF(ctx, item, extraction)
	case isArchiveMIMEType(item.MIMEType):
		return s.extractArchive(ctx, item, extraction)
	default:
		extraction.Warnings = append(extraction.Warnings, "该格式只提供元数据，不做内容提取")
		return extraction, nil
	}
}

// readObject reads at most limit bytes from the head of the immutable object.
func (s *Service) readObject(ctx context.Context, item Artifact, limit int64) ([]byte, error) {
	if limit <= 0 || limit > maxExtractionReadBytes {
		limit = maxExtractionReadBytes
	}
	reader, _, err := s.store.Open(ctx, item.StorageKey, ByteRange{})
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrObjectMissing
		}
		return nil, err
	}
	defer func() { _ = reader.Close() }()
	data, err := io.ReadAll(io.LimitReader(reader, limit))
	if err != nil {
		return nil, fmt.Errorf("读取 artifact 对象失败: %w", err)
	}
	return data, nil
}

func (s *Service) extractText(ctx context.Context, item Artifact, extraction Extraction) (Extraction, error) {
	data, err := s.readObject(ctx, item, int64(maxTextPreviewBytes))
	if err != nil {
		return Extraction{}, err
	}
	extraction.Kind = ExtractionText
	extraction.Extractor = "text-preview"
	extraction.Truncated = item.Size > int64(len(data))
	if !utf8.Valid(data) {
		// A declared text type that is not valid UTF-8 is reported as binary
		// instead of being shown as mojibake.
		extraction.Kind = ExtractionMetadata
		extraction.Extractor = "bounded-metadata"
		extraction.Truncated = false
		extraction.Warnings = append(extraction.Warnings, "内容不是合法 UTF-8，按二进制处理")
		return extraction, nil
	}
	extraction.Preview = string(data)
	return extraction, nil
}

func (s *Service) extractImage(ctx context.Context, item Artifact, extraction Extraction) (Extraction, error) {
	data, err := s.readObject(ctx, item, 64<<10)
	if err != nil {
		return Extraction{}, err
	}
	image, ok := parseImageHeader(data)
	if !ok {
		extraction.Warnings = append(extraction.Warnings, "无法从文件头识别图片尺寸")
		return extraction, nil
	}
	extraction.Kind = ExtractionImage
	extraction.Extractor = "image-header"
	extraction.Image = image
	return extraction, nil
}

func (s *Service) extractPDF(ctx context.Context, item Artifact, extraction Extraction) (Extraction, error) {
	data, err := s.readObject(ctx, item, maxPDFScanBytes)
	if err != nil {
		return Extraction{}, err
	}
	if !bytes.HasPrefix(data, []byte("%PDF-")) {
		extraction.Warnings = append(extraction.Warnings, "缺少 PDF 文件头，拒绝按 PDF 解析")
		return extraction, nil
	}
	pdf := &PDFExtraction{}
	// The version lives in the first line ("%PDF-1.7"); splitting on whitespace
	// would pick up the first body token instead.
	firstLine := string(data[:min(len(data), 16)])
	if index := strings.IndexAny(firstLine, "\r\n"); index >= 0 {
		firstLine = firstLine[:index]
	}
	pdf.Version = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(firstLine), "%PDF-"))
	pdf.Encrypted = bytes.Contains(data, []byte("/Encrypt"))
	pdf.Pages = scanPDFPageCount(data)
	extraction.Kind = ExtractionPDF
	extraction.Extractor = "pdf-header"
	extraction.PDF = pdf
	extraction.Warnings = append(extraction.Warnings, "页数来自有界头部扫描，加密或增量更新的 PDF 可能不准确")
	if item.Size > int64(len(data)) {
		extraction.Warnings = append(extraction.Warnings, "仅扫描了 PDF 的前 1 MiB")
	}
	return extraction, nil
}

// scanPDFPageCount looks for the largest /Count value in the scanned window.
// It is a heuristic by design: the caller is told so in the warnings.
func scanPDFPageCount(data []byte) int {
	pages := 0
	for offset := 0; ; {
		index := bytes.Index(data[offset:], []byte("/Count"))
		if index < 0 {
			return pages
		}
		position := offset + index + len("/Count")
		for position < len(data) && (data[position] == ' ' || data[position] == '\t' || data[position] == '\r' || data[position] == '\n') {
			position++
		}
		value := 0
		digits := 0
		for position < len(data) && data[position] >= '0' && data[position] <= '9' && digits < 9 {
			value = value*10 + int(data[position]-'0')
			position++
			digits++
		}
		if digits > 0 && value > pages {
			pages = value
		}
		offset = offset + index + len("/Count")
	}
}

func (s *Service) extractArchive(ctx context.Context, item Artifact, extraction Extraction) (Extraction, error) {
	data, err := s.readObject(ctx, item, maxExtractionReadBytes)
	if err != nil {
		return Extraction{}, err
	}
	switch {
	case bytes.HasPrefix(data, []byte("PK\x03\x04")) || bytes.HasPrefix(data, []byte("PK\x05\x06")):
		return s.extractZip(ctx, item, extraction, data)
	case len(data) >= 262 && bytes.Equal(data[257:262], []byte("ustar")):
		result, err := extractTar(data, extraction)
		if err == nil && item.Size > int64(len(data)) {
			result.Warnings = append(result.Warnings, fmt.Sprintf("仅扫描了归档的前 %d MiB", maxExtractionReadBytes>>20))
		}
		return result, err
	case bytes.HasPrefix(data, []byte{0x1f, 0x8b}):
		return s.extractGzip(ctx, item, extraction)
	default:
		extraction.Warnings = append(extraction.Warnings, "无法识别归档格式")
		return extraction, nil
	}
}

func (s *Service) extractZip(ctx context.Context, item Artifact, extraction Extraction, data []byte) (Extraction, error) {
	if item.Size > maxArchiveInMemoryBytes {
		extraction.Warnings = append(extraction.Warnings, fmt.Sprintf("归档超过 %d MiB，仅识别格式、不解析条目", maxArchiveInMemoryBytes>>20))
		extraction.Kind = ExtractionArchive
		extraction.Extractor = "archive-header"
		extraction.Archive = &ArchiveExtraction{Format: "zip"}
		return extraction, nil
	}
	// The central directory lives at the end of the file, so a zip listing needs
	// the whole object in memory. It is bounded by maxArchiveInMemoryBytes.
	full := data
	if item.Size > int64(len(full)) {
		complete, err := s.readObject(ctx, item, item.Size)
		if err != nil {
			return Extraction{}, err
		}
		full = complete
	}
	reader, err := zip.NewReader(bytes.NewReader(full), int64(len(full)))
	if err != nil {
		extraction.Warnings = append(extraction.Warnings, "zip 中央目录不可读，仅识别格式")
		extraction.Kind = ExtractionArchive
		extraction.Extractor = "archive-header"
		extraction.Archive = &ArchiveExtraction{Format: "zip"}
		return extraction, nil
	}
	archive := &ArchiveExtraction{Format: "zip", Entries: len(reader.File)}
	var compressedTotal int64
	nameBytes := 0
	for index, file := range reader.File {
		if index >= maxArchiveEntries || nameBytes >= maxArchiveNameBudget {
			archive.Truncated = true
			break
		}
		name := boundedArchiveName(file.Name)
		nameBytes += len(name)
		archive.Names = append(archive.Names, name)
		if archiveEscapesRoot(file.Name) {
			archive.SuspiciousPaths = append(archive.SuspiciousPaths, name)
		}
		archive.TotalUncompressed += int64(file.UncompressedSize64)
		compressedTotal += int64(file.CompressedSize64)
	}
	if compressedTotal > 0 && archive.TotalUncompressed/compressedTotal > maxExpansionRatio {
		extraction.Warnings = append(extraction.Warnings, "压缩比异常，已停止解析（可能是解压炸弹）")
		archive.Names = nil
		archive.SuspiciousPaths = nil
		archive.Truncated = true
	}
	if len(archive.SuspiciousPaths) > 0 {
		extraction.Warnings = append(extraction.Warnings, "归档包含可逃逸根目录的成员名；Abot 不会解压内容")
	}
	extraction.Kind = ExtractionArchive
	extraction.Extractor = "archive-index"
	extraction.Archive = archive
	if archive.Entries > len(archive.Names) && !archive.Truncated {
		extraction.Warnings = append(extraction.Warnings, "条目清单已截断")
	}
	return extraction, nil
}

func extractTar(data []byte, extraction Extraction) (Extraction, error) {
	archive := &ArchiveExtraction{Format: "tar"}
	reader := tar.NewReader(bytes.NewReader(data))
	nameBytes := 0
	for {
		header, err := reader.Next()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				extraction.Warnings = append(extraction.Warnings, "tar 头部不可读，条目清单可能不完整")
			}
			break
		}
		archive.Entries++
		if len(archive.Names) >= maxArchiveEntries || nameBytes >= maxArchiveNameBudget {
			archive.Truncated = true
			continue
		}
		name := boundedArchiveName(header.Name)
		nameBytes += len(name)
		archive.Names = append(archive.Names, name)
		if archiveEscapesRoot(header.Name) {
			archive.SuspiciousPaths = append(archive.SuspiciousPaths, name)
		}
		if header.Size > 0 {
			archive.TotalUncompressed += header.Size
		}
	}
	if len(archive.SuspiciousPaths) > 0 {
		extraction.Warnings = append(extraction.Warnings, "归档包含可逃逸根目录的成员名；Abot 不会解压内容")
	}
	extraction.Kind = ExtractionArchive
	extraction.Extractor = "archive-index"
	extraction.Archive = archive
	return extraction, nil
}

func (s *Service) extractGzip(ctx context.Context, item Artifact, extraction Extraction) (Extraction, error) {
	archive := &ArchiveExtraction{Format: "gzip", Entries: 1}
	reader, _, err := s.store.Open(ctx, item.StorageKey, ByteRange{})
	if err != nil {
		return Extraction{}, err
	}
	defer func() { _ = reader.Close() }()
	gz, err := gzip.NewReader(reader)
	if err != nil {
		extraction.Warnings = append(extraction.Warnings, "gzip 头部不可读，仅识别格式")
		extraction.Kind = ExtractionArchive
		extraction.Extractor = "archive-header"
		extraction.Archive = archive
		return extraction, nil
	}
	defer func() { _ = gz.Close() }()
	if name := boundedArchiveName(gz.Name); name != "" {
		// A gzip member is one stream; its optional original name is useful
		// context and is bounded before it reaches the projection.
		archive.Names = []string{name}
	}
	// The trial inflate is what proves the member is actually readable, and the
	// read budget is what stops a decompression bomb.
	inflated, inflateErr := io.ReadAll(io.LimitReader(gz, maxGzipInflateBytes+1))
	archive.TotalUncompressed = int64(len(inflated))
	if inflateErr != nil {
		extraction.Warnings = append(extraction.Warnings, "gzip 内容在解压预算内不可读")
	} else if int64(len(inflated)) > maxGzipInflateBytes {
		archive.Truncated = true
		archive.TotalUncompressed = maxGzipInflateBytes
		extraction.Warnings = append(extraction.Warnings, fmt.Sprintf("仅试解压了前 %d MiB", maxGzipInflateBytes>>20))
	}
	if item.Size > 0 && archive.TotalUncompressed/item.Size > maxExpansionRatio {
		extraction.Warnings = append(extraction.Warnings, "压缩比异常，可能是解压炸弹")
	}
	extraction.Kind = ExtractionArchive
	extraction.Extractor = "archive-header"
	extraction.Archive = archive
	return extraction, nil
}

// boundedArchiveName keeps a member name short and free of control characters so
// a hostile archive cannot smuggle terminal escapes or unbounded text into the
// projection.
func boundedArchiveName(name string) string {
	name = strings.TrimSpace(name)
	var builder strings.Builder
	for _, character := range name {
		if character < 0x20 || character == 0x7f {
			continue
		}
		builder.WriteRune(character)
		if builder.Len() >= maxArchiveNameBytes {
			break
		}
	}
	return builder.String()
}

// archiveEscapesRoot reports the classic zip-slip / tar traversal shapes.
func archiveEscapesRoot(name string) bool {
	normalized := strings.ReplaceAll(strings.TrimSpace(name), "\\", "/")
	if strings.HasPrefix(normalized, "/") {
		return true
	}
	if len(normalized) >= 2 && normalized[1] == ':' {
		return true
	}
	for _, part := range strings.Split(normalized, "/") {
		if part == ".." {
			return true
		}
	}
	return false
}

// parseImageHeader reads only the fixed header of the supported formats.
func parseImageHeader(data []byte) (*ImageExtraction, bool) {
	switch {
	case len(data) >= 24 && bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")) && bytes.Equal(data[12:16], []byte("IHDR")):
		return &ImageExtraction{Format: "png", Width: int(binary.BigEndian.Uint32(data[16:20])), Height: int(binary.BigEndian.Uint32(data[20:24]))}, true
	case len(data) >= 10 && (bytes.HasPrefix(data, []byte("GIF87a")) || bytes.HasPrefix(data, []byte("GIF89a"))):
		return &ImageExtraction{Format: "gif", Width: int(binary.LittleEndian.Uint16(data[6:8])), Height: int(binary.LittleEndian.Uint16(data[8:10]))}, true
	case len(data) >= 12 && bytes.HasPrefix(data, []byte("RIFF")) && bytes.Equal(data[8:12], []byte("WEBP")):
		return parseWebPHeader(data)
	case len(data) >= 4 && data[0] == 0xff && data[1] == 0xd8:
		return parseJPEGHeader(data)
	default:
		return nil, false
	}
}

func parseWebPHeader(data []byte) (*ImageExtraction, bool) {
	if len(data) < 30 {
		return nil, false
	}
	switch string(data[12:16]) {
	case "VP8X":
		width := int(uint32(data[24])|uint32(data[25])<<8|uint32(data[26])<<16) + 1
		height := int(uint32(data[27])|uint32(data[28])<<8|uint32(data[29])<<16) + 1
		return &ImageExtraction{Format: "webp", Width: width, Height: height}, true
	case "VP8L":
		if len(data) < 25 {
			return nil, false
		}
		bits := binary.LittleEndian.Uint32(data[21:25])
		return &ImageExtraction{Format: "webp", Width: int(bits&0x3fff) + 1, Height: int((bits>>14)&0x3fff) + 1}, true
	case "VP8 ":
		if len(data) < 30 {
			return nil, false
		}
		return &ImageExtraction{Format: "webp", Width: int(binary.LittleEndian.Uint16(data[26:28]) & 0x3fff), Height: int(binary.LittleEndian.Uint16(data[28:30]) & 0x3fff)}, true
	default:
		return nil, false
	}
}

// parseJPEGHeader walks the marker chain until a start-of-frame segment reveals
// the dimensions. The scan is bounded by the caller's read limit.
func parseJPEGHeader(data []byte) (*ImageExtraction, bool) {
	position := 2
	for position+9 < len(data) {
		if data[position] != 0xff {
			position++
			continue
		}
		marker := data[position+1]
		if marker == 0xd8 || marker == 0x01 || (marker >= 0xd0 && marker <= 0xd7) {
			position += 2
			continue
		}
		length := int(binary.BigEndian.Uint16(data[position+2 : position+4]))
		if length < 2 {
			return nil, false
		}
		// SOF0..SOF15 except the DHT/JPG/DAC markers carry the frame geometry.
		if marker >= 0xc0 && marker <= 0xcf && marker != 0xc4 && marker != 0xc8 && marker != 0xcc {
			if position+9 >= len(data) {
				return nil, false
			}
			height := int(binary.BigEndian.Uint16(data[position+5 : position+7]))
			width := int(binary.BigEndian.Uint16(data[position+7 : position+9]))
			return &ImageExtraction{Format: "jpeg", Width: width, Height: height}, true
		}
		position += 2 + length
	}
	return nil, false
}

func (s *Service) persistExtraction(ctx context.Context, item Artifact, extraction Extraction) (Extraction, error) {
	encoded, err := json.Marshal(extraction)
	if err != nil {
		return Extraction{}, fmt.Errorf("编码提取结果失败: %w", err)
	}
	if len(encoded) > MaxExtractionBytes {
		return Extraction{}, fmt.Errorf("%w: 提取结果超过 %d 字节上限", ErrInvalidRequest, MaxExtractionBytes)
	}
	var stored map[string]any
	if err := json.Unmarshal(encoded, &stored); err != nil {
		return Extraction{}, fmt.Errorf("编码提取结果失败: %w", err)
	}
	updated := item
	updated.Metadata = cloneMetadata(item.Metadata)
	if updated.Metadata == nil {
		updated.Metadata = make(map[string]any, 1)
	}
	updated.Metadata[ExtractionMetadataKey] = stored
	updated.Version = item.Version + 1
	updated.UpdatedAt = s.now().UTC()
	if err := updated.Validate(); err != nil {
		return Extraction{}, err
	}
	if err := s.repository.Update(ctx, updated, int64(item.Version)); err != nil {
		if errors.Is(err, ErrConflict) {
			// A concurrent request persisted a projection first. Prefer the
			// stored one so every caller observes the same immutable result.
			if current, getErr := s.repository.Get(ctx, item.ID); getErr == nil {
				if existing, ok := current.StoredExtraction(); ok {
					return existing, nil
				}
			}
		}
		return Extraction{}, err
	}
	return extraction, nil
}

func isTextMIMEType(value string) bool {
	value = normalizeMIME(value)
	return strings.HasPrefix(value, "text/") || value == "application/json" || value == "application/xml" ||
		value == "application/javascript" || value == "application/x-yaml" || value == "application/yaml" ||
		strings.HasSuffix(value, "+json") || strings.HasSuffix(value, "+xml")
}

func isImageMIMEType(value string) bool {
	return strings.HasPrefix(normalizeMIME(value), "image/")
}

func isPDFMIMEType(value string) bool {
	return normalizeMIME(value) == "application/pdf"
}

func isArchiveMIMEType(value string) bool {
	switch normalizeMIME(value) {
	case "application/zip", "application/x-zip-compressed", "application/x-tar", "application/gzip", "application/x-gzip":
		return true
	default:
		return false
	}
}
