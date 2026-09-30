package ozon

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestPostFormSendsMultipart(t *testing.T) {
	var gotFields = map[string]string{}
	var gotFile, gotFileName, gotType, gotAuth string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Client-Id") + "/" + r.Header.Get("Api-Key")
		if !strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data; boundary=") {
			t.Errorf("Content-Type: %q", r.Header.Get("Content-Type"))
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Fatalf("тело не multipart: %v", err)
		}
		for k, v := range r.MultipartForm.Value {
			gotFields[k] = v[0]
		}
		fh := r.MultipartForm.File["files"]
		if len(fh) != 1 {
			t.Fatalf("файлов: %d", len(fh))
		}
		gotFileName = fh[0].Filename
		gotType = fh[0].Header.Get("Content-Type")
		f, _ := fh[0].Open()
		b, _ := io.ReadAll(f)
		gotFile = string(b)
		_, _ = w.Write([]byte(`{"id":42}`))
	}))
	defer srv.Close()

	c := New("cid", "key", WithBaseURL(srv.URL))
	raw, err := c.PostForm(context.Background(), "/v1/product/certificate/create",
		map[string]string{"name": `Сертификат "А"`, "number": "RU-1"},
		[]FormFile{{Field: "files", Name: "scan.pdf", Data: []byte("%PDF-1.4")}})
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"id":42}` {
		t.Errorf("ответ: %s", raw)
	}
	if gotFields["number"] != "RU-1" || gotFields["name"] != `Сертификат "А"` {
		t.Errorf("поля: %v", gotFields)
	}
	if gotFile != "%PDF-1.4" || gotFileName != "scan.pdf" || gotType != "application/pdf" {
		t.Errorf("файл: %q %q %q", gotFile, gotFileName, gotType)
	}
	if gotAuth != "cid/key" {
		t.Errorf("авторизация: %q", gotAuth)
	}
}

func TestPostFormDoesNotRetryServerErrors(t *testing.T) {
	// Повтор создания на 5xx мог бы породить дубликат документа.
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"message":"boom"}`))
	}))
	defer srv.Close()

	c := New("cid", "key", WithBaseURL(srv.URL), WithMaxRetries(3))
	_, err := c.PostForm(context.Background(), "/v1/product/certificate/create", nil,
		[]FormFile{{Field: "files", Name: "a.png", Data: []byte{1}}})
	if err == nil {
		t.Fatal("ожидалась ошибка")
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Errorf("запросов %d, ожидался один", n)
	}
}
