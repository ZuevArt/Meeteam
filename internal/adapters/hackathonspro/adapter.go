package hackathonspro

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
	defaultFeedUID = "131632209651"
	defaultRecID   = "442995264"
)

var (
	spacesRE = regexp.MustCompile(`\s+`)
	tagsRE   = regexp.MustCompile(`(?s)<[^>]+>`)
	breaksRE = regexp.MustCompile(`(?i)<br\s*/?>|</(?:div|p|li|h[1-6]|ul|ol)>`)

	ruMonthPattern   = `(январ(?:я|ь)|феврал(?:я|ь)|март(?:а)?|апрел(?:я|ь)|ма(?:я|й)|июн(?:я|ь)|июл(?:я|ь)|август(?:а)?|сентябр(?:я|ь)|октябр(?:я|ь)|ноябр(?:я|ь)|декабр(?:я|ь))`
	crossMonthRE     = regexp.MustCompile(`(?i)([0-3]?\d)\s+` + ruMonthPattern + `\s*(?:[-–—]|по)\s*([0-3]?\d)\s+` + ruMonthPattern + `(?:\s+(\d{4}))?`)
	sameMonthRE      = regexp.MustCompile(`(?i)([0-3]?\d)\s*(?:[-–—]|по)\s*([0-3]?\d)\s+` + ruMonthPattern + `(?:\s+(\d{4}))?`)
	monthRangeRE     = regexp.MustCompile(`(?i)` + ruMonthPattern + `(?:\s+(\d{4}))?\s*(?:[-–—]|по)\s*` + ruMonthPattern + `(?:\s+(\d{4}))?`)
	singleDateRE     = regexp.MustCompile(`(?i)([0-3]?\d)\s+` + ruMonthPattern + `(?:\s+(\d{4}))?`)
	eventDateLabelRE = regexp.MustCompile(`(?i)(?:^|\s)дат[аы]\s*:\s*`)
	registrationRE   = regexp.MustCompile(`(?i)(?:регистрац(?:ия|ии|ию)[^.!?]{0,45}?(?:до|закроется|закрывается)|(?:успей\s+)?подать\s+заявк[уа]\s+до|заявки\s+до)\s*`)
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
		ID:             model.HackathonsProSourceID,
		Name:           "Hackathons.pro",
		BaseURL:        "https://hackathons.pro/",
		Enabled:        true,
		Status:         "never",
		AdapterVersion: "hackathonspro-tilda-v1",
	}
}

