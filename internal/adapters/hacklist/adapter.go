package hacklist

import (
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
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"

	"meeteam/internal/model"
)

const defaultCatalogURL = "https://hacklist.ru/"

var spacesRE = regexp.MustCompile(`\s+`)

type Config struct {
	Client      *http.Client
	CatalogURL  string
	Concurrency int
	MinDelay    time.Duration
	Now         func() time.Time
}

type Adapter struct {
	client      *http.Client
	catalogURL  string
	concurrency int
	minDelay    time.Duration
	now         func() time.Time

	limiterMu   sync.Mutex
	nextRequest time.Time
}

type catalogCard struct {
	SourceEventID string
	Title         string
	SourceURL     string
	StartDate     string
	FormatLabel   string
	ImageURL      string
	Tags          []string
}

type rawEvent struct {
	SourceEventID string
	Title         string
	Description   string
	StartDate     string
	EndDate       string
	FormatLabel   string
	Organizer     string
	OfficialURL   string
	SourceURL     string
	ImageURL      string
	Tags          []string
}

type detailOutcome struct {
	index  int
	event  model.Event
	reject *model.RejectedEvent
}

func New(config Config) *Adapter {
	if config.Client == nil {
		config.Client = &http.Client{Timeout: 30 * time.Second}
	}
	if config.CatalogURL == "" {
		config.CatalogURL = defaultCatalogURL
	}
	if config.Concurrency <= 0 {
		config.Concurrency = 2
	}
	if config.MinDelay <= 0 {
		config.MinDelay = 250 * time.Millisecond
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &Adapter{
		client:      config.Client,
		catalogURL:  config.CatalogURL,
		concurrency: config.Concurrency,
		minDelay:    config.MinDelay,
		now:         config.Now,
	}
}

func (a *Adapter) Source() model.SourceState {
	return model.SourceState{
		ID:             model.HackListSourceID,
		Name:           "HackList",
		BaseURL:        defaultCatalogURL,
		Enabled:        true,
		Status:         "never",
		AdapterVersion: "hacklist-html-schema-v1",
	}
}

func (a *Adapter) Fetch(ctx context.Context) (model.ImportResult, error) {
	started := a.now().UTC()
	result := model.ImportResult{SourceID: model.HackListSourceID, StartedAt: started}

	body, err := a.getHTML(ctx, a.catalogURL)
	if err != nil {
		result.FinishedAt = a.now().UTC()
		return result, fmt.Errorf("fetch public catalog: %w", err)
	}
	cards, err := parseCatalogHTML(body, a.catalogURL)
	if err != nil {
		result.FinishedAt = a.now().UTC()
		return result, err
	}
	if len(cards) == 0 {
		result.FinishedAt = a.now().UTC()
		return result, errors.New("contract error: the open-registration section returned zero cards")
	}
	result.FetchedCount = len(cards)

	jobs := make(chan int)
	outcomes := make(chan detailOutcome, len(cards))
	workerCount := a.concurrency
	if workerCount > len(cards) {
		workerCount = len(cards)
	}
	var workers sync.WaitGroup
	for worker := 0; worker < workerCount; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for index := range jobs {
				outcomes <- a.fetchCard(ctx, index, cards[index], started)
			}
		}()
	}
	go func() {
		for index := range cards {
			jobs <- index
		}
		close(jobs)
		workers.Wait()
		close(outcomes)
	}()

	ordered := make([]detailOutcome, 0, len(cards))
	for outcome := range outcomes {
		ordered = append(ordered, outcome)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].index < ordered[j].index })
	for _, outcome := range ordered {
		if outcome.reject != nil {
			result.Rejected = append(result.Rejected, *outcome.reject)
			continue
		}
		result.Events = append(result.Events, outcome.event)
	}
	sort.Slice(result.Events, func(i, j int) bool {
		return result.Events[i].StartAt.Before(*result.Events[j].StartAt)
	})
	result.ValidCount = len(result.Events)
	result.FinishedAt = a.now().UTC()
	if result.ValidCount == 0 {
		return result, errors.New("contract error: no HackList card passed required-field validation")
	}
	return result, nil
}

