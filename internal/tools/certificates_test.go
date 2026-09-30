package tools

import (
	"encoding/base64"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MainAlexStark/ozon-seller-mcp/internal/files"
	"github.com/MainAlexStark/ozon-seller-mcp/ozon"
)

func TestCertificatesFillPaging(t *testing.T) {
	// Без page_size Ozon отвечает пустым списком вместо первой
	// страницы — то есть выглядит как «документов нет».
	var body map[string]any
	_, server, closeFn := fakeOzon(t, ModeReadOnly, captureBody(t, &body))
	defer closeFn()

	for _, tool := range []string{"ozon_certificates_list", "ozon_certification_required"} {
		if _, isErr := callTool(t, server, tool, map[string]any{}); isErr {
			t.Fatalf("%s: вызов без аргументов должен проходить", tool)
		}
		if body["page"] != float64(1) || body["page_size"] == nil {
			t.Errorf("%s: постраничность не достроена: %v", tool, body)
		}
	}
}

func TestCertificateBindSendsNumbers(t *testing.T) {
	// Идентификаторы товаров здесь обязаны быть числами: строку
	// "123456" эти методы не принимают.
	var body map[string]any
	_, server, closeFn := fakeOzon(t, ModeWrite, captureBody(t, &body))
	defer closeFn()

	if _, isErr := callTool(t, server, "ozon_certificate_bind", map[string]any{
		"certificate_id": 77,
		"product_id":     []any{"123456", 789},
	}); isErr {
		t.Fatal("строковые идентификаторы должны приводиться к числам")
	}

	ids, ok := body["product_id"].([]any)
	if !ok || len(ids) != 2 {
		t.Fatalf("product_id не отправлены: %v", body)
	}
	for _, id := range ids {
		if _, isNumber := id.(float64); !isNumber {
			t.Errorf("идентификатор ушёл не числом: %T (%v)", id, id)
		}
	}
	if body["certificate_id"] != float64(77) {
		t.Errorf("certificate_id не отправлен: %v", body["certificate_id"])
	}
}

func TestCertificateWritesNeedWriteMode(t *testing.T) {
	_, server, closeFn := fakeOzon(t, ModeReadOnly, func(w http.ResponseWriter, r *http.Request) {
		t.Error("запрос не должен был уйти в Ozon")
	})
	defer closeFn()

	cases := []struct {
		tool string
		args map[string]any
	}{
		{"ozon_certificate_bind", map[string]any{"certificate_id": 1, "product_id": []any{1}}},
		{"ozon_certificate_unbind", map[string]any{"certificate_id": 1, "product_id": []any{1}}},
		{"ozon_barcode_generate", map[string]any{"product_ids": []any{1}}},
		{"ozon_barcode_bind", map[string]any{"barcodes": []any{map[string]any{"barcode": "460", "sku": 1}}}},
	}

	for _, c := range cases {
		if _, isErr := callTool(t, server, c.tool, c.args); !isErr {
			t.Errorf("%s должен отказывать в режиме чтения", c.tool)
		}
	}
}

func TestBarcodeToolsLimitBatchSize(t *testing.T) {
	// Пачка ограничена дважды: общим заслоном на размер записи и
	// пределом самого метода. Важно, что запрос при этом не уходит:
	// узнать «каким товарам штрихкод успели присвоить» после отказа
	// посреди пачки неоткуда.
	var reached bool
	_, server, closeFn := fakeOzon(t, ModeWrite, func(w http.ResponseWriter, r *http.Request) {
		reached = true
	})
	defer closeFn()

	ids := make([]any, maxBarcodeItems+1)
	for i := range ids {
		ids[i] = i + 1
	}

	body, isErr := callTool(t, server, "ozon_barcode_generate", map[string]any{"product_ids": ids})
	if !isErr {
		t.Fatal("превышение размера пачки должно отклоняться")
	}
	if reached {
		t.Error("запрос не должен был уйти в Ozon")
	}
	if !contains(body, "100") {
		t.Errorf("сообщение должно называть предел: %s", body)
	}
}

