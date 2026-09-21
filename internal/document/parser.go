// Package document 负责把常见办公文档转换为带来源信息的文本块。
//
// 这个包只做本地、确定性的解析，不调用任何外部 AI，也不会修改原始
// Artifact。原始文件仍由 Artifact 负责保存，解析结果只在当前请求中作为
// 模型上下文使用。
package document

import (
	"archive/zip"
	"bytes"
	"compress/zlib"
	"context"
	"encoding/csv"
	"encoding/xml"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	pdf "github.com/ledongthuc/pdf"
	"github.com/xuri/excelize/v2"
)

const (
	// DefaultRenderBytes 限制一次送入模型的解析文本大小，防止长文档挤占整个上下文。
	DefaultRenderBytes = 96 << 10

	// maxParseInputBytes 与 Agent 的附件物化上限保持一致，避免解析层被单独调用时失去边界。
	maxParseInputBytes = 10 << 20
	// maxParsedBytes 是单个附件在内存中保留的解析文本上限，原始文件不会被这个值覆盖。
	maxParsedBytes = 512 << 10
	// maxParsedBlocks 限制极端文档产生过多小文本块。
	maxParsedBlocks = 4096

	maxPDFPages              = 200
	maxOOXMLMemberBytes      = 16 << 20
	maxOOXMLMembers          = 64
	maxXLSXSheets            = 64
	maxXLSXRowsPerSheet      = 2000
	maxXLSXNonEmptyCells     = 50000
	maxXLSXCellBytes         = 4096
	maxCSVRows               = 2000
	maxCSVCellBytes          = 4096
	maxWordTableCellBytes    = 8192
	maxDocumentWarningLength = 512
	// 内嵌图片也要有独立边界，避免一个文档中的大量图片把主进程内存吃满。
	maxEmbeddedImages          = 12
	maxEmbeddedImageBytes      = 8 << 20
	maxEmbeddedImageTotalBytes = 24 << 20
	maxPDFImageDimension       = 8192
	maxPDFDecodedImageBytes    = 16 << 20
)

// Block 是一个带有稳定来源定位信息的文本块。
// Locator 例如“PDF 第 3 页”“Word 表格 1 行 2”“Sheet1!A1:C1”。
type Block struct {
	Locator string
	Text    string
}

// Image 是文档内嵌的图片。图片只在当前请求的解析管线中暂存，调用方应在
// 完成视觉分析后丢弃 Data，不要把它写入会话历史。
type Image struct {
	Locator  string
	Name     string
	MIMEType string
	Data     []byte
}

// Result 是一次本地解析的结果。Blocks 是正文，Images 是待交给视觉子 Agent
// 分析的内嵌图片；两者都不改变 Artifact 中的原始文件。
type Result struct {
	Name      string
	MIMEType  string
	Kind      string
	Parsed    bool
	Truncated bool
	Blocks    []Block
	Images    []Image
	Warnings  []string

	parsedBytes int
	imageBytes  int
}

// IsDocumentAttachment 判断一个附件是否应该进入文档解析路径。
// 旧版 .doc/.xls 也返回 true，这样它们不会被误当作可直接阅读的二进制送给模型，
// 而是得到明确的“不支持该格式”的提示。
func IsDocumentAttachment(name, mimeType string) bool {
	return isDocumentKind(documentKind(name, mimeType))
}

// Parse 按文件类型执行本地解析。解析失败会保留在 Result.Warnings 中，
// 这样上层仍能把可理解的失败原因反馈给模型和用户，而不是把原始二进制冒充正文。
func Parse(ctx context.Context, name, mimeType string, data []byte) (Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	result := Result{
		Name:     strings.TrimSpace(name),
		MIMEType: normalizeMIME(mimeType),
		Kind:     documentKind(name, mimeType),
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if len(data) > maxParseInputBytes {
		result.addWarning("附件超过本地文档解析上限，未读取正文")
		return result, nil
	}
	if len(data) == 0 {
		result.addWarning("附件为空，未读取到正文")
		return result, nil
	}

	// 文件签名优先于上传时的 MIME，避免客户端错误标注导致错误解析器被调用。
	if sniffed := sniffDocumentKind(data); sniffed != "" {
		result.Kind = sniffed
	}

	switch result.Kind {
	case "pdf":
		parsePDF(ctx, data, &result)
	case "docx", "docm":
		parseDOCX(ctx, data, &result)
	case "xlsx", "xlsm":
		parseXLSX(ctx, data, &result)
	case "csv", "tsv":
		parseCSV(ctx, data, result.Kind == "tsv", &result)
	case "doc":
		result.addWarning("旧版 .doc 二进制格式未接入本地解析器，请先转换为 .docx 或 PDF")
	case "xls":
		result.addWarning("旧版 .xls 二进制格式未接入本地解析器，请先转换为 .xlsx 或 CSV")
	case "ole":
		result.addWarning("旧版 Office 二进制格式未接入本地解析器，请先转换为 .docx、.xlsx 或 PDF")
	default:
		result.addWarning("该附件类型没有可用的本地文档解析器")
	}
	return result, nil
}

// IsDocumentData 根据文件内容补充判断文档类型，解决平台把 Word/Excel 文件名
// 改成随机 ID、MIME 标成 application/octet-stream 时无法进入解析层的问题。
func IsDocumentData(name, mimeType string, data []byte) bool {
	return DetectDocumentKind(name, mimeType, data) != ""
}

// DetectDocumentKind 返回元数据或内容签名推断出的文档类型。元数据已明确时优先
// 使用元数据；只有平台没有给出可识别类型时才读取内容签名，避免覆盖合法扩展名。
func DetectDocumentKind(name, mimeType string, data []byte) string {
	kind := documentKind(name, mimeType)
	if isDocumentKind(kind) {
		return kind
	}
	return sniffDocumentKind(data)
}

// CanonicalMIMEType 把解析器识别出的类型转换成稳定 MIME，供能力协商和后续
// provider 边界共用；旧 Office 容器使用内部标记，仍会进入可解释的提示路径。
func CanonicalMIMEType(kind string) string {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "pdf":
		return "application/pdf"
	case "docx":
		return "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	case "docm":
		return "application/vnd.ms-word.document.macroenabled.12"
	case "doc":
		return "application/msword"
	case "xlsx":
		return "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	case "xlsm":
		return "application/vnd.ms-excel.sheet.macroenabled.12"
	case "xls":
		return "application/vnd.ms-excel"
	case "csv":
		return "text/csv"
	case "tsv":
		return "text/tab-separated-values"
	case "ole":
		return "application/x-ole-storage"
	default:
		return ""
	}
}

