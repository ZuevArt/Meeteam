package hackathonradar

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	stdhtml "html"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"meeteam/internal/browser"
	"meeteam/internal/model"
)

const (
	defaultCatalogURL = "https://www.hackathonradar.com/database"
	defaultMaxPages   = 20
	flightMarker      = `self.__next_f.push([1,`
)

var (
	spacesRE    = regexp.MustCompile(`\s+`)
	tagsRE      = regexp.MustCompile(`(?s)<[^>]+>`)
	breaksRE    = regexp.MustCompile(`(?i)<br\s*/?>|</(?:div|p|li|h[1-6]|ul|ol)>`)
	referenceRE = regexp.MustCompile(`^\$[0-9a-z]+$`)
	sha256HexRE = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

type Config struct {
	Client     *http.Client
	CatalogURL string
	ChromePath string
	MinDelay   time.Duration
	MaxPages   int
	Now        func() time.Time
}

type Adapter struct {
	client     *http.Client
	catalogURL string
	chromePath string
	useBrowser bool
	minDelay   time.Duration
	maxPages   int
	now        func() time.Time

	limiterMu   sync.Mutex
	nextRequest time.Time
}

type databasePage struct {
	Events     []rawEvent
	Pagination pagination
}

type pagination struct {
	Page       int  `json:"page"`
	Limit      int  `json:"limit"`
	Total      int  `json:"total"`
	TotalPages int  `json:"totalPages"`
	HasNext    bool `json:"hasNext"`
	HasPrev    bool `json:"hasPrev"`
}

type rawEvent struct {
	ID                   string         `json:"id"`
	Slug                 string         `json:"slug"`
	Title                string         `json:"title"`
	StartAt              string         `json:"start_at"`
	EndAt                string         `json:"end_at"`
	Timezone             string         `json:"timezone"`
	Categories           []string       `json:"categories"`
	Type                 string         `json:"type"`
	VenueName            *string        `json:"venue_name"`
	Address              *string        `json:"address"`
	City                 *string        `json:"city"`
	Country              *string        `json:"country"`
	WebsiteURL           string         `json:"website_url"`
	RegistrationURL      string         `json:"registration_url"`
	Description          string         `json:"description"`
	DescriptionPlainText *string        `json:"description_plain_text"`
	Language             string         `json:"language"`
	Hash                 string         `json:"hash"`
	Organizers           []rawOrganizer `json:"organizers"`
}

type rawOrganizer struct {
	Name string `json:"name"`
}

func New(config Config) *Adapter {
	useBrowser := config.Client == nil && config.CatalogURL == ""
	if config.Client == nil {
		config.Client = &http.Client{Timeout: 35 * time.Second}
	}
	if config.CatalogURL == "" {
		config.CatalogURL = defaultCatalogURL
	}
	if config.MinDelay <= 0 {
		config.MinDelay = 250 * time.Millisecond
	}
	if config.MaxPages <= 0 {
		config.MaxPages = defaultMaxPages
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &Adapter{
		client:     config.Client,
		catalogURL: config.CatalogURL,
		chromePath: config.ChromePath,
		useBrowser: useBrowser,
		minDelay:   config.MinDelay,
		maxPages:   config.MaxPages,
		now:        config.Now,
	}
}

func (a *Adapter) Source() model.SourceState {
	return model.SourceState{
		ID:             model.HackathonRadarSourceID,
		Name:           "HackathonRadar",
		BaseURL:        "https://www.hackathonradar.com/discover",
		Enabled:        true,
		Status:         "never",
		AdapterVersion: "hackathonradar-next-html-v2",
	}
}

func (a *Adapter) Fetch(ctx context.Context) (model.ImportResult, error) {
	started := a.now().UTC()
	result := model.ImportResult{SourceID: model.HackathonRadarSourceID, StartedAt: started}

	first, err := a.fetchPage(ctx, 1)
	if err != nil {
		result.FinishedAt = a.now().UTC()
		return result, fmt.Errorf("fetch public database page 1: %w", err)
	}
	if len(first.Events) == 0 {
		result.FinishedAt = a.now().UTC()
		return result, errors.New("contract error: the public database returned zero events")
	}
	if first.Pagination.TotalPages < 1 {
		result.FinishedAt = a.now().UTC()
		return result, errors.New("contract error: pagination metadata is missing")
	}
	if first.Pagination.TotalPages > a.maxPages {
		result.FinishedAt = a.now().UTC()
		return result, fmt.Errorf("contract error: public page count %d exceeds safety limit %d", first.Pagination.TotalPages, a.maxPages)
	}

	allEvents := append([]rawEvent(nil), first.Events...)
	for pageNumber := 2; pageNumber <= first.Pagination.TotalPages; pageNumber++ {
		page, err := a.fetchPage(ctx, pageNumber)
		if err != nil {
			result.FinishedAt = a.now().UTC()
			return result, fmt.Errorf("fetch public database page %d: %w", pageNumber, err)
		}
		if page.Pagination.Page != pageNumber {
			result.FinishedAt = a.now().UTC()
			return result, fmt.Errorf("contract error: requested page %d, received page %d", pageNumber, page.Pagination.Page)
		}
		if len(page.Events) == 0 {
			result.FinishedAt = a.now().UTC()
			return result, fmt.Errorf("contract error: page %d was empty before pagination ended", pageNumber)
		}
		allEvents = append(allEvents, page.Events...)
	}

	today := startOfDay(a.now().UTC())
	seen := make(map[string]struct{}, len(allEvents))
	for _, raw := range allEvents {
		key := strings.TrimSpace(raw.ID)
		if key == "" {
			key = strings.TrimSpace(raw.Slug)
		}
		if _, exists := seen[key]; key != "" && exists {
			continue
		}
		if key != "" {
			seen[key] = struct{}{}
		}
		result.FetchedCount++

		event, reject := normalizeEvent(raw, started, today)
		if reject != nil {
			result.Rejected = append(result.Rejected, *reject)
			continue
		}
		result.Events = append(result.Events, event)
	}

	sort.Slice(result.Events, func(i, j int) bool {
		return result.Events[i].StartAt.Before(*result.Events[j].StartAt)
	})
	result.ValidCount = len(result.Events)
	result.FinishedAt = a.now().UTC()
	if result.ValidCount == 0 {
		return result, errors.New("contract error: no event passed required-field validation")
	}
	if first.Pagination.Total > 0 && result.FetchedCount < first.Pagination.Total {
		result.Warnings = append(result.Warnings, fmt.Sprintf("catalog changed during pagination: expected at least %d records, received %d unique records", first.Pagination.Total, result.FetchedCount))
	}
	return result, nil
}

func (a *Adapter) fetchPage(ctx context.Context, pageNumber int) (databasePage, error) {
	u, err := url.Parse(a.catalogURL)
	if err != nil {
		return databasePage{}, fmt.Errorf("parse catalog URL: %w", err)
	}
	query := u.Query()
	query.Set("page", fmt.Sprintf("%d", pageNumber))
	query.Set("sortBy", "title")
	query.Set("sortOrder", "asc")
	u.RawQuery = query.Encode()
	if a.useBrowser {
		return a.fetchPageViaBrowser(ctx, u.String())
	}

	var body []byte
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if err := a.waitForSlot(ctx); err != nil {
			return databasePage{}, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return databasePage{}, err
		}
		req.Header.Set("Accept", "text/html,application/xhtml+xml")
		req.Header.Set("User-Agent", "MeeteamRATPoC/0.1 (educational parser research)")
		resp, err := a.client.Do(req)
		if err != nil {
			lastErr = err
			if !sleepContext(ctx, time.Duration(attempt+1)*350*time.Millisecond) {
				return databasePage{}, ctx.Err()
			}
			continue
		}
		body, err = io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			break
		}
		lastErr = fmt.Errorf("unexpected HTTP status %d", resp.StatusCode)
		body = nil
		if resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode < 500 {
			return databasePage{}, lastErr
		}
		if !sleepContext(ctx, retryDelay(attempt, resp.Header.Get("Retry-After"))) {
			return databasePage{}, ctx.Err()
		}
	}
	if len(body) == 0 {
		return databasePage{}, lastErr
	}
	page, err := parseDatabaseHTML(body)
	if err != nil {
		return databasePage{}, err
	}
	return page, nil
}