func TestBarcodeGenerateSendsProductIDs(t *testing.T) {
	var body map[string]any
	_, server, closeFn := fakeOzon(t, ModeWrite, captureBody(t, &body))
	defer closeFn()

	if _, isErr := callTool(t, server, "ozon_barcode_generate", map[string]any{
		"product_ids": []any{111, 222},
	}); isErr {
		t.Fatal("вызов должен проходить")
	}
	ids, ok := body["product_ids"].([]any)
	if !ok || len(ids) != 2 {
		t.Fatalf("product_ids не отправлены: %v", body)
	}
}

func TestNewPathsAreProbedBySelftest(t *testing.T) {
	// Пути собраны из документации и клиентских библиотек, но
	// проверить их можно только живым ключом. Самодиагностика —
	// единственное место, где это произойдёт.
	probed := map[string]bool{}
	for _, p := range probes() {
		probed[p.Path] = true
	}

	for _, path := range []string{
		ozon.PathReturnsList,
		ozon.PathQuestionCount,
		ozon.PathCertificationList,
		ozon.PathCertificateList,
	} {
		if !probed[path] {
			t.Errorf("путь %s не проверяется в ozon_api_selftest", path)
		}
	}
}

// --- Справочники, info, delete ---

func TestCertificateReferencesUseGetWhereOzonRequiresIt(t *testing.T) {
	// types и accordance-types на POST отвечают 404: это GET без тела.
	for tool, path := range map[string]string{
		"ozon_certificate_types":             ozon.PathCertificateTypes,
		"ozon_certificate_accordance_types":  ozon.PathCertificateAccordance,
		"ozon_certificate_statuses":          ozon.PathCertificateStatuses,
		"ozon_certificate_rejection_reasons": ozon.PathCertificateRejections,
		"ozon_certificate_product_statuses":  ozon.PathCertificateProductState,
	} {
		var method, gotPath string
		_, server, closeFn := fakeOzon(t, ModeReadOnly, func(w http.ResponseWriter, r *http.Request) {
			method, gotPath = r.Method, r.URL.Path
			_, _ = w.Write([]byte(`{"result":[]}`))
		})
		if _, isErr := callTool(t, server, tool, map[string]any{}); isErr {
			t.Fatalf("%s: справочник должен читаться без аргументов", tool)
		}
		closeFn()

		wantMethod := http.MethodPost
		if tool == "ozon_certificate_types" || tool == "ozon_certificate_accordance_types" {
			wantMethod = http.MethodGet
		}
		if gotPath != path || method != wantMethod {
			t.Errorf("%s: ушло %s %s, ожидалось %s %s", tool, method, gotPath, wantMethod, path)
		}
	}
}

func TestCertificateInfoSendsNumber(t *testing.T) {
	var body map[string]any
	_, server, closeFn := fakeOzon(t, ModeReadOnly, captureBody(t, &body))
	defer closeFn()

	if _, isErr := callTool(t, server, "ozon_certificate_info",
		map[string]any{"certificate_number": "  RU C-CN.123  "}); isErr {
		t.Fatal("info должен проходить")
	}
	if body["certificate_number"] != "RU C-CN.123" {
		t.Errorf("номер не очищен от пробелов: %v", body)
	}

	if _, isErr := callTool(t, server, "ozon_certificate_info", map[string]any{}); !isErr {
		t.Error("без номера вызов должен отклоняться")
	}
}

func TestCertificateDeleteNeedsConfirmation(t *testing.T) {
	var body map[string]any
	called := false
	_, server, closeFn := fakeOzon(t, ModeWrite, func(w http.ResponseWriter, r *http.Request) {
		called = true
		captureBody(t, &body)(w, r)
	})
	defer closeFn()

	text, isErr := callTool(t, server, "ozon_certificate_delete", map[string]any{"certificate_id": 77})
	if !isErr || called {
		t.Fatalf("без confirm_delete запрос не должен уйти: isErr=%v called=%v", isErr, called)
	}
	if !strings.Contains(text, "confirm_delete") {
		t.Errorf("отказ не объясняет, чего не хватает: %s", text)
	}

	if _, isErr := callTool(t, server, "ozon_certificate_delete",
		map[string]any{"certificate_id": 77, "confirm_delete": true}); isErr {
		t.Fatal("с подтверждением удаление должно проходить")
	}
	if body["certificate_id"] != float64(77) {
		t.Errorf("certificate_id не отправлен: %v", body)
	}
	if _, has := body["confirm_delete"]; has {
		t.Error("confirm_delete — наш заслон, в Ozon он уходить не должен")
	}
}

