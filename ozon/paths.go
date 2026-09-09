package ozon

// Пути методов Seller API.
//
// Ozon отключает старые версии по расписанию, поэтому все пути собраны
// здесь: когда очередная версия переезжает, правка в одном файле.
//
// Состояние на сентябрь 2026 с учётом уже прошедших отключений:
//
//	/v3/product/info/stocks   -> /v4/product/info/stocks
//	/v4/product/info/prices   -> /v5/product/info/prices
//	/v2/product/info/list     -> /v3/product/info/list
//	/v2/product/list          -> /v3/product/list
//	/v3/products/info/attributes -> /v4/product/info/attributes
//	/v1/product/pictures/info -> /v2/product/pictures/info
//	/v3/finance/transaction/list   -> /v1/finance/accrual/*
//	/v3/finance/transaction/totals -> /v1/finance/accrual/*
//	/v1/warehouse/list        -> /v2/warehouse/list
//	/v1/delivery-method/list  -> /v2/delivery-method/list
//	/v1/product/info/stocks-by-warehouse/fbs -> /v2/...
//	/v2/posting/fbo/list      -> /v3/posting/fbo/list (отключён 31.08.2026)
//	/v2/supply-order/{list,get} -> /v3/supply-order/{list,get}
//	/v1/analytics/stock_on_warehouses -> /v2/analytics/stock_on_warehouses
//
// Инструмент ozon_api_selftest прогоняет все read-методы по одному
// запросу и показывает, какие из них перестали отвечать: это дешевле,
// чем узнать об отключении из упавшего конвейера.
const (
	// --- Категории и характеристики ---
	PathCategoryTree            = "/v1/description-category/tree"
	PathCategoryAttribute       = "/v1/description-category/attribute"
	PathCategoryAttributeValues = "/v1/description-category/attribute/values"

	// --- Каталог: чтение ---
	PathProductList       = "/v3/product/list"
	PathProductInfoList   = "/v3/product/info/list"
	PathProductAttributes = "/v4/product/info/attributes"
	PathPicturesInfo      = "/v2/product/pictures/info"
	PathContentRating     = "/v1/product/rating-by-sku"

	// --- Каталог: запись ---
	PathProductImport     = "/v3/product/import"
	PathProductImportInfo = "/v1/product/import/info"
	PathProductAttrUpdate = "/v1/product/attributes/update"
	PathPicturesImport    = "/v1/product/pictures/import"
	PathProductVisibility = "/v1/product/visibility/set"
	PathProductArchive    = "/v1/product/archive"
	PathProductUnarchive  = "/v1/product/unarchive"

	// --- Цены ---
	PathPricesInfo   = "/v5/product/info/prices"
	PathPricesImport = "/v1/product/import/prices"

	// --- Остатки ---
	PathStocksInfo        = "/v4/product/info/stocks"
	PathStocksImport      = "/v2/products/stocks"
	PathStocksByWarehouse = "/v2/product/info/stocks-by-warehouse/fbs"

	// --- Склады ---
	PathWarehouseList = "/v2/warehouse/list"

	// --- Отправления FBS ---
	PathPostingFBSList = "/v3/posting/fbs/list"
	PathPostingFBSGet  = "/v3/posting/fbs/get"

	// --- FBO: заказы со складов Ozon ---
	//
	// Список переехал на v3 (курсор вместо offset), а получение одного
	// отправления осталось на v2: у Ozon версии переезжают поодиночке,
	// а не разделами, поэтому соседние методы вполне живут на разных.
	PathPostingFBOList = "/v3/posting/fbo/list"
	PathPostingFBOGet  = "/v2/posting/fbo/get"

	// --- FBO: поставки на склады Ozon ---
	PathSupplyOrderList     = "/v3/supply-order/list"
	PathSupplyOrderGet      = "/v3/supply-order/get"
	PathSupplyOrderBundle   = "/v1/supply-order/bundle"
	PathSupplyOrderCounters = "/v1/supply-order/status/counter"
	PathSupplyTimeslots     = "/v2/supply-order/timeslot/list"

	// --- FBO: склады Ozon и остатки на них ---
	PathClusterList       = "/v2/cluster/list"
	PathStockOnWarehouses = "/v2/analytics/stock_on_warehouses"

	// --- Аналитика ---
	PathAnalyticsData   = "/v1/analytics/data"
	PathAnalyticsStocks = "/v1/analytics/stocks"

	// --- Финансы ---
	// /v3/finance/transaction/list отключён в 2026 году. Замена —
	// три метода ниже, у каждого окно запроса ограничено месяцем.
	PathFinanceAccrualByDay    = "/v1/finance/accrual/by-day"
	PathFinanceAccrualPostings = "/v1/finance/accrual/postings"
	PathFinanceAccrualTypes    = "/v1/finance/accrual/types"

	// --- Отзывы и вопросы ---
	//
	// Отзывы и вопросы у Ozon разведены по разным разделам, и пути
	// у них разной формы: у отзывов дефис (review/change-status),
	// у вопросов — подчёркивание (question/change_status). Разница
	// не смысловая, а историческая, но 404 из-за неё настоящий.
	PathReviewList = "/v1/review/list"

	PathQuestionList         = "/v1/question/list"
	PathQuestionInfo         = "/v1/question/info"
	PathQuestionCount        = "/v1/question/count"
	PathQuestionAnswerList   = "/v1/question/answer/list"
	PathQuestionAnswerCreate = "/v1/question/answer/create"
	PathQuestionChangeStatus = "/v1/question/change_status"

	// --- Возвраты ---
	//
	// /v1/returns/list — единый список для FBS и FBO. До него схемы
	// разбирались разными методами, и сводить их приходилось руками.
	PathReturnsList        = "/v1/returns/list"
	PathReturnsDropoffInfo = "/v1/returns/company/fbs/info"

	// --- Штрихкоды ---
	PathBarcodeGenerate = "/v1/barcode/generate"
	PathBarcodeAdd      = "/v1/barcode/add"

	// --- Сертификаты и декларации ---
	//
	// Загрузка самого файла (/v1/product/certificate/create) идёт
	// multipart/form-data, а клиент этого пакета отправляет только
	// JSON. Поэтому загрузка остаётся ручной операцией в кабинете,
	// а через MCP доступно всё вокруг неё: что вообще требуется,
	// что уже загружено, к чему привязано и прошло ли проверку.
	PathCertificationList       = "/v2/product/certification/list"
	PathCertificateList         = "/v1/product/certificate/list"
	PathCertificateProductsList = "/v1/product/certificate/products/list"
	PathCertificateBind         = "/v1/product/certificate/bind"
	PathCertificateUnbind       = "/v1/product/certificate/unbind"
)