func (a *Adapter) fetchCard(ctx context.Context, index int, card catalogCard, observedAt time.Time) detailOutcome {
	reject := func(reason string) detailOutcome {
		return detailOutcome{
			index: index,
			reject: &model.RejectedEvent{
				SourceEventID: card.SourceEventID,
				Title:         card.Title,
				Reason:        reason,
			},
		}
	}
	if card.SourceEventID == "" || card.SourceURL == "" {
		return reject("missing or invalid source card URL")
	}
	body, err := a.getHTML(ctx, card.SourceURL)
	if err != nil {
		return reject("detail request failed: " + compactError(err))
	}
	raw, err := parseDetailHTML(body, card)
	if err != nil {
		return reject(err.Error())
	}
	event, rejected := normalizeEvent(raw, observedAt, a.now())
	if rejected != nil {
		return detailOutcome{index: index, reject: rejected}
	}
	return detailOutcome{index: index, event: event}
}

func parseCatalogHTML(body []byte, catalogURL string) ([]catalogCard, error) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(string(body)))
	if err != nil {
		return nil, fmt.Errorf("parse catalog HTML: %w", err)
	}
	var active *goquery.Selection
	doc.Find("main section").EachWithBreak(func(_ int, section *goquery.Selection) bool {
		heading := strings.ToLower(cleanText(section.Find("h1").First().Text()))
		if strings.Contains(heading, "регистрация открыта") {
			active = section
			return false
		}
		return true
	})
	if active == nil {
		return nil, errors.New("contract error: open-registration section was not found")
	}

	seen := make(map[string]struct{})
	var cards []catalogCard
	active.Find("article").Each(func(_ int, article *goquery.Selection) {
		href, _ := article.Find("h3 a[href]").First().Attr("href")
		sourceURL := resolveURL(catalogURL, href)
		sourceEventID := eventIDFromURL(sourceURL)
		if sourceEventID != "" {
			if _, exists := seen[sourceEventID]; exists {
				return
			}
			seen[sourceEventID] = struct{}{}
		}
		startDate, _ := article.Find("time[datetime]").First().Attr("datetime")
		image, _ := article.Find("img[src]").First().Attr("src")
		card := catalogCard{
			SourceEventID: sourceEventID,
			Title:         cleanText(article.Find("h3").First().Text()),
			SourceURL:     sourceURL,
			StartDate:     strings.TrimSpace(startDate),
			FormatLabel:   cleanText(article.Find("span").First().Text()),
			ImageURL:      resolveURL(catalogURL, image),
		}
		article.Find(`a[href*="/events/tags/"]`).Each(func(_ int, tag *goquery.Selection) {
			appendUnique(&card.Tags, cleanText(tag.Text()))
		})
		cards = append(cards, card)
	})
	return cards, nil
}

func parseDetailHTML(body []byte, card catalogCard) (rawEvent, error) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(string(body)))
	if err != nil {
		return rawEvent{}, fmt.Errorf("parse detail HTML: %w", err)
	}
	schema := findEventSchema(doc)
	if schema == nil {
		return rawEvent{}, errors.New("Schema.org Event payload was not found")
	}

	raw := rawEvent{
		SourceEventID: card.SourceEventID,
		Title:         firstNonEmpty(jsonString(schema["name"]), card.Title),
		StartDate:     firstNonEmpty(jsonString(schema["startDate"]), card.StartDate),
		EndDate:       jsonString(schema["endDate"]),
		FormatLabel:   card.FormatLabel,
		Organizer:     schemaOrganizer(schema["organizer"]),
		SourceURL:     card.SourceURL,
		ImageURL:      firstNonEmpty(schemaImage(schema["image"]), card.ImageURL),
		Tags:          append([]string(nil), card.Tags...),
	}
	if description, exists := doc.Find(`meta[name="description"]`).First().Attr("content"); exists {
		raw.Description = cleanText(description)
	}
	if raw.Description == "" {
		raw.Description = cleanText(jsonString(schema["description"]))
	}
	raw.OfficialURL = participationURL(doc, card.SourceURL)
	if raw.OfficialURL == "" {
		raw.OfficialURL = validateHTTPURL(jsonString(schema["url"]))
	}
	for _, tag := range schemaKeywords(schema["keywords"]) {
		appendUnique(&raw.Tags, tag)
	}
	return raw, nil
}

