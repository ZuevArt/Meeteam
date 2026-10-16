package lab

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"time"

	"meeteam/internal/adapters/devfolio"
	"meeteam/internal/adapters/devpost"
	"meeteam/internal/adapters/hackathonradar"
	"meeteam/internal/adapters/hackathonspro"
	"meeteam/internal/adapters/hackathonsrus"
	"meeteam/internal/adapters/hacklist"
	"meeteam/internal/importer"
	"meeteam/internal/model"
	"meeteam/internal/store"
)

type Request struct {
	SourceID string `json:"source_id"`
	Scenario string `json:"scenario"`
}

type Check struct {
	Label  string `json:"label"`
	Passed bool   `json:"passed"`
}

type State struct {
	Status        string     `json:"status"`
	EventCount    int        `json:"event_count"`
	LastSuccessAt *time.Time `json:"last_success_at,omitempty"`
}

type Report struct {
	SourceID        string          `json:"source_id"`
	Scenario        string          `json:"scenario"`
	Mode            string          `json:"mode"`
	Explanation     string          `json:"explanation"`
	ResponsePreview string          `json:"response_preview"`
	RequestCount    int64           `json:"request_count"`
	ElapsedMS       int64           `json:"elapsed_ms"`
	Before          State           `json:"before"`
	After           State           `json:"after"`
	Run             model.ImportRun `json:"run"`
	Error           string          `json:"error,omitempty"`
	Checks          []Check         `json:"checks"`
	Passed          bool            `json:"passed"`
}

var scenarios = map[string]string{
	"unavailable":    "HTTP 503: источник временно недоступен",
	"forbidden":      "HTTP 403: доступ запрещён",
	"rate_limited":   "HTTP 429: превышен лимит запросов",
	"timeout":        "Тайм-аут ответа источника",
	"changed_format": "Вместо ожидаемого формата пришёл HTML/JSON",
	"changed_field":  "Изменилось имя обязательного поля или DOM-селектора",
	"invalid_event":  "Карточка потеряла обязательную дату или Schema.org Event",
	"empty":          "Источник вернул пустой список",
	"recovery":       "Корректный ответ после сбоя",
}

// IsFaultScenario excludes recovery, which clears the persistent test setting.
func IsFaultScenario(scenario string) bool {
	_, known := scenarios[scenario]
	return known && scenario != "recovery"
}

// FaultAdapter is used for the next real import of a selected source. It never
// contacts the website, so the saved catalog exercises the normal failure path.
type FaultAdapter struct {
	Adapter  importer.Adapter
	Scenario string
}

func (a FaultAdapter) Source() model.SourceState { return a.Adapter.Source() }

func (a FaultAdapter) Fetch(_ context.Context) (model.ImportResult, error) {
	now := time.Now().UTC()
	result := model.ImportResult{SourceID: a.Adapter.Source().ID, StartedAt: now, FinishedAt: now}
	if a.Scenario == "invalid_event" {
		result.FetchedCount = 1
		result.Rejected = []model.RejectedEvent{{SourceEventID: "lab-invalid", Reason: scenarios[a.Scenario]}}
		return result, nil
	}
	return result, fmt.Errorf("тестовый сценарий: %s", scenarios[a.Scenario])
}

