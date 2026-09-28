package ozon

import (
	"encoding/base64"
	"testing"
)

func TestDecodeFileVariants(t *testing.T) {
	pdf := []byte("%PDF-1.4 test")

	f, err := decodeFile("/x", pdf)
	if err != nil || f.ContentType != "application/pdf" || string(f.Data) != string(pdf) {
		t.Errorf("двоичный PDF: %+v %v", f, err)
	}

	env := `{"file_content":"` + base64.StdEncoding.EncodeToString(pdf) + `","file_name":"a.pdf","content_type":"application/pdf"}`
	f, err = decodeFile("/x", []byte(env))
	if err != nil || f.Name != "a.pdf" || string(f.Data) != string(pdf) {
		t.Errorf("JSON с base64: %+v %v", f, err)
	}

	env = `{"file_content":"` + base64.RawURLEncoding.EncodeToString(pdf) + `"}`
	f, err = decodeFile("/x", []byte(env))
	if err != nil || f.ContentType != "application/pdf" {
		t.Errorf("URL-алфавит без дополнения и без content_type: %+v %v", f, err)
	}

	if _, err := decodeFile("/x", []byte(`{"result":{}}`)); err == nil {
		t.Error("JSON без содержимого — ошибка, а не пустой файл")
	}
	if _, err := decodeFile("/x", nil); err == nil {
		t.Error("пустой ответ — ошибка")
	}
}