func normalizeEvent(raw rawEvent, observedAt, now time.Time) (model.Event, *model.RejectedEvent) {
	reject := func(reason string) (model.Event, *model.RejectedEvent) {
		return model.Event{}, &model.RejectedEvent{
			SourceEventID: raw.SourceEventID,
			Title:         cleanText(raw.Title),
			Reason:        reason,
		}
	}
	if raw.SourceEventID == "" {
		return reject("missing source event id")
	}
	title := cleanText(raw.Title)
	if title == "" {
		return reject("missing title")
	}
	location, err := time.LoadLocation("Europe/Moscow")
	if err != nil {
		location = time.FixedZone("Europe/Moscow", 3*60*60)
	}
	startAt, precision, err := parseSourceDate(raw.StartDate, location)
	if err != nil {
		return reject("missing or invalid event start date")
	}
	var endAt *time.Time
	if strings.TrimSpace(raw.EndDate) != "" {
		parsedEnd, _, parseErr := parseSourceDate(raw.EndDate, location)
		if parseErr != nil {
			return reject("invalid event end date")
		}
		if parsedEnd.Before(startAt) {
			return reject("event end date is before start date")
		}
		endAt = &parsedEnd
	}
	today := time.Date(now.In(location).Year(), now.In(location).Month(), now.In(location).Day(), 0, 0, 0, 0, location)
	if endAt != nil && endAt.Before(today) {
		return reject("event has already ended")
	}
	if endAt == nil && startAt.Before(today) {
		return reject("event start date has already passed and no end date is provided")
	}
	officialURL := validateHTTPURL(raw.OfficialURL)
	if officialURL == "" {
		return reject("participation URL was not found")
	}
	sourceURL := validateHTTPURL(raw.SourceURL)
	if sourceURL == "" {
		return reject("invalid source card URL")
	}
	imageURL := validateHTTPURL(raw.ImageURL)
	description := truncateRunes(cleanText(raw.Description), 420)
	format := normalizeFormat(raw.FormatLabel)
	organizer := cleanText(raw.Organizer)
	tags := uniqueClean(raw.Tags)

	fingerprintInput := strings.Join([]string{
		raw.SourceEventID, title, raw.StartDate, raw.EndDate, format, organizer,
		officialURL, sourceURL, imageURL, description, strings.Join(tags, "|"),
	}, "\n")
	hash := sha256.Sum256([]byte(fingerprintInput))
	notes := []string{
		"card selected only from HackList's server-rendered open-registration section",
		"details decoded from the public Schema.org Event payload and HTML metadata",
		"a separate registration deadline is not exposed by the source",
	}
	if raw.StartDate == "" {
		notes = append(notes, "start date was missing from the catalog and recovered from the detail page")
	}

	return model.Event{
		ID:             model.HackListSourceID + ":" + raw.SourceEventID,
		SourceID:       model.HackListSourceID,
		SourceName:     "HackList",
		SourceEventID:  raw.SourceEventID,
		Title:          title,
		Description:    description,
		StartAt:        &startAt,
		EndAt:          endAt,
		StartPrecision: precision,
		Format:         format,
		Organizer:      organizer,
		OfficialURL:    officialURL,
		SourceURL:      sourceURL,
		ImageURL:       imageURL,
		Tags:           tags,
		ParsingNotes:   notes,
		RawFingerprint: hex.EncodeToString(hash[:]),
		FirstSeenAt:    observedAt,
		LastSeenAt:     observedAt,
	}, nil
}

func findEventSchema(doc *goquery.Document) map[string]any {
	var found map[string]any
	doc.Find(`script[type="application/ld+json"]`).EachWithBreak(func(_ int, script *goquery.Selection) bool {
		var value any
		if err := json.Unmarshal([]byte(script.Text()), &value); err != nil {
			return true
		}
		found = walkForEvent(value)
		return found == nil
	})
	return found
}

func walkForEvent(value any) map[string]any {
	switch typed := value.(type) {
	case map[string]any:
		if schemaTypeIsEvent(typed["@type"]) {
			return typed
		}
		if graph, ok := typed["@graph"]; ok {
			if found := walkForEvent(graph); found != nil {
				return found
			}
		}
	case []any:
		for _, item := range typed {
			if found := walkForEvent(item); found != nil {
				return found
			}
		}
	}
	return nil
}

func schemaTypeIsEvent(value any) bool {
	switch typed := value.(type) {
	case string:
		return strings.EqualFold(strings.TrimSpace(typed), "Event")
	case []any:
		for _, item := range typed {
			if schemaTypeIsEvent(item) {
				return true
			}
		}
	}
	return false
}

func schemaOrganizer(value any) string {
	switch typed := value.(type) {
	case map[string]any:
		return cleanText(jsonString(typed["name"]))
	case []any:
		for _, item := range typed {
			if name := schemaOrganizer(item); name != "" {
				return name
			}
		}
	}
	return ""
}

func schemaImage(value any) string {
	switch typed := value.(type) {
	case string:
		return validateHTTPURL(typed)
	case []any:
		for _, item := range typed {
			if image := schemaImage(item); image != "" {
				return image
			}
		}
	case map[string]any:
		return validateHTTPURL(jsonString(typed["url"]))
	}
	return ""
}