// Run exercises the production importer/store transitions in an isolated memory store.
// HTTP adapters additionally execute their real fetch/parser code against a local mock server.
func Run(ctx context.Context, input Request) (Report, error) {
	if input.SourceID == "" {
		return Report{}, errors.New("выберите источник")
	}
	label, ok := scenarios[input.Scenario]
	if !ok {
		return Report{}, errors.New("неизвестный сценарий")
	}
	started := time.Now()
	state := store.NewMemoryStore()
	baselineAt := started.Add(-2 * time.Hour).UTC()
	baseline := demoEvent(input.SourceID, "baseline", baselineAt)
	control := demoEvent("control", "untouched", baselineAt)
	_, _ = state.ApplySuccess(model.ImportResult{
		SourceID: input.SourceID, StartedAt: baselineAt, FinishedAt: baselineAt,
		FetchedCount: 1, ValidCount: 1, Events: []model.Event{baseline},
	})
	_, _ = state.ApplySuccess(model.ImportResult{
		SourceID: "control", StartedAt: baselineAt, FinishedAt: baselineAt,
		FetchedCount: 1, ValidCount: 1, Events: []model.Event{control},
	})
	beforeSnapshot := state.Snapshot()

	var adapter importer.Adapter
	var requests atomic.Int64
	mode := "Проверка общего конвейера импорта"
	explanation := "Сбой подан на границе адаптера; проверяются реальные переходы импортера и хранилища. Разбор формата этого источника здесь не проверяется."
	preview := label
	if usesRealHTTPAdapter(input.SourceID, input.Scenario) {
		mode = "Реальный адаптер + локальный HTTP-сайт"
		explanation = "Адаптер выполняет HTTP-запросы к локальной заглушке и разбирает её ответ. Рабочие сайты и сохранённый каталог не затрагиваются."
		stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			respond(w, r, input.SourceID, input.Scenario)
		}))
		defer stub.Close()
		client := &http.Client{Timeout: 90 * time.Millisecond}
		switch input.SourceID {
		case model.DevfolioSourceID:
			adapter = devfolio.New(devfolio.Config{Client: client, APIURL: stub.URL + "/api/search/hackathons", MinDelay: time.Nanosecond})
		case model.HackListSourceID:
			adapter = hacklist.New(hacklist.Config{Client: client, CatalogURL: stub.URL + "/", MinDelay: time.Nanosecond, Concurrency: 1})
		case model.DevpostSourceID:
			adapter = devpost.New(devpost.Config{Client: client, APIURL: stub.URL + "/api/hackathons", MinDelay: time.Nanosecond})
		case model.HackathonRadarSourceID:
			adapter = hackathonradar.New(hackathonradar.Config{Client: client, CatalogURL: stub.URL + "/database", MinDelay: time.Nanosecond})
		case model.HackathonsProSourceID:
			adapter = hackathonspro.New(hackathonspro.Config{Client: client, FeedURL: stub.URL + "/feed", PostURL: stub.URL + "/post", MinDelay: time.Nanosecond})
		case model.HackathonsRusSourceID:
			adapter = hackathonsrus.New(hackathonsrus.Config{Client: client, FeedURL: stub.URL + "/feed", PostURL: stub.URL + "/post", MinDelay: time.Nanosecond})
		}
		preview = responsePreview(input.SourceID, input.Scenario)
	} else {
		adapter = scriptedAdapter{sourceID: input.SourceID, scenario: input.Scenario}
	}

	run, runErr := importer.Run(ctx, adapter, state)
	afterSnapshot := state.Snapshot()
	before := summarize(beforeSnapshot, input.SourceID)
	after := summarize(afterSnapshot, input.SourceID)
	baselinePreserved := hasSameEvent(afterSnapshot, baseline)
	controlPreserved := hasSameEvent(afterSnapshot, control)
	failedExpected := input.Scenario != "recovery"
	checks := []Check{
		{Label: "Контрольный источник не затронут", Passed: controlPreserved},
		{Label: "Сохранённая карточка не потеряна", Passed: baselinePreserved},
	}
	if failedExpected {
		checks = append(checks,
			Check{Label: "Сбой записан только для выбранного источника", Passed: runErr != nil && run.Status == "failed" && after.Status == "failed"},
			Check{Label: "Дата последнего успеха не изменилась", Passed: sameTime(before.LastSuccessAt, after.LastSuccessAt)},
			Check{Label: "Количество опубликованных карточек не изменилось", Passed: before.EventCount == after.EventCount},
		)
	} else {
		checks = append(checks,
			Check{Label: "Восстановленный импорт завершился успешно", Passed: runErr == nil && (run.Status == "success" || run.Status == "partial")},
			Check{Label: "Дата последнего успеха обновилась", Passed: after.LastSuccessAt != nil && before.LastSuccessAt != nil && after.LastSuccessAt.After(*before.LastSuccessAt)},
		)
	}
	passed := true
	for _, check := range checks {
		passed = passed && check.Passed
	}
	report := Report{
		SourceID: input.SourceID, Scenario: input.Scenario, Mode: mode,
		Explanation: explanation, ResponsePreview: preview,
		RequestCount: requests.Load(), ElapsedMS: time.Since(started).Milliseconds(),
		Before: before, After: after, Run: run, Checks: checks, Passed: passed,
	}
	if runErr != nil {
		report.Error = runErr.Error()
	}
	return report, nil
}

func usesRealHTTPAdapter(sourceID, scenario string) bool {
	switch sourceID {
	case model.DevfolioSourceID, model.HackListSourceID:
		return true
	case model.DevpostSourceID, model.HackathonRadarSourceID, model.HackathonsProSourceID, model.HackathonsRusSourceID:
		return scenario != "invalid_event" && scenario != "recovery"
	default:
		return false
	}
}

func demoEvent(sourceID, suffix string, at time.Time) model.Event {
	start := at.Add(24 * time.Hour)
	return model.Event{
		ID: sourceID + ":" + suffix, SourceID: sourceID, SourceName: sourceID,
		SourceEventID: suffix, Title: "Сохранённое событие " + sourceID,
		StartAt: &start, StartPrecision: "datetime", OfficialURL: "https://example.org/event",
		SourceURL: "https://example.org/source", FirstSeenAt: at, LastSeenAt: at,
	}
}

type scriptedAdapter struct {
	sourceID string
	scenario string
}

func (a scriptedAdapter) Source() model.SourceState {
	return model.SourceState{ID: a.sourceID, Name: a.sourceID, Enabled: true}
}

func (a scriptedAdapter) Fetch(_ context.Context) (model.ImportResult, error) {
	now := time.Now().UTC()
	result := model.ImportResult{SourceID: a.sourceID, StartedAt: now, FinishedAt: now}
	if a.scenario == "recovery" {
		result.Events = []model.Event{demoEvent(a.sourceID, "new", now)}
		result.FetchedCount, result.ValidCount = 1, 1
		return result, nil
	}
	return result, fmt.Errorf("заглушка: %s", scenarios[a.scenario])
}

