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
	PathReviewList = "/v1/review/list"
)
