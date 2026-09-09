package tools

import (
	"net/http"
	"testing"

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
