package httpapi

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"Abot/internal/artifact"
)

const maxArtifactJSONRequestBytes = artifact.DefaultMaxBytes*4/3 + 2*1024*1024

type artifactUploadPayload struct {
	UserID         string         `json:"user_id"`
	ConversationID string         `json:"conversation_id"`
	InvocationID   string         `json:"invocation_id"`
	ProducerType   string         `json:"producer_type"`
	ProducerID     string         `json:"producer_id"`
	Kind           artifact.Kind  `json:"kind"`
	Name           string         `json:"name"`
	MIMEType       string         `json:"mime_type"`
	SecurityClass  string         `json:"security_class"`
	Metadata       map[string]any `json:"metadata"`
	ExpiresAt      *time.Time     `json:"expires_at"`
	Data           string         `json:"data"`
}

func (s *Server) requireArtifacts() (*artifact.Service, error) {
	if s.artifacts == nil {
		return nil, errors.New("Artifact 服务尚未装配")
	}
	return s.artifacts, nil
}

func artifactUserID(request *http.Request, bodyValue string) (string, error) {
	if request == nil {
		return strings.TrimSpace(bodyValue), nil
	}
	headerValue := strings.TrimSpace(request.Header.Get("X-Abot-User-ID"))
	queryValue := strings.TrimSpace(request.URL.Query().Get("user_id"))
	if headerValue != "" && queryValue != "" && headerValue != queryValue {
		return "", fmt.Errorf("%w: user_id header 与 query 不一致", artifact.ErrInvalidRequest)
	}
	if bodyValue = strings.TrimSpace(bodyValue); bodyValue != "" {
		if (headerValue != "" && headerValue != bodyValue) || (queryValue != "" && queryValue != bodyValue) {
			return "", fmt.Errorf("%w: 请求中的 user_id 不一致", artifact.ErrInvalidRequest)
		}
		return bodyValue, nil
	}
	if headerValue != "" {
		return headerValue, nil
	}
	return queryValue, nil
}

func (s *Server) createArtifact(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireArtifacts()
	if err != nil {
		writeError(writer, err)
		return
	}
	var putRequest artifact.PutRequest
	var reader io.Reader
	mediaType, _, mediaErr := mime.ParseMediaType(strings.TrimSpace(request.Header.Get("Content-Type")))
	if mediaErr != nil {
		mediaType = strings.TrimSpace(strings.Split(request.Header.Get("Content-Type"), ";")[0])
	}
	if strings.EqualFold(mediaType, "application/json") || strings.HasSuffix(strings.ToLower(mediaType), "+json") {
		var payload artifactUploadPayload
		if err := decodeSingleJSONWithLimit(writer, request, &payload, maxArtifactJSONRequestBytes); err != nil {
			writeError(writer, fmt.Errorf("请求体无效: %w", err))
			return
		}
		userID, userErr := artifactUserID(request, payload.UserID)
		if userErr != nil {
			writeError(writer, userErr)
			return
		}
		decoded, decodeErr := base64.StdEncoding.DecodeString(payload.Data)
		if decodeErr != nil {
			writeError(writer, fmt.Errorf("%w: data 必须是标准 base64", artifact.ErrInvalidRequest))
			return
		}
		putRequest = artifact.PutRequest{UserID: userID, ConversationID: payload.ConversationID, InvocationID: payload.InvocationID, ProducerType: payload.ProducerType, ProducerID: payload.ProducerID, Kind: payload.Kind, Name: payload.Name, MIMEType: payload.MIMEType, SecurityClass: payload.SecurityClass, Metadata: payload.Metadata, ExpiresAt: payload.ExpiresAt}
		if putRequest.Kind == "" {
			putRequest.Kind = artifact.KindInputAttachment
		}
		reader = bytes.NewReader(decoded)
	} else {
		userID, userErr := artifactUserID(request, "")
		if userErr != nil {
			writeError(writer, userErr)
			return
		}
		putRequest, err = artifactPutRequestFromHeaders(request, userID, mediaType)
		if err != nil {
			writeError(writer, err)
			return
		}
		request.Body = http.MaxBytesReader(writer, request.Body, artifact.DefaultMaxBytes+1)
		reader = request.Body
	}
	if strings.TrimSpace(putRequest.ConversationID) != "" && s.conversations != nil {
		if _, err := s.conversations.Get(request.Context(), putRequest.UserID, strings.TrimSpace(putRequest.ConversationID)); err != nil {
			writeError(writer, fmt.Errorf("artifact 关联的对话无效: %w", err))
			return
		}
	}
	item, err := service.Put(request.Context(), putRequest, reader)
	if err != nil {
		writeError(writer, err)
		return
	}
	writer.Header().Set("Location", "/api/v1/artifacts/"+item.ID)
	writeJSON(writer, http.StatusCreated, item)
}

