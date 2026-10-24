package tools

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MainAlexStark/ozon-seller-mcp/internal/files"
	"github.com/MainAlexStark/ozon-seller-mcp/internal/mcp"
)

// fakePDF — достаточно, чтобы файл узнавался как PDF.
const fakePDF = "%PDF-1.4\n% fake label\n%%EOF\n"

// routeOzon — подставной Ozon, отвечающий по пути и запоминающий
// тела запросов. Сборка ходит в Ozon несколькими вызовами, и проверять
// надо каждый из них, а не последний.
type routeOzon struct {
	mu     sync.Mutex
	routes map[string]func(body map[string]any) (int, string)
	calls  []recordedCall
}

type recordedCall struct {
	Path string
	Body map[string]any
}

func (o *routeOzon) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body := map[string]any{}
		_ = json.NewDecoder(r.Body).Decode(&body)

		o.mu.Lock()
		o.calls = append(o.calls, recordedCall{Path: r.URL.Path, Body: body})
		fn, ok := o.routes[r.URL.Path]
		o.mu.Unlock()

		if !ok {
			t.Errorf("неожиданный вызов %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		status, resp := fn(body)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(resp))
	}
}

func (o *routeOzon) callsTo(path string) []recordedCall {
	o.mu.Lock()
	defer o.mu.Unlock()
	var out []recordedCall
	for _, c := range o.calls {
		if c.Path == path {
			out = append(out, c)
		}
	}
	return out
}

func ok(resp string) func(map[string]any) (int, string) {
	return func(map[string]any) (int, string) { return http.StatusOK, resp }
}

// fbsServer поднимает сервер с папкой для файлов во временном каталоге.
func fbsServer(t *testing.T, mode Mode, o *routeOzon) (*mcp.Server, string, func()) {
	t.Helper()
	reg, server, closeFn := fakeOzon(t, mode, o.handler(t))
	dir := t.TempDir()
	store, err := files.NewDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	reg.SetFiles(store)
	return server, dir, closeFn
}

func TestUnfulfilledGetsWindowAroundNow(t *testing.T) {
	// «Что собирать» без дат. Метод требует пару дат, а просроченный
	// заказ — самый срочный, поэтому окно стоит вокруг «сейчас».
	o := &routeOzon{routes: map[string]func(map[string]any) (int, string){
		"/v4/posting/fbs/unfulfilled/list": ok(`{"postings":[],"count":0,"has_next":false}`),
	}}
	server, _, closeFn := fbsServer(t, ModeReadOnly, o)
	defer closeFn()

	if out, isErr := callTool(t, server, "ozon_fbs_unfulfilled_list", map[string]any{"status": "awaiting_packaging"}); isErr {
		t.Fatalf("вызов без дат должен проходить: %s", out)
	}

	calls := o.callsTo("/v4/posting/fbs/unfulfilled/list")
	if len(calls) != 1 {
		t.Fatalf("ожидался один вызов, было %d", len(calls))
	}
	body := calls[0].Body
	filter, _ := body["filter"].(map[string]any)
	if st, _ := filter["statuses"].([]any); len(st) != 1 || st[0] != "awaiting_packaging" {
		t.Errorf("status не перенесён в filter.statuses: %v", filter)
	}
	if _, leaked := body["status"]; leaked {
		t.Error("status на верхнем уровне не должен уходить в Ozon")
	}

	from, err1 := time.Parse(time.RFC3339, filter["cutoff_from"].(string))
	to, err2 := time.Parse(time.RFC3339, filter["cutoff_to"].(string))
	if err1 != nil || err2 != nil {
		t.Fatalf("окно не в RFC3339: %v", filter)
	}
	if !from.Before(time.Now()) || !to.After(time.Now()) {
		t.Errorf("окно должно охватывать сейчас: %s … %s", from, to)
	}
	if body["limit"] == nil {
		t.Error("лимит должен проставляться по умолчанию")
	}
}

// labelFileServer отдаёт готовый файл этикеток, как CDN Ozon.
func labelFileServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(fakePDF))
	}))
	t.Cleanup(srv.Close)
	labelPollEvery = 5 * time.Millisecond
	return srv
}

