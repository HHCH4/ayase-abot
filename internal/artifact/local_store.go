package artifact

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type LocalContentStore struct {
	root     string
	maxBytes int64
}

func NewLocalContentStore(root string) (*LocalContentStore, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return nil, fmt.Errorf("%w: artifact store root 不能为空", ErrInvalidRequest)
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("解析 artifact store root 失败: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(absolute, "objects"), 0o700); err != nil {
		return nil, fmt.Errorf("创建 artifact store 目录失败: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(absolute, "tmp"), 0o700); err != nil {
		return nil, fmt.Errorf("创建 artifact 临时目录失败: %w", err)
	}
	return &LocalContentStore{root: absolute, maxBytes: DefaultMaxBytes}, nil
}

func (s *LocalContentStore) Put(ctx context.Context, request PutRequest, reader io.Reader) (StoredObject, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := request.Validate(); err != nil {
		return StoredObject{}, err
	}
	if reader == nil {
		return StoredObject{}, fmt.Errorf("%w: 上传内容为空", ErrInvalidRequest)
	}
	maxBytes := request.MaxBytes
	if maxBytes == 0 {
		maxBytes = s.maxBytes
	}
	if maxBytes <= 0 || maxBytes > s.maxBytes {
		return StoredObject{}, fmt.Errorf("%w: artifact 大小上限无效", ErrInvalidRequest)
	}
	temporary, err := os.CreateTemp(filepath.Join(s.root, "tmp"), "upload-")
	if err != nil {
		return StoredObject{}, fmt.Errorf("创建 artifact 临时对象失败: %w", err)
	}
	temporaryName := temporary.Name()
	defer func() {
		_ = temporary.Close()
		_ = os.Remove(temporaryName)
	}()
	hasher := sha256.New()
	tee := io.MultiWriter(temporary, hasher)
	limited := io.LimitReader(reader, maxBytes+1)
	var prefix []byte
	buffer := make([]byte, 32*1024)
	var size int64
	for {
		if err := ctx.Err(); err != nil {
			return StoredObject{}, err
		}
		count, readErr := limited.Read(buffer)
		if count > 0 {
			if len(prefix) < 512 {
				need := 512 - len(prefix)
				if count < need {
					need = count
				}
				prefix = append(prefix, buffer[:need]...)
			}
			if _, writeErr := tee.Write(buffer[:count]); writeErr != nil {
				return StoredObject{}, fmt.Errorf("写入 artifact 临时对象失败: %w", writeErr)
			}
			size += int64(count)
			if size > maxBytes {
				return StoredObject{}, ErrTooLarge
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return StoredObject{}, fmt.Errorf("读取 artifact 上传内容失败: %w", readErr)
		}
	}
	if err := temporary.Sync(); err != nil {
		return StoredObject{}, fmt.Errorf("同步 artifact 临时对象失败: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return StoredObject{}, fmt.Errorf("关闭 artifact 临时对象失败: %w", err)
	}
	digest := "sha256:" + hex.EncodeToString(hasher.Sum(nil))
	contentType := normalizeMIME(request.MIMEType)
	sniffed := normalizeMIME(http.DetectContentType(prefix))
	if contentType == "" {
		contentType = sniffed
	}
	if !mimeCompatible(contentType, sniffed) {
		return StoredObject{}, fmt.Errorf("%w: 声明 MIME %q 与内容 %q 不一致", ErrInvalidRequest, contentType, sniffed)
	}
	key := contentAddressedKey(digest)
	target := filepath.Join(s.root, "objects", filepath.FromSlash(key))
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return StoredObject{}, fmt.Errorf("创建 artifact 对象目录失败: %w", err)
	}
	if _, err := os.Stat(target); err == nil {
		return StoredObject{Key: key, Digest: digest, Size: size, MIMEType: contentType}, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return StoredObject{}, fmt.Errorf("检查 artifact 对象失败: %w", err)
	}
	if err := os.Rename(temporaryName, target); err != nil {
		if _, statErr := os.Stat(target); statErr == nil {
			// Another writer won the race for the same content.
			return StoredObject{Key: key, Digest: digest, Size: size, MIMEType: contentType}, nil
		}
		return StoredObject{}, fmt.Errorf("提交 artifact 对象失败: %w", err)
	}
	return StoredObject{Key: key, Digest: digest, Size: size, MIMEType: contentType, Created: true}, nil
}

func (s *LocalContentStore) Open(ctx context.Context, key string, byteRange ByteRange) (io.ReadCloser, ObjectInfo, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, ObjectInfo{}, err
	}
	path, err := s.resolveKey(key)
	if err != nil {
		return nil, ObjectInfo{}, err
	}
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ObjectInfo{}, ErrObjectMissing
	}
	if err != nil {
		return nil, ObjectInfo{}, fmt.Errorf("打开 artifact 对象失败: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, ObjectInfo{}, fmt.Errorf("读取 artifact 对象状态失败: %w", err)
	}
	if err := byteRange.Validate(info.Size()); err != nil {
		_ = file.Close()
		return nil, ObjectInfo{}, err
	}
	objectInfo := ObjectInfo{Key: key, Digest: digestFromKey(key), Size: info.Size(), RangeStart: 0, RangeLength: info.Size()}
	if !byteRange.HasRange {
		return file, objectInfo, nil
	}
	if _, err := file.Seek(byteRange.Start, io.SeekStart); err != nil {
		_ = file.Close()
		return nil, ObjectInfo{}, err
	}
	objectInfo.RangeStart = byteRange.Start
	objectInfo.RangeLength = byteRange.Length
	return &sectionReadCloser{Reader: io.LimitReader(file, byteRange.Length), closer: file}, objectInfo, nil
}

func (s *LocalContentStore) Delete(ctx context.Context, key string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	path, err := s.resolveKey(key)
	if err != nil {
		return err
	}
	if err := os.Remove(path); errors.Is(err, os.ErrNotExist) {
		return ErrObjectMissing
	} else if err != nil {
		return fmt.Errorf("删除 artifact 对象失败: %w", err)
	}
	return nil
}

// ListObjects enumerates committed content-addressed objects. Keys that do not
// match the canonical layout are ignored: they were never written by this store
// and must not be reclaimed by a garbage-collection pass.
func (s *LocalContentStore) ListObjects(ctx context.Context) ([]StoredObjectMeta, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	objectsRoot := filepath.Join(s.root, "objects")
	result := make([]StoredObjectMeta, 0)
	err := filepath.WalkDir(objectsRoot, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		relative, relErr := filepath.Rel(objectsRoot, path)
		if relErr != nil {
			return nil
		}
		key := filepath.ToSlash(relative)
		if _, keyErr := s.resolveKey(key); keyErr != nil {
			return nil
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			if errors.Is(infoErr, os.ErrNotExist) {
				return nil
			}
			return infoErr
		}
		result = append(result, StoredObjectMeta{Key: key, Size: info.Size(), ModifiedAt: info.ModTime().UTC()})
		return nil
	})
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("枚举 artifact 对象失败: %w", err)
	}
	return result, nil
}

