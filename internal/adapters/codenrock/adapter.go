package codenrock

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/chromedp/chromedp"

	"meeteam/internal/model"
)

const defaultCatalogURL = "https://codenrock.com/contests"

var (
	spacesRE   = regexp.MustCompile(`\s+`)
	fullDateRE = regexp.MustCompile(`(?i)([0-3]?\d)\s+(январ(?:я|ь)|феврал(?:я|ь)|март(?:а)?|апрел(?:я|ь)|ма(?:я|й)|июн(?:я|ь)|июл(?:я|ь)|август(?:а)?|сентябр(?:я|ь)|октябр(?:я|ь)|ноябр(?:я|ь)|декабр(?:я|ь))\s+(\d{4})`)
)

type Config struct {
	CatalogURL string
	ChromePath string
	Timeout    time.Duration
	Now        func() time.Time
	Renderer   Renderer
}

type Adapter struct {
	catalogURL string
	now        func() time.Time
	renderer   Renderer
}

type Renderer interface {
	Render(context.Context, time.Time) ([]renderedMonth, error)
}

type renderedMonth struct {
	Label string
	HTML  string
}

type browserRenderer struct {
	catalogURL string
	chromePath string
	timeout    time.Duration
}

type rawCard struct {
	Slug              string
	Title             string
	Description       string
	RegistrationDates string
	EventDates        string
	SourceURL         string
	ImageURL          string
	OnPlatform        bool
}