// isDocumentKind 集中维护可以进入本地解析层的格式，防止格式识别和能力协商
// 各自维护一套列表后再次出现分支不一致。
func isDocumentKind(kind string) bool {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "pdf", "docx", "docm", "doc", "xlsx", "xlsm", "xls", "csv", "tsv", "ole":
		return true
	default:
		return false
	}
}

// sniffDocumentKind 只读取文件签名和 ZIP 中央目录，不解压成员，避免为了识别格式
// 扩大内存占用；真正解析时仍由各格式解析器执行独立大小限制。
func sniffDocumentKind(data []byte) string {
	if len(data) == 0 {
		return ""
	}
	// PDF 通常从文件头开始；允许前面存在 UTF-8 BOM 或少量空白，兼容部分上传器。
	prefix := data
	if len(prefix) > 1024 {
		prefix = prefix[:1024]
	}
	if bytes.Contains(prefix, []byte("%PDF-")) {
		return "pdf"
	}
	if !bytes.HasPrefix(data, []byte("PK\x03\x04")) && !bytes.HasPrefix(data, []byte("PK\x05\x06")) && !bytes.HasPrefix(data, []byte("PK\x07\x08")) {
		if bytes.HasPrefix(data, []byte("\xd0\xcf\x11\xe0\xa1\xb1\x1a\xe1")) {
			return "ole"
		}
		return ""
	}
	archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return ""
	}
	hasWord := false
	hasSpreadsheet := false
	hasMacro := false
	for _, file := range archive.File {
		name := strings.ReplaceAll(file.Name, "\\", "/")
		switch {
		case name == "word/document.xml":
			hasWord = true
		case name == "xl/workbook.xml":
			hasSpreadsheet = true
		case name == "word/vbaProject.bin" || name == "xl/vbaProject.bin":
			hasMacro = true
		}
	}
	if hasWord {
		if hasMacro {
			return "docm"
		}
		return "docx"
	}
	if hasSpreadsheet {
		if hasMacro {
			return "xlsm"
		}
		return "xlsx"
	}
	return ""
}

// Render 将解析块按请求需要渲染为带来源标记的模型文本，并执行最终大小限制。
// query 只用于在文本过长时优先保留包含相关词的块；不匹配时仍会保留文档开头，
// 避免模型拿到空结果。
func (r Result) Render(query string, maxBytes int) (string, bool) {
	if maxBytes <= 0 {
		maxBytes = DefaultRenderBytes
	}
	var output strings.Builder
	write := func(value string) bool {
		if value == "" {
			return true
		}
		remaining := maxBytes - output.Len()
		if remaining <= 0 {
			return false
		}
		if len([]byte(value)) <= remaining {
			output.WriteString(value)
			return true
		}
		prefix, _ := limitUTF8(value, remaining)
		output.WriteString(prefix)
		return false
	}

	header := fmt.Sprintf("文件：%s（类型=%s，MIME=%s）\n", displayName(r.Name), r.Kind, displayName(r.MIMEType))
	complete := write(header)
	blocks := r.Blocks
	if matched := matchingBlocks(r.Blocks, query); len(matched) > 0 && len(matched) < len(r.Blocks) {
		blocks = matched
	}
	for _, block := range blocks {
		if !complete {
			break
		}
		locator := displayName(block.Locator)
		text := cleanExtractedText(block.Text)
		if text == "" {
			continue
		}
		complete = write(fmt.Sprintf("[来源：%s]\n%s\n", locator, text))
	}
	if len(r.Blocks) == 0 {
		complete = complete && write("[正文] 未提取到可用文本。\n")
	}
	if len(r.Images) > 0 && complete {
		complete = write(fmt.Sprintf("[图片] 已提取 %d 个内嵌图片，等待视觉子 Agent 分析。\n", len(r.Images)))
	}
	for _, warning := range r.Warnings {
		if !complete {
			break
		}
		complete = write("[解析提示] " + displayName(warning) + "\n")
	}
	truncated := !complete || r.Truncated
	if truncated && output.Len() < maxBytes {
		marker := "[解析提示] 内容已截断，原始文件仍保留在 Artifact 中。\n"
		if len([]byte(marker)) <= maxBytes-output.Len() {
			output.WriteString(marker)
		}
	}
	return output.String(), truncated
}

