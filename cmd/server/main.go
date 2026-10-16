package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"meeteam/internal/adapters/codenrock"
	"meeteam/internal/adapters/devfolio"
	"meeteam/internal/adapters/devpost"
	"meeteam/internal/adapters/hackathonradar"
	"meeteam/internal/adapters/hackathonspro"
	"meeteam/internal/adapters/hackathonsrus"
	"meeteam/internal/adapters/hacklist"
	"meeteam/internal/adapters/kaggle"
	"meeteam/internal/importer"
	"meeteam/internal/lab"
	"meeteam/internal/model"
	"meeteam/internal/store"
)

type application struct {
	adapters     map[string]importer.Adapter
	adapterOrder []importer.Adapter
	store        *store.FileStore
	logger       *slog.Logger
	runMu        sync.Mutex
	labEnabled   bool
}

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	httpAddr := envOr("HTTP_ADDR", "127.0.0.1:8080")
	statePath := envOr("STATE_FILE", "data/state.json")
	fileStore, err := store.NewFileStore(statePath)
	if err != nil {
		logger.Error("open state store", "error", err)
		os.Exit(1)
	}

	hackathonsRusAdapter := hackathonsrus.New(hackathonsrus.Config{
		Concurrency: envInt("IMPORT_CONCURRENCY", 2),
		MinDelay:    time.Duration(envInt("IMPORT_DELAY_MS", 250)) * time.Millisecond,
	})
	codenrockAdapter := codenrock.New(codenrock.Config{
		ChromePath: envOr("CHROME_BIN", ""),
		Timeout:    time.Duration(envInt("CODENROCK_TIMEOUT_SECONDS", 110)) * time.Second,
	})
	hackathonsProAdapter := hackathonspro.New(hackathonspro.Config{
		Concurrency: envInt("IMPORT_CONCURRENCY", 2),
		MinDelay:    time.Duration(envInt("IMPORT_DELAY_MS", 250)) * time.Millisecond,
	})
	hackathonRadarAdapter := hackathonradar.New(hackathonradar.Config{
		ChromePath: envOr("CHROME_BIN", ""),
		MinDelay:   time.Duration(envInt("IMPORT_DELAY_MS", 250)) * time.Millisecond,
	})
	kaggleAdapter := kaggle.New(kaggle.Config{
		ChromePath:  envOr("CHROME_BIN", ""),
		Timeout:     time.Duration(envInt("KAGGLE_TIMEOUT_SECONDS", 115)) * time.Second,
		Concurrency: envInt("KAGGLE_CONCURRENCY", 3),
	})
	hackListAdapter := hacklist.New(hacklist.Config{
		Concurrency: envInt("IMPORT_CONCURRENCY", 2),
		MinDelay:    time.Duration(envInt("IMPORT_DELAY_MS", 250)) * time.Millisecond,
	})
	devpostAdapter := devpost.New(devpost.Config{
		MinDelay: time.Duration(envInt("IMPORT_DELAY_MS", 250)) * time.Millisecond,
	})
	devfolioAdapter := devfolio.New(devfolio.Config{
		MinDelay: time.Duration(envInt("IMPORT_DELAY_MS", 250)) * time.Millisecond,
	})
	adapterOrder := []importer.Adapter{hackathonsRusAdapter, codenrockAdapter, hackathonsProAdapter, hackathonRadarAdapter, kaggleAdapter, hackListAdapter, devpostAdapter, devfolioAdapter}
	adapters := make(map[string]importer.Adapter, len(adapterOrder))
	sources := make([]model.SourceState, 0, len(adapterOrder))
	for index, adapter := range adapterOrder {
		source := adapter.Source()
		source.ImportOrder = index + 1
		adapters[source.ID] = adapter
		sources = append(sources, source)
	}
	if err := fileStore.EnsureSources(sources); err != nil {
		logger.Error("register import sources", "error", err)
		os.Exit(1)
	}
	app := &application{
		adapters:     adapters,
		adapterOrder: adapterOrder,
		store:        fileStore,
		logger:       logger,
		labEnabled:   isLoopbackBind(httpAddr) && envBool("LAB_ENABLED", true),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", app.handleHealth)
	mux.HandleFunc("GET /api/events", app.handleEvents)
	mux.HandleFunc("GET /api/sources", app.handleSources)
	mux.HandleFunc("GET /api/import-runs", app.handleRuns)
	mux.HandleFunc("POST /api/imports/{source}", app.handleImport)
	mux.HandleFunc("POST /api/lab/run", app.handleLabRun)
	mux.HandleFunc("PUT /api/lab/scenarios/{source}", app.handleLabScenario)
	mux.Handle("/", staticHandler(envOr("WEB_DIST", "web/dist")))

	server := &http.Server{
		Addr:              httpAddr,
		Handler:           requestLogger(logger, mux),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      2 * time.Minute,
		IdleTimeout:       60 * time.Second,
	}

	if envBool("AUTO_IMPORT", true) {
		go func() {
			for _, adapter := range app.adapterOrder {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
				_, err := app.runImport(ctx, adapter)
				cancel()
				if err != nil {
					logger.Warn("initial import failed; previous snapshot stays available", "source", adapter.Source().ID, "error", err)
				}
			}
		}()
	}

	logger.Info("server listening", "address", "http://"+server.Addr)
	if err := server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		logger.Error("server stopped", "error", err)
		os.Exit(1)
	}
}