func TestLabelsViaV3TaskSavedAsFile(t *testing.T) {
	cdn := labelFileServer(t)
	polls := 0
	o := &routeOzon{routes: map[string]func(map[string]any) (int, string){
		"/v3/posting/fbs/package-label/create": ok(`{"tasks":[{"task_id":8,"task_type":"small_label"},{"task_id":7,"task_type":"big_label"}]}`),
		"/v2/posting/fbs/package-label/get": func(body map[string]any) (int, string) {
			id := body["task_id"].(float64)
			if id == 7 {
				polls++
				if polls == 1 {
					return http.StatusOK, `{"status":{"code":"in_progress"}}`
				}
			}
			return http.StatusOK, `{"status":{"code":"completed","postings_count":2,"printed_postings_count":2},
				"file_url":"` + cdn.URL + `/label-` + strconv.Itoa(int(id)) + `.pdf"}`
		},
	}}
	server, dir, closeFn := fbsServer(t, ModeReadOnly, o)
	defer closeFn()

	out, isErr := callTool(t, server, "ozon_fbs_labels", map[string]any{
		"posting_numbers": []any{"123-0001-1", "123-0002-1", "123-0001-1"},
	})
	if isErr {
		t.Fatalf("этикетки должны печататься и в режиме чтения: %s", out)
	}
	if len(o.callsTo("/v2/posting/fbs/package-label")) != 0 {
		t.Error("устаревший синхронный метод не должен вызываться")
	}
	created := o.callsTo("/v3/posting/fbs/package-label/create")
	nums, _ := created[0].Body["posting_numbers"].([]any)
	if len(nums) != 2 {
		t.Errorf("номера уходят в posting_numbers без повторов: %v", created[0].Body)
	}
	gets := o.callsTo("/v2/posting/fbs/package-label/get")
	if len(gets) < 3 || gets[0].Body["task_id"] != float64(7) {
		t.Errorf("сначала опрашивается обычная этикетка и до готовности: %v", gets)
	}

	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("ожидался один файл, в папке %d", len(entries))
	}
	path := filepath.Join(dir, entries[0].Name())
	data, _ := os.ReadFile(path)
	if string(data) != fakePDF || !strings.HasSuffix(path, ".pdf") {
		t.Errorf("файл этикеток сохранён неверно: %s", path)
	}
	if !strings.Contains(out, path) || strings.Index(out, path) > strings.Index(out, "/label-8.pdf") {
		t.Errorf("первым в ответе — сохранённая обычная этикетка, потом ссылка на маленькую:\n%s", out)
	}
	if strings.Contains(out, "PDF-1.4") {
		t.Error("содержимое файла не должно попадать в ответ")
	}
}

func TestManyLabelsReturnOzonLink(t *testing.T) {
	o := &routeOzon{routes: map[string]func(map[string]any) (int, string){
		"/v3/posting/fbs/package-label/create": ok(`{"tasks":[{"task_id":7,"task_type":"big_label"}]}`),
		"/v2/posting/fbs/package-label/get":    ok(`{"status":{"code":"completed"},"file_url":"https://cdn.ozon.ru/labels/7.pdf"}`),
	}}
	labelFileServer(t)
	server, dir, closeFn := fbsServer(t, ModeReadOnly, o)
	defer closeFn()

	var nums []any
	for i := 0; i < 25; i++ {
		nums = append(nums, "1-"+string(rune('a'+i))+"-1")
	}
	out, isErr := callTool(t, server, "ozon_fbs_labels", map[string]any{"posting_numbers": nums})
	if isErr {
		t.Fatal(out)
	}
	if !strings.Contains(out, "https://cdn.ozon.ru/labels/7.pdf") {
		t.Errorf("в ответе должна быть ссылка на файл Ozon:\n%s", out)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Error("большая пачка не скачивается")
	}
}

func TestLabelsUnprintedExplained(t *testing.T) {
	labelFileServer(t)
	o := &routeOzon{routes: map[string]func(map[string]any) (int, string){
		"/v3/posting/fbs/package-label/create": ok(`{"tasks":[{"task_id":7,"task_type":"big_label"}]}`),
		"/v2/posting/fbs/package-label/get": ok(`{"status":{"code":"error","postings_count":1,"printed_postings_count":0,
			"unprinted_postings":[{"posting_number":"1-1-1","message":"posting is not in awaiting_deliver"}]},
			"error":{"code":"NO_LABELS","message":"no labels"}}`),
	}}
	server, _, closeFn := fbsServer(t, ModeReadOnly, o)
	defer closeFn()

	out, isErr := callTool(t, server, "ozon_fbs_labels", map[string]any{"posting_numbers": []any{"1-1-1"}})
	if !isErr || !strings.Contains(out, "1-1-1: posting is not in awaiting_deliver") || !strings.Contains(out, "awaiting_deliver") {
		t.Errorf("отправление без этикетки должно называться с причиной:\n%s", out)
	}
}