// documentKind 仅依据文件元数据判断解析路线；具体解析时仍会检查 PDF 签名和 ZIP/XML 内容。
func documentKind(name, mimeType string) string {
	mimeType = normalizeMIME(mimeType)
	switch mimeType {
	case "application/pdf":
		return "pdf"
	case "application/vnd.openxmlformats-officedocument.wordprocessingml.document":
		return "docx"
	case "application/vnd.ms-word.document.macroenabled.12":
		return "docm"
	case "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":
		return "xlsx"
	case "application/vnd.ms-excel.sheet.macroenabled.12":
		return "xlsm"
	case "application/msword":
		return "doc"
	case "application/vnd.ms-excel":
		return "xls"
	case "application/x-ole-storage":
		return "ole"
	case "text/csv", "application/csv":
		return "csv"
	case "text/tab-separated-values":
		return "tsv"
	}

	switch strings.ToLower(filepath.Ext(strings.TrimSpace(name))) {
	case ".pdf":
		return "pdf"
	case ".docx":
		return "docx"
	case ".docm":
		return "docm"
	case ".xlsx":
		return "xlsx"
	case ".xlsm":
		return "xlsm"
	case ".doc":
		return "doc"
	case ".xls":
		return "xls"
	case ".csv":
		return "csv"
	case ".tsv":
		return "tsv"
	default:
		return "unknown"
	}
}

func normalizeMIME(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if index := strings.IndexByte(value, ';'); index >= 0 {
		value = strings.TrimSpace(value[:index])
	}
	return value
}

func displayName(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "未命名"
	}
	value = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, value)
	limited, _ := limitUTF8(value, maxDocumentWarningLength)
	return limited
}

func (r *Result) addWarning(value string) {
	if r == nil {
		return
	}
	value = displayName(value)
	if value == "" {
		return
	}
	for _, existing := range r.Warnings {
		if existing == value {
			return
		}
	}
	r.Warnings = append(r.Warnings, value)
}

func (r *Result) addBlock(locator, text string) {
	if r == nil {
		return
	}
	text = cleanExtractedText(text)
	if text == "" {
		return
	}
	if len(r.Blocks) >= maxParsedBlocks {
		r.Truncated = true
		return
	}
	remaining := maxParsedBytes - r.parsedBytes
	if remaining <= 0 {
		r.Truncated = true
		return
	}
	limited, cut := limitUTF8(text, remaining)
	if limited == "" {
		r.Truncated = true
		return
	}
	r.Blocks = append(r.Blocks, Block{Locator: displayName(locator), Text: limited})
	r.parsedBytes += len([]byte(limited))
	if cut {
		r.Truncated = true
	}
}

func (r *Result) addImage(locator, name, mimeType string, data []byte) {
	if r == nil || len(data) == 0 {
		return
	}
	if len(r.Images) >= maxEmbeddedImages {
		r.Truncated = true
		r.addWarning(fmt.Sprintf("内嵌图片超过 %d 个，仅保留前面的图片", maxEmbeddedImages))
		return
	}
	if len(data) > maxEmbeddedImageBytes {
		r.Truncated = true
		r.addWarning(fmt.Sprintf("内嵌图片 %s 超过 %d MB，已跳过", displayName(name), maxEmbeddedImageBytes>>20))
		return
	}
	if r.imageBytes+len(data) > maxEmbeddedImageTotalBytes {
		r.Truncated = true
		r.addWarning(fmt.Sprintf("内嵌图片总大小超过 %d MB，仅保留前面的图片", maxEmbeddedImageTotalBytes>>20))
		return
	}
	mimeType = normalizeMIME(mimeType)
	if !strings.HasPrefix(mimeType, "image/") {
		r.addWarning(fmt.Sprintf("内嵌图片 %s 类型无法识别，已跳过", displayName(name)))
		return
	}
	copyData := append([]byte(nil), data...)
	r.Images = append(r.Images, Image{
		Locator:  displayName(locator),
		Name:     displayName(name),
		MIMEType: mimeType,
		Data:     copyData,
	})
	r.imageBytes += len(copyData)
}

func cleanExtractedText(value string) string {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")
	value = strings.Map(func(r rune) rune {
		if r == 0 || unicode.IsControl(r) && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, value)
	lines := strings.Split(value, "\n")
	for index, line := range lines {
		lines[index] = strings.TrimSpace(line)
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func matchingBlocks(blocks []Block, query string) []Block {
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		return nil
	}
	matched := make([]Block, 0)
	for _, block := range blocks {
		value := strings.ToLower(block.Locator + "\n" + block.Text)
		if strings.Contains(value, query) {
			matched = append(matched, block)
			continue
		}
		// 对英文、数字查询按空白和标点拆分；中文通常没有空格，整句匹配失败时保留全部块，
		// 这样不会因为简单分词误删中文正文。
		terms := strings.FieldsFunc(query, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsPunct(r) })
		if len(terms) > 1 {
			for _, term := range terms {
				if len([]rune(term)) >= 2 && strings.Contains(value, term) {
					matched = append(matched, block)
					break
				}
			}
		}
	}
	return matched
}

func limitUTF8(value string, maxBytes int) (string, bool) {
	if maxBytes <= 0 {
		return "", len(value) > 0
	}
	data := []byte(value)
	if len(data) <= maxBytes {
		return value, false
	}
	end := maxBytes
	for end > 0 && !utf8.Valid(data[:end]) {
		end--
	}
	return string(data[:end]), true
}

func parsePDF(ctx context.Context, data []byte, result *Result) {
	reader, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		result.addWarning("PDF 读取失败：" + err.Error())
		return
	}
	result.Parsed = true
	pageCount := reader.NumPage()
	if pageCount <= 0 {
		result.addWarning("PDF 没有可读取的页面")
		extractPDFImages(ctx, data, result)
		return
	}
	if pageCount > maxPDFPages {
		pageCount = maxPDFPages
		result.Truncated = true
		result.addWarning(fmt.Sprintf("PDF 页面超过 %d 页，仅解析前面的页面", maxPDFPages))
	}
	for pageNumber := 1; pageNumber <= pageCount; pageNumber++ {
		if err := ctx.Err(); err != nil {
			result.addWarning("PDF 解析被取消")
			return
		}
		// 传 nil 让解析库按页面加载字体映射；传空 map 会让部分 PDF 只能得到空正文。
		text, pageErr := reader.Page(pageNumber).GetPlainText(nil)
		if pageErr != nil {
			result.addWarning(fmt.Sprintf("PDF 第 %d 页读取失败：%s", pageNumber, pageErr.Error()))
			continue
		}
		if suspiciousPDFText(text) {
			// 某些 CJK 字体没有可用的 ToUnicode 映射时，解析库可能把不同
			// 字形全部解成同一个字。宁可标记该页不可可靠读取，也不把错误正文
			// 送给模型造成事实性误导。
			result.addWarning(fmt.Sprintf("PDF 第 %d 页字体编码异常，未采用疑似错误的提取文本", pageNumber))
			continue
		}
		result.addBlock(fmt.Sprintf("PDF 第 %d 页", pageNumber), text)
	}
	extractPDFImages(ctx, data, result)
	if len(result.Blocks) == 0 {
		result.addWarning("PDF 没有提取到文本，可能是扫描件；当前解析层尚未执行 OCR")
	}
}

