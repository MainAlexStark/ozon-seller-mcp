package tools

import (
	"net/http"
	"strings"
	"testing"
)

func TestQuestionsListFillsStatus(t *testing.T) {
	var body map[string]any
	_, server, closeFn := fakeOzon(t, ModeReadOnly, captureBody(t, &body))
	defer closeFn()

	if _, isErr := callTool(t, server, "ozon_questions_list", map[string]any{}); isErr {
		t.Fatal("вызов без фильтра должен проходить")
	}
	filter, ok := body["filter"].(map[string]any)
	if !ok || filter["status"] != "ALL" {
		t.Fatalf("статус по умолчанию должен ничего не отсекать: %v", body)
	}

	callTool(t, server, "ozon_questions_list", map[string]any{
		"filter": map[string]any{"status": "NEW"},
	})
	filter, _ = body["filter"].(map[string]any)
	if filter["status"] != "NEW" {
		t.Errorf("заданный статус подменён: %v", filter["status"])
	}
}

func TestQuestionIDGoesAsString(t *testing.T) {
	// Идентификатор вопроса — строка, но выглядит как число, и модель
	// отправляет число: Ozon отвечает ошибкой типа при верном значении.
	var body map[string]any
	_, server, closeFn := fakeOzon(t, ModeReadOnly, captureBody(t, &body))
	defer closeFn()

	if _, isErr := callTool(t, server, "ozon_question_info", map[string]any{"question_id": 987654}); isErr {
		t.Fatal("числовой идентификатор должен приниматься")
	}
	if body["question_id"] != "987654" {
		t.Errorf("идентификатор ушёл не строкой: %T (%v)", body["question_id"], body["question_id"])
	}
}

func TestQuestionAnswerChecksTextLength(t *testing.T) {
	var reached bool
	_, server, closeFn := fakeOzon(t, ModeWrite, func(w http.ResponseWriter, r *http.Request) {
		reached = true
		_, _ = w.Write([]byte(`{"result":{}}`))
	})
	defer closeFn()

	body, isErr := callTool(t, server, "ozon_question_answer_create", map[string]any{
		"question_id": "q1", "sku": 123, "text": " ",
	})
	if !isErr {
		t.Fatal("пустой ответ должен отклоняться до публикации")
	}
	if reached {
		t.Error("запрос не должен был уйти в Ozon")
	}
	if !contains(body, "2") {
		t.Errorf("сообщение должно называть минимальную длину: %s", body)
	}

	long := strings.Repeat("я", maxAnswerRunes+1)
	if body, isErr = callTool(t, server, "ozon_question_answer_create", map[string]any{
		"question_id": "q1", "sku": 123, "text": long,
	}); !isErr {
		t.Fatal("слишком длинный ответ должен отклоняться")
	}
	if !contains(body, "3000") {
		t.Errorf("сообщение должно называть предел: %s", body)
	}

	// Кириллица считается символами, а не байтами: в 3000 русских
	// букв байтов вдвое больше, и проверка по длине строки отсекала бы
	// половину допустимых ответов.
	ok := strings.Repeat("я", maxAnswerRunes)
	if body, isErr = callTool(t, server, "ozon_question_answer_create", map[string]any{
		"question_id": "q1", "sku": 123, "text": ok,
	}); isErr {
		t.Errorf("ответ ровно в предел должен приниматься: %s", body)
	}
}

func TestQuestionAnswerNeedsSKU(t *testing.T) {
	_, server, closeFn := fakeOzon(t, ModeWrite, captureBody(t, new(map[string]any)))
	defer closeFn()

	body, isErr := callTool(t, server, "ozon_question_answer_create", map[string]any{
		"question_id": "q1", "text": "Влезет: ширина 28 см.",
	})
	if !isErr {
		t.Fatal("без sku ответ не принимается")
	}
	if !contains(body, "sku") {
		t.Errorf("сообщение должно объяснять, чего не хватает: %s", body)
	}
}

func TestQuestionWritesNeedWriteMode(t *testing.T) {
	// Ответ публикуется в карточке под именем магазина — это запись,
	// и в режиме чтения она обязана останавливаться.
	_, server, closeFn := fakeOzon(t, ModeReadOnly, func(w http.ResponseWriter, r *http.Request) {
		t.Error("запрос не должен был уйти в Ozon")
	})
	defer closeFn()

	cases := []struct {
		tool string
		args map[string]any
	}{
		{"ozon_question_answer_create", map[string]any{"question_id": "q1", "sku": 1, "text": "ответ"}},
		{"ozon_question_change_status", map[string]any{"question_ids": []any{"q1"}, "status": "PROCESSED"}},
	}

	for _, c := range cases {
		body, isErr := callTool(t, server, c.tool, c.args)
		if !isErr {
			t.Errorf("%s должен отказывать в режиме чтения", c.tool)
		}
		if !contains(body, "OZON_ALLOW_WRITES") {
			t.Errorf("%s: отказ должен объяснять, как включить запись: %s", c.tool, body)
		}
	}
}

func TestQuestionChangeStatusRejectsFilterOnlyValues(t *testing.T) {
	// ALL и UNPROCESSED описывают выборку, а не состояние: они есть
	// у фильтра списка, но проставить их нельзя.
	_, server, closeFn := fakeOzon(t, ModeWrite, func(w http.ResponseWriter, r *http.Request) {
		t.Error("запрос не должен был уйти в Ozon")
	})
	defer closeFn()

	body, isErr := callTool(t, server, "ozon_question_change_status", map[string]any{
		"question_ids": []any{"q1"}, "status": "ALL",
	})
	if !isErr {
		t.Fatal("статус ALL должен отклоняться до отправки")
	}
	if !contains(body, "PROCESSED") {
		t.Errorf("сообщение должно называть допустимые статусы: %s", body)
	}
}

func TestQuestionsExplainPremiumSubscription(t *testing.T) {
	// Отказ прав на вопросах почти всегда означает отсутствие
	// подписки, а не проблему с ключом. Разница существенная: ключ
	// чинится за минуту, подписка решается не сейчас.
	_, server, closeFn := fakeOzon(t, ModeReadOnly, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"code":7,"message":"access denied"}`))
	})
	defer closeFn()

	body, isErr := callTool(t, server, "ozon_questions_list", map[string]any{})
	if !isErr {
		t.Fatal("отказ прав должен доезжать как ошибка")
	}
	if !contains(body, "Premium Plus") {
		t.Errorf("подсказка про подписку не показана: %s", body)
	}
}