func TestLabelsNotReadyExplained(t *testing.T) {
	o := &routeOzon{routes: map[string]func(map[string]any) (int, string){
		"/v3/posting/fbs/package-label/create": func(map[string]any) (int, string) {
			return http.StatusBadRequest, `{"code":"INVALID_ARGUMENT","message":"The next postings aren't ready: 1-1-1"}`
		},
	}}
	server, _, closeFn := fbsServer(t, ModeReadOnly, o)
	defer closeFn()

	out, isErr := callTool(t, server, "ozon_fbs_labels", map[string]any{"posting_numbers": []any{"1-1-1"}})
	if !isErr {
		t.Fatal("ожидалась ошибка")
	}
	if !strings.Contains(out, "45–60 секунд") {
		t.Errorf("отказ «не готово» должен объясняться:\n%s", out)
	}
}

func TestLabelsBare400NotAboutCategory(t *testing.T) {
	// 400 без пояснения — не повод советовать характеристики категории.
	o := &routeOzon{routes: map[string]func(map[string]any) (int, string){
		"/v3/posting/fbs/package-label/create": func(map[string]any) (int, string) {
			return http.StatusBadRequest, `{"code":"INVALID_ARGUMENT","message":""}`
		},
	}}
	server, _, closeFn := fbsServer(t, ModeReadOnly, o)
	defer closeFn()

	out, isErr := callTool(t, server, "ozon_fbs_labels", map[string]any{"posting_numbers": []any{"1-1-1"}})
	if !isErr || strings.Contains(out, "ozon_category_attributes") || !strings.Contains(out, "awaiting_deliver") {
		t.Errorf("подсказка должна быть про статус отправления:\n%s", out)
	}
}

func TestLabelsWithoutStoreGiveOzonLink(t *testing.T) {
	// Без хранилища файл не теряется: остаётся ссылка Ozon.
	labelFileServer(t)
	o := &routeOzon{routes: map[string]func(map[string]any) (int, string){
		"/v3/posting/fbs/package-label/create": ok(`{"tasks":[{"task_id":7,"task_type":"big_label"}]}`),
		"/v2/posting/fbs/package-label/get":    ok(`{"status":{"code":"completed"},"file_url":"https://cdn.ozon.ru/labels/7.pdf"}`),
	}}
	_, server, closeFn := fakeOzon(t, ModeReadOnly, o.handler(t))
	defer closeFn()

	out, isErr := callTool(t, server, "ozon_fbs_labels", map[string]any{"posting_numbers": []any{"1-1-1"}})
	if isErr || !strings.Contains(out, "https://cdn.ozon.ru/labels/7.pdf") {
		t.Errorf("ожидалась ссылка Ozon:\n%s", out)
	}
}

// shipRoutes — отправление, ожидающее сборки, с двумя товарами.
func shipRoutes(status string) map[string]func(map[string]any) (int, string) {
	return map[string]func(map[string]any) (int, string){
		"/v3/posting/fbs/get": ok(`{"result":{"posting_number":"1-1-1","status":"` + status + `",
			"products":[{"sku":111,"quantity":2,"offer_id":"a"},{"sku":222,"quantity":1,"offer_id":"b"}]}}`),
		"/v4/posting/fbs/ship": ok(`{"result":["1-1-1"]}`),
	}
}

func TestShipBlockedInReadOnly(t *testing.T) {
	o := &routeOzon{routes: shipRoutes("awaiting_packaging")}
	server, _, closeFn := fbsServer(t, ModeReadOnly, o)
	defer closeFn()

	out, isErr := callTool(t, server, "ozon_fbs_ship", map[string]any{"posting_numbers": []any{"1-1-1"}})
	if !isErr || !strings.Contains(out, "прав на запись нет") {
		t.Errorf("сборка должна требовать права записи:\n%s", out)
	}
	if len(o.callsTo("/v4/posting/fbs/ship")) != 0 {
		t.Error("в режиме чтения запрос сборки не должен уходить")
	}
}