// extractPDFImages 读取常见 PDF Image XObject。它不依赖外部命令，也不执行 PDF
// 中的脚本；JPEG/JPEG2000 保留原始编码，Flate 图像转成 PNG 后再交给视觉模型。
func extractPDFImages(ctx context.Context, data []byte, result *Result) {
	if result == nil {
		return
	}
	offset := 0
	imageNumber := 0
	for offset < len(data) {
		if err := ctx.Err(); err != nil {
			result.addWarning("PDF 内嵌图片解析被取消")
			return
		}
		relative := bytes.Index(data[offset:], []byte("/Subtype"))
		if relative < 0 {
			break
		}
		subtypePosition := offset + relative
		dictionaryStart := bytes.LastIndex(data[:subtypePosition], []byte("<<"))
		if dictionaryStart < 0 {
			offset = subtypePosition + len("/Subtype")
			continue
		}
		dictionaryRelativeEnd := bytes.Index(data[subtypePosition:], []byte(">>"))
		if dictionaryRelativeEnd < 0 {
			break
		}
		dictionaryEnd := subtypePosition + dictionaryRelativeEnd + 2
		dictionary := data[dictionaryStart:dictionaryEnd]
		if !pdfDictionaryHasName(dictionary, "/Subtype", "Image") || pdfDictionaryInt(dictionary, "/Width") <= 0 || pdfDictionaryInt(dictionary, "/Height") <= 0 {
			offset = subtypePosition + len("/Subtype")
			continue
		}
		streamPosition := pdfKeywordPosition(data, dictionaryEnd, "stream")
		if streamPosition < 0 {
			offset = dictionaryEnd
			continue
		}
		payloadStart := skipPDFWhitespace(data, streamPosition+len("stream"))
		payloadEnd := -1
		if length := pdfDictionaryInt(dictionary, "/Length"); length >= 0 && length <= len(data)-payloadStart {
			payloadEnd = payloadStart + length
		} else if endRelative := bytes.Index(data[payloadStart:], []byte("endstream")); endRelative >= 0 {
			payloadEnd = payloadStart + endRelative
		}
		if payloadEnd < payloadStart || payloadEnd > len(data) {
			result.addWarning("PDF 内嵌图片数据边界无法确认，已跳过")
			offset = streamPosition + len("stream")
			continue
		}
		imageNumber++
		imageData, mimeType, decodeErr := decodePDFImage(dictionary, data[payloadStart:payloadEnd])
		if decodeErr != nil {
			result.addWarning(fmt.Sprintf("PDF 图片 %d 解析失败：%s", imageNumber, decodeErr.Error()))
		} else {
			result.addImage(fmt.Sprintf("PDF 图片 %d", imageNumber), pdfImageName(imageNumber, mimeType), mimeType, imageData)
		}
		offset = payloadEnd
	}
}

func pdfImageName(number int, mimeType string) string {
	extension := ".bin"
	switch normalizeMIME(mimeType) {
	case "image/jpeg":
		extension = ".jpg"
	case "image/jp2":
		extension = ".jp2"
	case "image/png":
		extension = ".png"
	}
	return fmt.Sprintf("pdf-image-%d%s", number, extension)
}

func pdfKeywordPosition(data []byte, start int, keyword string) int {
	if start < 0 {
		start = 0
	}
	for start < len(data) {
		relative := bytes.Index(data[start:], []byte(keyword))
		if relative < 0 {
			return -1
		}
		position := start + relative
		if pdfTokenBoundary(data, position, len(keyword)) {
			return position
		}
		start = position + len(keyword)
	}
	return -1
}

func pdfTokenBoundary(data []byte, position, length int) bool {
	if position < 0 || position+length > len(data) {
		return false
	}
	isToken := func(value byte) bool {
		return value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' || value >= '0' && value <= '9'
	}
	if position > 0 && isToken(data[position-1]) {
		return false
	}
	return position+length == len(data) || !isToken(data[position+length])
}

func skipPDFWhitespace(data []byte, position int) int {
	for position < len(data) {
		switch data[position] {
		case 0, '\t', '\n', '\f', '\r', ' ':
			position++
		default:
			return position
		}
	}
	return position
}

func pdfDictionaryHasName(dictionary []byte, key, expected string) bool {
	return pdfDictionaryName(dictionary, key) == "/"+expected
}