func (a *Adapter) fetchPageViaBrowser(ctx context.Context, pageURL string) (databasePage, error) {
	path, err := browser.ResolveChromePath(a.chromePath)
	if err != nil {
		return databasePage{}, err
	}
	profile, err := os.MkdirTemp("", "meeteam-radar-chrome-")
	if err != nil {
		return databasePage{}, fmt.Errorf("create Chrome profile: %w", err)
	}
	defer os.RemoveAll(profile)
	renderCtx, cancel := context.WithTimeout(ctx, 22*time.Second)
	defer cancel()
	command := exec.CommandContext(renderCtx, path,
		"--headless", "--disable-javascript", "--disable-gpu", "--no-sandbox", "--disable-background-networking",
		"--virtual-time-budget=12000", "--dump-dom", "--user-data-dir="+profile, pageURL,
	)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	body, err := command.Output()
	if err != nil {
		return databasePage{}, fmt.Errorf("Chrome render failed: %w: %s", err, truncateRunes(cleanText(stderr.String()), 220))
	}
	return parseDatabaseHTML(body)
}

func parseDatabaseHTML(body []byte) (databasePage, error) {
	chunks, err := decodeFlightChunks(body)
	if err != nil {
		return databasePage{}, err
	}
	for _, chunk := range chunks {
		dataIndex := strings.Index(chunk, `"data":`)
		paginationIndex := strings.Index(chunk, `"pagination":`)
		if dataIndex < 0 || paginationIndex < 0 {
			continue
		}

		var events []rawEvent
		decoder := json.NewDecoder(strings.NewReader(chunk[dataIndex+len(`"data":`):]))
		if err := decoder.Decode(&events); err != nil {
			return databasePage{}, fmt.Errorf("decode embedded events: %w", err)
		}
		var metadata pagination
		decoder = json.NewDecoder(strings.NewReader(chunk[paginationIndex+len(`"pagination":`):]))
		if err := decoder.Decode(&metadata); err != nil {
			return databasePage{}, fmt.Errorf("decode embedded pagination: %w", err)
		}
		return databasePage{Events: events, Pagination: metadata}, nil
	}
	return databasePage{}, errors.New("contract error: Next.js payload with public events was not found")
}