func New(config Config) *Adapter {
	if config.CatalogURL == "" {
		config.CatalogURL = defaultCatalogURL
	}
	if config.Timeout <= 0 {
		config.Timeout = 110 * time.Second
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.ChromePath == "" {
		config.ChromePath = strings.TrimSpace(os.Getenv("CHROME_BIN"))
	}
	if config.Renderer == nil {
		config.Renderer = &browserRenderer{
			catalogURL: config.CatalogURL,
			chromePath: config.ChromePath,
			timeout:    config.Timeout,
		}
	}
	return &Adapter{catalogURL: config.CatalogURL, now: config.Now, renderer: config.Renderer}
}

func (a *Adapter) Source() model.SourceState {
	return model.SourceState{
		ID:             model.CodenrockSourceID,
		Name:           "Codenrock",
		BaseURL:        "https://codenrock.com/",
		Enabled:        true,
		Status:         "never",
		AdapterVersion: "codenrock-rendered-v1",
	}
}

func (a *Adapter) Fetch(ctx context.Context) (model.ImportResult, error) {
	started := a.now().UTC()
	result := model.ImportResult{SourceID: model.CodenrockSourceID, StartedAt: started}

	months, err := a.renderer.Render(ctx, a.now())
	if err != nil {
		result.FinishedAt = a.now().UTC()
		return result, fmt.Errorf("render public catalog: %w", err)
	}
	if len(months) == 0 {
		result.FinishedAt = a.now().UTC()
		return result, errors.New("contract error: the rendered catalog returned no current or future month")
	}

	location := moscowLocation()
	today := startOfDay(a.now().In(location))
	seen := make(map[string]struct{})
	for _, month := range months {
		cards, parseErr := parseRenderedMonth(month.HTML, a.catalogURL)
		if parseErr != nil {
			result.Warnings = append(result.Warnings, fmt.Sprintf("%s: %v", month.Label, parseErr))
			continue
		}
		for _, card := range cards {
			if _, exists := seen[card.Slug]; exists {
				continue
			}
			seen[card.Slug] = struct{}{}
			result.FetchedCount++

			event, reject := normalizeCard(card, started, today, location)
			if reject != nil {
				result.Rejected = append(result.Rejected, *reject)
				continue
			}
			result.Events = append(result.Events, event)
		}
	}

	sort.Slice(result.Events, func(i, j int) bool {
		return result.Events[i].StartAt.Before(*result.Events[j].StartAt)
	})
	result.ValidCount = len(result.Events)
	result.FinishedAt = a.now().UTC()
	if result.FetchedCount == 0 {
		return result, errors.New("contract error: no event cards were found in the rendered catalog")
	}
	if result.ValidCount == 0 {
		return result, errors.New("contract error: no event passed required-field validation")
	}
	return result, nil
}

func (r *browserRenderer) Render(ctx context.Context, now time.Time) ([]renderedMonth, error) {
	renderCtx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	options := append([]chromedp.ExecAllocatorOption{}, chromedp.DefaultExecAllocatorOptions[:]...)
	options = append(options,
		chromedp.Flag("disable-background-networking", true),
		chromedp.Flag("disable-renderer-backgrounding", true),
		chromedp.Flag("hide-scrollbars", true),
		chromedp.Flag("no-sandbox", true),
		chromedp.WSURLReadTimeout(45*time.Second),
	)
	if r.chromePath != "" {
		options = append(options, chromedp.ExecPath(r.chromePath))
	}

	allocatorContext, cancelAllocator := chromedp.NewExecAllocator(renderCtx, options...)
	defer cancelAllocator()
	browserContext, cancelBrowser := chromedp.NewContext(allocatorContext)
	defer cancelBrowser()

	var labels []string
	if err := chromedp.Run(browserContext,
		chromedp.Navigate(r.catalogURL),
		chromedp.WaitVisible(`main button[data-testid^="month-slider-"]`, chromedp.ByQuery),
		chromedp.Poll(`document.querySelectorAll('main a[data-testid^="contest-card-"]').length > 0`, nil,
			chromedp.WithPollingTimeout(35*time.Second)),
		chromedp.Evaluate(`Array.from(document.querySelectorAll('main button[data-testid^="month-slider-"]')).map(button => button.textContent.trim())`, &labels),
	); err != nil {
		return nil, browserError(err)
	}

	selected := selectCurrentAndFutureMonths(labels, now.In(moscowLocation()))
	if len(selected) == 0 {
		return nil, errors.New("month selector contains no current or future month")
	}

	months := make([]renderedMonth, 0, len(selected))
	for _, label := range selected {
		quotedLabel := strconv.Quote(strings.ToLower(label))
		clickExpression := fmt.Sprintf(`(() => { const label = %s; const button = Array.from(document.querySelectorAll('main button[data-testid^="month-slider-"]')).find(item => item.textContent.trim().toLocaleLowerCase('ru-RU') === label); if (!button) return false; button.click(); return true; })()`, quotedLabel)
		readyExpression := fmt.Sprintf(`Array.from(document.querySelectorAll('main h2')).some(item => item.textContent.trim().toLocaleLowerCase('ru-RU') === %s) && document.querySelectorAll('main a[data-testid^="contest-card-"]').length > 0`, quotedLabel)
		var clicked bool
		var html string
		if err := chromedp.Run(browserContext,
			chromedp.Evaluate(clickExpression, &clicked),
			chromedp.Poll(readyExpression, nil, chromedp.WithPollingTimeout(20*time.Second)),
			chromedp.OuterHTML("main", &html, chromedp.ByQuery),
		); err != nil {
			return nil, fmt.Errorf("render month %q: %w", label, browserError(err))
		}
		if !clicked {
			return nil, fmt.Errorf("render month %q: selector button disappeared", label)
		}
		months = append(months, renderedMonth{Label: label, HTML: html})
	}
	return months, nil
}

func parseRenderedMonth(rawHTML, catalogURL string) ([]rawCard, error) {
	document, err := goquery.NewDocumentFromReader(strings.NewReader(rawHTML))
	if err != nil {
		return nil, fmt.Errorf("parse rendered HTML: %w", err)
	}
	base, err := url.Parse(catalogURL)
	if err != nil {
		return nil, fmt.Errorf("parse catalog URL: %w", err)
	}

	var cards []rawCard
	document.Find(`a[data-testid^="contest-card-"]`).Each(func(_ int, selection *goquery.Selection) {
		href, _ := selection.Attr("href")
		resolved, resolveErr := base.Parse(strings.TrimSpace(href))
		if resolveErr != nil || resolved == nil {
			return
		}
		slug := strings.Trim(strings.TrimPrefix(resolved.Path, "/contests/"), "/")
		imageURL, _ := selection.Find("img").First().Attr("src")
		cards = append(cards, rawCard{
			Slug:              slug,
			Title:             cleanText(selection.Find(`[data-testid$="-name"]`).First().Text()),
			Description:       cleanText(selection.Find("p").First().Text()),
			RegistrationDates: cleanText(selection.Find(`[data-testid$="-registration-dates"]`).First().Text()),
			EventDates:        cleanText(selection.Find(`[data-testid$="-event-dates"]`).First().Text()),
			SourceURL:         resolved.String(),
			ImageURL:          strings.TrimSpace(imageURL),
			OnPlatform:        selection.Find(`[data-testid$="-platform-badge"]`).Length() > 0,
		})
	})
	return cards, nil
}

func normalizeCard(card rawCard, observedAt, today time.Time, location *time.Location) (model.Event, *model.RejectedEvent) {
	reject := func(reason string) (model.Event, *model.RejectedEvent) {
		return model.Event{}, &model.RejectedEvent{SourceEventID: card.Slug, Title: card.Title, Reason: reason}
	}
	if card.Slug == "" {
		return reject("missing source event id")
	}
	if card.Title == "" {
		return reject("missing title")
	}
	if !validCodenrockURL(card.SourceURL) {
		return reject("missing or invalid Codenrock event URL")
	}

	eventDates := parseRussianDates(card.EventDates, location)
	if len(eventDates) == 0 {
		return reject("event start date was not found in the rendered card")
	}
	startAt := eventDates[0]
	endAt := eventDates[len(eventDates)-1]
	if endAt.Before(today) {
		return reject("event has already ended")
	}
	registrationDates := parseRussianDates(card.RegistrationDates, location)
	var deadlineAt *time.Time
	if len(registrationDates) > 0 {
		deadline := registrationDates[len(registrationDates)-1]
		deadlineAt = &deadline
	}

	fingerprintInput := strings.Join([]string{
		card.Slug,
		card.Title,
		card.Description,
		card.RegistrationDates,
		card.EventDates,
		card.ImageURL,
	}, "\n")
	hash := sha256.Sum256([]byte(fingerprintInput))
	notes := []string{
		"event dates parsed from the rendered public Codenrock card",
		"catalog rendered in headless Chrome because the public page is client-side",
	}
	if card.OnPlatform {
		notes = append(notes, "event is hosted on Codenrock")
	}
	if deadlineAt == nil {
		notes = append(notes, "registration deadline is absent in the catalog card")
	}

	return model.Event{
		ID:                     model.CodenrockSourceID + ":" + card.Slug,
		SourceID:               model.CodenrockSourceID,
		SourceName:             "Codenrock",
		SourceEventID:          card.Slug,
		Title:                  card.Title,
		Description:            card.Description,
		StartAt:                &startAt,
		EndAt:                  &endAt,
		StartPrecision:         "date",
		RegistrationDeadlineAt: deadlineAt,
		Format:                 detectFormat(card.Title + " " + card.Description),
		OfficialURL:            card.SourceURL,
		SourceURL:              card.SourceURL,
		ImageURL:               validateOptionalURL(card.ImageURL),
		Tags:                   detectTags(card.Title + " " + card.Description),
		ParsingNotes:           notes,
		RawFingerprint:         hex.EncodeToString(hash[:]),
		FirstSeenAt:            observedAt,
		LastSeenAt:             observedAt,
	}, nil
}

func selectCurrentAndFutureMonths(labels []string, now time.Time) []string {
	current := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
	limit := current.AddDate(1, 0, 0)
	type candidate struct {
		label string
		date  time.Time
	}
	var candidates []candidate
	seen := make(map[string]struct{})
	for _, label := range labels {
		label = cleanText(label)
		if _, exists := seen[strings.ToLower(label)]; exists {
			continue
		}
		date, ok := parseMonthLabel(label, now.Location())
		if !ok || date.Before(current) || !date.Before(limit) {
			continue
		}
		seen[strings.ToLower(label)] = struct{}{}
		candidates = append(candidates, candidate{label: label, date: date})
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].date.Before(candidates[j].date) })
	result := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		result = append(result, candidate.label)
	}
	return result
}