func pdfDictionaryName(dictionary []byte, key string) string {
	position := bytes.Index(dictionary, []byte(key))
	for position >= 0 {
		if (position == 0 || !isPDFNameByte(dictionary[position-1])) && pdfTokenBoundary(dictionary, position, len(key)) {
			cursor := skipPDFWhitespace(dictionary, position+len(key))
			if cursor < len(dictionary) && dictionary[cursor] == '/' {
				end := cursor + 1
				for end < len(dictionary) && isPDFNameByte(dictionary[end]) {
					end++
				}
				return string(dictionary[cursor:end])
			}
		}
		next := position + len(key)
		if next >= len(dictionary) {
			return ""
		}
		relative := bytes.Index(dictionary[next:], []byte(key))
		if relative < 0 {
			return ""
		}
		position = next + relative
	}
	return ""
}

func pdfDictionaryInt(dictionary []byte, key string) int {
	position := bytes.Index(dictionary, []byte(key))
	for position >= 0 {
		if (position == 0 || !isPDFNameByte(dictionary[position-1])) && pdfTokenBoundary(dictionary, position, len(key)) {
			cursor := skipPDFWhitespace(dictionary, position+len(key))
			sign := 1
			if cursor < len(dictionary) && dictionary[cursor] == '-' {
				sign = -1
				cursor++
			}
			start := cursor
			value := 0
			for cursor < len(dictionary) && dictionary[cursor] >= '0' && dictionary[cursor] <= '9' {
				value = value*10 + int(dictionary[cursor]-'0')
				cursor++
			}
			if cursor > start {
				return sign * value
			}
		}
		next := position + len(key)
		if next >= len(dictionary) {
			return -1
		}
		relative := bytes.Index(dictionary[next:], []byte(key))
		if relative < 0 {
			return -1
		}
		position = next + relative
	}
	return -1
}

func isPDFNameByte(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' || value >= '0' && value <= '9' || value == '#' || value == '_'
}

func pdfDictionaryFilter(dictionary []byte) string {
	position := bytes.Index(dictionary, []byte("/Filter"))
	if position < 0 {
		return ""
	}
	cursor := skipPDFWhitespace(dictionary, position+len("/Filter"))
	if cursor >= len(dictionary) {
		return ""
	}
	if dictionary[cursor] == '[' {
		cursor++
		cursor = skipPDFWhitespace(dictionary, cursor)
	}
	if cursor >= len(dictionary) || dictionary[cursor] != '/' {
		return ""
	}
	end := cursor + 1
	for end < len(dictionary) && isPDFNameByte(dictionary[end]) {
		end++
	}
	return string(dictionary[cursor:end])
}

func decodePDFImage(dictionary, payload []byte) ([]byte, string, error) {
	filter := pdfDictionaryFilter(dictionary)
	switch filter {
	case "/DCTDecode", "/DCT":
		return append([]byte(nil), payload...), "image/jpeg", nil
	case "/JPXDecode":
		return append([]byte(nil), payload...), "image/jp2", nil
	case "", "/FlateDecode", "/Fl":
		decoded := payload
		if filter != "" {
			reader, err := zlib.NewReader(bytes.NewReader(payload))
			if err != nil {
				return nil, "", fmt.Errorf("Flate 解压失败：%w", err)
			}
			decoded, err = io.ReadAll(io.LimitReader(reader, maxPDFDecodedImageBytes+1))
			closeErr := reader.Close()
			if err != nil {
				return nil, "", fmt.Errorf("Flate 读取失败：%w", err)
			}
			if closeErr != nil {
				return nil, "", fmt.Errorf("Flate 关闭失败：%w", closeErr)
			}
			if len(decoded) > maxPDFDecodedImageBytes {
				return nil, "", fmt.Errorf("解压后超过 %d MB", maxPDFDecodedImageBytes>>20)
			}
		}
		return encodePDFFlateImage(dictionary, decoded)
	default:
		return nil, "", fmt.Errorf("暂不支持过滤器 %s", filter)
	}
}

func encodePDFFlateImage(dictionary, decoded []byte) ([]byte, string, error) {
	width := pdfDictionaryInt(dictionary, "/Width")
	height := pdfDictionaryInt(dictionary, "/Height")
	if width <= 0 || height <= 0 || width > maxPDFImageDimension || height > maxPDFImageDimension {
		return nil, "", errors.New("图片尺寸无效或超过限制")
	}
	bits := pdfDictionaryInt(dictionary, "/BitsPerComponent")
	if bits <= 0 {
		bits = 8
	}
	if bits != 8 {
		return nil, "", fmt.Errorf("暂不支持 %d bit 图片", bits)
	}
	colors := 0
	switch pdfDictionaryName(dictionary, "/ColorSpace") {
	case "/DeviceGray":
		colors = 1
	case "/DeviceRGB":
		colors = 3
	default:
		return nil, "", errors.New("暂不支持该颜色空间")
	}
	rowBytes64 := int64(width) * int64(colors)
	totalBytes64 := rowBytes64 * int64(height)
	if rowBytes64 <= 0 || totalBytes64 <= 0 || totalBytes64 > maxPDFDecodedImageBytes {
		return nil, "", fmt.Errorf("解码后的图片超过 %d MB", maxPDFDecodedImageBytes>>20)
	}
	rowBytes := int(rowBytes64)
	totalBytes := int(totalBytes64)
	predictor := pdfDictionaryInt(dictionary, "/Predictor")
	if predictor > 1 {
		var err error
		decoded, err = decodePDFPredictor(decoded, width, height, colors, predictor, rowBytes)
		if err != nil {
			return nil, "", err
		}
	} else if len(decoded) < totalBytes {
		return nil, "", errors.New("解码后的像素数据不足")
	} else {
		decoded = decoded[:totalBytes]
	}

	var encoded bytes.Buffer
	if colors == 1 {
		picture := image.NewGray(image.Rect(0, 0, width, height))
		for row := 0; row < height; row++ {
			copy(picture.Pix[row*picture.Stride:row*picture.Stride+rowBytes], decoded[row*rowBytes:(row+1)*rowBytes])
		}
		if err := png.Encode(&encoded, picture); err != nil {
			return nil, "", fmt.Errorf("PNG 编码失败：%w", err)
		}
	} else {
		picture := image.NewRGBA(image.Rect(0, 0, width, height))
		for row := 0; row < height; row++ {
			for column := 0; column < width; column++ {
				source := row*rowBytes + column*3
				target := row*picture.Stride + column*4
				picture.Pix[target] = decoded[source]
				picture.Pix[target+1] = decoded[source+1]
				picture.Pix[target+2] = decoded[source+2]
				picture.Pix[target+3] = 0xff
			}
		}
		if err := png.Encode(&encoded, picture); err != nil {
			return nil, "", fmt.Errorf("PNG 编码失败：%w", err)
		}
	}
	return encoded.Bytes(), "image/png", nil
}