func decodeFlightChunks(body []byte) ([]string, error) {
	raw := string(body)
	var chunks []string
	for offset := 0; ; {
		index := strings.Index(raw[offset:], flightMarker)
		if index < 0 {
			break
		}
		start := offset + index + len(flightMarker)
		decoder := json.NewDecoder(strings.NewReader(raw[start:]))
		var chunk string
		if err := decoder.Decode(&chunk); err != nil {
			offset = start
			continue
		}
		chunks = append(chunks, chunk)
		offset = start + int(decoder.InputOffset())
	}
	if len(chunks) == 0 {
		return nil, errors.New("contract error: Next.js flight chunks were not found")
	}
	return chunks, nil
}

func normalizeEvent(raw rawEvent, observedAt, today time.Time) (model.Event, *model.RejectedEvent) {
	reject := func(reason string) (model.Event, *model.RejectedEvent) {
		return model.Event{}, &model.RejectedEvent{
			SourceEventID: strings.TrimSpace(raw.ID),
			Title:         cleanText(raw.Title),
			Reason:        reason,
		}
	}
	if strings.TrimSpace(raw.ID) == "" {
		return reject("missing source event id")
	}
	if strings.TrimSpace(raw.Slug) == "" {
		return reject("missing source event slug")
	}
	title := cleanText(raw.Title)
	if title == "" {
		return reject("missing title")
	}
	startAt, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(raw.StartAt))
	if err != nil {
		return reject("missing or invalid event start time")
	}
	endAt, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(raw.EndAt))
	if err != nil {
		return reject("missing or invalid event end time")
	}
	if endAt.Before(startAt) {
		return reject("event end time is before start time")
	}
	if endAt.Before(today) {
		return reject("event has already ended")
	}
	officialURL := chooseOfficialURL(raw)
	if officialURL == "" {
		return reject("official event URL was not found")
	}
	sourceURL := sourceEventURL(raw.Slug)
	if sourceURL == "" {
		return reject("invalid source event URL")
	}

	description := ""
	if raw.DescriptionPlainText != nil {
		description = cleanText(*raw.DescriptionPlainText)
	}
	if description == "" && !referenceRE.MatchString(strings.TrimSpace(raw.Description)) {
		description = htmlToText(raw.Description)
	}
	if description == "" {
		description = locationDescription(raw)
	}
	description = truncateRunes(description, 420)

	tags := make([]string, 0, len(raw.Categories))
	for _, category := range raw.Categories {
		category = cleanText(category)
		if category != "" && !containsFold(tags, category) {
			tags = append(tags, category)
		}
	}
	organizer := ""
	for _, candidate := range raw.Organizers {
		if organizer = cleanText(candidate.Name); organizer != "" {
			break
		}
	}

	fingerprintInput := strings.Join([]string{
		raw.ID, raw.Slug, title, raw.StartAt, raw.EndAt, raw.Type,
		strings.Join(tags, "|"), officialURL, description,
	}, "\n")
	hash := sha256.Sum256([]byte(fingerprintInput))
	notes := []string{
		"event fields decoded from the server-rendered public HackathonRadar database page",
		"public HTML pagination is used; the robots-disallowed API is not called",
		"catalog pages are sorted by title to keep page boundaries stable during import",
		"registration deadline is not exposed in the public database payload",
	}
	if sha256HexRE.MatchString(strings.ToLower(raw.Hash)) {
		notes = append(notes, "source content hash is present and the adapter also computes its own fingerprint")
	}

	return model.Event{
		ID:             model.HackathonRadarSourceID + ":" + raw.ID,
		SourceID:       model.HackathonRadarSourceID,
		SourceName:     "HackathonRadar",
		SourceEventID:  raw.ID,
		Title:          title,
		Description:    description,
		StartAt:        &startAt,
		EndAt:          &endAt,
		StartPrecision: "datetime",
		Format:         normalizeFormat(raw.Type),
		Organizer:      organizer,
		OfficialURL:    officialURL,
		SourceURL:      sourceURL,
		Tags:           tags,
		ParsingNotes:   notes,
		RawFingerprint: hex.EncodeToString(hash[:]),
		FirstSeenAt:    observedAt,
		LastSeenAt:     observedAt,
	}, nil
}

