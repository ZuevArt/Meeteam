package kaggle

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"

	"meeteam/internal/browser"
	"meeteam/internal/model"
)

const (
	defaultCatalogURL  = "https://www.kaggle.com/competitions?listOption=active&sortOption=oldest"
	defaultMaxPages    = 10
	defaultConcurrency = 3
	pageSize           = 20
)

var (
	spacesRE      = regexp.MustCompile(`\s+`)
	browserDateRE = regexp.MustCompile(`(?:Sun|Mon|Tue|Wed|Thu|Fri|Sat)\s+[A-Z][a-z]{2}\s+\d{1,2}\s+\d{4}\s+\d{2}:\d{2}:\d{2}\s+GMT[+-]\d{4}`)
)

type Config struct {
	CatalogURL  string
	ChromePath  string
	Timeout     time.Duration
	Concurrency int
	MaxPages    int
	Now         func() time.Time
	Renderer    Renderer
}

type Adapter struct {
	catalogURL string
	now        func() time.Time
	renderer   Renderer
}

type Renderer interface {
	Render(context.Context) ([]renderedCompetition, error)
}

type browserRenderer struct {
	catalogURL  string
	chromePath  string
	timeout     time.Duration
	concurrency int
	maxPages    int
}

type catalogCandidate struct {
	Slug      string
	SourceURL string
	ListText  string
	ListTitle string
}

type renderedCompetition struct {
	Slug        string
	SourceURL   string
	ListText    string
	HTML        string
	RenderError string
}

type rawCompetition struct {
	Slug        string
	Title       string
	Description string
	Organizer   string
	SourceURL   string
	ImageURL    string
	ListText    string
	StartAt     *time.Time
	EndAt       *time.Time
	Tags        []string
}

