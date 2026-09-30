package tools

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/MainAlexStark/ozon-seller-mcp/internal/files"
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
// Загрузка файла (ozon_certificate_create) идёт multipart/form-data
// через ozon.Client.PostForm. Файл модель передаёт либо путём на диске
// (только stdio — сервер на машине человека), либо base64 (везде).
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

	// --- Сертификаты: справочники ---
	//
	// Коды типов, статусов и причин отказа модель не должна помнить и
	// тем более придумывать: create принимает только коды из этих
	// справочников, а ответы list/info показывают статусы теми же кодами.

	r.Add(Spec{
		Name: "ozon_certificate_types",
		Path: ozon.PathCertificateTypes,
		Get:  true,
		Desc: "Справочник типов документов (сертификат соответствия, декларация и т.д.) с кодами. " +
			"Код отсюда идёт в type_code при ozon_certificate_create и в фильтр type у ozon_certificates_list.",
		Schema: schema(obj{}),
	})

	r.Add(Spec{
		Name: "ozon_certificate_accordance_types",
		Path: ozon.PathCertificateAccordance,
		Get:  true,
		Desc: "Справочник типов соответствия (обязательная и добровольная сертификация, отдельно для опасных товаров) с кодами. " +
			"Код отсюда идёт в accordance_type_code при ozon_certificate_create.",
		Schema: schema(obj{}),
	})

	r.Add(Spec{
		Name: "ozon_certificate_statuses",
		Path: ozon.PathCertificateStatuses,
		Desc: "Возможные статусы проверки документа с кодами — чтобы расшифровать status_code в ozon_certificates_list " +
			"и использовать его как фильтр status.",
		Schema: schema(obj{}),
	})

	r.Add(Spec{
		Name: "ozon_certificate_rejection_reasons",
		Path: ozon.PathCertificateRejections,
		Desc: "Возможные причины отказа в проверке документа с кодами — чтобы расшифровать rejection_reason_code, " +
			"когда ozon_certificates_list или ozon_certificate_info показывает отклонённый документ.",
		Schema: schema(obj{}),
	})

	r.Add(Spec{
		Name: "ozon_certificate_product_statuses",
		Path: ozon.PathCertificateProductState,
		Desc: "Возможные статусы товара при привязке к документу с кодами — для фильтра product_status_code " +
			"в ozon_certificate_products и расшифровки его ответа.",
		Schema: schema(obj{}),
	})

	r.Add(Spec{
		Name: "ozon_certificate_info",
		Path: ozon.PathCertificateInfo,
		Desc: "Один документ по его НОМЕРУ (не по идентификатору): статус проверки, причина отказа, комментарий модератора, " +
			"сроки, число привязанных товаров. Идентификатор certificate_id, нужный для привязки и удаления, — в ответе.",
		Schema: schema(obj{
			"certificate_number": str("Номер документа так, как он был указан при загрузке"),
		}, "certificate_number"),
		Build: func(a map[string]any) (any, error) {
			n, _ := a["certificate_number"].(string)
			n = strings.TrimSpace(n)
			if n == "" {
				return nil, fmt.Errorf("нужен certificate_number — номер документа")
			}
			return obj{"certificate_number": n}, nil
		},
	})

	// --- Сертификаты: загрузка и удаление ---

	r.Add(Spec{
		Name: "ozon_certificate_create",
		Path: ozon.PathCertificateCreate,
		Desc: "Загрузить документ (сертификат, декларацию) в кабинет вместе со сканом. ИЗМЕНЯЕТ ДАННЫЕ. " +
			"Ответ — идентификатор нового документа: он означает «загружено», а не «проверено». " +
			"Документ проходит модерацию; результат показывает ozon_certificate_info. " +
			"Чтобы документ заработал, его ещё нужно привязать к товарам (ozon_certificate_bind). " +
			"Коды type_code и accordance_type_code берите из ozon_certificate_types и ozon_certificate_accordance_types, не придумывайте. " +
			"Повторный вызов создаёт второй такой же документ: при сомнении сначала посмотрите ozon_certificates_list. " +
			"Скан — jpg, jpeg, png или pdf. Файл передаётся либо путём (file_path — только при локальном запуске по stdio), " +
			"либо содержимым в base64 (file_base64 вместе с file_name — работает везде, но годится для небольших сканов).",
		Write: true,
		Schema: schema(obj{
			"name":                 str("Название документа, не длиннее 100 символов"),
			"number":               str("Номер документа, не длиннее 100 символов"),
			"type_code":            str("Код типа документа из ozon_certificate_types"),
			"accordance_type_code": str("Код типа соответствия из ozon_certificate_accordance_types"),
			"issue_date":           str("Дата выдачи, ГГГГ-ММ-ДД"),
			"expire_date":          str("Дата окончания, ГГГГ-ММ-ДД; для бессрочных документов не указывать"),
			"files": arr(schema(obj{
				"file_path":   str("Путь к скану на диске (только stdio)"),
				"file_base64": str("Содержимое скана в base64"),
				"file_name":   str("Имя файла с расширением; обязательно вместе с file_base64"),
			}), "Сканы документа, от 1 до "+fmt.Sprint(maxCertificateFiles)),
		}, "name", "number", "type_code", "issue_date", "files"),
		Build: func(a map[string]any) (any, error) {
			return r.buildCertificateCreate(a)
		},
		Do: func(ctx context.Context, c *ozon.Client, payload any) (any, error) {
			req := payload.(*certificateCreate)
			return c.PostForm(ctx, ozon.PathCertificateCreate, req.fields, req.files)
		},
	})

	r.Add(Spec{
		Name: "ozon_certificate_delete",
		Path: ozon.PathCertificateDelete,
		Desc: "Удалить документ из кабинета. ИЗМЕНЯЕТ ДАННЫЕ, НЕОБРАТИМО: файл придётся загружать заново и проходить модерацию. " +
			"Со всех товаров документ пропадает; если он был обязательным для категории, товары могут уйти с витрины. " +
			"Поэтому требует confirm_delete: true — ставьте только после явного согласия человека. " +
			"Ответ Ozon с is_delete: false и текстом ошибки — это отказ, а не успех.",
		Write: true,
		Schema: schema(obj{
			"certificate_id": num("Идентификатор документа из ozon_certificates_list"),
			"confirm_delete": boolean("Человек подтвердил удаление"),
		}, "certificate_id", "confirm_delete"),
		Build: func(a map[string]any) (any, error) {
			id, ok := toInt(a["certificate_id"])
			if !ok {
				return nil, fmt.Errorf("нужен certificate_id — его показывает ozon_certificates_list")
			}
			if !argBool(a["confirm_delete"], false) {
				return nil, fmt.Errorf("удаление документа %d остановлено: нужен confirm_delete: true.\n\n"+
					"Документ исчезнет со всех товаров, вернуть его можно только повторной загрузкой с новой модерацией. "+
					"Проверьте, что он не нужен: ozon_certificate_products покажет, к каким товарам он привязан. "+
					"Спросите человека и, если он согласен, повторите вызов с confirm_delete: true.", id)
			}
			return obj{"certificate_id": id}, nil
		},
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

// Пределы загрузки. Это защита сервера, а не требование Ozon: скан
// документа — единицы мегабайт, а base64 в аргументе инструмента,
// который сервер держит в памяти, не должен быть неограниченным.
const (
	maxCertificateFiles     = 10
	maxCertificateFileBytes = 10 << 20
	maxCertificateFieldLen  = 100
)

// certificateDateLayout — формат дат в форме. Ozon описывает поля как
// дату-время; полночь UTC — наименее двусмысленный способ передать
// календарный день.
const certificateDateLayout = "2006-01-02T15:04:05Z"

// certificateExts — допустимые расширения сканов.
var certificateExts = map[string]bool{".jpg": true, ".jpeg": true, ".png": true, ".pdf": true}

// certificateCreate — разобранный и проверенный запрос загрузки.
type certificateCreate struct {
	fields map[string]string
	files  []ozon.FormFile
}

// localFiles — может ли сервер читать файлы с диска по пути из аргументов.
//
// Только в stdio: там сервер запущен на машине человека, и путь
// означает его собственный файл. В сервисе путь означал бы файл на
// сервере, где работают чужие люди, — читать его по слову модели
// нельзя ни при каких условиях.
func (r *Registry) localFiles() bool {
	_, ok := r.files.(*files.Dir)
	return ok
}

func (r *Registry) buildCertificateCreate(a map[string]any) (any, error) {
	fields := map[string]string{}

	for _, f := range []struct {
		key      string
		required bool
	}{
		{"name", true}, {"number", true}, {"type_code", true}, {"accordance_type_code", false},
	} {
		v, _ := a[f.key].(string)
		v = strings.TrimSpace(v)
		if v == "" {
			if f.required {
				return nil, fmt.Errorf("нужен %s", f.key)
			}
			continue
		}
		if (f.key == "name" || f.key == "number") && utf8.RuneCountInString(v) > maxCertificateFieldLen {
			return nil, fmt.Errorf("%s длиннее %d символов", f.key, maxCertificateFieldLen)
		}
		fields[f.key] = v
	}

	issue, err := certificateDate(a["issue_date"], "issue_date", true)
	if err != nil {
		return nil, err
	}
	fields["issue_date"] = issue
	if expire, err := certificateDate(a["expire_date"], "expire_date", false); err != nil {
		return nil, err
	} else if expire != "" {
		if expire < issue {
			return nil, fmt.Errorf("expire_date раньше issue_date")
		}
		fields["expire_date"] = expire
	}

	items, _ := a["files"].([]any)
	if len(items) == 0 {
		return nil, fmt.Errorf("нужен хотя бы один скан в files")
	}
	if len(items) > maxCertificateFiles {
		return nil, fmt.Errorf("сканов не больше %d, передано %d", maxCertificateFiles, len(items))
	}

	out := &certificateCreate{fields: fields}
	for i, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("files[%d]: ожидается объект", i)
		}
		f, err := r.certificateFile(m)
		if err != nil {
			return nil, fmt.Errorf("files[%d]: %w", i, err)
		}
		out.files = append(out.files, f)
	}
	return out, nil
}