func decodePDFPredictor(data []byte, width, height, colors, predictor, rowBytes int) ([]byte, error) {
	if predictor == 2 {
		needed := rowBytes * height
		if len(data) < needed {
			return nil, errors.New("TIFF 预测器数据不足")
		}
		output := append([]byte(nil), data[:needed]...)
		for row := 0; row < height; row++ {
			start := row * rowBytes
			for index := colors; index < rowBytes; index++ {
				output[start+index] += output[start+index-colors]
			}
		}
		return output, nil
	}
	if predictor < 10 || predictor > 15 {
		return nil, fmt.Errorf("暂不支持预测器 %d", predictor)
	}
	encodedRowBytes := rowBytes + 1
	needed := encodedRowBytes * height
	if len(data) < needed {
		return nil, errors.New("PNG 预测器数据不足")
	}
	output := make([]byte, rowBytes*height)
	for row := 0; row < height; row++ {
		encodedStart := row * encodedRowBytes
		filter := int(data[encodedStart])
		if predictor != 15 {
			filter = predictor - 10
		}
		if filter < 0 || filter > 4 {
			return nil, fmt.Errorf("PNG 预测器行过滤器 %d 无效", filter)
		}
		for column := 0; column < rowBytes; column++ {
			value := data[encodedStart+1+column]
			left := byte(0)
			if column >= colors {
				left = output[row*rowBytes+column-colors]
			}
			up := byte(0)
			if row > 0 {
				up = output[(row-1)*rowBytes+column]
			}
			upLeft := byte(0)
			if row > 0 && column >= colors {
				upLeft = output[(row-1)*rowBytes+column-colors]
			}
			switch filter {
			case 0:
			case 1:
				value += left
			case 2:
				value += up
			case 3:
				value += byte((int(left) + int(up)) / 2)
			case 4:
				value += pdfPaeth(left, up, upLeft)
			}
			output[row*rowBytes+column] = value
		}
	}
	return output, nil
}

func pdfPaeth(left, up, upLeft byte) byte {
	p := int(left) + int(up) - int(upLeft)
	pa := p - int(left)
	if pa < 0 {
		pa = -pa
	}
	pb := p - int(up)
	if pb < 0 {
		pb = -pb
	}
	pc := p - int(upLeft)
	if pc < 0 {
		pc = -pc
	}
	if pa <= pb && pa <= pc {
		return left
	}
	if pb <= pc {
		return up
	}
	return upLeft
}

func suspiciousPDFText(value string) bool {
	counts := make(map[rune]int)
	chineseCount := 0
	maxCount := 0
	for _, r := range value {
		if !unicode.Is(unicode.Han, r) {
			continue
		}
		chineseCount++
		counts[r]++
		if counts[r] > maxCount {
			maxCount = counts[r]
		}
	}
	return chineseCount >= 16 && len(counts) <= 3 && maxCount*100/chineseCount >= 70
}

func parseDOCX(ctx context.Context, data []byte, result *Result) {
	archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		result.addWarning("DOCX/ DOCM ZIP 容器读取失败：" + err.Error())
		return
	}
	result.Parsed = true
	type member struct {
		file   *zip.File
		source string
	}
	members := make([]member, 0, 4)
	for _, file := range archive.File {
		name := strings.ReplaceAll(file.Name, "\\", "/")
		switch {
		case name == "word/document.xml":
			members = append(members, member{file: file, source: "Word 正文"})
		case strings.HasPrefix(name, "word/header") && strings.HasSuffix(name, ".xml"):
			members = append(members, member{file: file, source: "Word 页眉 " + filepath.Base(name)})
		case strings.HasPrefix(name, "word/footer") && strings.HasSuffix(name, ".xml"):
			members = append(members, member{file: file, source: "Word 页脚 " + filepath.Base(name)})
		}
	}
	extractOOXMLImages(ctx, archive, "word/media/", "Word 图片", result)
	sort.SliceStable(members, func(i, j int) bool { return members[i].file.Name < members[j].file.Name })
	if len(members) == 0 {
		result.addWarning("DOCX/ DOCM 中没有找到可读取的正文 XML")
		return
	}
	if len(members) > maxOOXMLMembers {
		members = members[:maxOOXMLMembers]
		result.Truncated = true
		result.addWarning(fmt.Sprintf("Word XML 部件超过 %d 个，仅解析部分正文", maxOOXMLMembers))
	}
	if result.Kind == "docm" {
		result.addWarning("DOCM 中的宏不会被执行，仅读取文档正文")
	}
	for _, item := range members {
		if err := ctx.Err(); err != nil {
			result.addWarning("Word 文档解析被取消")
			return
		}
		data, readErr := readZipMember(item.file)
		if readErr != nil {
			result.addWarning(item.source + "读取失败：" + readErr.Error())
			continue
		}
		if parseErr := parseWordXML(ctx, data, item.source, result); parseErr != nil {
			if ctx.Err() != nil {
				result.addWarning("Word 文档解析被取消")
				return
			}
			result.addWarning(item.source + "解析失败：" + parseErr.Error())
		}
	}
	if len(result.Blocks) == 0 {
		result.addWarning("Word 文档没有提取到段落或表格文本")
	}
}

