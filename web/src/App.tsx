import { useEffect, useMemo, useState } from "react";
import type { Event, ImportRun, Source } from "./types";

const DATE_FORMAT = new Intl.DateTimeFormat("ru-RU", {
  day: "numeric",
  month: "long",
  year: "numeric",
  timeZone: "Europe/Moscow",
});

const DATE_TIME_FORMAT = new Intl.DateTimeFormat("ru-RU", {
  day: "numeric",
  month: "short",
  hour: "2-digit",
  minute: "2-digit",
  timeZone: "Europe/Moscow",
});

const formatLabels: Record<string, string> = {
  online: "Онлайн",
  offline: "Офлайн",
  hybrid: "Гибрид",
};

async function getJSON<T>(url: string): Promise<T> {
  const response = await fetch(url, { headers: { Accept: "application/json" } });
  if (!response.ok) {
    throw new Error(`API вернул ${response.status}`);
  }
  return response.json() as Promise<T>;
}

function formatDate(value?: string) {
  if (!value) return "Дата уточняется";
  return DATE_FORMAT.format(new Date(value));
}

function relativeFreshness(value?: string) {
  if (!value) return "нет успешных запусков";
  const elapsed = Date.now() - new Date(value).getTime();
  const minutes = Math.max(0, Math.round(elapsed / 60_000));
  if (minutes < 1) return "только что";
  if (minutes < 60) return `${minutes} мин назад`;
  const hours = Math.round(minutes / 60);
  if (hours < 48) return `${hours} ч назад`;
  return DATE_TIME_FORMAT.format(new Date(value));
}

