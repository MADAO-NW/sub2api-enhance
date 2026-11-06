package ingress

import (
	"compress/flate"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/klauspost/compress/zstd"
)

// maxDecompressedBodySize 是解压后的请求体上限，与原版 0.2.1 的 maxDecompressedBodySize 一致（决策 18）。
const maxDecompressedBodySize = 64 << 20

// ErrBodyTooLarge 表示解压后请求体超过上限，入口直接返回 413 且不转发。
var ErrBodyTooLarge = errors.New("解压后请求体超过 64MB 上限")

// ErrUnsupportedEncoding 表示入口无法解压该内容编码。
var ErrUnsupportedEncoding = errors.New("该内容编码尚未支持审核解析")

// spooledBody 是已经落到临时文件的请求体，可重复读取用于解析与转发。
type spooledBody struct {
	file     *os.File
	size     int64
	format   string
	encoding string
}

func (b *spooledBody) rewind() error {
	_, err := b.file.Seek(0, io.SeekStart)
	return err
}

func (b *spooledBody) close() {
	_ = b.file.Close()
	_ = os.Remove(b.file.Name())
}

// spoolRequest 把请求体流式写入临时文件：压缩编码按流解压，解压后超过 64MB 立即失败。
func spoolRequest(request *http.Request) (*spooledBody, error) {
	file, err := os.CreateTemp("", "sub2api-enhance-input-*")
	if err != nil {
		return nil, err
	}
	body := &spooledBody{file: file, format: "entity_bytes"}
	reader := io.Reader(request.Body)
	var closeReader func()
	encoding := strings.ToLower(strings.TrimSpace(request.Header.Get("Content-Encoding")))
	switch encoding {
	case "", "identity":
	case "gzip":
		gzipReader, err := gzip.NewReader(request.Body)
		if err != nil {
			body.close()
			return nil, ErrUnsupportedEncoding
		}
		reader, closeReader = gzipReader, func() { _ = gzipReader.Close() }
	case "deflate":
		flateReader := flate.NewReader(request.Body)
		reader, closeReader = flateReader, func() { _ = flateReader.Close() }
	case "zstd":
		zstdReader, err := zstd.NewReader(request.Body, zstd.WithDecoderConcurrency(1), zstd.WithDecoderLowmem(true))
		if err != nil {
			body.close()
			return nil, ErrUnsupportedEncoding
		}
		reader, closeReader = zstdReader, zstdReader.Close
	default:
		body.close()
		return nil, ErrUnsupportedEncoding
	}
	if closeReader != nil {
		defer closeReader()
		// 转发的是解压后的正文，不能把原始编码头交给原版。
		body.encoding = encoding
	}
	written, err := io.Copy(file, io.LimitReader(reader, maxDecompressedBodySize+1))
	if err != nil {
		body.close()
		return nil, err
	}
	if written > maxDecompressedBodySize {
		body.close()
		return nil, ErrBodyTooLarge
	}
	body.size = written
	if err := body.rewind(); err != nil {
		body.close()
		return nil, err
	}
	// multipart 请求只保留文本字段，按用户文本重新封装成可解析的聊天请求。
	if contentType, params, parseErr := mime.ParseMediaType(request.Header.Get("Content-Type")); parseErr == nil && contentType == "multipart/form-data" {
		normalized, err := multipartTextBody(body.file, params["boundary"])
		if err != nil {
			body.close()
			return nil, err
		}
		if err := body.replace(normalized); err != nil {
			body.close()
			return nil, err
		}
		body.format = "multipart_text_fields"
	}
	return body, nil
}

// replace 用重新封装后的正文覆盖临时文件内容。
func (b *spooledBody) replace(raw []byte) error {
	if err := b.file.Truncate(0); err != nil {
		return err
	}
	if _, err := b.file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if _, err := b.file.Write(raw); err != nil {
		return err
	}
	b.size = int64(len(raw))
	return b.rewind()
}

// multipartTextBody 只提取表单文本字段，忽略二进制文件内容，避免整段媒体进入解析。
func multipartTextBody(file *os.File, boundary string) ([]byte, error) {
	if boundary == "" {
		return nil, errors.New("媒体表单缺少 boundary")
	}
	reader := multipart.NewReader(file, boundary)
	texts := []any{}
	for {
		part, err := reader.NextRawPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		name, order := part.FormName(), len(texts)+1
		if part.FileName() == "" {
			raw, err := io.ReadAll(io.LimitReader(part, maxDecompressedBodySize))
			_ = part.Close()
			if err != nil {
				return nil, err
			}
			if utf8.Valid(raw) {
				texts = append(texts, map[string]any{"type": "text", "text": string(raw), "source_name": name, "source_order": order})
			}
			continue
		}
		_, _ = io.Copy(io.Discard, part)
		_ = part.Close()
	}
	return json.Marshal(map[string]any{"messages": []any{map[string]any{"role": "user", "content": texts}}})
}
