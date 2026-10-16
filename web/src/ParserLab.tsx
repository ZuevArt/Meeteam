import { useEffect, useMemo, useRef, useState } from "react";
import type { Source } from "./types";
import "./parser-lab.css";

type Check = { label: string; passed: boolean };
type State = { status: string; event_count: number; last_success_at?: string };
type Report = {
  source_id: string;
  scenario: string;
  mode: string;
  explanation: string;
  response_preview: string;
  request_count: number;
  elapsed_ms: number;
  before: State;
  after: State;
  run: { status: string; fetched_count: number; valid_count: number; rejected_count: number; error_summary?: string };
  error?: string;
  checks: Check[];
  passed: boolean;
};

const sources = [
  ["hackathons-rus", "Хакатоны.рус"],
  ["codenrock", "Codenrock"],
  ["hackathons-pro", "Hackathons.pro"],
  ["hackathon-radar", "HackathonRadar"],
  ["kaggle", "Kaggle"],
  ["hacklist", "HackList"],
  ["devpost", "Devpost"],
  ["devfolio", "Devfolio"],
] as const;

const scenarios = [
  { id: "unavailable", label: "Сервис недоступен", detail: "HTTP 503", icon: "503" },
  { id: "forbidden", label: "Доступ закрыт", detail: "HTTP 403", icon: "403" },
  { id: "rate_limited", label: "Слишком много запросов", detail: "HTTP 429", icon: "429" },
  { id: "timeout", label: "Сайт не отвечает", detail: "Тайм-аут", icon: "⏱" },
  { id: "changed_format", label: "Другой формат", detail: "HTML ↔ JSON", icon: "{}" },
  { id: "changed_field", label: "Изменились поля", detail: "JSON / DOM", icon: "≠" },
  { id: "invalid_event", label: "Карточка повреждена", detail: "Нет даты / схемы", icon: "!" },
  { id: "empty", label: "Пустой ответ", detail: "0 карточек", icon: "∅" },
  { id: "recovery", label: "Восстановление", detail: "Корректный ответ", icon: "↗" },
] as const;

function dateTime(value?: string) {
  if (!value) return "никогда";
  return new Intl.DateTimeFormat("ru-RU", {
    day: "2-digit", month: "2-digit", year: "numeric", hour: "2-digit", minute: "2-digit", second: "2-digit",
    timeZone: "Europe/Moscow",
  }).format(new Date(value));
}