func readZipMember(file *zip.File) ([]byte, error) {
	if file == nil {
		return nil, fmt.Errorf("ZIP 部件为空")
	}
	if file.UncompressedSize64 > maxOOXMLMemberBytes {
		return nil, fmt.Errorf("ZIP 部件解压后超过 %d MB", maxOOXMLMemberBytes>>20)
	}
	reader, err := file.Open()
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	data, err := io.ReadAll(io.LimitReader(reader, maxOOXMLMemberBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxOOXMLMemberBytes {
		return nil, fmt.Errorf("ZIP 部件解压后超过 %d MB", maxOOXMLMemberBytes>>20)
	}
	return data, nil
}

func extractOOXMLImages(ctx context.Context, archive *zip.Reader, prefix, locatorPrefix string, result *Result) {
	if archive == nil || result == nil {
		return
	}
	files := make([]*zip.File, 0)
	for _, file := range archive.File {
		name := strings.ReplaceAll(file.Name, "\\", "/")
		if strings.HasPrefix(name, prefix) && !strings.HasSuffix(name, "/") {
			files = append(files, file)
		}
	}
	sort.SliceStable(files, func(i, j int) bool { return files[i].Name < files[j].Name })
	for index, file := range files {
		if err := ctx.Err(); err != nil {
			result.addWarning("文档内嵌图片解析被取消")
			return
		}
		name := filepath.Base(strings.ReplaceAll(file.Name, "\\", "/"))
		data, err := readZipMember(file)
		if err != nil {
			result.addWarning(fmt.Sprintf("内嵌图片 %s 读取失败：%s", displayName(name), err.Error()))
			continue
		}
		mimeType := detectImageMIME(name, data)
		if mimeType == "" {
			result.addWarning(fmt.Sprintf("内嵌图片 %s 类型无法识别，已跳过", displayName(name)))
			continue
		}
		result.addImage(fmt.Sprintf("%s %d", locatorPrefix, index+1), name, mimeType, data)
	}
}

func detectImageMIME(name string, data []byte) string {
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
	if len(data) >= 2 && bytes.Equal(data[:2], []byte("BM")) {
		return "image/bmp"
	}
	if len(data) >= 4 && (bytes.Equal(data[:4], []byte{'I', 'I', '*', 0}) || bytes.Equal(data[:4], []byte{'M', 'M', 0, '*'})) {
		return "image/tiff"
	}

	switch strings.ToLower(filepath.Ext(strings.TrimSpace(name))) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".bmp":
		return "image/bmp"
	case ".tif", ".tiff":
		return "image/tiff"
	case ".jp2", ".j2k", ".jpf", ".jpx":
		return "image/jp2"
	default:
		return ""
	}
}

func parseWordXML(ctx context.Context, data []byte, source string, result *Result) error {
	decoder := xml.NewDecoder(bytes.NewReader(data))
	var paragraph strings.Builder
	var cell strings.Builder
	var row []string
	paragraphDepth := 0
	tableDepth := 0
	cellDepth := 0
	tableNumber := 0
	rowNumber := 0
	paragraphNumber := 0

	appendSpecial := func(value string) {
		if paragraphDepth > 0 {
			paragraph.WriteString(value)
		} else if cellDepth > 0 {
			cell.WriteString(value)
		}
	}

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		switch item := token.(type) {
		case xml.StartElement:
			switch item.Name.Local {
			case "tbl":
				tableDepth++
				if tableDepth == 1 {
					tableNumber++
				}
			case "tr":
				if tableDepth > 0 {
					row = row[:0]
					rowNumber++
				}
			case "tc":
				if tableDepth > 0 {
					cellDepth++
					cell.Reset()
				}
			case "p":
				paragraphDepth++
				if paragraphDepth == 1 {
					paragraph.Reset()
				}
			case "tab":
				appendSpecial("\t")
			case "br", "cr":
				appendSpecial("\n")
			}
		case xml.CharData:
			if paragraphDepth > 0 {
				paragraph.Write([]byte(item))
			} else if cellDepth > 0 {
				cell.Write([]byte(item))
			}
		case xml.EndElement:
			switch item.Name.Local {
			case "p":
				if paragraphDepth <= 0 {
					continue
				}
				paragraphDepth--
				text := cleanExtractedText(paragraph.String())
				paragraph.Reset()
				if text == "" {
					continue
				}
				if cellDepth > 0 {
					if cell.Len() > 0 {
						cell.WriteString("\n")
					}
					limited, cut := limitUTF8(text, maxWordTableCellBytes)
					cell.WriteString(limited)
					if cut {
						result.Truncated = true
					}
				} else {
					paragraphNumber++
					result.addBlock(fmt.Sprintf("%s 段落 %d", source, paragraphNumber), text)
				}
			case "tc":
				if cellDepth <= 0 {
					continue
				}
				text := cleanExtractedText(cell.String())
				limited, cut := limitUTF8(text, maxWordTableCellBytes)
				row = append(row, limited)
				if cut {
					result.Truncated = true
				}
				cell.Reset()
				cellDepth--
			case "tr":
				if tableDepth <= 0 || len(row) == 0 {
					continue
				}
				values := make([]string, 0, len(row))
				for column, value := range row {
					if strings.TrimSpace(value) == "" {
						values = append(values, fmt.Sprintf("列%d=", column+1))
						continue
					}
					values = append(values, fmt.Sprintf("列%d=%s", column+1, value))
				}
				result.addBlock(fmt.Sprintf("%s 表格 %d 行 %d", source, tableNumber, rowNumber), strings.Join(values, " | "))
				row = row[:0]
			case "tbl":
				if tableDepth > 0 {
					tableDepth--
				}
			}
		}
	}
	return nil
}

