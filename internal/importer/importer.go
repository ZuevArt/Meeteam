package importer

import (
	"context"

	"meeteam/internal/model"
)

type Adapter interface {
	Source() model.SourceState
	Fetch(context.Context) (model.ImportResult, error)
}

type Store interface {
	MarkRunning(source model.SourceState, startedAt model.ImportRun) error
	ApplySuccess(result model.ImportResult) (model.ImportRun, error)
	ApplyFailure(source model.SourceState, run model.ImportRun) error
	Snapshot() model.Snapshot
}
