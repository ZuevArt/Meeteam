package hackathonsrus

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
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"

	"meeteam/internal/model"
)

const (
	defaultFeedURL = "https://feeds.tildacdn.com/api/getfeed/"
	defaultPostURL = "https://feeds.tildacdn.com/api/getpost/"
	defaultFeedUID = "617755803461"
	defaultRecID   = "488755787"
)

var (
	spacesRE = regexp.MustCompile(`\s+`)
	tagsRE   = regexp.MustCompile(`(?s)<[^>]+>`)
	breaksRE = regexp.MustCompile(`(?i)<br\s*/?>|</(?:div|p|li|h[1-6]|ul|ol)>`)

	ruMonthPattern = `(январ(?:я|ь)|феврал(?:я|ь)|март(?:а)?|апрел(?:я|ь)|ма(?:я|й)|июн(?:я|ь)|июл(?:я|ь)|август(?:а)?|сентябр(?:я|ь)|октябр(?:я|ь)|ноябр(?:я|ь)|декабр(?:я|ь))`
	dayMonthRE     = regexp.MustCompile(`(?i)([0-3]?\d)(?:\s*(?:[-–—]|по)\s*[0-3]?\d)?\s+` + ruMonthPattern + `(?:\s+(\d{4}))?`)
	startLabelRE   = regexp.MustCompile(`(?i)(?:даты?\s+проведения|как\s+(?:будет\s+)?проход(?:ит|ить)\s+(?:хакатон|соревнование)|этапы\s+соревнования|отборочн(?:ый|ого)\s+(?:этап|тур)|очный\s+хакатон|хакатон\s+(?:пройдет|пройдёт|состоится|продлится)|финал\s+(?:пройдет|пройдёт|состоится)|соревновани[ея]\s+(?:пройдет|пройдёт|состоится))\s*[:?]?\s*`)
	registrationRE = regexp.MustCompile(`(?i)регистрац(?:ия|ии|ию)\s+(?:открыта\s+)?(?:до|закроется|закрывается)\s*`)
)

type Config struct {
	Client      *http.Client
	FeedURL     string
	PostURL     string
	FeedUID     string
	RecID       string
	Concurrency int
	MinDelay    time.Duration
	Now         func() time.Time
}

type Adapter struct {
	client      *http.Client
	feedURL     string
	postURL     string
	feedUID     string
	recID       string
	concurrency int
	minDelay    time.Duration
	now         func() time.Time

	limiterMu   sync.Mutex
	nextRequest time.Time
}

type feedResponse struct {
	FeedUID   string     `json:"feeduid"`
	FeedTitle string     `json:"feedtitle"`
	FeedTZ    string     `json:"feedtz"`
	Total     int        `json:"total"`
	Size      int        `json:"size"`
	Slice     int        `json:"slice"`
	Posts     []feedPost `json:"posts"`
}

type postResponse struct {
	Post feedPost `json:"post"`
}

type feedPost struct {
	UID       string     `json:"uid"`
	Title     string     `json:"title"`
	Date      string     `json:"date"`
	Descr     string     `json:"descr"`
	URL       string     `json:"url"`
	Image     string     `json:"image"`
	Text      string     `json:"text"`
	FeedTZ    string     `json:"feedtz"`
	PostParts []postPart `json:"postparts"`
}

type postPart struct {
	Title string `json:"parttitle"`
}

type detailOutcome struct {
	index  int
	event  model.Event
	reject *model.RejectedEvent
}

func New(config Config) *Adapter {
	if config.Client == nil {
		config.Client = &http.Client{Timeout: 25 * time.Second}
	}
	if config.FeedURL == "" {
		config.FeedURL = defaultFeedURL
	}
	if config.PostURL == "" {
		config.PostURL = defaultPostURL
	}
	if config.FeedUID == "" {
		config.FeedUID = defaultFeedUID
	}
	if config.RecID == "" {
		config.RecID = defaultRecID
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
		feedURL:     config.FeedURL,
		postURL:     config.PostURL,
		feedUID:     config.FeedUID,
		recID:       config.RecID,
		concurrency: config.Concurrency,
		minDelay:    config.MinDelay,
		now:         config.Now,
	}
}

func (a *Adapter) Source() model.SourceState {
	return model.SourceState{
		ID:             model.HackathonsRusSourceID,
		Name:           "Хакатоны.рус",
		BaseURL:        "https://www.хакатоны.рус/",
		Enabled:        true,
		Status:         "never",
		AdapterVersion: "hackathonsrus-tilda-v1",
	}
}