func schemaKeywords(value any) []string {
	var result []string
	switch typed := value.(type) {
	case string:
		for _, part := range strings.Split(typed, ",") {
			appendUnique(&result, cleanText(part))
		}
	case []any:
		for _, item := range typed {
			appendUnique(&result, cleanText(jsonString(item)))
		}
	}
	return result
}

func participationURL(doc *goquery.Document, sourceURL string) string {
	var result string
	doc.Find("main article a[href]").EachWithBreak(func(_ int, link *goquery.Selection) bool {
		if !strings.Contains(strings.ToLower(cleanText(link.Text())), "участвовать") {
			return true
		}
		href, _ := link.Attr("href")
		candidate := validateHTTPURL(resolveURL(sourceURL, href))
		if candidate == "" {
			return true
		}
		result = candidate
		return false
	})
	return result
}

func parseSourceDate(value string, location *time.Location) (time.Time, string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, "", errors.New("empty date")
	}
	if parsed, err := time.ParseInLocation("2006-01-02", value, location); err == nil {
		return parsed, "date", nil
	}
	if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return parsed, "datetime", nil
	}
	return time.Time{}, "", fmt.Errorf("unsupported date %q", value)
}

func normalizeFormat(value string) string {
	normalized := strings.ToLower(cleanText(value))
	hasOnline := strings.Contains(normalized, "онлайн") || strings.Contains(normalized, "online")
	hasOffline := strings.Contains(normalized, "офлайн") || strings.Contains(normalized, "offline")
	switch {
	case hasOnline && hasOffline:
		return "hybrid"
	case hasOnline:
		return "online"
	case hasOffline:
		return "offline"
	default:
		return ""
	}
}

func eventIDFromURL(value string) string {
	parsed, err := url.Parse(value)
	if err != nil {
		return ""
	}
	trimmed := strings.Trim(parsed.Path, "/")
	if trimmed == "" {
		return ""
	}
	base := path.Base(trimmed)
	if base == "." || base == "/" || strings.ContainsAny(base, "?#") {
		return ""
	}
	return base
}

func resolveURL(baseURL, reference string) string {
	base, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return ""
	}
	ref, err := url.Parse(strings.TrimSpace(reference))
	if err != nil {
		return ""
	}
	return validateHTTPURL(base.ResolveReference(ref).String())
}

func validateHTTPURL(value string) string {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" {
		return ""
	}
	return parsed.String()
}

func jsonString(value any) string {
	if text, ok := value.(string); ok {
		return strings.TrimSpace(text)
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func appendUnique(values *[]string, value string) {
	value = cleanText(value)
	if value == "" {
		return
	}
	for _, current := range *values {
		if strings.EqualFold(current, value) {
			return
		}
	}
	*values = append(*values, value)
}

func uniqueClean(values []string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		appendUnique(&result, value)
	}
	return result
}

func cleanText(value string) string {
	value = stdhtml.UnescapeString(value)
	value = strings.NewReplacer("\u00a0", " ", "\u202f", " ").Replace(value)
	return strings.TrimSpace(spacesRE.ReplaceAllString(value, " "))
}

func truncateRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit]) + "…"
}

func compactError(err error) string {
	if err == nil {
		return ""
	}
	return truncateRunes(cleanText(err.Error()), 240)
}

func (a *Adapter) getHTML(ctx context.Context, destination string) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if err := a.waitForSlot(ctx); err != nil {
			return nil, err
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, destination, nil)
		if err != nil {
			return nil, err
		}
		request.Header.Set("Accept", "text/html,application/xhtml+xml")
		request.Header.Set("User-Agent", "MeeteamRATPoC/0.1 (educational parser research)")
		response, err := a.client.Do(request)
		if err != nil {
			lastErr = err
			if !sleepContext(ctx, time.Duration(attempt+1)*350*time.Millisecond) {
				return nil, ctx.Err()
			}
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(response.Body, 6<<20))
		response.Body.Close()
		if readErr != nil {
			lastErr = readErr
			continue
		}
		if response.StatusCode >= 200 && response.StatusCode < 300 {
			if len(body) == 0 {
				return nil, errors.New("source returned an empty document")
			}
			return body, nil
		}
		lastErr = fmt.Errorf("unexpected HTTP status %d", response.StatusCode)
		if response.StatusCode != http.StatusTooManyRequests && response.StatusCode < 500 {
			return nil, lastErr
		}
		if !sleepContext(ctx, retryDelay(attempt, response.Header.Get("Retry-After"))) {
			return nil, ctx.Err()
		}
	}
	return nil, lastErr
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