export default function ParserLab() {
  const [sourceID, setSourceID] = useState<string>("devfolio");
  const [scenario, setScenario] = useState<string>("unavailable");
  const [report, setReport] = useState<Report | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [sourceStates, setSourceStates] = useState<Source[]>([]);
  const [settingBusy, setSettingBusy] = useState(false);
  const [settingMessage, setSettingMessage] = useState("");
  const resultsRef = useRef<HTMLElement>(null);
  const currentSource = useMemo(() => sources.find(([id]) => id === sourceID)?.[1] ?? sourceID, [sourceID]);
  const isRealAdapter = sourceID === "devfolio" || sourceID === "hacklist"
    || (!["codenrock", "kaggle"].includes(sourceID) && !["invalid_event", "recovery"].includes(scenario));
  const activeScenario = sourceStates.find((source) => source.id === sourceID)?.test_scenario;

  useEffect(() => {
    void fetch("/api/sources").then(async (response) => {
      if (!response.ok) throw new Error(`API вернул ${response.status}`);
      const payload = await response.json() as { sources: Source[] };
      setSourceStates(payload.sources ?? []);
    }).catch(() => setSettingMessage("Не удалось загрузить настройки источников."));
  }, []);

  async function setSourceScenario(nextScenario: string) {
    setSettingBusy(true);
    setSettingMessage("");
    try {
      const response = await fetch(`/api/lab/scenarios/${encodeURIComponent(sourceID)}`, {
        method: "PUT",
        headers: { "Content-Type": "application/json", Accept: "application/json" },
        body: JSON.stringify({ scenario: nextScenario }),
      });
      const payload = await response.json();
      if (!response.ok) throw new Error(payload.message ?? `API вернул ${response.status}`);
      setSourceStates((previous) => previous.map((source) => source.id === sourceID ? payload.source as Source : source));
      setSettingMessage(nextScenario
        ? `Сценарий сохранён для ${currentSource}. Следующий импорт покажет ${nextScenario === "invalid_event" ? "предупреждение" : "ошибку"}.`
        : `Тестовый сценарий для ${currentSource} снят. Следующий импорт обратится к настоящему сайту.`);
    } catch (cause) {
      setSettingMessage(cause instanceof Error ? cause.message : "Не удалось сохранить сценарий");
    } finally {
      setSettingBusy(false);
    }
  }

  async function run() {
    setBusy(true);
    setReport(null);
    setError("");
    try {
      const response = await fetch("/api/lab/run", {
        method: "POST",
        headers: { "Content-Type": "application/json", Accept: "application/json" },
        body: JSON.stringify({ source_id: sourceID, scenario }),
      });
      const payload = await response.json();
      if (!response.ok) throw new Error(payload.message ?? `API вернул ${response.status}`);
      setReport(payload as Report);
      if (window.matchMedia("(max-width: 980px)").matches) {
        window.requestAnimationFrame(() => resultsRef.current?.scrollIntoView({
          behavior: window.matchMedia("(prefers-reduced-motion: reduce)").matches ? "auto" : "smooth",
          block: "start",
        }));
      }
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "Не удалось выполнить сценарий");
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="app-shell lab-page">
      <header className="topbar">
        <a className="brand" href="/" aria-label="Meeteam, к ленте">
          <span className="brand-mark">M</span><span>Meeteam</span>
        </a>
        <a className="topbar-link" href="/">← Вернуться к ленте</a>
      </header>
      <main>
        <section className="lab-hero">
          <div className="lab-eyebrow"><span className="lab-live-dot" /> ТЕСТОВЫЙ СТЕНД</div>
          <h1>Что будет, если источник сломается?</h1>
          <p>Проверьте сбой на временной копии данных или сохраните сценарий для источника. Сохранённый сценарий сработает при следующем импорте на основной странице.</p>
        </section>

        <div className="lab-layout">
          <section className="lab-controls" aria-labelledby="lab-setup-title">
            <div className="lab-section-heading"><span>01 / НАСТРОЙКА</span><h2 id="lab-setup-title">Выберите источник и случай</h2></div>
            <label className="lab-field">
              <span>Источник</span>
              <select value={sourceID} onChange={(event) => { setSourceID(event.target.value); setReport(null); setSettingMessage(""); }}>
                {sources.map(([id, name]) => <option key={id} value={id}>{name}</option>)}
              </select>
            </label>
            <div className="lab-scope" role="note">
              <strong>{isRealAdapter ? "Проверка реального адаптера" : "Проверка конвейера импорта"}</strong>
              <p>{isRealAdapter
                ? `Для ${currentSource} заглушка отдаёт HTTP-ответ настоящему адаптеру. Проверяется разбор JSON или HTML и сохранение снимка.`
                : `Для ${currentSource} сбой подаётся на границе адаптера. Проверяется сохранение данных и изоляция источника; разбор его формата здесь не тестируется.`}</p>
            </div>
            <fieldset className="scenario-list">
              <legend>Сценарий</legend>
              {scenarios.map((item) => (
                <label className={`scenario-option ${scenario === item.id ? "is-selected" : ""}`} key={item.id}>
                  <input type="radio" name="scenario" value={item.id} checked={scenario === item.id} onChange={() => { setScenario(item.id); setReport(null); }} />
                  <span className="scenario-icon" aria-hidden="true">{item.icon}</span>
                  <span className="scenario-name"><strong>{item.label}</strong><small>{item.detail}</small></span>
                  <span className="scenario-radio" aria-hidden="true" />
                </label>
              ))}
            </fieldset>
            <button className="lab-run" type="button" onClick={() => void run()} disabled={busy}>
              {busy ? "Выполняем сценарий…" : "Проверить изолированно"}<span aria-hidden="true">→</span>
            </button>
            <div className="lab-persistent">
              <strong>Проверка в основной ленте</strong>
              <p>Сейчас для {currentSource}: {activeScenario ? scenarios.find((item) => item.id === activeScenario)?.label ?? activeScenario : "тестовый сбой выключен"}. Настройка сохраняется после перезапуска сервера.</p>
              <div className="lab-persistent-actions">
                <button type="button" disabled={settingBusy || scenario === "recovery"} onClick={() => void setSourceScenario(scenario)}>Сохранить сценарий</button>
                <button type="button" disabled={settingBusy || !activeScenario} onClick={() => void setSourceScenario("")}>Снять сбой</button>
              </div>
              {settingMessage && <p className="lab-setting-message" role="status">{settingMessage}</p>}
              <a href="/">Перейти к источнику в ленте →</a>
            </div>
          </section>

          <section className="lab-results" ref={resultsRef} aria-labelledby="lab-results-title" aria-live="polite">
            <div className="lab-section-heading"><span>02 / РЕЗУЛЬТАТ</span><h2 id="lab-results-title">Что произошло</h2></div>
            {error && <div className="lab-error" role="alert"><strong>Запуск не выполнен</strong><p>{error}</p><p>Убедитесь, что Go API запущен локально.</p></div>}
            {!error && !report && <div className="lab-empty"><span aria-hidden="true">↗</span><h3>Результат появится здесь</h3><p>Выберите сценарий слева и запустите проверку. Можно сравнить состояние до и после сбоя.</p></div>}
            {report && (
              <div className="lab-report">
                <div className={`lab-verdict ${report.passed ? "is-passed" : "is-failed"}`}>
                  <span className="verdict-icon" aria-hidden="true">{report.passed ? "✓" : "!"}</span>
                  <div><small>ИТОГ ПРОВЕРКИ</small><strong>{report.passed ? "Ожидаемое поведение подтверждено" : "Обнаружено несоответствие"}</strong></div>
                </div>
                <div className="lab-method"><strong>{report.mode}</strong><p>{report.explanation}</p></div>
                <div className="lab-comparison">
                  <div><small>ДО ЗАПУСКА</small><strong>{report.before.event_count} событий</strong><span>Успех: {dateTime(report.before.last_success_at)}</span><code>{report.before.status}</code></div>
                  <span className="comparison-arrow" aria-hidden="true">→</span>
                  <div><small>ПОСЛЕ ЗАПУСКА</small><strong>{report.after.event_count} событий</strong><span>Успех: {dateTime(report.after.last_success_at)}</span><code>{report.after.status}</code></div>
                </div>
                <div className="lab-response"><small>ОТВЕТ ЗАГЛУШКИ</small><code>{report.response_preview}</code><span>{report.request_count} HTTP-запросов · {report.elapsed_ms} мс</span></div>
                {report.error && <div className="lab-trace"><small>ОШИБКА АДАПТЕРА</small><code>{report.error}</code></div>}
                <div className="lab-checks"><h3>Проверки</h3>{report.checks.map((check) => <div key={check.label}><span className={check.passed ? "check-ok" : "check-fail"}>{check.passed ? "✓" : "×"}</span>{check.label}</div>)}</div>
              </div>
            )}
          </section>
        </div>
        <footer><span>Meeteam · лаборатория отказов</span><span>Изолированная проверка и сохранённый тестовый сбой не обращаются к внешним источникам</span></footer>
      </main>
    </div>
  );
}
