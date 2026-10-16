package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"meeteam/internal/model"
)

type persistedState struct {
	Events  map[string]model.Event       `json:"events"`
	Sources map[string]model.SourceState `json:"sources"`
	Runs    []model.ImportRun            `json:"runs"`
}

type FileStore struct {
	mu    sync.RWMutex
	path  string
	state persistedState
}

// NewMemoryStore uses the production state transitions without writing a file.
func NewMemoryStore() *FileStore {
	return &FileStore{state: persistedState{
		Events:  make(map[string]model.Event),
		Sources: make(map[string]model.SourceState),
	}}
}

func NewFileStore(path string) (*FileStore, error) {
	s := &FileStore{
		path: path,
		state: persistedState{
			Events:  make(map[string]model.Event),
			Sources: make(map[string]model.SourceState),
		},
	}

	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read state: %w", err)
	}
	if err := json.Unmarshal(data, &s.state); err != nil {
		return nil, fmt.Errorf("decode state: %w", err)
	}
	if s.state.Events == nil {
		s.state.Events = make(map[string]model.Event)
	}
	if s.state.Sources == nil {
		s.state.Sources = make(map[string]model.SourceState)
	}
	return s, nil
}

func (s *FileStore) EnsureSources(sources []model.SourceState) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	changed := false
	for _, source := range sources {
		current, exists := s.state.Sources[source.ID]
		before := current
		mergeSourceMetadata(&current, source)
		if !exists || current != before {
			s.state.Sources[source.ID] = current
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return s.persistLocked()
}

// SetTestScenario persists a local fault selected in the parser lab.
func (s *FileStore) SetTestScenario(sourceID, scenario string) (model.SourceState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	source, ok := s.state.Sources[sourceID]
	if !ok {
		return model.SourceState{}, fmt.Errorf("unknown source: %s", sourceID)
	}
	source.TestScenario = scenario
	if scenario == "" {
		source.TestSetAt = nil
	} else {
		now := time.Now().UTC()
		source.TestSetAt = &now
	}
	s.state.Sources[sourceID] = source
	return source, s.persistLocked()
}

func (s *FileStore) TestScenario(sourceID string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state.Sources[sourceID].TestScenario
}

func (s *FileStore) MarkRunning(source model.SourceState, run model.ImportRun) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	current := s.state.Sources[source.ID]
	mergeSourceMetadata(&current, source)
	current.Status = "running"
	current.LastAttemptAt = &run.StartedAt
	current.LastError = ""
	s.state.Sources[source.ID] = current
	return s.persistLocked()
}

func (s *FileStore) ApplySuccess(result model.ImportResult) (model.ImportRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, incoming := range result.Events {
		if existing, ok := s.state.Events[incoming.ID]; ok {
			incoming.FirstSeenAt = existing.FirstSeenAt
		}
		if incoming.FirstSeenAt.IsZero() {
			incoming.FirstSeenAt = result.FinishedAt
		}
		incoming.LastSeenAt = result.FinishedAt
		s.state.Events[incoming.ID] = incoming
	}

	source := s.state.Sources[result.SourceID]
	source.ID = result.SourceID
	source.Enabled = true
	source.LastAttemptAt = &result.StartedAt
	if result.ValidCount > 0 {
		source.LastSuccessAt = &result.FinishedAt
	}
	source.FetchedCount = result.FetchedCount
	source.ValidCount = result.ValidCount
	source.RejectedCount = len(result.Rejected)
	source.LastError = ""
	if len(result.Rejected) > 0 {
		source.Status = "partial"
	} else {
		source.Status = "success"
	}
	s.state.Sources[result.SourceID] = source

	run := model.ImportRun{
		ID:            fmt.Sprintf("%s-%d", result.SourceID, result.StartedAt.UnixNano()),
		SourceID:      result.SourceID,
		StartedAt:     result.StartedAt,
		FinishedAt:    result.FinishedAt,
		Status:        source.Status,
		FetchedCount:  result.FetchedCount,
		ValidCount:    result.ValidCount,
		UpsertedCount: len(result.Events),
		RejectedCount: len(result.Rejected),
		Rejected:      result.Rejected,
	}
	s.appendRunLocked(run)
	return run, s.persistLocked()
}

func (s *FileStore) ApplyFailure(source model.SourceState, run model.ImportRun) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	current := s.state.Sources[source.ID]
	mergeSourceMetadata(&current, source)
	current.Status = "failed"
	current.LastAttemptAt = &run.StartedAt
	current.LastError = run.ErrorSummary
	s.state.Sources[source.ID] = current
	s.appendRunLocked(run)
	return s.persistLocked()
}

func (s *FileStore) Snapshot() model.Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()

	events := make([]model.Event, 0, len(s.state.Events))
	for _, event := range s.state.Events {
		events = append(events, event)
	}
	sort.Slice(events, func(i, j int) bool {
		if events[i].StartAt == nil {
			return false
		}
		if events[j].StartAt == nil {
			return true
		}
		return events[i].StartAt.Before(*events[j].StartAt)
	})

	sources := make([]model.SourceState, 0, len(s.state.Sources))
	for _, source := range s.state.Sources {
		sources = append(sources, source)
	}
	sort.SliceStable(sources, func(i, j int) bool {
		leftOrder := sources[i].ImportOrder
		rightOrder := sources[j].ImportOrder
		switch {
		case leftOrder > 0 && rightOrder > 0 && leftOrder != rightOrder:
			return leftOrder < rightOrder
		case leftOrder > 0 && rightOrder == 0:
			return true
		case leftOrder == 0 && rightOrder > 0:
			return false
		default:
			return sources[i].Name < sources[j].Name
		}
	})

	runs := append([]model.ImportRun(nil), s.state.Runs...)
	return model.Snapshot{Events: events, Sources: sources, Runs: runs}
}

func (s *FileStore) appendRunLocked(run model.ImportRun) {
	s.state.Runs = append([]model.ImportRun{run}, s.state.Runs...)
	if len(s.state.Runs) > 30 {
		s.state.Runs = s.state.Runs[:30]
	}
}

func (s *FileStore) persistLocked() error {
	if s.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	data, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write temporary state: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("commit state: %w", err)
	}
	return nil
}

func mergeSourceMetadata(dst *model.SourceState, src model.SourceState) {
	dst.ID = src.ID
	dst.Name = src.Name
	dst.BaseURL = src.BaseURL
	if src.ImportOrder > 0 {
		dst.ImportOrder = src.ImportOrder
	}
	dst.Enabled = src.Enabled
	dst.AdapterVersion = src.AdapterVersion
	if dst.Status == "" {
		dst.Status = "never"
	}
}

func NowUTC() time.Time { return time.Now().UTC() }
