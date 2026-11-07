Участники, работавшие очно: **[укажите имена участников]**.

# Практика 6. Проверка отказоустойчивости парсера Meeteam

## Сценарий

Разработчик открывает лабораторию парсера, выбирает **Devfolio** и сценарий **«Сервис недоступен — HTTP 503»**, запускает изолированную проверку и видит, что ранее сохранённая карточка не пропала. Настоящий Devfolio и рабочий `data/state.json` при этом не затрагиваются. Сценарий реализован полностью.

Ожидаемый результат: адаптер выполняет предусмотренные повторы, запуск получает статус `failed`, дата последнего успеха и число опубликованных карточек не меняются, контрольный источник остаётся нетронутым.

## Путь действия по коду

| Файл | Роль в сценарии |
|---|---|
| [web/src/main.tsx](web/src/main.tsx) | По URL `/parser-lab` выбирает страницу лаборатории вместо основной ленты. |
| [web/src/ParserLab.tsx](web/src/ParserLab.tsx), `run` | Получает выбранные `sourceID=devfolio` и `scenario=unavailable`, отправляет `POST /api/lab/run`, затем показывает состояние до и после, ошибку и результаты проверок. |
| [cmd/server/main.go](cmd/server/main.go), `handleLabRun` | Разрешает лабораторию только для loopback-адреса, проверяет входной JSON, задаёт тайм-аут и вызывает `lab.Run`. |
| [internal/lab/lab.go](internal/lab/lab.go), `Run` | Создаёт временный `MemoryStore`, сохранённую карточку Devfolio и контрольную карточку. Локальный HTTP-сервер имитирует ответ 503. |
| [internal/adapters/devfolio/adapter.go](internal/adapters/devfolio/adapter.go) | Настоящий адаптер отправляет запросы локальной заглушке, повторяет временную ошибку и после трёх ответов 503 возвращает ошибку. |
| [internal/importer/run.go](internal/importer/run.go), `Run` | Выполняет тот же конвейер, что и рабочий импорт: переводит источник в `running`, формирует неуспешный `ImportRun` и вызывает `ApplyFailure`. |
| [internal/store/file.go](internal/store/file.go), `ApplyFailure` | Обновляет статус и текст ошибки, но не очищает ранее сохранённые события. `FileStore` записывает рабочее состояние через временный файл и атомарный `rename`. |
| [web/src/ParserLab.tsx](web/src/ParserLab.tsx) | Получает итоговый JSON и показывает пользователю пять проверок сохранности данных. |

Заглушкой является только внешний сайт: `httptest.Server` воспроизводимо отвечает 503. Вместо Devfolio-адаптера заглушка не используется. Проверка работает во временной памяти, поэтому основной каталог не изменяется.

## Проверка

7 ноября 2026 года сценарий проверен локально со следующими данными:

```json
{"source_id":"devfolio","scenario":"unavailable"}
```

Адаптер выполнил три HTTP-запроса и вернул:

```text
fetch Devfolio offset 0: request failed after retries: HTTP 503
```

Сокращённый результат API:

```json
{
  "source_id": "devfolio",
  "scenario": "unavailable",
  "request_count": 3,
  "before": {"status": "success", "event_count": 1},
  "after": {"status": "failed", "event_count": 1},
  "run": {"status": "failed", "fetched_count": 0, "valid_count": 0},
  "passed": true
}
```

Все пять проверок прошли: контрольный источник не изменился, сохранённая карточка не потеряна, сбой записан только для Devfolio, дата последнего успеха не изменилась, число опубликованных карточек осталось равным одному.

## Технологические решения

1. **Независимые Go-адаптеры для каждого источника.** Они приводят JSON, HTML, Tilda/Next.js и страницы с JavaScript к общим `Event` и `ImportResult`. Реализация: [internal/importer/importer.go](internal/importer/importer.go), [internal/importer/run.go](internal/importer/run.go), [internal/model/model.go](internal/model/model.go), `internal/adapters/*/adapter.go`. Альтернатива — один универсальный HTML-парсер или обязательный headless Chrome. Текущий вариант локализует изменение конкретного сайта и не запускает тяжёлый браузер там, где достаточно JSON.
2. **Last known good и атомарный JSON.** При 403, 429, 503, тайм-ауте или изменении формата неуспешный импорт меняет статус источника, но сохраняет последнюю достоверную выборку. Реализация: [internal/importer/run.go](internal/importer/run.go), [internal/store/file.go](internal/store/file.go), отображение ошибки — [web/src/App.tsx](web/src/App.tsx). Альтернатива — PostgreSQL с транзакциями. База лучше для нескольких процессов и большого объёма, но JSON позволяет запустить RAT‑PoC без Docker и миграций; интерфейс `Store` позволяет заменить хранилище позже.

## Запуск и повторение

Требуются Go 1.24+, Node.js 20+ и npm. Из корня репозитория:

```bash
cd web
npm ci
npm run build
cd ..

STATE_FILE=/tmp/meeteam-quest6-demo.json \
AUTO_IMPORT=false \
HTTP_ADDR=127.0.0.1:18080 \
go run ./cmd/server
```

1. Открыть <http://127.0.0.1:18080/parser-lab>.
2. Выбрать `Devfolio` и «Сервис недоступен — HTTP 503».
3. Нажать «Проверить изолированно».
4. Убедиться, что показано «Ожидаемое поведение подтверждено», карточек было и осталось `1`, статус после запуска — `failed`, все пять проверок зелёные.

Проверка без интерфейса:

```bash
curl -X POST http://127.0.0.1:18080/api/lab/run \
  -H 'Content-Type: application/json' \
  -d '{"source_id":"devfolio","scenario":"unavailable"}'
```