func TestCertificateDeleteBlockedInReadOnly(t *testing.T) {
	_, server, closeFn := fakeOzon(t, ModeReadOnly, func(w http.ResponseWriter, r *http.Request) {
		t.Error("запрос не должен был уйти в Ozon")
	})
	defer closeFn()

	if _, isErr := callTool(t, server, "ozon_certificate_delete",
		map[string]any{"certificate_id": 1, "confirm_delete": true}); !isErr {
		t.Error("в режиме чтения удаление должно блокироваться")
	}
}

// --- Загрузка ---

// multipartOzon запоминает разобранную форму.
type multipartOzon struct {
	fields map[string]string
	files  map[string]string // имя файла -> содержимое
	calls  int
}

func (m *multipartOzon) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m.calls++
		if r.URL.Path != ozon.PathCertificateCreate {
			t.Errorf("неожиданный путь %s", r.URL.Path)
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("тело не multipart: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		m.fields = map[string]string{}
		for k, v := range r.MultipartForm.Value {
			m.fields[k] = v[0]
		}
		m.files = map[string]string{}
		for _, fh := range r.MultipartForm.File["files"] {
			f, _ := fh.Open()
			b, _ := io.ReadAll(f)
			m.files[fh.Filename] = string(b)
		}
		_, _ = w.Write([]byte(`{"id":501}`))
	}
}

func validCreateArgs() map[string]any {
	return map[string]any{
		"name":                 "Сертификат на фигурки",
		"number":               "RU C-CN.АБ12.В.00001",
		"type_code":            "certificate_of_conformity",
		"accordance_type_code": "mandatory",
		"issue_date":           "2026-03-01",
		"expire_date":          "2029-02-28",
		"files": []any{map[string]any{
			"file_base64": base64.StdEncoding.EncodeToString([]byte(fakePDF)),
			"file_name":   "scan.pdf",
		}},
	}
}

func TestCertificateCreateUploadsMultipart(t *testing.T) {
	m := &multipartOzon{}
	_, server, closeFn := fakeOzon(t, ModeWrite, m.handler(t))
	defer closeFn()

	text, isErr := callTool(t, server, "ozon_certificate_create", validCreateArgs())
	if isErr {
		t.Fatalf("загрузка должна проходить: %s", text)
	}
	if !strings.Contains(text, "501") {
		t.Errorf("идентификатор нового документа не дошёл до модели: %s", text)
	}
	if m.fields["number"] != "RU C-CN.АБ12.В.00001" || m.fields["type_code"] != "certificate_of_conformity" ||
		m.fields["accordance_type_code"] != "mandatory" {
		t.Errorf("поля формы: %v", m.fields)
	}
	if m.fields["issue_date"] != "2026-03-01T00:00:00Z" || m.fields["expire_date"] != "2029-02-28T00:00:00Z" {
		t.Errorf("даты: %v", m.fields)
	}
	if m.files["scan.pdf"] != fakePDF {
		t.Errorf("файл не доехал целиком: %q", m.files)
	}
}

func TestCertificateCreateBlockedInReadOnly(t *testing.T) {
	m := &multipartOzon{}
	_, server, closeFn := fakeOzon(t, ModeReadOnly, m.handler(t))
	defer closeFn()

	if _, isErr := callTool(t, server, "ozon_certificate_create", validCreateArgs()); !isErr || m.calls != 0 {
		t.Errorf("в режиме чтения загрузка должна блокироваться: isErr=%v, вызовов %d", isErr, m.calls)
	}
}

