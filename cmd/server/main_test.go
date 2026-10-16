package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"meeteam/internal/importer"
	"meeteam/internal/model"
	"meeteam/internal/store"
)

type testAdapter struct{ calls int }

func (a *testAdapter) Source() model.SourceState {
	return model.SourceState{ID: model.DevfolioSourceID, Name: "Devfolio", Enabled: true}
}

func (a *testAdapter) Fetch(context.Context) (model.ImportResult, error) {
	a.calls++
	now := time.Now().UTC()
	start := now.Add(24 * time.Hour)
	return model.ImportResult{
		SourceID: model.DevfolioSourceID, StartedAt: now, FinishedAt: now,
		FetchedCount: 1, ValidCount: 1,
		Events: []model.Event{{ID: "devfolio:one", SourceID: model.DevfolioSourceID, Title: "Saved event", StartAt: &start}},
	}, nil
}

var _ importer.Adapter = (*testAdapter)(nil)

func TestPersistentLabScenarioAffectsNextImport(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	state, err := store.NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	adapter := &testAdapter{}
	if err := state.EnsureSources([]model.SourceState{adapter.Source()}); err != nil {
		t.Fatal(err)
	}
	app := &application{store: state}
	if _, err := app.runImport(context.Background(), adapter); err != nil {
		t.Fatal(err)
	}
	if _, err := state.SetTestScenario(model.DevfolioSourceID, "unavailable"); err != nil {
		t.Fatal(err)
	}

	reopened, err := store.NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	app.store = reopened
	run, err := app.runImport(context.Background(), adapter)
	if err == nil || run.Status != "failed" {
		t.Fatalf("want failed import, got run=%+v err=%v", run, err)
	}
	if adapter.calls != 1 {
		t.Fatalf("live adapter called during simulated failure: %d", adapter.calls)
	}
	snapshot := reopened.Snapshot()
	if len(snapshot.Events) != 1 || snapshot.Sources[0].TestScenario != "unavailable" || snapshot.Sources[0].Status != "failed" {
		t.Fatalf("failed import should keep saved event and scenario: %+v", snapshot)
	}
	if _, err := reopened.SetTestScenario(model.DevfolioSourceID, "invalid_event"); err != nil {
		t.Fatal(err)
	}
	lastSuccess := snapshot.Sources[0].LastSuccessAt
	run, err = app.runImport(context.Background(), adapter)
	if err != nil || run.Status != "partial" || run.RejectedCount != 1 {
		t.Fatalf("want warning, got run=%+v err=%v", run, err)
	}
	if adapter.calls != 1 {
		t.Fatal("live adapter called during simulated warning")
	}
	if got := reopened.Snapshot().Sources[0].LastSuccessAt; got == nil || !got.Equal(*lastSuccess) {
		t.Fatal("warning with no valid events advanced last success")
	}
	if _, err := reopened.SetTestScenario(model.DevfolioSourceID, ""); err != nil {
		t.Fatal(err)
	}
	run, err = app.runImport(context.Background(), adapter)
	if err != nil || run.Status != "success" || adapter.calls != 2 {
		t.Fatalf("want live recovery, got run=%+v err=%v calls=%d", run, err, adapter.calls)
	}
}