func New(config Config) *Adapter {
	if config.CatalogURL == "" {
		config.CatalogURL = defaultCatalogURL
	}
	if config.Timeout <= 0 {
		config.Timeout = 110 * time.Second
	}
	if config.Concurrency <= 0 {
		config.Concurrency = defaultConcurrency
	}
	if config.MaxPages <= 0 {
		config.MaxPages = defaultMaxPages
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.ChromePath == "" {
		config.ChromePath = strings.TrimSpace(os.Getenv("CHROME_BIN"))
	}
	if config.Renderer == nil {
		config.Renderer = &browserRenderer{
			catalogURL:  config.CatalogURL,
			chromePath:  config.ChromePath,
			timeout:     config.Timeout,
			concurrency: config.Concurrency,
			maxPages:    config.MaxPages,
		}
	}
	return &Adapter{catalogURL: config.CatalogURL, now: config.Now, renderer: config.Renderer}
}

func (a *Adapter) Source() model.SourceState {
	return model.SourceState{
		ID:             model.KaggleSourceID,
		Name:           "Kaggle",
		BaseURL:        "https://www.kaggle.com/competitions",
		Enabled:        true,
		Status:         "never",
		AdapterVersion: "kaggle-rendered-v1",
	}
}

func (a *Adapter) Fetch(ctx context.Context) (model.ImportResult, error) {
	started := a.now().UTC()
	result := model.ImportResult{SourceID: model.KaggleSourceID, StartedAt: started}

	rendered, err := a.renderer.Render(ctx)
	if err != nil {
		result.FinishedAt = a.now().UTC()
		return result, fmt.Errorf("render public active competitions: %w", err)
	}
	if len(rendered) == 0 {
		result.FinishedAt = a.now().UTC()
		return result, errors.New("contract error: the rendered Kaggle catalog returned no active competitions")
	}

	seen := make(map[string]struct{}, len(rendered))
	for _, page := range rendered {
		slug := strings.TrimSpace(page.Slug)
		if _, exists := seen[slug]; slug != "" && exists {
			continue
		}
		if slug != "" {
			seen[slug] = struct{}{}
		}
		result.FetchedCount++

		if page.RenderError != "" {
			result.Rejected = append(result.Rejected, model.RejectedEvent{
				SourceEventID: slug,
				Reason:        "detail page could not be rendered: " + cleanText(page.RenderError),
			})
			continue
		}
		raw, parseErr := parseRenderedCompetition(page)
		if parseErr != nil {
			result.Rejected = append(result.Rejected, model.RejectedEvent{
				SourceEventID: slug,
				Reason:        parseErr.Error(),
			})
			continue
		}
		event, reject := normalizeCompetition(raw, started)
		if reject != nil {
			result.Rejected = append(result.Rejected, *reject)
			continue
		}
		result.Events = append(result.Events, event)
	}

	sort.Slice(result.Events, func(i, j int) bool {
		left := sortableDate(result.Events[i])
		right := sortableDate(result.Events[j])
		if left.Equal(right) {
			return result.Events[i].Title < result.Events[j].Title
		}
		return left.Before(right)
	})
	result.ValidCount = len(result.Events)
	result.FinishedAt = a.now().UTC()
	if result.ValidCount == 0 {
		return result, errors.New("contract error: no Kaggle competition passed required-field validation")
	}
	return result, nil
}

func (r *browserRenderer) Render(ctx context.Context) ([]renderedCompetition, error) {
	renderCtx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	chromePath, err := resolveChromePath(r.chromePath)
	if err != nil {
		return nil, err
	}
	r.chromePath = chromePath
	candidates, err := r.renderCatalog(renderCtx)
	if err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		return nil, errors.New("contract error: active competition links were not found")
	}

	rendered := make([]renderedCompetition, len(candidates))
	jobs := make(chan int)
	workerCount := r.concurrency
	if workerCount > len(candidates) {
		workerCount = len(candidates)
	}
	var workers sync.WaitGroup
	for worker := 0; worker < workerCount; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for index := range jobs {
				candidate := candidates[index]
				item := renderedCompetition{
					Slug:      candidate.Slug,
					SourceURL: candidate.SourceURL,
					ListText:  candidate.ListText,
				}
				var html string
				var err error
				for attempt := 0; attempt < 2; attempt++ {
					jobContext, cancelJob := context.WithTimeout(renderCtx, 28*time.Second)
					html, err = r.dumpDOM(jobContext, candidate.SourceURL)
					cancelJob()
					if err == nil || renderCtx.Err() != nil {
						break
					}
				}
				if err != nil {
					item.RenderError = err.Error()
				} else {
					item.HTML = html
				}
				rendered[index] = item
			}
		}()
	}
	for index := range candidates {
		select {
		case <-renderCtx.Done():
			close(jobs)
			workers.Wait()
			return nil, renderCtx.Err()
		case jobs <- index:
		}
	}
	close(jobs)
	workers.Wait()
	return rendered, nil
}

func (r *browserRenderer) renderCatalog(ctx context.Context) ([]catalogCandidate, error) {
	seen := make(map[string]struct{})
	var result []catalogCandidate
	for pageNumber := 1; pageNumber <= r.maxPages; pageNumber++ {
		pageURL, err := catalogPageURL(r.catalogURL, pageNumber)
		if err != nil {
			return nil, err
		}
		html, err := r.dumpDOM(ctx, pageURL)
		if err != nil {
			return nil, fmt.Errorf("render catalog page %d: %w", pageNumber, err)
		}

		pageCandidates, err := parseCatalogPage(html, pageURL)
		if err != nil {
			return nil, fmt.Errorf("parse catalog page %d: %w", pageNumber, err)
		}
		newCount := 0
		for _, candidate := range pageCandidates {
			if isOngoingCandidate(candidate) {
				continue
			}
			if _, exists := seen[candidate.Slug]; exists {
				continue
			}
			seen[candidate.Slug] = struct{}{}
			result = append(result, candidate)
			newCount++
		}
		if len(pageCandidates) < pageSize || newCount == 0 {
			return result, nil
		}
	}
	return nil, fmt.Errorf("contract error: active catalog exceeded safety limit of %d pages", r.maxPages)
}

func isOngoingCandidate(candidate catalogCandidate) bool {
	return strings.Contains(strings.ToLower(candidate.ListText), "ongoing")
}