func TestCertificateCreateFilePathOnlyForLocalStdio(t *testing.T) {
	dir := t.TempDir()
	scan := filepath.Join(dir, "decl.pdf")
	if err := os.WriteFile(scan, []byte(fakePDF), 0o600); err != nil {
		t.Fatal(err)
	}
	args := validCreateArgs()
	args["files"] = []any{map[string]any{"file_path": scan}}

	// Сервис: хранилище в памяти — путь означал бы файл на сервере.
	m := &multipartOzon{}
	reg, server, closeFn := fakeOzon(t, ModeWrite, m.handler(t))
	reg.SetFiles(files.NewMemory("https://x.example", time.Hour, 1<<20))
	text, isErr := callTool(t, server, "ozon_certificate_create", args)
	closeFn()
	if !isErr || m.calls != 0 || !strings.Contains(text, "file_base64") {
		t.Errorf("в сервисе file_path должен отклоняться с подсказкой про base64: isErr=%v calls=%d %s", isErr, m.calls, text)
	}

	// Хранилища нет вовсе — тоже не локальный запуск.
	m = &multipartOzon{}
	_, server, closeFn = fakeOzon(t, ModeWrite, m.handler(t))
	_, isErr = callTool(t, server, "ozon_certificate_create", args)
	closeFn()
	if !isErr || m.calls != 0 {
		t.Error("без папки файлов путь на диске читаться не должен")
	}

	// stdio: папка на диске — путь читается, имя берётся из пути.
	m = &multipartOzon{}
	reg, server, closeFn = fakeOzon(t, ModeWrite, m.handler(t))
	defer closeFn()
	store, err := files.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	reg.SetFiles(store)
	if text, isErr := callTool(t, server, "ozon_certificate_create", args); isErr {
		t.Fatalf("в stdio путь должен читаться: %s", text)
	}
	if m.files["decl.pdf"] != fakePDF {
		t.Errorf("файл с диска не доехал: %q", m.files)
	}
}

func TestCertificateCreateRejectsBadInput(t *testing.T) {
	cases := map[string]func(a map[string]any){
		"нет номера":       func(a map[string]any) { delete(a, "number") },
		"нет типа":         func(a map[string]any) { delete(a, "type_code") },
		"нет даты выдачи":  func(a map[string]any) { delete(a, "issue_date") },
		"кривая дата":      func(a map[string]any) { a["issue_date"] = "01.03.2026" },
		"конец раньше":     func(a map[string]any) { a["expire_date"] = "2020-01-01" },
		"нет сканов":       func(a map[string]any) { a["files"] = []any{} },
		"длинное название": func(a map[string]any) { a["name"] = strings.Repeat("я", 101) },
		"не тот формат": func(a map[string]any) {
			a["files"] = []any{map[string]any{"file_base64": "AAAA", "file_name": "scan.exe"}}
		},
		"base64 без имени": func(a map[string]any) {
			a["files"] = []any{map[string]any{"file_base64": "AAAA"}}
		},
		"не base64": func(a map[string]any) {
			a["files"] = []any{map[string]any{"file_base64": "!!!", "file_name": "a.pdf"}}
		},
		"и путь, и base64": func(a map[string]any) {
			a["files"] = []any{map[string]any{"file_base64": "AAAA", "file_name": "a.pdf", "file_path": "/x/a.pdf"}}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			m := &multipartOzon{}
			_, server, closeFn := fakeOzon(t, ModeWrite, m.handler(t))
			defer closeFn()

			args := validCreateArgs()
			mutate(args)
			if _, isErr := callTool(t, server, "ozon_certificate_create", args); !isErr {
				t.Error("вызов должен отклоняться")
			}
			if m.calls != 0 {
				t.Error("некорректный запрос не должен доходить до Ozon")
			}
		})
	}
}

func TestCertificateCreateFileNameCannotEscape(t *testing.T) {
	// Имя файла уходит в заголовок формы: путь и кавычки в нём — мусор.
	m := &multipartOzon{}
	_, server, closeFn := fakeOzon(t, ModeWrite, m.handler(t))
	defer closeFn()

	args := validCreateArgs()
	args["files"] = []any{map[string]any{
		"file_base64": base64.StdEncoding.EncodeToString([]byte(fakePDF)),
		"file_name":   `../../etc/"x".pdf`,
	}}
	if text, isErr := callTool(t, server, "ozon_certificate_create", args); isErr {
		t.Fatalf("%s", text)
	}
	for name := range m.files {
		if strings.ContainsAny(name, `/\`) {
			t.Errorf("в имени файла остался путь: %q", name)
		}
	}
}
