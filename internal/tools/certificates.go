package tools

import (
	"fmt"

	"github.com/MainAlexStark/ozon-seller-mcp/ozon"
)

// RegisterCertificates добавляет сертификаты, декларации и штрихкоды —
// документальную сторону карточки.
//
// Почему это одна тема. И там, и там речь про условие, без которого
// товар не продаётся, при том что с самой карточкой всё в порядке:
// без штрихкода товар не примут на складе, без сертификата карточку
// не выпустят в продажу в категории, где он обязателен. Обе проверки
// срабатывают поздно — на приёмке или на модерации, — и обе видны
// заранее, если знать, куда смотреть.
//
// Чего здесь нет: загрузки файла сертификата. Метод
// /v1/product/certificate/create принимает multipart/form-data, а
// клиент этого пакета отправляет JSON; переписывать клиент ради одной
// операции, которая всё равно требует человека с документом на руках,
// смысла мало. Поэтому файл загружается в кабинете, а через MCP
// доступно всё вокруг: нужен ли сертификат вообще, какие уже
// загружены, к чему привязаны и прошли ли проверку.
func (r *Registry) RegisterCertificates() {
	// --- Сертификаты: чтение ---

	r.Add(Spec{
		Name: "ozon_certification_required",
		Path: ozon.PathCertificationList,
		Desc: "Категории, для которых Ozon требует сертификат или декларацию, с признаком обязательности. " +
			"Смотрите сюда до создания карточки: отказ модерации из-за недостающего документа " +
			"обнаруживается через сутки, а этот список — сразу.",
		Schema: schema(obj{
			"page":      obj{"type": "integer", "default": 1, "minimum": 1},
			"page_size": obj{"type": "integer", "default": 100, "maximum": 1000},
		}),
		Build: func(a map[string]any) (any, error) {
			return withPaging(a, 100), nil
		},
	})

	r.Add(Spec{
		Name: "ozon_certificates_list",
		Path: ozon.PathCertificateList,
		Desc: "Сертификаты и декларации, загруженные в ваш кабинет: номер, тип, статус проверки, срок действия. " +
			"Идентификатор отсюда нужен, чтобы привязать документ к товарам.",
		Schema: schema(obj{
			"offer_id":  str("Артикул товара — показать только связанные с ним документы"),
			"status":    str("Статус документа"),
			"type":      str("Тип документа"),
			"page":      obj{"type": "integer", "default": 1, "minimum": 1},
			"page_size": obj{"type": "integer", "default": 100, "maximum": 1000},
		}),
		Build: func(a map[string]any) (any, error) {
			return withPaging(a, 100), nil
		},
	})

	r.Add(Spec{
		Name: "ozon_certificate_products",
		Path: ozon.PathCertificateProductsList,
		Desc: "Товары, привязанные к сертификату, и статус проверки каждого. " +
			"Привязка не мгновенная: товар может висеть в проверке или отвалиться с ошибкой, " +
			"и увидеть это можно только здесь — успешный ответ на привязку об этом не говорит.",
		Schema: schema(obj{
			"certificate_id":      num("Идентификатор сертификата из ozon_certificates_list"),
			"product_status_code": str("Фильтр по статусу проверки товара"),
			"page":                obj{"type": "integer", "default": 1, "minimum": 1},
			"page_size":           obj{"type": "integer", "default": 100, "maximum": 1000},
		}, "certificate_id"),
		Build: func(a map[string]any) (any, error) {
			return withPaging(a, 100), nil
		},
	})

	// --- Сертификаты: запись ---

	r.Add(Spec{
		Name: "ozon_certificate_bind",
		Path: ozon.PathCertificateBind,
		Desc: "Привязать сертификат к товарам. ИЗМЕНЯЕТ ДАННЫЕ. " +
			"Ответ означает «привязка принята», а не «товары проверены»: " +
			"результат проверки показывает ozon_certificate_products.",
		Write: true,
		Schema: schema(obj{
			"certificate_id": num("Идентификатор сертификата из ozon_certificates_list"),
			"product_id":     arr(obj{"type": "integer"}, "ID товаров, к которым относится документ"),
		}, "certificate_id", "product_id"),
		Batch: func(a map[string]any) int {
			ids, _ := a["product_id"].([]any)
			return len(ids)
		},
		Build: buildCertificateLink,
	})

	r.Add(Spec{
		Name: "ozon_certificate_unbind",
		Path: ozon.PathCertificateUnbind,
		Desc: "Отвязать товары от сертификата. ИЗМЕНЯЕТ ДАННЫЕ. " +
			"Если документ был обязательным для категории, товар после этого может уйти с витрины.",
		Write: true,
		Schema: schema(obj{
			"certificate_id": num("Идентификатор сертификата"),
			"product_id":     arr(obj{"type": "integer"}, "ID товаров, которые нужно отвязать"),
		}, "certificate_id", "product_id"),
		Batch: func(a map[string]any) int {
			ids, _ := a["product_id"].([]any)
			return len(ids)
		},
		Build: buildCertificateLink,
	})

	// --- Штрихкоды ---

	r.Add(Spec{
		Name: "ozon_barcode_generate",
		Path: ozon.PathBarcodeGenerate,
		Desc: "Выпустить штрихкоды для товаров, у которых их нет. ИЗМЕНЯЕТ ДАННЫЕ. " +
			"Без штрихкода товар не примут на складе Ozon. " +
			"Если штрихкод у товара уже есть, но не указан в кабинете, нужен не этот инструмент, " +
			"а ozon_barcode_bind: повторная генерация создаёт второй код на тот же товар.",
		Write: true,
		Schema: schema(obj{
			"product_ids": arr(obj{"type": "integer"}, "ID товаров, не больше 100 за вызов"),
		}, "product_ids"),
		Batch: func(a map[string]any) int {
			ids, _ := a["product_ids"].([]any)
			return len(ids)
		},
		Build: func(a map[string]any) (any, error) {
			ids, err := intList(a["product_ids"])
			if err != nil {
				return nil, fmt.Errorf("product_ids: %w", err)
			}
			if len(ids) > maxBarcodeItems {
				return nil, fmt.Errorf(
					"за вызов принимается не больше %d товаров, передано %d",
					maxBarcodeItems, len(ids))
			}
			return obj{"product_ids": ids}, nil
		},
	})

	r.Add(Spec{
		Name: "ozon_barcode_bind",
		Path: ozon.PathBarcodeAdd,
		Desc: "Привязать к товарам уже существующие штрихкоды — те, что напечатаны на упаковке производителя. " +
			"ИЗМЕНЯЕТ ДАННЫЕ. Нужен, когда штрихкод есть физически, но не указан в кабинете.",
		Write: true,
		Schema: schema(obj{
			"barcodes": arr(schema(obj{
				"barcode": str("Штрихкод, не длиннее 100 символов"),
				"sku":     num("SKU товара"),
			}, "barcode", "sku"), "Пары «штрихкод — товар», не больше 100 за вызов"),
		}, "barcodes"),
		Batch: func(a map[string]any) int {
			items, _ := a["barcodes"].([]any)
			return len(items)
		},
		Build: func(a map[string]any) (any, error) {
			items, _ := a["barcodes"].([]any)
			if len(items) == 0 {
				return nil, fmt.Errorf("пустой список штрихкодов")
			}
			if len(items) > maxBarcodeItems {
				return nil, fmt.Errorf(
					"за вызов принимается не больше %d штрихкодов, передано %d",
					maxBarcodeItems, len(items))
			}
			return a, nil
		},
	})
}

