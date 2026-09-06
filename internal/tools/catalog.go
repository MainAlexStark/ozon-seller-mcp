package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/MainAlexStark/ozon-seller-mcp/internal/mcp"
	"github.com/MainAlexStark/ozon-seller-mcp/ozon"
)

// RegisterCatalog добавляет инструменты работы с каталогом и карточками.
func (r *Registry) RegisterCatalog() {
	// --- Категории ---

	r.Add(Spec{
		Name: "ozon_category_tree",
		Path: ozon.PathCategoryTree,
		Desc: "Дерево категорий и типов товара Ozon. Нужен, чтобы получить description_category_id и type_id — " +
			"без них товар не создать. Ответ большой: указывайте language и при возможности сужайте выборку.",
		Schema: schema(obj{
			"language": obj{"type": "string", "enum": []string{"RU", "EN"}, "default": "RU"},
		}),
	})

	r.Add(Spec{
		Name: "ozon_category_attributes",
		Path: ozon.PathCategoryAttribute,
		Desc: "Характеристики конкретной категории: какие обязательны, какие словарные. " +
			"Смотрите сюда, когда импорт товара падает с ошибкой валидации — почти всегда это незаполненная обязательная характеристика.",
		Schema: schema(obj{
			"description_category_id": num("ID категории из ozon_category_tree"),
			"type_id":                 num("ID типа товара из ozon_category_tree"),
			"language":                obj{"type": "string", "enum": []string{"RU", "EN"}, "default": "RU"},
		}, "description_category_id", "type_id"),
	})

	r.Add(Spec{
		Name: "ozon_category_attribute_values",
		Path: ozon.PathCategoryAttributeValues,
		Desc: "Допустимые значения словарной характеристики. Значение вне словаря Ozon не примет.",
		Schema: schema(obj{
			"description_category_id": num("ID категории"),
			"type_id":                 num("ID типа товара"),
			"attribute_id":            num("ID характеристики из ozon_category_attributes"),
			"limit":                   obj{"type": "integer", "default": 100},
			"last_value_id":           num("Для постраничного обхода"),
			"language":                obj{"type": "string", "enum": []string{"RU", "EN"}, "default": "RU"},
		}, "description_category_id", "type_id", "attribute_id"),
	})

	// --- Каталог: чтение ---

	r.Add(Spec{
		Name: "ozon_product_list",
		Path: ozon.PathProductList,
		Desc: "Список товаров магазина с фильтрами по видимости и артикулам. Возвращает product_id и offer_id — " +
			"отправная точка почти для любой задачи с каталогом.",
		Schema: schema(obj{
			"filter": schema(obj{
				"offer_id":   arr(obj{"type": "string"}, "Артикулы продавца"),
				"product_id": arr(obj{"type": "integer"}, "ID товаров в системе Ozon"),
				"visibility": obj{
					"type":        "string",
					"enum":        []string{"ALL", "VISIBLE", "INVISIBLE", "EMPTY_STOCK", "READY_TO_SUPPLY", "STATE_FAILED_MODERATION"},
					"description": "ALL — все товары; VISIBLE — видимые покупателю; EMPTY_STOCK — без остатков",
				},
			}),
			"last_id": str("Курсор постраничного обхода из предыдущего ответа"),
			"limit":   obj{"type": "integer", "default": 100, "maximum": 1000},
		}),
		Build: func(a map[string]any) (any, error) {
			if _, ok := a["limit"]; !ok {
				a["limit"] = 100
			}
			return a, nil
		},
	})

	r.Add(Spec{
		Name: "ozon_product_info",
		Path: ozon.PathProductInfoList,
		Desc: "Подробности товаров: название, категория, статусы, изображения, штрихкоды. " +
			"Принимает до 1000 идентификаторов за раз.",
		Schema: schema(obj{
			"offer_id":   arr(obj{"type": "string"}, "Артикулы продавца"),
			"product_id": arr(obj{"type": "integer"}, "ID товаров Ozon"),
			"sku":        arr(obj{"type": "integer"}, "SKU"),
		}),
	})

	r.Add(Spec{
		Name: "ozon_product_attributes",
		Path: ozon.PathProductAttributes,
		Desc: "Заполненные характеристики товаров. Нужен, чтобы понять, что уже указано в карточке, " +
			"прежде чем что-то менять.",
		Schema: schema(obj{
			"filter": schema(obj{
				"offer_id":   arr(obj{"type": "string"}, "Артикулы продавца"),
				"product_id": arr(obj{"type": "integer"}, "ID товаров"),
				"visibility": obj{"type": "string", "default": "ALL"},
			}),
			"limit":   obj{"type": "integer", "default": 100},
			"last_id": str("Курсор постраничного обхода"),
		}),
		Build: func(a map[string]any) (any, error) {
			if _, ok := a["limit"]; !ok {
				a["limit"] = 100
			}
			return a, nil
		},
	})

	r.Add(Spec{
		Name:   "ozon_product_pictures",
		Path:   ozon.PathPicturesInfo,
		Desc:   "Изображения карточек и статус их загрузки.",
		Schema: schema(obj{"product_id": arr(obj{"type": "integer"}, "ID товаров")}, "product_id"),
	})

	r.Add(Spec{
		Name: "ozon_content_rating",
		Path: ozon.PathContentRating,
		Desc: "Контент-рейтинг карточек по SKU и что именно снижает балл. " +
			"Прямая подсказка, какие поля дозаполнить, чтобы товар лучше показывался в поиске.",
		Schema: schema(obj{"skus": arr(obj{"type": "integer"}, "Список SKU")}, "skus"),
	})

	r.Add(Spec{
		Name:   "ozon_warehouse_list",
		Path:   ozon.PathWarehouseList,
		Desc:   "Список складов продавца. warehouse_id отсюда нужен для обновления остатков.",
		Schema: schema(obj{}),
	})

	// --- Каталог: запись ---

	r.Add(Spec{
		Name: "ozon_product_import",
		Path: ozon.PathProductImport,
		Desc: "Создать или обновить карточки товаров. ИЗМЕНЯЕТ ДАННЫЕ. " +
			"Метод асинхронный: возвращает task_id, результат смотрите через ozon_import_status. " +
			"Успешный ответ означает «задача принята», а не «товар создан».",
		Write: true,
		Schema: schema(obj{
			"items": arr(obj{"type": "object"}, "Товары в формате Ozon Seller API: offer_id, name, description_category_id, type_id, price, vat, габариты, attributes"),
		}, "items"),
	})

	r.Add(Spec{
		Name:  "ozon_product_update_attributes",
		Path:  ozon.PathProductAttrUpdate,
		Desc:  "Обновить характеристики существующих карточек. ИЗМЕНЯЕТ ДАННЫЕ.",
		Write: true,
		Schema: schema(obj{
			"items": arr(obj{"type": "object"}, "Записи вида {offer_id, attributes: [{id, values: [...]}]}"),
		}, "items"),
	})

	r.Add(Spec{
		Name:  "ozon_pictures_import",
		Path:  ozon.PathPicturesImport,
		Desc:  "Загрузить изображения в карточку по URL. ИЗМЕНЯЕТ ДАННЫЕ. Файлы должны лежать в публично доступном хранилище — Ozon забирает их сам.",
		Write: true,
		Schema: schema(obj{
			"product_id":  num("ID товара"),
			"images":      arr(obj{"type": "string"}, "Ссылки на изображения"),
			"color_image": str("Ссылка на образец цвета"),
		}, "product_id", "images"),
	})

	r.Add(Spec{
		Name:  "ozon_product_visibility_set",
		Path:  ozon.PathProductVisibility,
		Desc:  "Управление видимостью товаров в продаже. ИЗМЕНЯЕТ ДАННЫЕ.",
		Write: true,
		Schema: schema(obj{
			"product_ids": arr(obj{"type": "integer"}, "ID товаров"),
			"visible":     boolean("true — показывать покупателям, false — скрыть"),
		}, "product_ids", "visible"),
	})

	// --- Статус импорта: собственный обработчик из-за ожидания ---

	r.AddCustom(mcp.Tool{
		Name: "ozon_import_status",
		Description: "Статус задачи импорта товаров по task_id. " +
			"С wait=true опрашивает Ozon до завершения задачи — так вы сразу увидите ошибки валидации, " +
			"а не «задача принята».",
		InputSchema: schema(obj{
			"task_id": num("ID задачи из ozon_product_import"),
			"wait":    boolean("Дождаться завершения (до 5 минут)"),
		}, "task_id"),
		Handler: func(ctx context.Context, raw json.RawMessage) (string, error) {
			var a struct {
				TaskID int64 `json:"task_id"`
				Wait   bool  `json:"wait"`
			}
			if err := json.Unmarshal(raw, &a); err != nil {
				return "", err
			}

			var (
				items []ozon.ImportItemStatus
				err   error
			)
			if a.Wait {
				items, err = r.client.WaitImport(ctx, a.TaskID, 5*time.Minute)
			} else {
				items, err = r.client.ImportInfo(ctx, a.TaskID)
			}
			if err != nil {
				return "", decorate(err)
			}

			out, _ := json.MarshalIndent(items, "", "  ")
			body := string(out)

			// Ошибки валидации прячутся внутри массива и легко теряются
			// при беглом чтении — вытаскиваем их наверх.
			if summary := summarizeImport(items); summary != "" {
				body = summary + "\n\n" + body
			}
			return r.safety.TrimResponse(body), nil
		},
	})
}

// summarizeImport выносит наверх итог задачи импорта.
func summarizeImport(items []ozon.ImportItemStatus) string {
	var failed int
	for _, it := range items {
		if len(it.Errors) > 0 {
			failed++
		}
	}
	if failed == 0 {
		return ""
	}
	return fmt.Sprintf("ВНИМАНИЕ: %d из %d позиций не прошли валидацию — подробности в поле errors ниже. "+
		"Чаще всего это незаполненная обязательная характеристика категории (ozon_category_attributes).",
		failed, len(items))
}