func (a *Adapter) Fetch(ctx context.Context) (model.ImportResult, error) {
	started := a.now().UTC()
	result := model.ImportResult{SourceID: model.HackathonsRusSourceID, StartedAt: started}

	feed, err := a.fetchFeed(ctx)
	if err != nil {
		result.FinishedAt = a.now().UTC()
		return result, fmt.Errorf("fetch feed: %w", err)
	}
	result.FetchedCount = len(feed.Posts)
	if len(feed.Posts) == 0 {
		result.FinishedAt = a.now().UTC()
		return result, errors.New("contract error: the upcoming-events feed returned zero posts")
	}

	location, err := time.LoadLocation(feed.FeedTZ)
	if err != nil || location == nil {
		location = time.FixedZone("Europe/Moscow", 3*60*60)
		result.Warnings = append(result.Warnings, "source timezone was missing or invalid; Europe/Moscow was used")
	}

	jobs := make(chan int)
	outcomes := make(chan detailOutcome, len(feed.Posts))
	var workers sync.WaitGroup
	for worker := 0; worker < a.concurrency; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for index := range jobs {
				post := feed.Posts[index]
				outcomes <- a.buildEvent(ctx, index, post, location)
			}
		}()
	}
	go func() {
		for index := range feed.Posts {
			jobs <- index
		}
		close(jobs)
		workers.Wait()
		close(outcomes)
	}()

	ordered := make([]detailOutcome, 0, len(feed.Posts))
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
	result.ValidCount = len(result.Events)
	result.FinishedAt = a.now().UTC()
	if result.ValidCount == 0 {
		return result, errors.New("contract error: no event passed required-field validation")
	}
	return result, nil
}

func (a *Adapter) fetchFeed(ctx context.Context) (feedResponse, error) {
	u, err := url.Parse(a.feedURL)
	if err != nil {
		return feedResponse{}, fmt.Errorf("parse feed endpoint: %w", err)
	}
	query := u.Query()
	query.Set("feeduid", a.feedUID)
	query.Set("recid", a.recID)
	query.Set("slice", "1")
	query.Set("sort[date]", "desc")
	query.Set("filters[date][>]", "now")
	query.Set("getparts", "true")
	u.RawQuery = query.Encode()

	var feed feedResponse
	if err := a.getJSON(ctx, u.String(), &feed); err != nil {
		return feedResponse{}, err
	}
	if feed.FeedUID != "" && feed.FeedUID != a.feedUID {
		return feedResponse{}, fmt.Errorf("contract error: expected feed %s, got %s", a.feedUID, feed.FeedUID)
	}
	return feed, nil
}