func TestShipWholePostingIntoOneBox(t *testing.T) {
	o := &routeOzon{routes: shipRoutes("awaiting_packaging")}
	server, _, closeFn := fbsServer(t, ModeWrite, o)
	defer closeFn()

	out, isErr := callTool(t, server, "ozon_fbs_ship", map[string]any{"posting_numbers": []any{"1-1-1"}})
	if isErr {
		t.Fatal(out)
	}

	calls := o.callsTo("/v4/posting/fbs/ship")
	if len(calls) != 1 {
		t.Fatalf("ожидался один вызов сборки, было %d", len(calls))
	}
	packages, _ := calls[0].Body["packages"].([]any)
	if len(packages) != 1 {
		t.Fatalf("всё должно уйти в одну коробку: %v", packages)
	}
	products, _ := packages[0].(map[string]any)["products"].([]any)
	if len(products) != 2 {
		t.Fatalf("в коробке должны быть оба товара: %v", products)
	}
	first := products[0].(map[string]any)
	if first["product_id"] != float64(111) || first["quantity"] != float64(2) {
		t.Errorf("product_id — это SKU из отправления: %v", first)
	}
	if !strings.Contains(out, `"shipped": 1`) {
		t.Errorf("в отчёте должно быть число собранных:\n%s", out)
	}
}

func TestShipRefusesWrongStatusBeforeCallingOzon(t *testing.T) {
	o := &routeOzon{routes: shipRoutes("awaiting_deliver")}
	server, _, closeFn := fbsServer(t, ModeWrite, o)
	defer closeFn()

	out, isErr := callTool(t, server, "ozon_fbs_ship", map[string]any{"posting_numbers": []any{"1-1-1"}})
	if !isErr || !strings.Contains(out, "awaiting_packaging") {
		t.Errorf("уже собранное отправление должно объясняться статусом:\n%s", out)
	}
	if len(o.callsTo("/v4/posting/fbs/ship")) != 0 {
		t.Error("сборка не должна вызываться для неподходящего статуса")
	}
}

func TestShipReportsPartialFailure(t *testing.T) {
	// Ошибка на одном отправлении не останавливает остальные, и отчёт
	// называет и собранные, и несобранные.
	routes := shipRoutes("awaiting_packaging")
	routes["/v4/posting/fbs/ship"] = func(body map[string]any) (int, string) {
		if body["posting_number"] == "2-2-2" {
			return http.StatusBadRequest, `{"code":"INVALID_ARGUMENT","message":"posting status is not awaiting_packaging"}`
		}
		return http.StatusOK, `{"result":["1-1-1"]}`
	}
	o := &routeOzon{routes: routes}
	server, _, closeFn := fbsServer(t, ModeWrite, o)
	defer closeFn()

	out, isErr := callTool(t, server, "ozon_fbs_ship", map[string]any{
		"posting_number":  "",
		"posting_numbers": []any{"1-1-1", "2-2-2"},
	})
	if isErr {
		t.Fatalf("частичный успех — не ошибка вызова:\n%s", out)
	}
	if !strings.Contains(out, `"failed": 1`) || !strings.Contains(out, "2-2-2") {
		t.Errorf("отчёт должен назвать несобранное:\n%s", out)
	}
}

func TestShipPackagesNeedSinglePosting(t *testing.T) {
	o := &routeOzon{routes: shipRoutes("awaiting_packaging")}
	server, _, closeFn := fbsServer(t, ModeWrite, o)
	defer closeFn()

	out, isErr := callTool(t, server, "ozon_fbs_ship", map[string]any{
		"packages": []any{map[string]any{"products": []any{map[string]any{"product_id": 111, "quantity": 1}}}},
	})
	if !isErr || !strings.Contains(out, "posting_number") {
		t.Errorf("раскладка без номера отправления должна отклоняться:\n%s", out)
	}
}

func TestShipCustomPackagesPassedAsIs(t *testing.T) {
	o := &routeOzon{routes: shipRoutes("awaiting_packaging")}
	server, _, closeFn := fbsServer(t, ModeWrite, o)
	defer closeFn()

	boxes := []any{
		map[string]any{"products": []any{map[string]any{"product_id": 111, "quantity": 2}}},
		map[string]any{"products": []any{map[string]any{"product_id": 222, "quantity": 1}}},
	}
	if out, isErr := callTool(t, server, "ozon_fbs_ship", map[string]any{
		"posting_number": "1-1-1", "packages": boxes,
	}); isErr {
		t.Fatal(out)
	}
	if len(o.callsTo("/v3/posting/fbs/get")) != 0 {
		t.Error("при заданной раскладке состав запрашивать незачем")
	}
	calls := o.callsTo("/v4/posting/fbs/ship")
	if packages, _ := calls[0].Body["packages"].([]any); len(packages) != 2 {
		t.Errorf("раскладка должна уйти как задана: %v", calls[0].Body)
	}
}