func parseMonthLabel(label string, location *time.Location) (time.Time, bool) {
	parts := strings.Fields(strings.ToLower(cleanText(label)))
	if len(parts) != 2 {
		return time.Time{}, false
	}
	month, ok := russianMonth(parts[0])
	if !ok {
		return time.Time{}, false
	}
	year, err := strconv.Atoi(parts[1])
	if err != nil || year < 2000 || year > 2200 {
		return time.Time{}, false
	}
	return time.Date(year, month, 1, 0, 0, 0, 0, location), true
}

func parseRussianDates(text string, location *time.Location) []time.Time {
	matches := fullDateRE.FindAllStringSubmatch(text, -1)
	result := make([]time.Time, 0, len(matches))
	for _, match := range matches {
		day, dayErr := strconv.Atoi(match[1])
		month, monthOK := russianMonth(match[2])
		year, yearErr := strconv.Atoi(match[3])
		if dayErr != nil || yearErr != nil || !monthOK || day < 1 || day > 31 {
			continue
		}
		parsed := time.Date(year, month, day, 0, 0, 0, 0, location)
		if parsed.Day() == day && parsed.Month() == month {
			result = append(result, parsed)
		}
	}
	return result
}

func russianMonth(value string) (time.Month, bool) {
	value = strings.ToLower(value)
	switch {
	case strings.HasPrefix(value, "январ"):
		return time.January, true
	case strings.HasPrefix(value, "феврал"):
		return time.February, true
	case strings.HasPrefix(value, "март"):
		return time.March, true
	case strings.HasPrefix(value, "апрел"):
		return time.April, true
	case value == "май" || value == "мая":
		return time.May, true
	case strings.HasPrefix(value, "июн"):
		return time.June, true
	case strings.HasPrefix(value, "июл"):
		return time.July, true
	case strings.HasPrefix(value, "август"):
		return time.August, true
	case strings.HasPrefix(value, "сентябр"):
		return time.September, true
	case strings.HasPrefix(value, "октябр"):
		return time.October, true
	case strings.HasPrefix(value, "ноябр"):
		return time.November, true
	case strings.HasPrefix(value, "декабр"):
		return time.December, true
	default:
		return 0, false
	}
}