func (a *Adapter) buildEvent(ctx context.Context, index int, summary feedPost, location *time.Location) detailOutcome {
	reject := func(reason string) detailOutcome {
		return detailOutcome{index: index, reject: &model.RejectedEvent{SourceEventID: summary.UID, Title: cleanText(summary.Title), Reason: reason}}
	}
	if strings.TrimSpace(summary.UID) == "" {
		return reject("missing source event id")
	}

	u, err := url.Parse(a.postURL)
	if err != nil {
		return reject("invalid post endpoint")
	}
	query := u.Query()
	query.Set("postuid", summary.UID)
	u.RawQuery = query.Encode()

	var response postResponse
	if err := a.getJSON(ctx, u.String(), &response); err != nil {
		return reject("detail request failed: " + compactError(err))
	}
	post := response.Post
	if post.UID == "" {
		post = summary
	}
	if post.Title == "" {
		post.Title = summary.Title
	}
	if post.URL == "" {
		post.URL = summary.URL
	}
	if post.Image == "" {
		post.Image = summary.Image
	}
	if post.Descr == "" {
		post.Descr = summary.Descr
	}
	if post.Date == "" {
		post.Date = summary.Date
	}
	if len(post.PostParts) == 0 {
		post.PostParts = summary.PostParts
	}

	title := cleanText(post.Title)
	if title == "" {
		return reject("missing title")
	}
	if !validHTTPURL(post.URL) {
		return reject("missing or invalid source URL")
	}
	officialURL := pickOfficialURL(post.Text)
	if officialURL == "" {
		return reject("official registration or event URL was not found")
	}

	listingAt := parseTildaDate(post.Date, location)
	referenceYear := a.now().In(location).Year()
	if listingAt != nil {
		referenceYear = listingAt.Year()
	}
	plainText := htmlToText(post.Text)
	startAt, startNote := parseStartDate(plainText, referenceYear, location)
	if startAt == nil {
		return reject("event start date was not found in the source text")
	}
	deadlineAt := parseRegistrationDeadline(plainText, referenceYear, location)

	tags := make([]string, 0, len(post.PostParts))
	for _, part := range post.PostParts {
		if value := cleanText(part.Title); value != "" {
			tags = append(tags, value)
		}
	}
	description := cleanText(post.Descr)
	if description == "" {
		description = truncateRunes(plainText, 360)
	}

	now := a.now().UTC()
	fingerprintInput := strings.Join([]string{post.UID, title, post.Date, post.Text, officialURL}, "\n")
	hash := sha256.Sum256([]byte(fingerprintInput))
	event := model.Event{
		ID:                     model.HackathonsRusSourceID + ":" + post.UID,
		SourceID:               model.HackathonsRusSourceID,
		SourceName:             "Хакатоны.рус",
		SourceEventID:          post.UID,
		Title:                  title,
		Description:            description,
		StartAt:                startAt,
		StartPrecision:         "date",
		RegistrationDeadlineAt: deadlineAt,
		ListingExpiresAt:       listingAt,
		Format:                 detectFormat(tags, plainText),
		OfficialURL:            officialURL,
		SourceURL:              post.URL,
		ImageURL:               validateOptionalURL(post.Image),
		Tags:                   tags,
		ParsingNotes: []string{
			startNote,
			"Tilda feed date is stored as listing_expires_at because its business meaning varies between cards",
		},
		RawFingerprint: hex.EncodeToString(hash[:]),
		FirstSeenAt:    now,
		LastSeenAt:     now,
	}
	if deadlineAt == nil {
		event.ParsingNotes = append(event.ParsingNotes, "registration deadline is absent or could not be parsed")
	}
	return detailOutcome{index: index, event: event}
}

func (a *Adapter) getJSON(ctx context.Context, endpoint string, target any) error {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if err := a.waitForSlot(ctx); err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "MeeteamRATPoC/0.1 (educational parser research)")

		resp, err := a.client.Do(req)
		if err != nil {
			lastErr = err
			if !sleepContext(ctx, retryDelay(attempt, "")) {
				return ctx.Err()
			}
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		if readErr != nil {
			lastErr = readErr
			continue
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			if err := json.Unmarshal(body, target); err != nil {
				return fmt.Errorf("decode JSON: %w", err)
			}
			return nil
		}
		lastErr = fmt.Errorf("unexpected HTTP status %d", resp.StatusCode)
		if resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode < 500 {
			return lastErr
		}
		if !sleepContext(ctx, retryDelay(attempt, resp.Header.Get("Retry-After"))) {
			return ctx.Err()
		}
	}
	return lastErr
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
	if seconds, err := strconv.Atoi(strings.TrimSpace(retryAfter)); err == nil && seconds > 0 {
		delay := time.Duration(seconds) * time.Second
		if delay > 5*time.Second {
			return 5 * time.Second
		}
		return delay
	}
	return time.Duration(attempt+1) * 300 * time.Millisecond
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

func htmlToText(value string) string {
	value = breaksRE.ReplaceAllString(value, " ")
	value = tagsRE.ReplaceAllString(value, " ")
	return cleanText(stdhtml.UnescapeString(value))
}

func cleanText(value string) string {
	value = strings.NewReplacer("\u00a0", " ", "\u202f", " ").Replace(value)
	return strings.TrimSpace(spacesRE.ReplaceAllString(value, " "))
}

func parseTildaDate(value string, location *time.Location) *time.Time {
	parsed, err := time.ParseInLocation("2006-01-02 15:04", strings.TrimSpace(value), location)
	if err != nil {
		return nil
	}
	return &parsed
}

func parseStartDate(text string, referenceYear int, location *time.Location) (*time.Time, string) {
	if label := startLabelRE.FindStringIndex(text); label != nil {
		window := text[label[1]:]
		if len(window) > 240 {
			window = window[:240]
		}
		if parsed := parseFirstRussianDate(window, referenceYear, location); parsed != nil {
			return parsed, "start date parsed from an explicit event-date label"
		}
	}

	if parsed := parseFirstNonRegistrationDate(text, referenceYear, location); parsed != nil {
		return parsed, "start date parsed heuristically from the first date expression"
	}
	return nil, ""
}

