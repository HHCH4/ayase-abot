package artifact

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func putBytes(t *testing.T, service *Service, userID, name, mimeType string, data []byte) Artifact {
	t.Helper()
	item, err := service.Put(context.Background(), PutRequest{
		UserID: userID, Kind: KindInputAttachment, Name: name, MIMEType: mimeType,
	}, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("写入 artifact 失败: %v", err)
	}
	return item
}

func pngHeader(width, height uint32) []byte {
	data := []byte("\x89PNG\r\n\x1a\n")
	data = append(data, 0, 0, 0, 13)
	data = append(data, []byte("IHDR")...)
	var size [8]byte
	binary.BigEndian.PutUint32(size[0:4], width)
	binary.BigEndian.PutUint32(size[4:8], height)
	data = append(data, size[:]...)
	return append(data, make([]byte, 64)...)
}

func gifHeader(width, height uint16) []byte {
	data := []byte("GIF89a")
	data = append(data, byte(width), byte(width>>8), byte(height), byte(height>>8))
	return append(data, make([]byte, 32)...)
}

func jpegHeader(width, height uint16) []byte {
	data := []byte{0xff, 0xd8}
	data = append(data, 0xff, 0xc0, 0x00, 0x11, 0x08)
	data = append(data, byte(height>>8), byte(height))
	data = append(data, byte(width>>8), byte(width))
	return append(data, make([]byte, 32)...)
}