// certificateFile читает один скан: с диска (stdio) или из base64.
func (r *Registry) certificateFile(m map[string]any) (ozon.FormFile, error) {
	path, _ := m["file_path"].(string)
	b64, _ := m["file_base64"].(string)
	name, _ := m["file_name"].(string)
	path, b64, name = strings.TrimSpace(path), strings.TrimSpace(b64), strings.TrimSpace(name)

	var data []byte
	switch {
	case path != "" && b64 != "":
		return ozon.FormFile{}, fmt.Errorf("укажите либо file_path, либо file_base64")

	case path != "":
		if !r.localFiles() {
			return ozon.FormFile{}, fmt.Errorf("file_path работает только при локальном запуске по stdio; " +
				"здесь передайте содержимое скана в file_base64 вместе с file_name")
		}
		if !certificateExts[strings.ToLower(filepath.Ext(path))] {
			return ozon.FormFile{}, fmt.Errorf("допустимы jpg, jpeg, png, pdf")
		}
		info, err := os.Stat(path)
		if err != nil {
			return ozon.FormFile{}, fmt.Errorf("не открыть файл: %w", err)
		}
		if !info.Mode().IsRegular() {
			return ozon.FormFile{}, fmt.Errorf("%s — не обычный файл", path)
		}
		if info.Size() > maxCertificateFileBytes {
			return ozon.FormFile{}, fmt.Errorf("файл больше %d МБ", maxCertificateFileBytes>>20)
		}
		data, err = os.ReadFile(path)
		if err != nil {
			return ozon.FormFile{}, fmt.Errorf("не прочитать файл: %w", err)
		}
		if name == "" {
			name = filepath.Base(path)
		}

	case b64 != "":
		if name == "" {
			return ozon.FormFile{}, fmt.Errorf("с file_base64 нужен file_name с расширением")
		}
		// Сначала длина строки, потом декодирование: не выделять
		// память под гигантский аргумент.
		if len(b64) > maxCertificateFileBytes*4/3+8 {
			return ozon.FormFile{}, fmt.Errorf("файл больше %d МБ", maxCertificateFileBytes>>20)
		}
		var err error
		data, err = base64.StdEncoding.DecodeString(b64)
		if err != nil {
			if data, err = base64.RawStdEncoding.DecodeString(b64); err != nil {
				return ozon.FormFile{}, fmt.Errorf("file_base64 не разобрать как base64")
			}
		}

	default:
		return ozon.FormFile{}, fmt.Errorf("нужен file_path или file_base64")
	}

	name = filepath.Base(name)
	if !certificateExts[strings.ToLower(filepath.Ext(name))] {
		return ozon.FormFile{}, fmt.Errorf("допустимы jpg, jpeg, png, pdf, а не %q", filepath.Ext(name))
	}
	if len(data) == 0 {
		return ozon.FormFile{}, fmt.Errorf("файл пуст")
	}
	if len(data) > maxCertificateFileBytes {
		return ozon.FormFile{}, fmt.Errorf("файл больше %d МБ", maxCertificateFileBytes>>20)
	}
	return ozon.FormFile{Field: "files", Name: name, Data: data}, nil
}

// certificateDate принимает ГГГГ-ММ-ДД (и полную RFC 3339, откуда берётся
// только день) и отдаёт дату в формате формы.
func certificateDate(v any, field string, required bool) (string, error) {
	s, _ := v.(string)
	s = strings.TrimSpace(s)
	if s == "" {
		if required {
			return "", fmt.Errorf("нужен %s в формате ГГГГ-ММ-ДД", field)
		}
		return "", nil
	}
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		t, err = time.Parse(time.RFC3339, s)
		if err != nil {
			return "", fmt.Errorf("%s: ожидается ГГГГ-ММ-ДД, получено %q", field, s)
		}
	}
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC).Format(certificateDateLayout), nil
}
