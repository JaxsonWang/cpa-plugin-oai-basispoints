package basispoints

import (
	"bytes"
	"container/list"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
	"sync"
)

// 只缓存摘要和文件 ID，不保存图片或凭据；容量不限制单次请求的图片数量。
const maxAttachmentCacheEntries = 512

type cachedAttachment struct {
	key    [sha256.Size]byte
	fileID string
}

type pendingAttachment struct {
	done   chan struct{}
	fileID string
	err    error
}

type attachmentCache struct {
	mu      sync.Mutex
	entries map[[sha256.Size]byte]*list.Element
	order   list.List
	pending map[[sha256.Size]byte]*pendingAttachment
}

func (c *attachmentCache) getOrUpload(key [sha256.Size]byte, upload func() (string, error)) (string, error) {
	c.mu.Lock()
	if entry := c.entries[key]; entry != nil {
		c.order.MoveToFront(entry)
		fileID := entry.Value.(cachedAttachment).fileID
		c.mu.Unlock()
		return fileID, nil
	}
	if pending := c.pending[key]; pending != nil {
		c.mu.Unlock()
		<-pending.done
		return pending.fileID, pending.err
	}
	if c.pending == nil {
		c.pending = make(map[[sha256.Size]byte]*pendingAttachment)
	}
	pending := &pendingAttachment{done: make(chan struct{})}
	c.pending[key] = pending
	c.mu.Unlock()

	fileID, err := upload()
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.pending, key)
	if err == nil {
		if c.entries == nil {
			c.entries = make(map[[sha256.Size]byte]*list.Element)
		}
		c.entries[key] = c.order.PushFront(cachedAttachment{key: key, fileID: fileID})
		if c.order.Len() > maxAttachmentCacheEntries {
			oldest := c.order.Back()
			delete(c.entries, oldest.Value.(cachedAttachment).key)
			c.order.Remove(oldest)
		}
	}
	pending.fileID, pending.err = fileID, err
	close(pending.done)
	return fileID, err
}

type inlineImage struct {
	mediaType string
	filename  string
	data      []byte
}

func decodeInlineImage(dataURL string) (inlineImage, error) {
	metadata, encoded, found := strings.Cut(dataURL[5:], ",")
	if !found {
		return inlineImage{}, fail(400, "invalid_image", "input_image data URL is missing its data separator")
	}
	isBase64 := strings.HasSuffix(strings.ToLower(metadata), ";base64")
	if isBase64 {
		metadata = metadata[:len(metadata)-len(";base64")]
	}
	mediaType, _, err := mime.ParseMediaType(metadata)
	if err != nil || !strings.HasPrefix(mediaType, "image/") {
		return inlineImage{}, fail(400, "invalid_image", "input_image data URL must declare an image media type")
	}
	decoded, err := url.PathUnescape(encoded)
	if err != nil {
		return inlineImage{}, fail(400, "invalid_image", "input_image data URL has invalid percent encoding")
	}
	var data []byte
	if isBase64 {
		data, err = base64.StdEncoding.DecodeString(decoded)
	} else {
		data = []byte(decoded)
	}
	if err != nil || len(data) == 0 {
		return inlineImage{}, fail(400, "invalid_image", "input_image data URL contains empty or invalid image data")
	}
	// 以字节签名确定格式，避免 MIME 别名或系统扩展名数据库产生无后缀、.jfif 等文件名。
	mediaType = http.DetectContentType(data)
	var extension string
	switch mediaType {
	case "image/png":
		extension = ".png"
	case "image/jpeg":
		extension = ".jpeg"
	case "image/gif":
		extension = ".gif"
	case "image/webp":
		extension = ".webp"
	default:
		return inlineImage{}, fail(400, "invalid_image", "input_image bytes must identify a supported format: PNG, JPEG, GIF, or WebP")
	}
	return inlineImage{mediaType: mediaType, filename: "image" + extension, data: data}, nil
}

func attachmentURL(responsesURL string) (string, error) {
	base, err := url.Parse(strings.TrimRight(responsesURL, "/"))
	if err != nil || base.Host == "" || (base.Scheme != "https" && base.Scheme != "http") || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return "", fail(500, "invalid_config", "cannot derive attachments endpoint from responses_url")
	}
	// 官方附件接口与 responses 同目录、同源，不能把自定义上游的凭据发往其他站点。
	return base.ResolveReference(&url.URL{Path: "attachments"}).String(), nil
}

