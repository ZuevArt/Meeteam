package importer

import (
	"context"
	"fmt"
	"time"

	"meeteam/internal/model"
)

// Run executes the same import and last-known-good transition for live and lab adapters.
func Run(ctx context.Context, adapter Adapter, state Store) (model.ImportRun, error) {
	source := adapter.Source()
	started := time.Now().UTC()
	pending := model.ImportRun{SourceID: source.ID, StartedAt: started, Status: "running"}
	if err := state.MarkRunning(source, pending); err != nil {
		return model.ImportRun{}, err
	}

	result, fetchErr := adapter.Fetch(ctx)
	if result.StartedAt.IsZero() {
		result.StartedAt = started
	}
	if result.FinishedAt.IsZero() {
		result.FinishedAt = time.Now().UTC()
	}
	if fetchErr != nil {
		failed := model.ImportRun{
			ID:            fmt.Sprintf("%s-%d", source.ID, result.StartedAt.UnixNano()),
			SourceID:      source.ID,
			StartedAt:     result.StartedAt,
			FinishedAt:    result.FinishedAt,
			Status:        "failed",
			FetchedCount:  result.FetchedCount,
			ValidCount:    result.ValidCount,
			RejectedCount: len(result.Rejected),
			ErrorCode:     "source_import_failed",
			ErrorSummary:  fetchErr.Error(),
			Rejected:      result.Rejected,
		}
		if err := state.ApplyFailure(source, failed); err != nil {
			return failed, fmt.Errorf("record failed import: %w", err)
		}
		return failed, fetchErr
	}

	run, err := state.ApplySuccess(result)
	if err != nil {
		return run, err
	}
	return run, nil
}