func parseXLSX(ctx context.Context, data []byte, result *Result) {
	if archive, archiveErr := zip.NewReader(bytes.NewReader(data), int64(len(data))); archiveErr == nil {
		extractOOXMLImages(ctx, archive, "xl/media/", "Excel 图片", result)
	} else {
		result.addWarning("Excel 内嵌图片目录读取失败：" + archiveErr.Error())
	}
	// Excelize 只读取工作表值，限制 ZIP 解压大小，并且不执行宏或公式。
	workbook, err := excelize.OpenReader(bytes.NewReader(data), excelize.Options{
		RawCellValue:      true,
		UnzipSizeLimit:    64 << 20,
		UnzipXMLSizeLimit: 8 << 20,
	})
	if err != nil {
		result.addWarning("XLSX/XLSM 工作簿读取失败：" + err.Error())
		return
	}
	defer workbook.Close()
	result.Parsed = true
	if result.Kind == "xlsm" {
		result.addWarning("XLSM 中的宏不会被执行，仅读取工作表单元格")
	}
	sheets := workbook.GetSheetList()
	if len(sheets) > maxXLSXSheets {
		sheets = sheets[:maxXLSXSheets]
		result.Truncated = true
		result.addWarning(fmt.Sprintf("工作表超过 %d 个，仅解析部分工作表", maxXLSXSheets))
	}
	totalCells := 0
	for _, sheet := range sheets {
		if err := ctx.Err(); err != nil {
			result.addWarning("Excel 文档解析被取消")
			return
		}
		rows, rowsErr := workbook.GetRows(sheet, excelize.Options{RawCellValue: true})
		if rowsErr != nil {
			result.addWarning(fmt.Sprintf("工作表 %s 读取失败：%s", displayName(sheet), rowsErr.Error()))
			continue
		}
		rowLimit := len(rows)
		if rowLimit > maxXLSXRowsPerSheet {
			rowLimit = maxXLSXRowsPerSheet
			result.Truncated = true
			result.addWarning(fmt.Sprintf("工作表 %s 行数超过 %d，仅解析前面的行", displayName(sheet), maxXLSXRowsPerSheet))
		}
		for rowIndex := 0; rowIndex < rowLimit; rowIndex++ {
			if err := ctx.Err(); err != nil {
				result.addWarning("Excel 文档解析被取消")
				return
			}
			row := rows[rowIndex]
			fields := make([]string, 0, len(row))
			firstColumn, lastColumn := 0, 0
			for columnIndex, value := range row {
				value = cleanExtractedText(value)
				if value == "" {
					continue
				}
				if firstColumn == 0 {
					firstColumn = columnIndex + 1
				}
				lastColumn = columnIndex + 1
				totalCells++
				if totalCells > maxXLSXNonEmptyCells {
					result.Truncated = true
					result.addWarning(fmt.Sprintf("非空单元格超过 %d，仅解析前面的内容", maxXLSXNonEmptyCells))
					return
				}
				limited, cut := limitUTF8(value, maxXLSXCellBytes)
				fields = append(fields, fmt.Sprintf("%s=%s", excelCellName(columnIndex+1, rowIndex+1), limited))
				if cut {
					result.Truncated = true
				}
			}
			if len(fields) == 0 {
				continue
			}
			locator := fmt.Sprintf("%s!%s%d:%s%d", displayName(sheet), excelColumnName(firstColumn), rowIndex+1, excelColumnName(lastColumn), rowIndex+1)
			result.addBlock(locator, strings.Join(fields, " | "))
		}
	}
	result.addWarning("Excel 公式按文件中的原始值读取，不执行宏、外部引用或公式副作用")
	if len(result.Blocks) == 0 {
		result.addWarning("Excel 工作簿没有提取到非空单元格")
	}
}

func excelCellName(column, row int) string {
	return fmt.Sprintf("%s%d", excelColumnName(column), row)
}

func excelColumnName(column int) string {
	if column <= 0 {
		return "A"
	}
	var result []byte
	for column > 0 {
		column--
		result = append([]byte{byte('A' + column%26)}, result...)
		column /= 26
	}
	return string(result)
}

func parseCSV(ctx context.Context, data []byte, tabSeparated bool, result *Result) {
	reader := csv.NewReader(bytes.NewReader(data))
	reader.FieldsPerRecord = -1
	reader.TrimLeadingSpace = true
	if tabSeparated {
		reader.Comma = '\t'
	}
	result.Parsed = true
	rowNumber := 0
	for {
		if err := ctx.Err(); err != nil {
			result.addWarning("CSV 文档解析被取消")
			return
		}
		row, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			result.addWarning("CSV 读取失败：" + err.Error())
			break
		}
		rowNumber++
		if rowNumber > maxCSVRows {
			result.Truncated = true
			result.addWarning(fmt.Sprintf("CSV 行数超过 %d，仅解析前面的行", maxCSVRows))
			break
		}
		fields := make([]string, 0, len(row))
		for column, value := range row {
			value = cleanExtractedText(value)
			limited, cut := limitUTF8(value, maxCSVCellBytes)
			fields = append(fields, fmt.Sprintf("%s=%s", excelCellName(column+1, rowNumber), limited))
			if cut {
				result.Truncated = true
			}
		}
		if len(fields) > 0 {
			result.addBlock(fmt.Sprintf("CSV 第 %d 行", rowNumber), strings.Join(fields, " | "))
		}
	}
	if len(result.Blocks) == 0 {
		result.addWarning("CSV 没有提取到行内容")
	}
}