func (r *browserRenderer) dumpDOM(ctx context.Context, destination string) (string, error) {
	profileDirectory, err := os.MkdirTemp("", "meeteam-kaggle-chrome-")
	if err != nil {
		return "", fmt.Errorf("create temporary Chrome profile: %w", err)
	}
	defer os.RemoveAll(profileDirectory)

	arguments := []string{
		"--headless",
		"--disable-gpu",
		"--disable-background-networking",
		"--disable-renderer-backgrounding",
		"--hide-scrollbars",
		"--no-sandbox",
		"--window-size=1440,1200",
		"--lang=en-US",
		"--virtual-time-budget=30000",
		"--dump-dom",
		"--user-data-dir=" + profileDirectory,
		destination,
	}
	command := chromeCommand(ctx, r.chromePath, arguments)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		message := truncateRunes(cleanText(stderr.String()), 280)
		if message != "" {
			return "", fmt.Errorf("Chrome render failed: %w: %s", err, message)
		}
		return "", fmt.Errorf("Chrome render failed: %w", err)
	}
	if len(output) == 0 {
		return "", errors.New("Chrome rendered an empty document")
	}
	return string(output), nil
}

func chromeCommand(ctx context.Context, chromePath string, arguments []string) *exec.Cmd {
	if runtime.GOOS == "darwin" && runtime.GOARCH == "amd64" {
		if output, err := exec.Command("/usr/sbin/sysctl", "-n", "hw.optional.arm64").Output(); err == nil && strings.TrimSpace(string(output)) == "1" {
			armArguments := append([]string{"-arm64", chromePath}, arguments...)
			return exec.CommandContext(ctx, "/usr/bin/arch", armArguments...)
		}
	}
	return exec.CommandContext(ctx, chromePath, arguments...)
}