func (a *Adapter) Fetch(ctx context.Context) (model.ImportResult, error) {
	started := a.now().UTC()
	result := model.ImportResult{SourceID: model.HackathonsProSourceID, StartedAt: started}

	feed, err := a.fetchFeed(ctx)
	if err != nil {
		result.FinishedAt = a.now().UTC()
		return result, fmt.Errorf("fetch feed: %w", err)
	}
	result.FetchedCount = len(feed.Posts)
	if len(feed.Posts) == 0 {
		result.FinishedAt = a.now().UTC()
		return result, errors.New("contract error: the current-events feed returned zero posts")
	}

	location, err := time.LoadLocation(feed.FeedTZ)
	if err != nil || location == nil {
		location = time.FixedZone("Europe/Moscow", 3*60*60)
		result.Warnings = append(result.Warnings, "source timezone was missing or invalid; Europe/Moscow was used")
	}
	today := startOfDay(a.now().In(location))

	jobs := make(chan int)
	outcomes := make(chan detailOutcome, len(feed.Posts))
	var workers sync.WaitGroup
	for worker := 0; worker < a.concurrency; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for index := range jobs {
				outcomes <- a.buildEvent(ctx, index, feed.Posts[index], today, location)
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
	sort.SliceStable(result.Events, func(i, j int) bool {
		return result.Events[i].StartAt.Before(*result.Events[j].StartAt)
	})
	result.ValidCount = len(result.Events)
	result.FinishedAt = a.now().UTC()
	if result.ValidCount == 0 {
		return result, errors.New("contract error: no event passed required-field validation")
	}
	return result, nil
}

func (a *Adapter) fetchFeed(ctx context.Context) (feedResponse, error) {
	feed, err := a.fetchFeedPage(ctx, 1)
	if err != nil {
		return feedResponse{}, err
	}
	if feed.FeedUID != "" && feed.FeedUID != a.feedUID {
		return feedResponse{}, fmt.Errorf("contract error: expected feed %s, got %s", a.feedUID, feed.FeedUID)
	}
	pageSize := feed.Size
	if pageSize <= 0 {
		pageSize = len(feed.Posts)
	}
	for page := 2; pageSize > 0 && len(feed.Posts) < feed.Total; page++ {
		next, err := a.fetchFeedPage(ctx, page)
		if err != nil {
			return feedResponse{}, fmt.Errorf("fetch feed page %d: %w", page, err)
		}
		if len(next.Posts) == 0 {
			return feedResponse{}, fmt.Errorf("contract error: feed page %d was empty before total %d was reached", page, feed.Total)
		}
		feed.Posts = append(feed.Posts, next.Posts...)
	}
	return feed, nil
}

func (a *Adapter) fetchFeedPage(ctx context.Context, page int) (feedResponse, error) {
	u, err := url.Parse(a.feedURL)
	if err != nil {
		return feedResponse{}, fmt.Errorf("parse feed endpoint: %w", err)
	}
	query := u.Query()
	query.Set("feeduid", a.feedUID)
	query.Set("recid", a.recID)
	query.Set("slice", strconv.Itoa(page))
	query.Set("sort[date]", "desc")
	query.Set("filters[date]", "")
	query.Set("getparts", "true")
	u.RawQuery = query.Encode()

	var feed feedResponse
	if err := a.getJSON(ctx, u.String(), &feed); err != nil {
		return feedResponse{}, err
	}
	return feed, nil
}

func (a *Adapter) buildEvent(ctx context.Context, index int, summary feedPost, today time.Time, location *time.Location) detailOutcome {
	reject := func(reason string) detailOutcome {
		return detailOutcome{index: index, reject: &model.RejectedEvent{
			SourceEventID: summary.UID,
			Title:         cleanText(summary.Title),
			Reason:        reason,
		}}
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
	post := mergePost(response.Post, summary)
	title := cleanText(post.Title)
	if title == "" {
		return reject("missing title")
	}
	if !validSourceURL(post.URL) {
		return reject("missing or invalid Hackathons.pro event URL")
	}
	officialURL := pickOfficialURL(post.Text)
	if officialURL == "" {
		return reject("official registration or event URL was not found")
	}

	plainText := htmlToText(post.Text)
	description := htmlToText(post.Descr)
	dateText := eventDateText(plainText, description)
	referenceYear := a.now().In(location).Year()
	startAt, endAt, precision, dateNote := parseEventRange(dateText, referenceYear, location)
	if startAt == nil || endAt == nil {
		return reject("event date range was not found in the source card")
	}
	if endAt.Before(today) {
		return reject("event has already ended")
	}
	deadlineAt := parseRegistrationDeadline(plainText, startAt.Year(), location)

	if description == "" {
		description = truncateRunes(plainText, 360)
	}
	tags := detectTags(title + " " + description + " " + plainText)
	observedAt := a.now().UTC()
	fingerprintInput := strings.Join([]string{post.UID, title, post.Date, post.Descr, post.Text, officialURL}, "\n")
	hash := sha256.Sum256([]byte(fingerprintInput))
	parsingNotes := []string{
		dateNote,
		"current and expired cards share one feed; finished events are filtered by parsed end date",
		"Tilda post date is publication metadata and is not used as an event date",
	}
	if deadlineAt == nil {
		parsingNotes = append(parsingNotes, "registration deadline is absent or could not be parsed")
	}

	return detailOutcome{index: index, event: model.Event{
		ID:                     model.HackathonsProSourceID + ":" + post.UID,
		SourceID:               model.HackathonsProSourceID,
		SourceName:             "Hackathons.pro",
		SourceEventID:          post.UID,
		Title:                  title,
		Description:            description,
		StartAt:                startAt,
		EndAt:                  endAt,
		StartPrecision:         precision,
		RegistrationDeadlineAt: deadlineAt,
		Format:                 detectFormat(description + " " + plainText),
		OfficialURL:            officialURL,
		SourceURL:              post.URL,
		ImageURL:               validateOptionalURL(post.Image),
		Tags:                   tags,
		ParsingNotes:           parsingNotes,
		RawFingerprint:         hex.EncodeToString(hash[:]),
		FirstSeenAt:            observedAt,
		LastSeenAt:             observedAt,
	}}
}

func mergePost(post, summary feedPost) feedPost {
	if post.UID == "" {
		post.UID = summary.UID
	}
	if post.Title == "" {
		post.Title = summary.Title
	}
	if post.Date == "" {
		post.Date = summary.Date
	}
	if post.Descr == "" {
		post.Descr = summary.Descr
	}
	if post.URL == "" {
		post.URL = summary.URL
	}
	if post.Image == "" {
		post.Image = summary.Image
	}
	if post.Text == "" {
		post.Text = summary.Text
	}
	if len(post.PostParts) == 0 {
		post.PostParts = summary.PostParts
	}
	return post
}

func eventDateText(plainText, description string) string {
	if match := eventDateLabelRE.FindStringIndex(plainText); match != nil {
		window := plainText[match[1]:]
		if formatIndex := strings.Index(strings.ToLower(window), "формат:"); formatIndex >= 0 {
			window = window[:formatIndex]
		}
		return truncateRunes(cleanText(window), 180)
	}
	return truncateRunes(description, 180)
}

func parseEventRange(text string, referenceYear int, location *time.Location) (*time.Time, *time.Time, string, string) {
	text = cleanText(text)
	if match := crossMonthRE.FindStringSubmatch(text); match != nil {
		startDay, startMonth, startOK := parsedDayMonth(match[1], match[2])
		endDay, endMonth, endOK := parsedDayMonth(match[3], match[4])
		endYear := parsedYear(match[5], referenceYear)
		startYear := endYear
		if startMonth > endMonth {
			startYear--
		}
		start := validDate(startYear, startMonth, startDay, location)
		end := validDate(endYear, endMonth, endDay, location)
		if startOK && endOK && start != nil && end != nil && !end.Before(*start) {
			return start, end, "date", "start and end dates parsed from a cross-month range"
		}
	}
	if match := sameMonthRE.FindStringSubmatch(text); match != nil {
		startDay, startMonth, startOK := parsedDayMonth(match[1], match[3])
		endDay, endErr := strconv.Atoi(match[2])
		year := parsedYear(match[4], referenceYear)
		start := validDate(year, startMonth, startDay, location)
		end := validDate(year, startMonth, endDay, location)
		if startOK && endErr == nil && start != nil && end != nil && !end.Before(*start) {
			return start, end, "date", "start and end dates parsed from a same-month range"
		}
	}
	if match := monthRangeRE.FindStringSubmatch(text); match != nil {
		startMonth, startOK := russianMonth(match[1])
		endMonth, endOK := russianMonth(match[3])
		endYear := parsedYear(match[4], parsedYear(match[2], referenceYear))
		startYear := parsedYear(match[2], endYear)
		if match[2] == "" && startMonth > endMonth {
			startYear--
		}
		if startOK && endOK {
			start := time.Date(startYear, startMonth, 1, 0, 0, 0, 0, location)
			end := time.Date(endYear, endMonth+1, 0, 0, 0, 0, 0, location)
			if !end.Before(start) {
				return &start, &end, "month", "month-level start and end dates parsed from the event summary"
			}
		}
	}
	if match := singleDateRE.FindStringSubmatchIndex(text); match != nil {
		prefixStart := match[0] - 12
		if prefixStart < 0 {
			prefixStart = 0
		}
		prefix := strings.ToLower(text[prefixStart:match[0]])
		if strings.Contains(prefix, "до ") {
			return nil, nil, "", ""
		}
		dayText := text[match[2]:match[3]]
		monthText := text[match[4]:match[5]]
		yearText := ""
		if match[6] >= 0 {
			yearText = text[match[6]:match[7]]
		}
		day, month, ok := parsedDayMonth(dayText, monthText)
		date := validDate(parsedYear(yearText, referenceYear), month, day, location)
		if ok && date != nil {
			return date, date, "date", "single event date parsed from the event summary"
		}
	}
	return nil, nil, "", ""
}

func parseRegistrationDeadline(text string, referenceYear int, location *time.Location) *time.Time {
	match := registrationRE.FindStringIndex(text)
	if match == nil {
		return nil
	}
	window := text[match[1]:]
	if len([]rune(window)) > 120 {
		window = string([]rune(window)[:120])
	}
	dateMatch := singleDateRE.FindStringSubmatch(window)
	if dateMatch == nil {
		return nil
	}
	day, month, ok := parsedDayMonth(dateMatch[1], dateMatch[2])
	if !ok {
		return nil
	}
	return validDate(parsedYear(dateMatch[3], referenceYear), month, day, location)
}

func parsedDayMonth(dayText, monthText string) (int, time.Month, bool) {
	day, err := strconv.Atoi(dayText)
	month, ok := russianMonth(monthText)
	return day, month, err == nil && ok && day >= 1 && day <= 31
}

func parsedYear(value string, fallback int) int {
	year, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || year < 2000 || year > 2200 {
		return fallback
	}
	return year
}

func validDate(year int, month time.Month, day int, location *time.Location) *time.Time {
	parsed := time.Date(year, month, day, 0, 0, 0, 0, location)
	if parsed.Year() != year || parsed.Month() != month || parsed.Day() != day {
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
		if strings.Contains(label, "регистрац") || strings.Contains(label, "зарегистр") {
			score = 100
		} else if strings.Contains(label, "подробност") || strings.Contains(label, "участв") || strings.Contains(label, "подать") || strings.Contains(label, "сайт") {
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
	for _, domain := range []string{"hackathons.pro", "t.me", "telegram.me", "vk.com", "vk.ru", "max.ru", "dzen.ru", "habr.com", "static.tildacdn.com"} {
		if host == domain || strings.HasSuffix(host, "."+domain) {
			return true
		}
	}
	return false
}

func validSourceURL(value string) bool {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return false
	}
	host := strings.ToLower(strings.TrimPrefix(parsed.Hostname(), "www."))
	return host == "hackathons.pro" && strings.HasPrefix(parsed.Path, "/tpost/")
}

func validHTTPURL(value string) bool {
	parsed, err := url.Parse(strings.TrimSpace(value))
	return err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Hostname() != ""
}

func validateOptionalURL(value string) string {
	if validHTTPURL(value) {
		return strings.TrimSpace(value)
	}
	return ""
}

func detectFormat(text string) string {
	lower := strings.ToLower(text)
	online := strings.Contains(lower, "онлайн") || strings.Contains(lower, "online")
	offline := strings.Contains(lower, "офлайн") || strings.Contains(lower, "оффлайн") || strings.Contains(lower, "очный") || strings.Contains(lower, "конгресс-центр") || strings.Contains(lower, "г. москва") || strings.Contains(lower, "ул.")
	switch {
	case strings.Contains(lower, "гибрид") || online && offline:
		return "hybrid"
	case online:
		return "online"
	case offline:
		return "offline"
	default:
		return ""
	}
}

func detectTags(text string) []string {
	lower := strings.ToLower(text)
	var tags []string
	for _, candidate := range []struct{ needle, label string }{
		{needle: "хакатон", label: "Хакатон"},
		{needle: "конференц", label: "Конференция"},
		{needle: "олимпиад", label: "Олимпиада"},
		{needle: "соревнован", label: "Соревнование"},
		{needle: "конкурс", label: "Конкурс"},
		{needle: "стартап", label: "Стартапы"},
		{needle: "data science", label: "Data Science"},
		{needle: "машинн", label: "ML"},
		{needle: "искусственн", label: "AI"},
		{needle: " ии", label: "AI"},
	} {
		if strings.Contains(lower, candidate.needle) && !contains(tags, candidate.label) {
			tags = append(tags, candidate.label)
		}
	}
	return tags
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
	value = stdhtml.UnescapeString(value)
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

func truncateRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit]) + "…"
}

func compactError(err error) string {
	return truncateRunes(cleanText(err.Error()), 180)
}