func (s *Service) uploadInputImages(request ExecutorRequest, body map[string]any, c credential, cfg Config) error {
	items, _ := body["input"].([]any)
	for i, value := range items {
		item := objectValue(value)
		itemType := stringValue(item["type"])
		if stringValue(item["role"]) != "user" || (itemType != "" && itemType != "message") {
			continue
		}
		parts, _ := item["content"].([]any)
		var updated []any
		for j, value := range parts {
			part := objectValue(value)
			if stringValue(part["type"]) != "input_image" {
				continue
			}
			imageURL := stringValue(part["image_url"])
			fileID := stringValue(part["file_id"])
			if fileID != "" && imageURL != "" {
				return fail(400, "invalid_image", fmt.Sprintf("input[%d].content[%d]: input_image cannot contain both image_url and file_id", i, j))
			}
			if fileID == "" {
				if len(imageURL) < 5 || !strings.EqualFold(imageURL[:5], "data:") {
					continue
				}
				image, err := decodeInlineImage(imageURL)
				if err != nil {
					return fail(400, "invalid_image", fmt.Sprintf("input[%d].content[%d]: %s", i, j, err))
				}
				endpoint, err := attachmentURL(cfg.ResponsesURL)
				if err != nil {
					return err
				}
				hash := sha256.New()
				_, _ = hash.Write(jsonBytes([]string{endpoint, c.AccountID, c.AuthMode, c.AccessToken, image.mediaType}))
				_, _ = hash.Write(image.data)
				var key [sha256.Size]byte
				copy(key[:], hash.Sum(nil))
				fileID, err = s.attachments.getOrUpload(key, func() (string, error) {
					return s.uploadImage(request, endpoint, image, c)
				})
				if err != nil {
					return err
				}
			}
			if updated == nil {
				updated = append([]any(nil), parts...)
			}
			// Basis Points 的文件引用不接受公开 Responses API 的 detail 等字段。
			updated[j] = map[string]any{"type": "input_image", "file_id": fileID}
		}
		if updated != nil {
			copy := cloneObject(item)
			copy["content"] = updated
			items[i] = copy
		}
	}
	return nil
}

func (s *Service) uploadImage(request ExecutorRequest, endpoint string, image inlineImage, c credential) (string, error) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	partHeaders := make(textproto.MIMEHeader)
	partHeaders.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{"name": "file", "filename": image.filename}))
	partHeaders.Set("Content-Type", image.mediaType)
	part, err := writer.CreatePart(partHeaders)
	if err != nil {
		return "", fail(500, "attachment_encoding", "cannot encode image attachment")
	}
	if _, err := part.Write(image.data); err != nil {
		return "", fail(500, "attachment_encoding", "cannot write image attachment")
	}
	if err := writer.Close(); err != nil {
		return "", fail(500, "attachment_encoding", "cannot finish image attachment")
	}
	headers := authHeaders(c, false)
	headers.Set("Content-Type", writer.FormDataContentType())
	var response upstreamResponse
	if err := s.call("host.http.do", map[string]any{
		"host_callback_id": request.HostCallbackID,
		"method":           http.MethodPost,
		"url":              endpoint,
		"headers":          headers,
		"body":             body.Bytes(),
	}, &response); err != nil {
		return "", fail(502, "attachment_transport", "Basis Points attachment upload transport failed: "+attachmentErrorMessage([]byte(err.Error()), c, image))
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", fail(response.StatusCode, "attachment_upload_error", fmt.Sprintf("Basis Points attachment upload HTTP %d: %s", response.StatusCode, attachmentErrorMessage(response.Body, c, image)))
	}
	var result struct {
		FileID string `json:"openai_file_id"`
	}
	if json.Unmarshal(response.Body, &result) != nil || strings.TrimSpace(result.FileID) == "" {
		return "", fail(502, "invalid_attachment_response", "Basis Points attachment upload returned no openai_file_id")
	}
	return strings.TrimSpace(result.FileID), nil
}

func attachmentErrorMessage(raw []byte, c credential, image inlineImage) string {
	message := string(raw)
	for _, secret := range []string{c.AccessToken, c.AccountID, c.Email, base64.StdEncoding.EncodeToString(image.data), string(image.data)} {
		if secret != "" {
			message = strings.ReplaceAll(message, secret, "[REDACTED]")
		}
	}
	return redactTokenMessage(errorMessage([]byte(message)))
}