func (app *application) runImport(ctx context.Context, adapter importer.Adapter) (model.ImportRun, error) {
	if !app.runMu.TryLock() {
		return model.ImportRun{}, errors.New("import is already running")
	}
	defer app.runMu.Unlock()

	if scenario := app.store.TestScenario(adapter.Source().ID); lab.IsFaultScenario(scenario) {
		adapter = lab.FaultAdapter{Adapter: adapter, Scenario: scenario}
	}
	return importer.Run(ctx, adapter, app.store)
}

func (app *application) handleLabScenario(w http.ResponseWriter, request *http.Request) {
	if !app.labEnabled || !requestIsLoopback(request.RemoteAddr) {
		writeJSON(w, http.StatusForbidden, map[string]string{"message": "diagnostic lab is local only"})
		return
	}
	sourceID := request.PathValue("source")
	if _, ok := app.adapters[sourceID]; !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "unknown source"})
		return
	}
	var input struct {
		Scenario string `json:"scenario"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, request.Body, 4096))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil || (input.Scenario != "" && !lab.IsFaultScenario(input.Scenario)) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "unknown test scenario"})
		return
	}
	source, err := app.store.SetTestScenario(sourceID, input.Scenario)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"source": source})
}

func (app *application) handleLabRun(w http.ResponseWriter, request *http.Request) {
	if !app.labEnabled || !requestIsLoopback(request.RemoteAddr) {
		writeJSON(w, http.StatusForbidden, map[string]string{"message": "diagnostic lab is local only"})
		return
	}
	var input lab.Request
	if err := json.NewDecoder(http.MaxBytesReader(w, request.Body, 4096)).Decode(&input); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "invalid lab request"})
		return
	}
	if _, ok := app.adapters[input.SourceID]; !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "unknown source"})
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 10*time.Second)
	defer cancel()
	report, err := lab.Run(ctx, input)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, report)
}

func isLoopbackBind(address string) bool {
	host, _, err := net.SplitHostPort(address)
	return err == nil && (host == "localhost" || net.ParseIP(host).IsLoopback())
}

func requestIsLoopback(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	return err == nil && net.ParseIP(host).IsLoopback()
}

func (app *application) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "time": time.Now().UTC()})
}

func (app *application) handleEvents(w http.ResponseWriter, _ *http.Request) {
	snapshot := app.store.Snapshot()
	writeJSON(w, http.StatusOK, map[string]any{
		"events":       snapshot.Events,
		"count":        len(snapshot.Events),
		"generated_at": time.Now().UTC(),
	})
}

func (app *application) handleSources(w http.ResponseWriter, _ *http.Request) {
	snapshot := app.store.Snapshot()
	writeJSON(w, http.StatusOK, map[string]any{"sources": snapshot.Sources})
}

func (app *application) handleRuns(w http.ResponseWriter, _ *http.Request) {
	snapshot := app.store.Snapshot()
	writeJSON(w, http.StatusOK, map[string]any{"runs": snapshot.Runs})
}

func (app *application) handleImport(w http.ResponseWriter, request *http.Request) {
	sourceID := request.PathValue("source")
	adapter, ok := app.adapters[sourceID]
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"error":   "unknown_source",
			"message": "unknown import source: " + sourceID,
		})
		return
	}

	ctx, cancel := context.WithTimeout(request.Context(), 2*time.Minute)
	defer cancel()
	run, err := app.runImport(ctx, adapter)
	if err != nil {
		status := http.StatusBadGateway
		if err.Error() == "import is already running" {
			status = http.StatusConflict
		}
		writeJSON(w, status, map[string]any{
			"error":   "import_failed",
			"message": err.Error(),
			"run":     run,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"run": run})
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		slog.Error("write JSON response", "error", err)
	}
}

func staticHandler(root string) http.Handler {
	absolute, err := filepath.Abs(root)
	if err != nil {
		absolute = root
	}
	files := http.FileServer(http.Dir(absolute))
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if strings.HasPrefix(request.URL.Path, "/api/") {
			http.NotFound(w, request)
			return
		}
		path := filepath.Join(absolute, filepath.Clean(request.URL.Path))
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			files.ServeHTTP(w, request)
			return
		}
		index := filepath.Join(absolute, "index.html")
		if _, err := os.Stat(index); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{
				"message": "frontend is not built; run npm install and npm run dev in ./web",
			})
			return
		}
		http.ServeFile(w, request, index)
	})
}

func requestLogger(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		started := time.Now()
		next.ServeHTTP(w, request)
		logger.Info("http request", "method", request.Method, "path", request.URL.Path, "duration", time.Since(started))
	})
}

func envOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func envInt(name string, fallback int) int {
	value, err := strconv.Atoi(strings.TrimSpace(os.Getenv(name)))
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}

func envBool(name string, fallback bool) bool {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return fallback
	}
	return parsed
}