// maxBarcodeItems — сколько товаров принимают методы штрихкодов.
//
// Ozon называет и второй предел: не больше 20 вызовов в минуту. Он
// живёт в лимитере (группа barcode), потому что относится не к вызову,
// а к их частоте.
const maxBarcodeItems = 100

// buildCertificateLink собирает тело привязки и отвязки.
//
// Оба метода принимают одно и то же: идентификатор документа и список
// товаров. Разница только в том, что происходит с товарами дальше.
func buildCertificateLink(a map[string]any) (any, error) {
	id, ok := toInt(a["certificate_id"])
	if !ok {
		return nil, fmt.Errorf("нужен certificate_id — его показывает ozon_certificates_list")
	}

	ids, err := intList(a["product_id"])
	if err != nil {
		return nil, fmt.Errorf("product_id: %w", err)
	}

	return obj{"certificate_id": id, "product_id": ids}, nil
}

// withPaging достраивает постраничность.
//
// У сертификатов она не курсорная, а страничная, и оба поля
// обязательны: без page_size Ozon отвечает пустым списком вместо
// первой страницы — то есть выглядит как «документов нет», хотя они
// есть. Молчаливая пустота хуже ошибки, поэтому значения проставляем.
func withPaging(a map[string]any, size int) map[string]any {
	if a == nil {
		a = map[string]any{}
	}
	if n, ok := toInt(a["page"]); !ok || n < 1 {
		a["page"] = 1
	}
	if _, ok := toInt(a["page_size"]); !ok {
		a["page_size"] = size
	}
	return a
}

// intList приводит список идентификаторов к целым числам.
//
// Зеркало stringList: там идентификаторы обязаны быть строками, здесь —
// числами, и модель точно так же присылает то, что видела в прошлом
// ответе. Строку "123456" Ozon в этих методах не принимает.
func intList(v any) ([]int, error) {
	items, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("ожидается список, получено %T", v)
	}

	out := make([]int, 0, len(items))
	for _, item := range items {
		n, ok := toInt(item)
		if !ok {
			return nil, fmt.Errorf("идентификатор должен быть числом, получено %T (%v)", item, item)
		}
		out = append(out, n)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("список пуст")
	}
	return out, nil
}