func TestCancelNeedsConfirmation(t *testing.T) {
	o := &routeOzon{routes: map[string]func(map[string]any) (int, string){
		"/v2/posting/fbs/cancel": ok(`{"result":true}`),
	}}
	server, _, closeFn := fbsServer(t, ModeWrite, o)
	defer closeFn()

	out, isErr := callTool(t, server, "ozon_fbs_cancel", map[string]any{
		"posting_number": "1-1-1", "cancel_reason_id": 352,
	})
	if !isErr || !strings.Contains(out, "confirm_cancel") {
		t.Errorf("отмена без подтверждения должна останавливаться:\n%s", out)
	}

	out, isErr = callTool(t, server, "ozon_fbs_cancel", map[string]any{
		"posting_number": "1-1-1", "cancel_reason_id": 402, "confirm_cancel": true,
	})
	if !isErr || !strings.Contains(out, "cancel_reason_message") {
		t.Errorf("причина 402 без пояснения должна отклоняться:\n%s", out)
	}
	if len(o.callsTo("/v2/posting/fbs/cancel")) != 0 {
		t.Fatal("ни одна из отклонённых отмен не должна была уйти в Ozon")
	}

	if out, isErr = callTool(t, server, "ozon_fbs_cancel", map[string]any{
		"posting_number": "1-1-1", "cancel_reason_id": 352, "confirm_cancel": true,
	}); isErr {
		t.Fatal(out)
	}
	calls := o.callsTo("/v2/posting/fbs/cancel")
	if len(calls) != 1 {
		t.Fatalf("подтверждённая отмена должна уйти один раз, ушло %d", len(calls))
	}
	if _, leaked := calls[0].Body["confirm_cancel"]; leaked {
		t.Error("confirm_cancel — наш аргумент, в Ozon он уходить не должен")
	}
}

func TestStatusSetRoutesByStatus(t *testing.T) {
	o := &routeOzon{routes: map[string]func(map[string]any) (int, string){
		"/v2/fbs/posting/last-mile": ok(`{"result":[{"posting_number":"1-1-1","result":true}]}`),
	}}
	server, _, closeFn := fbsServer(t, ModeWrite, o)
	defer closeFn()

	if out, isErr := callTool(t, server, "ozon_fbs_status_set", map[string]any{
		"posting_numbers": []any{"1-1-1"}, "status": "last_mile",
	}); isErr {
		t.Fatal(out)
	}
	if len(o.callsTo("/v2/fbs/posting/last-mile")) != 1 {
		t.Error("статус last_mile должен уходить в свой метод")
	}

	out, isErr := callTool(t, server, "ozon_fbs_status_set", map[string]any{
		"posting_numbers": []any{"1-1-1"}, "status": "lost",
	})
	if !isErr || !strings.Contains(out, "допустимо") {
		t.Errorf("неизвестный статус должен отклоняться:\n%s", out)
	}
}

func TestActCreateNormalizesDate(t *testing.T) {
	o := &routeOzon{routes: map[string]func(map[string]any) (int, string){
		"/v2/posting/fbs/act/create": ok(`{"result":{"id":42}}`),
	}}
	server, _, closeFn := fbsServer(t, ModeWrite, o)
	defer closeFn()

	if out, isErr := callTool(t, server, "ozon_fbs_act_create", map[string]any{
		"delivery_method_id": 5, "departure_date": "2026-09-28",
	}); isErr {
		t.Fatal(out)
	}
	body := o.callsTo("/v2/posting/fbs/act/create")[0].Body
	got, _ := body["departure_date"].(string)
	d, err := time.Parse(time.RFC3339, got)
	if err != nil {
		t.Fatalf("дата должна уйти в RFC3339: %q", got)
	}
	if d.UTC().Format(time.RFC3339) != "2026-09-27T21:00:00Z" {
		t.Errorf("день понимается как полночь по Москве, получено %s", d.UTC())
	}
	if _, has := body["containers_count"]; has {
		t.Error("containers_count без доверительной приёмки не передаётся")
	}
}