func parseFirstNonRegistrationDate(text string, referenceYear int, location *time.Location) *time.Time {
	for _, match := range dayMonthRE.FindAllStringSubmatchIndex(text, -1) {
		start := match[0]
		contextStart := start - 100
		if contextStart < 0 {
			contextStart = 0
		}
		prefix := strings.ToLower(text[contextStart:start])
		if registrationIndex := strings.LastIndex(prefix, "регистрац"); registrationIndex >= 0 && len(prefix)-registrationIndex < 70 {
			continue
		}
		if parsed := parseFirstRussianDate(text[start:], referenceYear, location); parsed != nil {
			return parsed
		}
	}
	return nil
}

func parseRegistrationDeadline(text string, referenceYear int, location *time.Location) *time.Time {
	match := registrationRE.FindStringIndex(text)
	if match == nil {
		return nil
	}
	window := text[match[1]:]
	if len(window) > 120 {
		window = window[:120]
	}
	return parseFirstRussianDate(window, referenceYear, location)
}

func parseFirstRussianDate(text string, referenceYear int, location *time.Location) *time.Time {
	match := dayMonthRE.FindStringSubmatch(text)
	if match == nil {
		return nil
	}
	day, err := strconv.Atoi(match[1])
	if err != nil || day < 1 || day > 31 {
		return nil
	}
	month, ok := russianMonth(match[2])
	if !ok {
		return nil
	}
	year := referenceYear
	if match[3] != "" {
		if parsedYear, err := strconv.Atoi(match[3]); err == nil {
			year = parsedYear
		}
	}
	parsed := time.Date(year, month, day, 0, 0, 0, 0, location)
	if parsed.Day() != day || parsed.Month() != month {
		return nil
	}
	return &parsed
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

func pickOfficialURL(rawHTML string) string {
	document, err := goquery.NewDocumentFromReader(strings.NewReader(rawHTML))
	if err != nil {
		return ""
	}
	type candidate struct {
		url   string
		score int
	}
	var candidates []candidate
	document.Find("a[href]").Each(func(_ int, selection *goquery.Selection) {
		href, ok := selection.Attr("href")
		if !ok || !validHTTPURL(href) || excludedExternalHost(href) {
			return
		}
		label := strings.ToLower(cleanText(selection.Text()))
		score := 10
		if strings.Contains(label, "зарегистр") || strings.Contains(label, "регистрац") {
			score = 100
		} else if strings.Contains(label, "участв") || strings.Contains(label, "подать") || strings.Contains(label, "сайт") {
			score = 70
		}
		candidates = append(candidates, candidate{url: href, score: score})
	})
	if len(candidates) == 0 {
		return ""
	}
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].score > candidates[j].score })
	return candidates[0].url
}

func excludedExternalHost(rawURL string) bool {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return true
	}
	host := strings.ToLower(strings.TrimPrefix(parsed.Hostname(), "www."))
	excluded := []string{
		"xn--80aa3anexr8c.xn--p1acf",
		"хакатоны.рус",
		"t.me",
		"telegram.me",
		"vk.com",
		"vk.ru",
		"max.ru",
		"dzen.ru",
		"habr.com",
		"static.tildacdn.com",
	}
	for _, domain := range excluded {
		if host == domain || strings.HasSuffix(host, "."+domain) {
			return true
		}
	}
	return false
}

func validHTTPURL(value string) bool {
	parsed, err := url.Parse(strings.TrimSpace(value))
	return err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host != ""
}

func validateOptionalURL(value string) string {
	if validHTTPURL(value) {
		return value
	}
	return ""
}

func detectFormat(tags []string, text string) string {
	joined := strings.ToLower(strings.Join(tags, " ") + " " + text)
	online := strings.Contains(joined, "online") || strings.Contains(joined, "онлайн")
	offline := strings.Contains(joined, "offline") || strings.Contains(joined, "офлайн")
	switch {
	case online && offline:
		return "hybrid"
	case online:
		return "online"
	case offline:
		return "offline"
	default:
		return ""
	}
}

func truncateRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit]) + "…"
}

func compactError(err error) string {
	value := cleanText(err.Error())
	return truncateRunes(value, 180)
}