func chooseOfficialURL(raw rawEvent) string {
	website := validateHTTPURL(raw.WebsiteURL)
	registration := validateHTTPURL(raw.RegistrationURL)
	if website != "" {
		return website
	}
	return registration
}

func sourceEventURL(slug string) string {
	slug = strings.Trim(strings.TrimSpace(slug), "/")
	if slug == "" || strings.ContainsAny(slug, "?#") {
		return ""
	}
	return "https://www.hackathonradar.com/database/hackathon/" + url.PathEscape(slug)
}

func normalizeFormat(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "online", "virtual":
		return "online"
	case "in-person", "in person", "offline":
		return "offline"
	case "hybrid":
		return "hybrid"
	default:
		return ""
	}
}

func locationDescription(raw rawEvent) string {
	parts := make([]string, 0, 3)
	for _, value := range []*string{raw.VenueName, raw.City, raw.Country} {
		if value == nil {
			continue
		}
		cleaned := cleanText(*value)
		if cleaned != "" && !containsFold(parts, cleaned) {
			parts = append(parts, cleaned)
		}
	}
	if len(parts) == 0 && raw.Address != nil {
		parts = append(parts, cleanText(*raw.Address))
	}
	return strings.Join(parts, ", ")
}

func validateHTTPURL(value string) string {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" {
		return ""
	}
	return parsed.String()
}

func htmlToText(value string) string {
	value = stdhtml.UnescapeString(value)
	value = breaksRE.ReplaceAllString(value, " ")
	value = tagsRE.ReplaceAllString(value, " ")
	return cleanText(value)
}

func cleanText(value string) string {
	value = stdhtml.UnescapeString(value)
	value = strings.NewReplacer("\u00a0", " ", "\u202f", " ").Replace(value)
	return strings.TrimSpace(spacesRE.ReplaceAllString(value, " "))
}

func containsFold(values []string, value string) bool {
	for _, current := range values {
		if strings.EqualFold(current, value) {
			return true
		}
	}
	return false
}

func truncateRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit]) + "…"
}

func startOfDay(value time.Time) time.Time {
	return time.Date(value.Year(), value.Month(), value.Day(), 0, 0, 0, 0, value.Location())
}

func (a *Adapter) waitForSlot(ctx context.Context) error {
	a.limiterMu.Lock()
	now := time.Now()
	slot := now
	if a.nextRequest.After(now) {
		slot = a.nextRequest
	}
	a.nextRequest = slot.Add(a.minDelay)
	a.limiterMu.Unlock()
	if delay := time.Until(slot); delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
	return nil
}

func retryDelay(attempt int, retryAfter string) time.Duration {
	if seconds, err := time.ParseDuration(strings.TrimSpace(retryAfter) + "s"); err == nil && seconds > 0 {
		if seconds > 5*time.Second {
			return 5 * time.Second
		}
		return seconds
	}
	return time.Duration(attempt+1) * 350 * time.Millisecond
}

func sleepContext(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