func respond(w http.ResponseWriter, r *http.Request, sourceID, scenario string) {
	switch scenario {
	case "unavailable":
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	case "forbidden":
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	case "rate_limited":
		w.Header().Set("Retry-After", "1")
		http.Error(w, "too many requests", http.StatusTooManyRequests)
		return
	case "timeout":
		select {
		case <-time.After(250 * time.Millisecond):
		case <-r.Context().Done():
		}
		return
	case "changed_format":
		if sourceID != model.HackListSourceID && sourceID != model.HackathonRadarSourceID {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<html><body>Новый формат каталога</body></html>"))
		} else {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"events":[{"title":"Новая схема"}]}`))
		}
		return
	case "changed_field":
		if sourceID != model.HackListSourceID && sourceID != model.HackathonRadarSourceID {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"items":[{"name":"Поле hits переименовано"}]}`))
		} else {
			_, _ = w.Write([]byte("<main><section><h1>Приём заявок</h1><article>Новая разметка</article></section></main>"))
		}
		return
	case "empty":
		if sourceID != model.HackListSourceID && sourceID != model.HackathonRadarSourceID {
			_, _ = w.Write([]byte(`{"hits":{"total":{"value":0},"hits":[]}}`))
		} else {
			_, _ = w.Write([]byte("<main><section><h1>Регистрация открыта</h1></section></main>"))
		}
		return
	}
	if sourceID == model.DevfolioSourceID {
		name := "Восстановленный хакатон"
		start := time.Now().UTC().Add(48 * time.Hour).Format(time.RFC3339)
		end := time.Now().UTC().Add(72 * time.Hour).Format(time.RFC3339)
		deadline := time.Now().UTC().Add(24 * time.Hour).Format(time.RFC3339)
		if scenario == "invalid_event" {
			start = ""
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"hits":{"total":{"value":1},"hits":[{"_source":{"uuid":"lab-1","name":%q,"starts_at":%q,"ends_at":%q,"is_online":true,"hackathon_setting":{"reg_ends_at":%q,"subdomain":"lab-demo"}}}]}}`, name, start, end, deadline)
		return
	}
	w.Header().Set("Content-Type", "text/html")
	if strings.HasPrefix(r.URL.Path, "/events/") {
		if scenario == "invalid_event" {
			_, _ = w.Write([]byte("<main><article>Schema.org Event удалён</article></main>"))
			return
		}
		start := time.Now().UTC().Add(48 * time.Hour).Format("2006-01-02")
		_, _ = fmt.Fprintf(w, `<html><head><script type="application/ld+json">{"@context":"https://schema.org","@type":"Event","name":"Восстановленный хакатон","startDate":%q,"url":"https://example.org/register"}</script></head><body><main><article><a href="https://example.org/register">Участвовать</a></article></main></body></html>`, start)
		return
	}
	_, _ = w.Write([]byte(`<main><section><h1>Регистрация открыта</h1><article><h3><a href="/events/lab-demo/">Восстановленный хакатон</a></h3></article></section></main>`))
}

func responsePreview(sourceID, scenario string) string {
	if sourceID == model.DevfolioSourceID {
		switch scenario {
		case "changed_format":
			return "200 text/html · <html>Новый формат каталога</html>"
		case "changed_field":
			return `200 application/json · {"items":[...]} вместо {"hits":...}`
		case "invalid_event":
			return `200 application/json · hits[0]._source.starts_at = ""`
		case "empty":
			return `200 application/json · hits.total.value = 0`
		}
	} else if sourceID == model.HackListSourceID {
		switch scenario {
		case "changed_format":
			return `200 application/json · {"events":[...]}`
		case "changed_field":
			return "200 text/html · заголовок секции изменён"
		case "invalid_event":
			return "200 text/html · в карточке нет Schema.org Event"
		case "empty":
			return "200 text/html · секция регистрации без карточек"
		}
	}
	if scenario == "changed_format" || scenario == "changed_field" || scenario == "empty" {
		if sourceID == model.HackathonRadarSourceID {
			return "200 · изменённый JSON или HTML без ожидаемой структуры каталога"
		}
		return "200 · изменённый HTML или JSON без ожидаемой структуры каталога"
	}
	return scenarios[scenario]
}

func summarize(snapshot model.Snapshot, sourceID string) State {
	state := State{}
	for _, event := range snapshot.Events {
		if event.SourceID == sourceID {
			state.EventCount++
		}
	}
	for _, source := range snapshot.Sources {
		if source.ID == sourceID {
			state.Status = source.Status
			state.LastSuccessAt = source.LastSuccessAt
			break
		}
	}
	return state
}

func hasSameEvent(snapshot model.Snapshot, expected model.Event) bool {
	for _, event := range snapshot.Events {
		if event.ID == expected.ID {
			return event.Title == expected.Title && event.LastSeenAt.Equal(expected.LastSeenAt)
		}
	}
	return false
}

func sameTime(left, right *time.Time) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.Equal(*right)
}