func artifactPutRequestFromHeaders(request *http.Request, userID, mediaType string) (artifact.PutRequest, error) {
	query := request.URL.Query()
	kind := artifact.Kind(strings.TrimSpace(firstNonEmpty(query.Get("kind"), request.Header.Get("X-Abot-Artifact-Kind"))))
	if kind == "" {
		kind = artifact.KindInputAttachment
	}
	name := strings.TrimSpace(firstNonEmpty(query.Get("name"), request.Header.Get("X-Abot-Artifact-Name")))
	if name == "" {
		name = "artifact.bin"
	}
	maxBytes := int64(0)
	if raw := strings.TrimSpace(firstNonEmpty(query.Get("max_bytes"), request.Header.Get("X-Abot-Artifact-Max-Bytes"))); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return artifact.PutRequest{}, fmt.Errorf("%w: max_bytes 无效", artifact.ErrInvalidRequest)
		}
		maxBytes = parsed
	}
	return artifact.PutRequest{
		UserID: userID, ConversationID: firstNonEmpty(query.Get("conversation_id"), request.Header.Get("X-Abot-Conversation-ID")),
		InvocationID: firstNonEmpty(query.Get("invocation_id"), request.Header.Get("X-Abot-Invocation-ID")),
		ProducerType: firstNonEmpty(query.Get("producer_type"), request.Header.Get("X-Abot-Artifact-Producer-Type")),
		ProducerID:   firstNonEmpty(query.Get("producer_id"), request.Header.Get("X-Abot-Artifact-Producer-ID")),
		Kind:         kind, Name: name, MIMEType: firstNonEmpty(query.Get("mime_type"), request.Header.Get("X-Abot-Artifact-MIME"), mediaType),
		SecurityClass: firstNonEmpty(query.Get("security_class"), request.Header.Get("X-Abot-Artifact-Security-Class")), MaxBytes: maxBytes,
	}, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func (s *Server) listArtifacts(writer http.ResponseWriter, request *http.Request) {
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
	items, err := service.List(request.Context(), userID, request.URL.Query().Get("conversation_id"))
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"artifacts": items})
}

func (s *Server) getArtifact(writer http.ResponseWriter, request *http.Request) {
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
	item, err := service.Get(request.Context(), userID, request.PathValue("id"))
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, item)
}

func (s *Server) getArtifactContent(writer http.ResponseWriter, request *http.Request) {
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
	rangeValue, rangeErr := parseArtifactRange(request.Header.Get("Range"))
	if rangeErr != nil {
		writer.Header().Set("Content-Range", "bytes */*")
		writeJSON(writer, http.StatusRequestedRangeNotSatisfiable, map[string]string{"error": rangeErr.Error()})
		return
	}
	// Resolve suffix/open-ended ranges after authorization, when immutable
	// metadata supplies the object size.
	if rangeValue.HasRange && (rangeValue.Start < 0 || rangeValue.Length < 0) {
		item, getErr := service.Get(request.Context(), userID, request.PathValue("id"))
		if getErr != nil {
			writeError(writer, getErr)
			return
		}
		if item.Size <= 0 {
			writer.Header().Set("Content-Range", "bytes */0")
			writeJSON(writer, http.StatusRequestedRangeNotSatisfiable, map[string]string{"error": "Range 超出空 artifact"})
			return
		}
		if rangeValue.Start < 0 {
			if rangeValue.Length > item.Size {
				rangeValue = artifact.ByteRange{Start: 0, Length: item.Size, HasRange: true}
			} else {
				rangeValue.Start = item.Size - rangeValue.Length
			}
		} else {
			if rangeValue.Start >= item.Size {
				writer.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", item.Size))
				writeJSON(writer, http.StatusRequestedRangeNotSatisfiable, map[string]string{"error": "Range 起点超出 artifact"})
				return
			}
			rangeValue.Length = item.Size - rangeValue.Start
		}
	}
	reader, info, item, err := service.Open(request.Context(), userID, request.PathValue("id"), rangeValue)
	if err != nil {
		writeError(writer, err)
		return
	}
	defer reader.Close()
	if info.Size != item.Size || (item.Digest != "" && info.Digest != "" && item.Digest != info.Digest) {
		writeError(writer, artifact.ErrObjectMissing)
		return
	}
	writer.Header().Set("Content-Type", item.MIMEType)
	writer.Header().Set("Content-Length", strconv.FormatInt(info.RangeLength, 10))
	writer.Header().Set("Accept-Ranges", "bytes")
	writer.Header().Set("ETag", strconv.Quote(item.Digest))
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.Header().Set("Content-Disposition", `attachment; filename="`+strings.ReplaceAll(item.Name, `"`, "'")+`"`)
	if rangeValue.HasRange {
		writer.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", info.RangeStart, info.RangeStart+info.RangeLength-1, info.Size))
		writer.WriteHeader(http.StatusPartialContent)
	} else {
		writer.WriteHeader(http.StatusOK)
	}
	if _, err := io.CopyN(writer, reader, info.RangeLength); err != nil && !errors.Is(err, io.EOF) {
		return
	}
}