// SweepTemporaryObjects removes partially written upload files older than the
// cutoff. Committed objects live in a different directory and are never
// touched, so a crash during upload cannot leak temporary bytes forever.
func (s *LocalContentStore) SweepTemporaryObjects(ctx context.Context, before time.Time) (int, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if before.IsZero() {
		return 0, fmt.Errorf("%w: 临时对象清理截止时间不能为空", ErrInvalidRequest)
	}
	cutoff := before.UTC()
	entries, err := os.ReadDir(filepath.Join(s.root, "tmp"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, fmt.Errorf("读取 artifact 临时目录失败: %w", err)
	}
	removed := 0
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return removed, err
		}
		if entry.IsDir() {
			continue
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			if errors.Is(infoErr, os.ErrNotExist) {
				continue
			}
			return removed, fmt.Errorf("读取 artifact 临时对象失败: %w", infoErr)
		}
		if info.ModTime().UTC().After(cutoff) {
			continue
		}
		if removeErr := os.Remove(filepath.Join(s.root, "tmp", entry.Name())); removeErr != nil {
			if errors.Is(removeErr, os.ErrNotExist) {
				continue
			}
			return removed, fmt.Errorf("删除 artifact 临时对象失败: %w", removeErr)
		}
		removed++
	}
	return removed, nil
}

func (s *LocalContentStore) resolveKey(key string) (string, error) {
	key = strings.TrimSpace(key)
	if !strings.HasPrefix(key, "sha256/") || strings.Contains(key, "\\") || filepath.Clean(filepath.FromSlash(key)) != filepath.FromSlash(key) {
		return "", fmt.Errorf("%w: storage key 无效", ErrInvalidRequest)
	}
	parts := strings.Split(key, "/")
	if len(parts) != 3 || len(parts[1]) != 2 || len(parts[2]) != 64 {
		return "", fmt.Errorf("%w: storage key 无效", ErrInvalidRequest)
	}
	for _, value := range parts[1:] {
		for _, character := range value {
			if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f')) {
				return "", fmt.Errorf("%w: storage key 无效", ErrInvalidRequest)
			}
		}
	}
	return filepath.Join(s.root, "objects", filepath.FromSlash(key)), nil
}

func contentAddressedKey(digest string) string {
	hexValue := strings.TrimPrefix(digest, "sha256:")
	return filepath.ToSlash(filepath.Join("sha256", hexValue[:2], hexValue))
}

func digestFromKey(key string) string {
	parts := strings.Split(key, "/")
	if len(parts) == 3 {
		return "sha256:" + parts[2]
	}
	return ""
}

func mimeCompatible(declared, sniffed string) bool {
	declared = normalizeMIME(declared)
	sniffed = normalizeMIME(sniffed)
	if declared == "" || declared == "application/octet-stream" || sniffed == "" || sniffed == "application/octet-stream" {
		return true
	}
	if declared == sniffed {
		return true
	}
	// Browser/file declarations often use application/json while net/http
	// conservatively sniffs JSON as text/plain. Keep this narrow exception;
	// image/audio/video mismatches remain fail-closed.
	if declared == "application/json" && sniffed == "text/plain" {
		return true
	}
	if strings.HasPrefix(declared, "text/") && strings.HasPrefix(sniffed, "text/") {
		return true
	}
	return false
}

type sectionReadCloser struct {
	io.Reader
	closer io.Closer
}

func (reader *sectionReadCloser) Close() error { return reader.closer.Close() }