func TestActBarcodeGivesImageAndText(t *testing.T) {
	png := "\x89PNG\r\n\x1a\n" + strings.Repeat("\x00", 16)
	o := &routeOzon{routes: map[string]func(map[string]any) (int, string){
		"/v2/posting/fbs/act/get-barcode/text": ok(`{"result":"4600000000042"}`),
		"/v2/posting/fbs/act/get-barcode":      ok(png),
	}}
	server, dir, closeFn := fbsServer(t, ModeReadOnly, o)
	defer closeFn()

	out, isErr := callTool(t, server, "ozon_fbs_act_documents", map[string]any{"id": 42, "doc": "barcode"})
	if isErr {
		t.Fatal(out)
	}
	if !strings.Contains(out, "4600000000042") {
		t.Errorf("в ответе должно быть значение штрихкода:\n%s", out)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || filepath.Ext(entries[0].Name()) != ".png" {
		t.Errorf("картинка штрихкода должна сохраниться как .png: %v", entries)
	}
}

func TestActDocumentRejectsUnknownDoc(t *testing.T) {
	o := &routeOzon{routes: map[string]func(map[string]any) (int, string){}}
	server, _, closeFn := fbsServer(t, ModeReadOnly, o)
	defer closeFn()

	out, isErr := callTool(t, server, "ozon_fbs_act_documents", map[string]any{"id": 42, "doc": "invoice"})
	if !isErr || !strings.Contains(out, "допустимо") {
		t.Errorf("неизвестный документ должен отклоняться до запроса:\n%s", out)
	}
}

func TestCancelReasonsPicksMethod(t *testing.T) {
	o := &routeOzon{routes: map[string]func(map[string]any) (int, string){
		"/v2/posting/fbs/cancel-reason/list": ok(`{"result":[]}`),
		"/v1/posting/fbs/cancel-reason":      ok(`{"result":[]}`),
	}}
	server, _, closeFn := fbsServer(t, ModeReadOnly, o)
	defer closeFn()

	callTool(t, server, "ozon_fbs_cancel_reasons", map[string]any{})
	callTool(t, server, "ozon_fbs_cancel_reasons", map[string]any{"posting_numbers": []any{"1-1-1"}})

	if len(o.callsTo("/v2/posting/fbs/cancel-reason/list")) != 1 {
		t.Error("без номеров — общий справочник")
	}
	per := o.callsTo("/v1/posting/fbs/cancel-reason")
	if len(per) != 1 || per[0].Body["related_posting_numbers"] == nil {
		t.Errorf("с номерами — причины для отправлений: %v", per)
	}
}

func TestPostingsListSpeaksV4(t *testing.T) {
	// Старые вызовы (dir, offset, status строкой, limit 1000) должны
	// уходить в /v4/posting/fbs/list в новом формате, а не отказом.
	o := &routeOzon{routes: map[string]func(map[string]any) (int, string){
		"/v4/posting/fbs/list": ok(`{"postings":[],"has_next":false,"cursor":""}`),
	}}
	server, _, closeFn := fbsServer(t, ModeReadOnly, o)
	defer closeFn()

	args := map[string]any{
		"dir":    "asc",
		"offset": 50,
		"limit":  1000,
		"filter": map[string]any{"status": "awaiting_packaging"},
	}
	if out, isErr := callTool(t, server, "ozon_postings_list", args); isErr {
		t.Fatalf("вызов должен проходить: %s", out)
	}
	calls := o.callsTo("/v4/posting/fbs/list")
	if len(calls) != 1 {
		t.Fatalf("ожидался один вызов, было %d", len(calls))
	}
	body := calls[0].Body
	if body["sort_dir"] != "ASC" {
		t.Errorf("sort_dir: %v", body["sort_dir"])
	}
	for _, k := range []string{"dir", "offset", "status"} {
		if _, leaked := body[k]; leaked {
			t.Errorf("%s не должен уходить в v4", k)
		}
	}
	if l, _ := body["limit"].(float64); l != 100 {
		t.Errorf("limit не опущен до 100: %v", body["limit"])
	}
	filter, _ := body["filter"].(map[string]any)
	if st, _ := filter["statuses"].([]any); len(st) != 1 || st[0] != "awaiting_packaging" {
		t.Errorf("statuses: %v", filter)
	}
	if filter["since"] == nil || filter["to"] == nil {
		t.Errorf("период не подставлен: %v", filter)
	}
}
