package ozon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"path/filepath"
	"sort"
	"strings"
)

// FormFile — файл, прикладываемый к multipart-запросу.
type FormFile struct {
	// Field — имя поля формы (у Ozon для сертификатов это "files").
	Field string
	// Name — имя файла; по расширению определяется тип содержимого.
	Name string
	Data []byte
}

// PostForm отправляет multipart/form-data и возвращает сырой JSON.
//
// Отдельно от Call по двум причинам. Тело здесь — не JSON, и собирать
// его надо иначе. И, главное, повторов нет: Call повторяет запрос при
// 429 и 5xx, что для чтения безопасно, а для создания — нет. Если
// Ozon принял сертификат, но ответ потерялся в пути, повтор создаст
// второй такой же документ. Лимитер при этом соблюдается, как везде.
func (c *Client) PostForm(ctx context.Context, path string, fields map[string]string, files []FormFile) (json.RawMessage, error) {
	body, contentType, err := buildForm(fields, files)
	if err != nil {
		return nil, fmt.Errorf("ozon: сборка формы %s: %w", path, err)
	}

	if c.limiter != nil {
		if err := c.limiter.Wait(ctx, path); err != nil {
			return nil, err
		}
	}
	return c.doAs(ctx, http.MethodPost, path, body, contentType)
}

// buildForm собирает тело multipart. Поля идут в отсортированном
// порядке — ради воспроизводимости тестов и разбора логов.
func buildForm(fields map[string]string, files []FormFile) ([]byte, string, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)

	names := make([]string, 0, len(fields))
	for k := range fields {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		if err := w.WriteField(k, fields[k]); err != nil {
			return nil, "", err
		}
	}

	for _, f := range files {
		h := textproto.MIMEHeader{}
		h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="%s"; filename="%s"`,
			escapeQuotes(f.Field), escapeQuotes(f.Name)))
		h.Set("Content-Type", formFileType(f.Name))
		part, err := w.CreatePart(h)
		if err != nil {
			return nil, "", err
		}
		if _, err := part.Write(f.Data); err != nil {
			return nil, "", err
		}
	}

	if err := w.Close(); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), w.FormDataContentType(), nil
}

var quoteEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\r", "", "\n", "")

func escapeQuotes(s string) string { return quoteEscaper.Replace(s) }

// formFileType определяет тип файла по расширению.
func formFileType(name string) string {
	if t := mime.TypeByExtension(strings.ToLower(filepath.Ext(name))); t != "" {
		return t
	}
	return "application/octet-stream"
}
