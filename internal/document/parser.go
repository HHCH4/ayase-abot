// Package document 负责把常见办公文档转换为带来源信息的文本块。
//
// 这个包只做本地、确定性的解析，不调用任何外部 AI，也不会修改原始
// Artifact。原始文件仍由 Artifact 负责保存，解析结果只在当前请求中作为
// 模型上下文使用。
package document

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/csv"
	"encoding/xml"
	"fmt"
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
)

// Block 是一个带有稳定来源定位信息的文本块。
// Locator 例如“PDF 第 3 页”“Word 表格 1 行 2”“Sheet1!A1:C1”。
type Block struct {
	Locator string
	Text    string
}

// Result 是一次本地解析的结果。它不包含原始二进制，只包含可安全送入模型的派生文本。
type Result struct {
	Name      string
	MIMEType  string
	Kind      string
	Parsed    bool
	Truncated bool
	Blocks    []Block
	Warnings  []string

	parsedBytes int
}

// IsDocumentAttachment 判断一个附件是否应该进入文档解析路径。
// 旧版 .doc/.xls 也返回 true，这样它们不会被误当作可直接阅读的二进制送给模型，
// 而是得到明确的“不支持该格式”的提示。
func IsDocumentAttachment(name, mimeType string) bool {
	switch documentKind(name, mimeType) {
	case "pdf", "docx", "docm", "doc", "xlsx", "xlsm", "xls", "csv", "tsv":
		return true
	default:
		return false
	}
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
	if IsDocumentAttachment(name, mimeType) {
		return true
	}
	return sniffDocumentKind(data) != ""
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
	if len(result.Blocks) == 0 {
		result.addWarning("PDF 没有提取到文本，可能是扫描件；当前解析层尚未执行 OCR")
	}
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
