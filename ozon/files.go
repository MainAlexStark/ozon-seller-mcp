package ozon

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// File — документ, который Ozon отдаёт файлом: этикетки, акт,
// накладная, штрихкод отгрузки.
type File struct {
	Name        string
	ContentType string
	Data        []byte
}

// CallFile выполняет POST к методу, отвечающему файлом.
//
// Такие методы отвечают по-разному, и разница не описана нигде, кроме
// самих ответов: одни присылают PDF как есть (тело начинается с %PDF),
// другие — JSON с полями file_content (base64), file_name и content_type.
// Какой вариант придёт, зависит от метода и, судя по всему, от версии
// шлюза, поэтому здесь понимаются оба, а не тот, что записан в
// документации сегодня.
func (c *Client) CallFile(ctx context.Context, path string, payload any) (File, error) {
	raw, err := c.Call(ctx, path, payload)
	if err != nil {
		return File{}, err
	}
	return decodeFile(path, raw)
}

// decodeFile разбирает ответ файлового метода.
func decodeFile(path string, raw []byte) (File, error) {
	if len(raw) == 0 {
		return File{}, fmt.Errorf("ozon: %s вернул пустой ответ вместо файла", path)
	}

	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		// Двоичный ответ. Тип определяем по содержимому: заголовок
		// Content-Type до этого места не доезжает, а по сигнатуре PDF
		// и PNG узнаются надёжно.
		return File{ContentType: sniff(raw), Data: raw}, nil
	}

	var envelope struct {
		FileContent string `json:"file_content"`
		FileName    string `json:"file_name"`
		ContentType string `json:"content_type"`
		Content     string `json:"content"`
	}
	if err := json.Unmarshal(trimmed, &envelope); err != nil {
		return File{}, fmt.Errorf("ozon: %s: ответ не файл и не JSON: %w", path, err)
	}

	content := envelope.FileContent
	if content == "" {
		content = envelope.Content
	}
	if content == "" {
		return File{}, fmt.Errorf("ozon: %s: в ответе нет содержимого файла: %s", path, clip(string(trimmed), 300))
	}

	data, err := decodeBase64(content)
	if err != nil {
		// Не base64 — значит, содержимое пришло строкой как есть.
		// Так бывает у текстовых файлов; для PDF это была бы ошибка,
		// и её покажет sniff, вернув text/plain.
		data = []byte(content)
	}

	ct := envelope.ContentType
	if ct == "" {
		ct = sniff(data)
	}
	return File{Name: envelope.FileName, ContentType: ct, Data: data}, nil
}

// decodeBase64 понимает и стандартный алфавит, и URL-безопасный,
// с дополнением и без: какой именно пришлёт шлюз, заранее не сказать.
func decodeBase64(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding,
		base64.URLEncoding, base64.RawURLEncoding,
	} {
		if data, err := enc.DecodeString(s); err == nil {
			return data, nil
		}
	}
	return nil, fmt.Errorf("не base64")
}

// sniff определяет тип файла по содержимому.
func sniff(data []byte) string {
	if bytes.HasPrefix(data, []byte("%PDF")) {
		return "application/pdf"
	}
	return http.DetectContentType(data)
}

// clip укорачивает строку для сообщения об ошибке.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