func TestExtractTextPreviewAndTruncation(t *testing.T) {
	service, _, _ := newTestService(t)
	ctx := context.Background()
	item := putBytes(t, service, "user-1", "notes.txt", "text/plain", []byte("hello extraction"))

	extraction, err := service.Extract(ctx, "user-1", item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if extraction.Kind != ExtractionText || extraction.Preview != "hello extraction" || extraction.Truncated {
		t.Fatalf("文本提取不正确: %+v", extraction)
	}
	if extraction.SourceDigest != item.Digest || extraction.SourceSize != item.Size || extraction.Version != ExtractionVersion {
		t.Fatalf("提取结果缺少对象身份: %+v", extraction)
	}

	large := putBytes(t, service, "user-1", "big.txt", "text/plain", []byte(strings.Repeat("x", maxTextPreviewBytes*2)))
	truncated, err := service.Extract(ctx, "user-1", large.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !truncated.Truncated || len(truncated.Preview) != maxTextPreviewBytes {
		t.Fatalf("大文件预览必须被截断: truncated=%v len=%d", truncated.Truncated, len(truncated.Preview))
	}
}

func TestExtractNonUTF8TextFallsBackToMetadata(t *testing.T) {
	service, _, _ := newTestService(t)
	// 声明为文本但包含非法 UTF-8 字节：必须降级为二进制而不是展示乱码。
	payload := append([]byte("text"), 0xff, 0xfe, 0xfd)
	item := putBytes(t, service, "user-1", "odd.txt", "text/plain", payload)
	extraction, err := service.Extract(context.Background(), "user-1", item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if extraction.Kind != ExtractionMetadata || extraction.Preview != "" {
		t.Fatalf("非法 UTF-8 必须降级为元数据: %+v", extraction)
	}
	if len(extraction.Warnings) == 0 {
		t.Fatal("降级必须带上警告说明")
	}
}

func TestExtractUnknownFormatStaysMetadataOnly(t *testing.T) {
	service, _, _ := newTestService(t)
	item := putBytes(t, service, "user-1", "blob.bin", "application/octet-stream", []byte{0x00, 0x01, 0x02, 0x03, 0x04})
	extraction, err := service.Extract(context.Background(), "user-1", item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if extraction.Kind != ExtractionMetadata || extraction.Preview != "" {
		t.Fatalf("未知格式只应返回元数据: %+v", extraction)
	}
}

func TestExtractImageHeaderDimensions(t *testing.T) {
	service, _, _ := newTestService(t)
	ctx := context.Background()
	cases := []struct {
		name     string
		mimeType string
		data     []byte
		format   string
		width    int
		height   int
	}{
		{"png", "image/png", pngHeader(640, 480), "png", 640, 480},
		{"gif", "image/gif", gifHeader(320, 240), "gif", 320, 240},
		{"jpeg", "image/jpeg", jpegHeader(600, 300), "jpeg", 600, 300},
	}
	for _, testCase := range cases {
		item := putBytes(t, service, "user-1", testCase.name+"."+testCase.format, testCase.mimeType, testCase.data)
		extraction, err := service.Extract(ctx, "user-1", item.ID)
		if err != nil {
			t.Fatalf("%s: %v", testCase.name, err)
		}
		if extraction.Kind != ExtractionImage || extraction.Image == nil {
			t.Fatalf("%s: 未识别为图片: %+v", testCase.name, extraction)
		}
		if extraction.Image.Format != testCase.format || extraction.Image.Width != testCase.width || extraction.Image.Height != testCase.height {
			t.Fatalf("%s: 尺寸不正确: %+v", testCase.name, extraction.Image)
		}
	}
}

func TestExtractPDFHeader(t *testing.T) {
	service, _, _ := newTestService(t)
	body := "%PDF-1.7\n1 0 obj\n<< /Type /Pages /Kids [2 0 R] /Count 3 >>\nendobj\n" +
		strings.Repeat("% padding line\n", 40) +
		"trailer\n<< /Encrypt 4 0 R >>\n%%EOF\n"
	item := putBytes(t, service, "user-1", "doc.pdf", "application/pdf", []byte(body))
	extraction, err := service.Extract(context.Background(), "user-1", item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if extraction.Kind != ExtractionPDF || extraction.PDF == nil {
		t.Fatalf("未识别为 PDF: %+v", extraction)
	}
	if extraction.PDF.Version != "1.7" || extraction.PDF.Pages != 3 || !extraction.PDF.Encrypted {
		t.Fatalf("PDF 元数据不正确: %+v", extraction.PDF)
	}
}

func TestExtractZipListingAndSuspiciousPaths(t *testing.T) {
	service, _, _ := newTestService(t)
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for _, name := range []string{"docs/readme.txt", "src/main.go", "../escape.txt", "/absolute.txt"} {
		entry, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write([]byte("content of " + name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	item := putBytes(t, service, "user-1", "bundle.zip", "application/zip", buffer.Bytes())

	extraction, err := service.Extract(context.Background(), "user-1", item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if extraction.Kind != ExtractionArchive || extraction.Archive == nil {
		t.Fatalf("未识别为归档: %+v", extraction)
	}
	if extraction.Archive.Format != "zip" || extraction.Archive.Entries != 4 {
		t.Fatalf("zip 条目统计不正确: %+v", extraction.Archive)
	}
	if len(extraction.Archive.SuspiciousPaths) != 2 {
		t.Fatalf("必须标出逃逸根目录的条目: %+v", extraction.Archive.SuspiciousPaths)
	}
	if len(extraction.Warnings) == 0 {
		t.Fatal("可疑条目必须带警告")
	}
}

func TestExtractZipBombRatioIsFlagged(t *testing.T) {
	service, _, _ := newTestService(t)
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	entry, err := writer.Create("bomb.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entry.Write(bytes.Repeat([]byte("a"), 8<<20)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	item := putBytes(t, service, "user-1", "bomb.zip", "application/zip", buffer.Bytes())

	extraction, err := service.Extract(context.Background(), "user-1", item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if extraction.Archive == nil || len(extraction.Archive.Names) != 0 {
		t.Fatalf("异常压缩比必须停止解析条目: %+v", extraction.Archive)
	}
	flagged := false
	for _, warning := range extraction.Warnings {
		if strings.Contains(warning, "炸弹") {
			flagged = true
		}
	}
	if !flagged {
		t.Fatalf("必须报告解压炸弹风险: %+v", extraction.Warnings)
	}
}

func TestExtractTarAndGzip(t *testing.T) {
	service, _, _ := newTestService(t)
	ctx := context.Background()

	var tarBuffer bytes.Buffer
	tarWriter := tar.NewWriter(&tarBuffer)
	payload := []byte("tar member content")
	for _, name := range []string{"one.txt", "nested/two.txt", "../escape.txt"} {
		if err := tarWriter.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(payload))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tarWriter.Write(payload); err != nil {
			t.Fatal(err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	tarItem := putBytes(t, service, "user-1", "bundle.tar", "application/x-tar", tarBuffer.Bytes())
	tarExtraction, err := service.Extract(ctx, "user-1", tarItem.ID)
	if err != nil {
		t.Fatal(err)
	}
	if tarExtraction.Archive == nil || tarExtraction.Archive.Format != "tar" || tarExtraction.Archive.Entries != 3 {
		t.Fatalf("tar 提取不正确: %+v", tarExtraction)
	}
	if len(tarExtraction.Archive.SuspiciousPaths) != 1 {
		t.Fatalf("tar 必须标出逃逸条目: %+v", tarExtraction.Archive.SuspiciousPaths)
	}

	var gzipBuffer bytes.Buffer
	gzipWriter := gzip.NewWriter(&gzipBuffer)
	gzipWriter.Name = "report.csv"
	original := bytes.Repeat([]byte("column,value\n"), 64)
	if _, err := gzipWriter.Write(original); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	gzipItem := putBytes(t, service, "user-1", "report.csv.gz", "application/x-gzip", gzipBuffer.Bytes())
	gzipExtraction, err := service.Extract(ctx, "user-1", gzipItem.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gzipExtraction.Archive == nil || gzipExtraction.Archive.Format != "gzip" {
		t.Fatalf("gzip 提取不正确: %+v", gzipExtraction)
	}
	if gzipExtraction.Archive.TotalUncompressed != int64(len(original)) {
		t.Fatalf("gzip 解压大小不正确: %+v", gzipExtraction.Archive)
	}
	if len(gzipExtraction.Archive.Names) != 1 || gzipExtraction.Archive.Names[0] != "report.csv" {
		t.Fatalf("gzip 原始文件名未提取: %+v", gzipExtraction.Archive)
	}
}

func TestExtractIsIdempotentAndVersioned(t *testing.T) {
	service, repository, _ := newTestService(t)
	ctx := context.Background()
	item := putBytes(t, service, "user-1", "notes.txt", "text/plain", []byte("first"))

	first, err := service.Extract(ctx, "user-1", item.ID)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := repository.Get(ctx, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := stored.StoredExtraction(); !ok {
		t.Fatalf("提取结果必须持久化到 metadata: %+v", stored.Metadata)
	}
	if stored.Version != item.Version+1 {
		t.Fatalf("持久化必须推进版本: %d", stored.Version)
	}

	second, err := service.Extract(ctx, "user-1", item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !second.ExtractedAt.Equal(first.ExtractedAt) {
		t.Fatalf("重复提取必须复用已存储结果: %+v vs %+v", first.ExtractedAt, second.ExtractedAt)
	}

	// 旧版本的投影必须被当作失效，而不是被继续复用。
	staleMetadata := cloneMetadata(stored.Metadata)
	staleMetadata[ExtractionMetadataKey].(map[string]any)["version"] = "0"
	stale := stored
	stale.Metadata = staleMetadata
	stale.Version = stored.Version + 1
	if err := repository.Update(ctx, stale, int64(stored.Version)); err != nil {
		t.Fatal(err)
	}
	if _, ok := stale.StoredExtraction(); ok {
		t.Fatal("版本不匹配的投影必须视为失效")
	}
	refreshed, err := service.Extract(ctx, "user-1", item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.Version != ExtractionVersion {
		t.Fatalf("重新提取必须写入当前版本: %+v", refreshed)
	}
}

func TestExtractHonoursOwnershipAndQuarantine(t *testing.T) {
	service, repository, _ := newTestService(t)
	ctx := context.Background()
	item := putBytes(t, service, "user-1", "private.txt", "text/plain", []byte("owner only"))
	if _, err := service.Extract(ctx, "user-2", item.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("跨用户提取必须被拒绝, got %v", err)
	}

	quarantined := item
	quarantined.Status = StatusQuarantined
	quarantined.SecurityClass = "secret"
	quarantined.Version = item.Version + 1
	if err := repository.Update(ctx, quarantined, int64(item.Version)); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Extract(ctx, "user-1", item.ID); !errors.Is(err, ErrQuarantined) {
		t.Fatalf("隔离内容必须拒绝提取, got %v", err)
	}
}

func TestExtractArchiveNameBudgetKeepsResultPersistable(t *testing.T) {
	service, repository, _ := newTestService(t)
	ctx := context.Background()
	// 60 个长名字条目会超出提取投影预算；清单必须被截断，而不是让提取直接失败。
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for index := 0; index < maxArchiveEntries; index++ {
		entry, err := writer.Create(fmt.Sprintf("%s-%03d.txt", strings.Repeat("n", 180), index))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write([]byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	item := putBytes(t, service, "user-1", "many.zip", "application/zip", buffer.Bytes())

	extraction, err := service.Extract(ctx, "user-1", item.ID)
	if err != nil {
		t.Fatalf("名称预算必须截断清单而不是让提取失败: %v", err)
	}
	if extraction.Archive == nil || !extraction.Archive.Truncated {
		t.Fatalf("超出名称预算的清单必须标记截断: %+v", extraction.Archive)
	}
	if len(extraction.Archive.Names) >= maxArchiveEntries {
		t.Fatalf("清单必须被截断: %d", len(extraction.Archive.Names))
	}
	if extraction.Archive.Entries != maxArchiveEntries {
		t.Fatalf("条目总数仍应被报告: %d", extraction.Archive.Entries)
	}
	stored, err := repository.Get(ctx, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := stored.StoredExtraction(); !ok {
		t.Fatal("截断后的投影必须仍然可持久化")
	}
}