func catalogPageURL(catalogURL string, pageNumber int) (string, error) {
	parsed, err := url.Parse(catalogURL)
	if err != nil {
		return "", fmt.Errorf("parse catalog URL: %w", err)
	}
	query := parsed.Query()
	query.Set("listOption", "active")
	query.Set("sortOption", "oldest")
	query.Set("page", strconv.Itoa(pageNumber))
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

func parseCatalogPage(rawHTML, pageURL string) ([]catalogCandidate, error) {
	document, err := goquery.NewDocumentFromReader(strings.NewReader(rawHTML))
	if err != nil {
		return nil, fmt.Errorf("parse rendered HTML: %w", err)
	}
	base, err := url.Parse(pageURL)
	if err != nil {
		return nil, fmt.Errorf("parse page URL: %w", err)
	}

	seen := make(map[string]struct{})
	var candidates []catalogCandidate
	document.Find(`a[href]`).Each(func(_ int, selection *goquery.Selection) {
		href, _ := selection.Attr("href")
		resolved, resolveErr := base.Parse(strings.TrimSpace(href))
		if resolveErr != nil || resolved == nil {
			return
		}
		slug, ok := kaggleSlug(resolved)
		if !ok {
			return
		}
		if _, exists := seen[slug]; exists {
			return
		}
		seen[slug] = struct{}{}
		title, _ := selection.Attr("aria-label")
		candidates = append(candidates, catalogCandidate{
			Slug:      slug,
			SourceURL: "https://www.kaggle.com/competitions/" + url.PathEscape(slug),
			ListText:  cleanText(selection.Text()),
			ListTitle: cleanText(title),
		})
	})
	return candidates, nil
}

func parseRenderedCompetition(rendered renderedCompetition) (rawCompetition, error) {
	document, err := goquery.NewDocumentFromReader(strings.NewReader(rendered.HTML))
	if err != nil {
		return rawCompetition{}, fmt.Errorf("parse rendered detail HTML: %w", err)
	}
	raw := rawCompetition{
		Slug:      strings.TrimSpace(rendered.Slug),
		SourceURL: strings.TrimSpace(rendered.SourceURL),
		ListText:  cleanText(rendered.ListText),
		Title:     cleanText(document.Find("h1").First().Text()),
	}
	raw.Description, _ = document.Find(`meta[name="description"]`).First().Attr("content")
	raw.Description = cleanText(raw.Description)
	raw.ImageURL, _ = document.Find(`meta[property="og:image"]`).First().Attr("content")
	raw.ImageURL = strings.TrimSpace(raw.ImageURL)

	document.Find(`a[aria-label]`).EachWithBreak(func(_ int, selection *goquery.Selection) bool {
		label, _ := selection.Attr("aria-label")
		if strings.HasSuffix(label, "'s profile") {
			raw.Organizer = cleanText(strings.TrimSuffix(label, "'s profile"))
			return false
		}
		return true
	})

	document.Find(`a[href*="/competitions?tagIds="]`).Each(func(_ int, selection *goquery.Selection) {
		tag := cleanText(selection.Text())
		if tag != "" && !containsFold(raw.Tags, tag) {
			raw.Tags = append(raw.Tags, tag)
		}
	})
	if startAt, ok := findLabeledTime(document, "Start"); ok {
		raw.StartAt = &startAt
	}
	if endAt, ok := findLabeledTime(document, "Close"); ok {
		raw.EndAt = &endAt
	}
	return raw, nil
}

func findLabeledTime(document *goquery.Document, label string) (time.Time, bool) {
	var result time.Time
	found := false
	document.Find("*").EachWithBreak(func(_ int, selection *goquery.Selection) bool {
		if selection.Children().Length() != 0 || cleanText(selection.Text()) != label {
			return true
		}
		current := selection.Parent()
		for depth := 0; depth < 5 && current.Length() > 0; depth++ {
			if containsMultipleTimelineLabels(current) {
				return true
			}
			current.Find(`[title]`).EachWithBreak(func(_ int, candidate *goquery.Selection) bool {
				value, _ := candidate.Attr("title")
				parsed, ok := parseBrowserTime(value)
				if !ok {
					return true
				}
				result = parsed
				found = true
				return false
			})
			if found {
				return false
			}
			current = current.Parent()
		}
		return true
	})
	return result, found
}

func containsMultipleTimelineLabels(selection *goquery.Selection) bool {
	labels := make(map[string]struct{}, 2)
	selection.Find("*").EachWithBreak(func(_ int, child *goquery.Selection) bool {
		if child.Children().Length() != 0 {
			return true
		}
		value := cleanText(child.Text())
		if value == "Start" || value == "Close" {
			labels[value] = struct{}{}
		}
		return len(labels) < 2
	})
	return len(labels) > 1
}

func parseBrowserTime(value string) (time.Time, bool) {
	match := browserDateRE.FindString(value)
	if match == "" {
		return time.Time{}, false
	}
	parsed, err := time.Parse("Mon Jan 2 2006 15:04:05 GMT-0700", match)
	if err != nil {
		return time.Time{}, false
	}
	return parsed.UTC(), true
}

func normalizeCompetition(raw rawCompetition, observedAt time.Time) (model.Event, *model.RejectedEvent) {
	reject := func(reason string) (model.Event, *model.RejectedEvent) {
		return model.Event{}, &model.RejectedEvent{
			SourceEventID: strings.TrimSpace(raw.Slug),
			Title:         cleanText(raw.Title),
			Reason:        reason,
		}
	}
	if raw.Slug == "" {
		return reject("missing source competition id")
	}
	if !validKaggleURL(raw.SourceURL, raw.Slug) {
		return reject("missing or invalid Kaggle competition URL")
	}
	title := cleanText(raw.Title)
	if title == "" {
		return reject("missing title")
	}
	if raw.StartAt == nil || raw.StartAt.IsZero() {
		return reject("missing or invalid competition start time")
	}
	if raw.EndAt != nil {
		if raw.EndAt.Before(*raw.StartAt) {
			return reject("competition close time is before start time")
		}
		if raw.EndAt.Before(observedAt) {
			return reject("competition has already closed")
		}
	}

	tags := append([]string(nil), raw.Tags...)
	for _, candidate := range classificationTags(raw.ListText) {
		if !containsFold(tags, candidate) {
			tags = append(tags, candidate)
		}
	}
	if len(tags) == 0 {
		tags = append(tags, "Kaggle")
	}

	description := truncateRunes(cleanText(raw.Description), 420)
	fingerprintInput := strings.Join([]string{
		raw.Slug,
		title,
		description,
		raw.StartAt.Format(time.RFC3339Nano),
		formatOptionalTime(raw.EndAt),
		raw.Organizer,
		raw.ImageURL,
		strings.Join(tags, "|"),
	}, "\n")
	hash := sha256.Sum256([]byte(fingerprintInput))
	notes := []string{
		"competition fields parsed from the rendered public Kaggle catalog and detail page",
		"only the public active filter is traversed; Kaggle's authenticated API and private endpoints are not called",
		"Kaggle competitions run on the online platform",
	}
	if raw.EndAt == nil {
		notes = append(notes, "Kaggle marks the competition as ongoing and exposes no close date")
	} else {
		notes = append(notes, "registration deadline and listing expiry use Kaggle's exact close time")
	}

	event := model.Event{
		ID:             model.KaggleSourceID + ":" + raw.Slug,
		SourceID:       model.KaggleSourceID,
		SourceName:     "Kaggle",
		SourceEventID:  raw.Slug,
		Title:          title,
		Description:    description,
		StartAt:        raw.StartAt,
		EndAt:          raw.EndAt,
		StartPrecision: "datetime",
		Format:         "online",
		Organizer:      cleanText(raw.Organizer),
		OfficialURL:    raw.SourceURL,
		SourceURL:      raw.SourceURL,
		ImageURL:       validateOptionalURL(raw.ImageURL),
		Tags:           tags,
		ParsingNotes:   notes,
		RawFingerprint: hex.EncodeToString(hash[:]),
		FirstSeenAt:    observedAt,
		LastSeenAt:     observedAt,
	}
	if raw.EndAt != nil {
		deadline := *raw.EndAt
		expires := *raw.EndAt
		event.RegistrationDeadlineAt = &deadline
		event.ListingExpiresAt = &expires
	}
	return event, nil
}

func kaggleSlug(value *url.URL) (string, bool) {
	if value == nil {
		return "", false
	}
	host := strings.ToLower(strings.TrimPrefix(value.Hostname(), "www."))
	if host != "kaggle.com" {
		return "", false
	}
	path := strings.Trim(value.EscapedPath(), "/")
	parts := strings.Split(path, "/")
	if len(parts) != 2 || parts[0] != "competitions" {
		return "", false
	}
	slug, err := url.PathUnescape(parts[1])
	if err != nil || strings.TrimSpace(slug) == "" || strings.ContainsAny(slug, "/?#") {
		return "", false
	}
	return slug, true
}

func validKaggleURL(value, expectedSlug string) bool {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Scheme != "https" {
		return false
	}
	slug, ok := kaggleSlug(parsed)
	return ok && slug == expectedSlug
}

func classificationTags(listText string) []string {
	lower := strings.ToLower(listText)
	var tags []string
	for _, candidate := range []struct {
		needle string
		label  string
	}{
		{needle: "hackathon", label: "Hackathon"},
		{needle: "code competition", label: "Code Competition"},
		{needle: "simulation competition", label: "Simulation"},
		{needle: "featured", label: "Featured"},
		{needle: "research", label: "Research"},
		{needle: "getting started", label: "Getting Started"},
		{needle: "playground", label: "Playground"},
	} {
		if strings.Contains(lower, candidate.needle) && !containsFold(tags, candidate.label) {
			tags = append(tags, candidate.label)
		}
	}
	return tags
}

func sortableDate(event model.Event) time.Time {
	for _, candidate := range []*time.Time{event.RegistrationDeadlineAt, event.EndAt, event.StartAt} {
		if candidate != nil {
			return *candidate
		}
	}
	return time.Date(9999, time.December, 31, 0, 0, 0, 0, time.UTC)
}

func formatOptionalTime(value *time.Time) string {
	if value == nil {
		return ""
	}
	return value.Format(time.RFC3339Nano)
}

func validateOptionalURL(value string) string {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" {
		return ""
	}
	return parsed.String()
}

func cleanText(value string) string {
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

func resolveChromePath(configured string) (string, error) {
	return browser.ResolveChromePath(configured)
}