func parseArtifactRange(value string) (artifact.ByteRange, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return artifact.ByteRange{}, nil
	}
	if !strings.HasPrefix(value, "bytes=") || strings.Contains(value, ",") {
		return artifact.ByteRange{}, fmt.Errorf("%w: Range 仅支持单个 bytes 范围", artifact.ErrInvalidRequest)
	}
	part := strings.TrimSpace(strings.TrimPrefix(value, "bytes="))
	pieces := strings.Split(part, "-")
	if len(pieces) != 2 {
		return artifact.ByteRange{}, fmt.Errorf("%w: Range 格式无效", artifact.ErrInvalidRequest)
	}
	if strings.TrimSpace(pieces[0]) == "" {
		length, err := strconv.ParseInt(strings.TrimSpace(pieces[1]), 10, 64)
		if err != nil || length <= 0 {
			return artifact.ByteRange{}, fmt.Errorf("%w: Range 长度无效", artifact.ErrInvalidRequest)
		}
		// A suffix range is resolved after metadata is loaded. Keep a marker
		// here; the HTTP handler converts it before calling the service.
		return artifact.ByteRange{Start: -1, Length: length, HasRange: true}, nil
	}
	start, err := strconv.ParseInt(strings.TrimSpace(pieces[0]), 10, 64)
	if err != nil || start < 0 {
		return artifact.ByteRange{}, fmt.Errorf("%w: Range 起点无效", artifact.ErrInvalidRequest)
	}
	length := int64(0)
	if strings.TrimSpace(pieces[1]) == "" {
		return artifact.ByteRange{Start: start, Length: -1, HasRange: true}, nil
	}
	end, err := strconv.ParseInt(strings.TrimSpace(pieces[1]), 10, 64)
	if err != nil || end < start {
		return artifact.ByteRange{}, fmt.Errorf("%w: Range 终点无效", artifact.ErrInvalidRequest)
	}
	// Guard the inclusive end calculation against int64 overflow before the
	// range is resolved against artifact metadata.
	if end-start == int64(^uint64(0)>>1) {
		return artifact.ByteRange{}, fmt.Errorf("%w: Range 长度无效", artifact.ErrInvalidRequest)
	}
	length = end - start + 1
	return artifact.ByteRange{Start: start, Length: length, HasRange: true}, nil
}

func (s *Server) getArtifactPreview(writer http.ResponseWriter, request *http.Request) {
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
	item, err := service.Get(request.Context(), userID, request.PathValue("id"))
	if err != nil {
		writeError(writer, err)
		return
	}
	response := map[string]any{"artifact": item.Ref()}
	if preview, ok := item.Metadata["preview"].(string); ok {
		response["preview"] = truncateArtifactPreview(preview)
		writeJSON(writer, http.StatusOK, response)
		return
	}
	if !artifactMIMEIsText(item.MIMEType) || item.Size == 0 {
		writeJSON(writer, http.StatusOK, response)
		return
	}
	length := item.Size
	if length > artifact.MaxPreviewBytes {
		length = artifact.MaxPreviewBytes
	}
	reader, _, _, err := service.Open(request.Context(), userID, item.ID, artifact.ByteRange{Start: 0, Length: length, HasRange: true})
	if err != nil {
		writeError(writer, err)
		return
	}
	data, readErr := io.ReadAll(io.LimitReader(reader, artifact.MaxPreviewBytes))
	_ = reader.Close()
	if readErr != nil {
		writeError(writer, readErr)
		return
	}
	response["preview"] = string(data)
	response["truncated"] = item.Size > int64(len(data))
	writeJSON(writer, http.StatusOK, response)
}

func artifactMIMEIsText(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	return strings.HasPrefix(value, "text/") || value == "application/json" || value == "application/xml" || value == "application/javascript" || strings.HasSuffix(value, "+json") || strings.HasSuffix(value, "+xml")
}

func truncateArtifactPreview(value string) string {
	if len([]byte(value)) <= artifact.MaxPreviewBytes {
		return value
	}
	return string([]byte(value)[:artifact.MaxPreviewBytes]) + "…"
}

func (s *Server) deleteArtifact(writer http.ResponseWriter, request *http.Request) {
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
	if err := service.Delete(request.Context(), userID, request.PathValue("id")); err != nil {
		writeError(writer, err)
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}