func detectFormat(text string) string {
	lower := strings.ToLower(text)
	switch {
	case strings.Contains(lower, "гибрид"):
		return "hybrid"
	case strings.Contains(lower, "офлайн") || strings.Contains(lower, "оффлайн") || strings.Contains(lower, "очный финал"):
		return "offline"
	case strings.Contains(lower, "онлайн"):
		return "online"
	default:
		return ""
	}
}

func detectTags(text string) []string {
	lower := strings.ToLower(text)
	var tags []string
	for _, candidate := range []struct {
		needle string
		label  string
	}{
		{needle: "хакатон", label: "Хакатон"},
		{needle: "геймджем", label: "Game Jam"},
		{needle: "ctf", label: "CTF"},
		{needle: "машинн", label: "ML"},
		{needle: " ml", label: "ML"},
		{needle: "искусственн", label: "AI"},
		{needle: " ии", label: "AI"},
		{needle: "олимпиад", label: "Олимпиада"},
		{needle: "акселератор", label: "Акселератор"},
	} {
		if strings.Contains(lower, candidate.needle) && !contains(tags, candidate.label) {
			tags = append(tags, candidate.label)
		}
	}
	return tags
}

func validCodenrockURL(value string) bool {
	parsed, err := url.Parse(value)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return false
	}
	host := strings.ToLower(strings.TrimPrefix(parsed.Hostname(), "www."))
	return host == "codenrock.com" && strings.HasPrefix(parsed.Path, "/contests/")
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

func contains(values []string, value string) bool {
	for _, current := range values {
		if current == value {
			return true
		}
	}
	return false
}

func startOfDay(value time.Time) time.Time {
	return time.Date(value.Year(), value.Month(), value.Day(), 0, 0, 0, 0, value.Location())
}

func moscowLocation() *time.Location {
	location, err := time.LoadLocation("Europe/Moscow")
	if err != nil {
		return time.FixedZone("Europe/Moscow", 3*60*60)
	}
	return location
}

func browserError(err error) error {
	if err == nil {
		return nil
	}
	message := err.Error()
	if strings.Contains(message, "executable file not found") || strings.Contains(message, "could not find") {
		return errors.New("Chrome or Chromium was not found; set CHROME_BIN to the browser executable")
	}
	return err
}