function App() {
  const [events, setEvents] = useState<Event[]>([]);
  const [sources, setSources] = useState<Source[]>([]);
  const [lastRun, setLastRun] = useState<ImportRun | null>(null);
  const [query, setQuery] = useState("");
  const [format, setFormat] = useState("all");
  const [loading, setLoading] = useState(true);
  const [refreshing, setRefreshing] = useState<string | null>(null);
  const [error, setError] = useState("");

  const load = async (quiet = false) => {
    if (!quiet) setLoading(true);
    try {
      const [eventPayload, sourcePayload, runPayload] = await Promise.all([
        getJSON<{ events: Event[] }>("/api/events"),
        getJSON<{ sources: Source[] }>("/api/sources"),
        getJSON<{ runs: ImportRun[] }>("/api/import-runs"),
      ]);
      setEvents(eventPayload.events ?? []);
      setSources(sourcePayload.sources ?? []);
      setLastRun(runPayload.runs?.[0] ?? null);
      setError("");
    } catch (loadError) {
      setError(loadError instanceof Error ? loadError.message : "Не удалось получить данные");
    } finally {
      if (!quiet) setLoading(false);
    }
  };

  useEffect(() => {
    void load();
  }, []);

  useEffect(() => {
    if (!sources.some((source) => source.status === "running")) return;
    const timer = window.setInterval(() => void load(true), 1800);
    return () => window.clearInterval(timer);
  }, [sources]);

  const runImport = async (sourceID: string) => {
    setRefreshing(sourceID);
    setError("");
    try {
      const response = await fetch(`/api/imports/${encodeURIComponent(sourceID)}`, { method: "POST" });
      const payload = await response.json();
      if (!response.ok) {
        throw new Error(payload.message ?? `Импорт завершился с кодом ${response.status}`);
      }
      await load(true);
    } catch (importError) {
      setError(importError instanceof Error ? importError.message : "Импорт не выполнен");
      await load(true);
    } finally {
      setRefreshing(null);
    }
  };

  const visibleEvents = useMemo(() => {
    const normalizedQuery = query.trim().toLocaleLowerCase("ru-RU");
    return events
      .filter((event) => format === "all" || event.format === format)
      .filter((event) => {
        if (!normalizedQuery) return true;
        return [event.title, event.description, ...(event.tags ?? [])]
          .join(" ")
          .toLocaleLowerCase("ru-RU")
          .includes(normalizedQuery);
      })
      .sort((a, b) => {
        const left = a.registration_deadline_at ?? a.listing_expires_at ?? a.end_at ?? a.start_at ?? "9999";
        const right = b.registration_deadline_at ?? b.listing_expires_at ?? b.end_at ?? b.start_at ?? "9999";
        return left.localeCompare(right);
      });
  }, [events, format, query]);

  const hasRunningSource = sources.some((source) => source.status === "running");
  const orderedSources = useMemo(() => [...sources].sort((left, right) => {
    const leftOrder = left.import_order || Number.MAX_SAFE_INTEGER;
    const rightOrder = right.import_order || Number.MAX_SAFE_INTEGER;
    return leftOrder - rightOrder || left.name.localeCompare(right.name, "ru-RU");
  }), [sources]);
  const importedSourceCount = orderedSources.filter((source) => Boolean(source.last_success_at)).length;
  const staleSources = orderedSources.filter((source) => source.last_success_at
    && Date.now() - new Date(source.last_success_at).getTime() > 48 * 60 * 60 * 1000);

  return (
    <div className="app-shell">
      <header className="topbar">
        <a className="brand" href="#top" aria-label="Meeteam, в начало">
          <span className="brand-mark">M</span>
          <span>Meeteam</span>
        </a>
      </header>

      <main id="top">
        <section className="hero">
          <div>
            <h1>События, для которых нужна команда</h1>
          </div>
          <p className="hero-copy">
            Живая лента Meeteam объединяет карточки из нескольких каталогов мероприятий.
          </p>
        </section>

        <details className="sources-panel">
          <summary className="sources-summary">
            <span className="sources-summary-title">Источники</span>
            <span className="sources-summary-meta">
              {importedSourceCount} из {orderedSources.length} импортировано
            </span>
            <ChevronIcon />
          </summary>
          <section className="source-list" aria-label="Состояние источников">
            {orderedSources.map((source) => {
              const stale = staleSources.some((item) => item.id === source.id);
              const isRefreshing = refreshing === source.id || source.status === "running";
              const isImported = Boolean(source.last_success_at);
              const indicatorClass = isRefreshing
                ? "is-running"
                : isImported
                  ? "is-imported"
                  : source.status === "failed"
                    ? "is-failed"
                    : "is-pending";
              const indicatorLabel = isRefreshing
                ? "Импорт выполняется"
                : isImported
                  ? "Источник импортирован"
                  : source.status === "failed"
                    ? "Последняя попытка не удалась"
                    : "Источник ещё не импортирован";
              return (
                <article className={`source-console ${stale ? "is-stale" : ""}`} key={source.id}>
                  <div className="console-primary">
                    <span className={`source-state ${indicatorClass}`} title={indicatorLabel} aria-label={indicatorLabel}>
                      {isRefreshing ? <span className="source-spinner" /> : isImported ? <CheckIcon /> : source.status === "failed" ? "!" : "·"}
                    </span>
                    <strong>{source.name}</strong>
                    <span className="console-meta">
                      Последний успех: {relativeFreshness(source.last_success_at)}
                    </span>
                  </div>
                  <div className="console-stats" aria-label={`Статистика импорта ${source.name}`}>
                    <div>
                      <strong>{source.fetched_count}</strong>
                      <span>получено</span>
                    </div>
                    <div>
                      <strong>{source.valid_count}</strong>
                      <span>валидно</span>
                    </div>
                    <div>
                      <strong>{source.rejected_count}</strong>
                      <span>отклонено</span>
                    </div>
                  </div>
                  <button
                    className="refresh-button"
                    onClick={() => void runImport(source.id)}
                    disabled={Boolean(refreshing) || hasRunningSource}
                  >
                    <RefreshIcon />
                    {isRefreshing ? "Обновляем…" : "Запустить импорт"}
                  </button>
                </article>
              );
            })}
          </section>
        </details>

        {staleSources.length > 0 && (
          <div className="notice warning" role="status">
            Данные старше 48 часов: {staleSources.map((source) => source.name).join(", ")}.
            Последняя успешная версия сохранена, но сроки стоит сверить.
          </div>
        )}
        {orderedSources.filter((source) => source.status === "failed").map((source) => (
          <div className="notice error" role="alert" key={source.id}>
            {source.name}: последний запуск не удался. Лента продолжает показывать последнюю успешную версию.
            {source.last_error && <span>{source.last_error}</span>}
          </div>
        ))}
        {error && <div className="notice error" role="alert">{error}</div>}

        <section className="catalog" aria-labelledby="catalog-title">
          <div className="catalog-heading">
            <div>
              <h2 id="catalog-title">Мероприятия</h2>
            </div>
            <span className="result-count">{visibleEvents.length} из {events.length}</span>
          </div>

          <div className="filters">
            <label className="search-field">
              <span className="sr-only">Поиск мероприятий</span>
              <SearchIcon />
              <input
                value={query}
                onChange={(event) => setQuery(event.target.value)}
                placeholder="Название, направление или тег"
              />
            </label>
            <div className="format-switch" aria-label="Формат мероприятия">
              {[
                ["all", "Все"],
                ["online", "Онлайн"],
                ["offline", "Офлайн"],
                ["hybrid", "Гибрид"],
              ].map(([value, label]) => (
                <button
                  key={value}
                  className={format === value ? "is-active" : ""}
                  onClick={() => setFormat(value)}
                >
                  {label}
                </button>
              ))}
            </div>
          </div>

          {loading ? (
            <div className="state-panel">
              <span className="loader" />
              <h3>Читаем сохранённую версию</h3>
              <p>Это займёт несколько секунд.</p>
            </div>
          ) : visibleEvents.length === 0 ? (
            <div className="state-panel">
              <h3>{events.length ? "Ничего не найдено" : "Лента пока пуста"}</h3>
              <p>{events.length ? "Попробуйте изменить фильтры." : "Запустите импорт, чтобы получить первые карточки."}</p>
            </div>
          ) : (
            <div className="event-grid">
              {visibleEvents.map((event, index) => (
                <article className="event-card" key={event.id} style={{ "--delay": `${Math.min(index, 9) * 45}ms` } as React.CSSProperties}>
                  <div
                    className="event-cover"
                    style={event.image_url ? { backgroundImage: `url(${event.image_url})` } : undefined}
                  >
                    <div className="cover-topline">
                      <span>{formatLabels[event.format ?? ""] ?? "Формат уточняется"}</span>
                      <span className="verified-mark" title="Есть проверяемая ссылка на источник">↗</span>
                    </div>
                    <div className="date-block">
                      <span>Старт</span>
                      <strong>{formatDate(event.start_at)}</strong>
                    </div>
                  </div>

                  <div className="event-body">
                    <div className="tag-row">
                      {(event.tags ?? []).filter((tag) => !/регистрация|online|offline/i.test(tag)).slice(0, 3).map((tag) => (
                        <span key={tag}>{tag}</span>
                      ))}
                    </div>
                    <h3>{event.title}</h3>
                    <p>{event.description || "Описание появится после следующей сверки карточки."}</p>

                    <dl className="event-facts">
                      <div>
                        <dt>Регистрация</dt>
                        <dd>{event.registration_deadline_at ? `до ${formatDate(event.registration_deadline_at)}` : "срок не указан"}</dd>
                      </div>
                      {event.end_at && (
                        <div>
                          <dt>Завершение</dt>
                          <dd>{formatDate(event.end_at)}</dd>
                        </div>
                      )}
                      <div>
                        <dt>Источник</dt>
                        <dd>{event.source_name}</dd>
                      </div>
                    </dl>

                    <div className="card-actions">
                      <a className="primary-link" href={event.official_url} target="_blank" rel="noreferrer">
                        К мероприятию <ArrowIcon />
                      </a>
                      <a className="source-link" href={event.source_url} target="_blank" rel="noreferrer">
                        Карточка источника
                      </a>
                    </div>

                    <details className="diagnostics">
                      <summary>Как распознана карточка</summary>
                      <ul>
                        {(event.parsing_notes ?? []).map((note) => <li key={note}>{note}</li>)}
                      </ul>
                    </details>
                  </div>
                </article>
              ))}
            </div>
          )}
        </section>

        <footer>
          <span>Meeteam · вертикальный срез парсера</span>
          <span>{lastRun ? `Последний цикл: ${DATE_TIME_FORMAT.format(new Date(lastRun.finished_at))}` : "Циклов импорта пока нет"}</span>
        </footer>
      </main>
    </div>
  );
}

function SearchIcon() {
  return <svg aria-hidden="true" viewBox="0 0 24 24"><circle cx="11" cy="11" r="6" /><path d="m16 16 4 4" /></svg>;
}

function RefreshIcon() {
  return <svg aria-hidden="true" viewBox="0 0 24 24"><path d="M20 7v5h-5" /><path d="M19 12a7 7 0 1 0-2 5" /></svg>;
}

function CheckIcon() {
  return <svg aria-hidden="true" viewBox="0 0 24 24"><path d="m5 12 4 4L19 7" /></svg>;
}

function ChevronIcon() {
  return <svg className="summary-chevron" aria-hidden="true" viewBox="0 0 24 24"><path d="m7 9 5 5 5-5" /></svg>;
}

function ArrowIcon() {
  return <svg aria-hidden="true" viewBox="0 0 24 24"><path d="M5 12h14M14 7l5 5-5 5" /></svg>;
}

export default App;
