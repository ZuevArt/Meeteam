package devpost

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

	"meeteam/internal/model"
)

const (
	defaultAPIURL   = "https://devpost.com/api/hackathons"
	defaultMaxPages = 20
)

var (
	spacesRE     = regexp.MustCompile(`\s+`)
	tagsRE       = regexp.MustCompile(`(?s)<[^>]+>`)
	crossMonthRE = regexp.MustCompile(`(?i)^([a-z]{3})\s+(\d{1,2})(?:,\s*(\d{4}))?\s*-\s*([a-z]{3})\s+(\d{1,2}),\s*(\d{4})$`)
	sameMonthRE  = regexp.MustCompile(`(?i)^([a-z]{3})\s+(\d{1,2})\s*-\s*(\d{1,2}),\s*(\d{4})$`)
	singleDateRE = regexp.MustCompile(`(?i)^([a-z]{3})\s+(\d{1,2}),\s*(\d{4})$`)
)

type Config struct {
	Client   *http.Client
	APIURL   string
	MinDelay time.Duration
	MaxPages int
	Now      func() time.Time
}

type Adapter struct {
	client   *http.Client
	apiURL   string
	minDelay time.Duration
	maxPages int
	now      func() time.Time

	limiterMu   sync.Mutex
	nextRequest time.Time
}

type apiResponse struct {
	Hackathons []rawHackathon `json:"hackathons"`
	Meta       apiMeta        `json:"meta"`
}

type apiMeta struct {
	TotalCount int  `json:"total_count"`
	PerPage    int  `json:"per_page"`
	Fuzzy      bool `json:"fuzzy"`
}

type rawHackathon struct {
	ID                    int64             `json:"id"`
	Title                 string            `json:"title"`
	DisplayedLocation     displayedLocation `json:"displayed_location"`
	OpenState             string            `json:"open_state"`
	ThumbnailURL          string            `json:"thumbnail_url"`
	URL                   string            `json:"url"`
	TimeLeftToSubmission  string            `json:"time_left_to_submission"`
	SubmissionPeriodDates string            `json:"submission_period_dates"`
	Themes                []rawTheme        `json:"themes"`
	PrizeAmount           string            `json:"prize_amount"`
	RegistrationsCount    int               `json:"registrations_count"`
	OrganizationName      string            `json:"organization_name"`
}

type displayedLocation struct {
	Location string `json:"location"`
}

type rawTheme struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

func New(config Config) *Adapter {
	if config.Client == nil {
		config.Client = &http.Client{Timeout: 30 * time.Second}
	}
	if strings.TrimSpace(config.APIURL) == "" {
		config.APIURL = defaultAPIURL
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
		client:   config.Client,
		apiURL:   config.APIURL,
		minDelay: config.MinDelay,
		maxPages: config.MaxPages,
		now:      config.Now,
	}
}

func (a *Adapter) Source() model.SourceState {
	return model.SourceState{
		ID:             model.DevpostSourceID,
		Name:           "Devpost",
		BaseURL:        "https://devpost.com/hackathons",
		Enabled:        true,
		Status:         "never",
		AdapterVersion: "devpost-public-api-v1",
	}
}

func (a *Adapter) Fetch(ctx context.Context) (model.ImportResult, error) {
	started := a.now().UTC()
	result := model.ImportResult{SourceID: model.DevpostSourceID, StartedAt: started}

	firstPage, err := a.fetchPage(ctx, 1)
	if err != nil {
		result.FinishedAt = a.now().UTC()
		return result, fmt.Errorf("fetch Devpost page 1: %w", err)
	}
	if firstPage.Meta.PerPage <= 0 {
		result.FinishedAt = a.now().UTC()
		return result, errors.New("contract error: Devpost returned an invalid per_page value")
	}
	if firstPage.Meta.TotalCount <= 0 || len(firstPage.Hackathons) == 0 {
		result.FinishedAt = a.now().UTC()
		return result, errors.New("contract error: Devpost returned zero open hackathons")
	}
	totalPages := (firstPage.Meta.TotalCount + firstPage.Meta.PerPage - 1) / firstPage.Meta.PerPage
	if totalPages > a.maxPages {
		result.FinishedAt = a.now().UTC()
		return result, fmt.Errorf("contract error: Devpost requires %d pages, above the safety limit %d", totalPages, a.maxPages)
	}

	all := append([]rawHackathon(nil), firstPage.Hackathons...)
	for page := 2; page <= totalPages; page++ {
		payload, fetchErr := a.fetchPage(ctx, page)
		if fetchErr != nil {
			result.FinishedAt = a.now().UTC()
			return result, fmt.Errorf("fetch Devpost page %d: %w", page, fetchErr)
		}
		if len(payload.Hackathons) == 0 {
			result.Warnings = append(result.Warnings, fmt.Sprintf("Devpost page %d became empty while the catalog was changing", page))
			break
		}
		all = append(all, payload.Hackathons...)
	}

	seen := make(map[int64]struct{}, len(all))
	for _, raw := range all {
		if _, duplicate := seen[raw.ID]; duplicate && raw.ID != 0 {
			continue
		}
		if raw.ID != 0 {
			seen[raw.ID] = struct{}{}
		}
		result.FetchedCount++
		event, rejected := normalizeEvent(raw, started)
		if rejected != nil {
			result.Rejected = append(result.Rejected, *rejected)
			continue
		}
		result.Events = append(result.Events, event)
	}
	if result.FetchedCount < firstPage.Meta.TotalCount {
		result.Warnings = append(result.Warnings, fmt.Sprintf(
			"Devpost reported %d open hackathons, but %d unique records were received during pagination",
			firstPage.Meta.TotalCount,
			result.FetchedCount,
		))
	}
	sort.Slice(result.Events, func(i, j int) bool {
		left := result.Events[i].RegistrationDeadlineAt
		right := result.Events[j].RegistrationDeadlineAt
		if left == nil {
			return false
		}
		if right == nil {
			return true
		}
		if left.Equal(*right) {
			return result.Events[i].Title < result.Events[j].Title
		}
		return left.Before(*right)
	})
	result.ValidCount = len(result.Events)
	result.FinishedAt = a.now().UTC()
	if result.ValidCount == 0 {
		return result, errors.New("contract error: no Devpost record passed required-field validation")
	}
	return result, nil
}

func (a *Adapter) fetchPage(ctx context.Context, page int) (apiResponse, error) {
	destination, err := url.Parse(a.apiURL)
	if err != nil {
		return apiResponse{}, fmt.Errorf("parse API URL: %w", err)
	}
	query := destination.Query()
	query.Set("status", "open")
	query.Set("page", strconv.Itoa(page))
	destination.RawQuery = query.Encode()

	body, err := a.getJSON(ctx, destination.String())
	if err != nil {
		return apiResponse{}, err
	}
	var payload apiResponse
	if err := json.Unmarshal(body, &payload); err != nil {
		return apiResponse{}, fmt.Errorf("decode API response: %w", err)
	}
	return payload, nil
}

func normalizeEvent(raw rawHackathon, observedAt time.Time) (model.Event, *model.RejectedEvent) {
	reject := func(reason string) (model.Event, *model.RejectedEvent) {
		return model.Event{}, &model.RejectedEvent{
			SourceEventID: sourceEventID(raw.ID),
			Title:         cleanText(raw.Title),
			Reason:        reason,
		}
	}
	if raw.ID <= 0 {
		return reject("missing or invalid source event id")
	}
	if !strings.EqualFold(strings.TrimSpace(raw.OpenState), "open") {
		return reject("record is not open for submissions")
	}
	title := cleanText(raw.Title)
	if title == "" {
		return reject("missing title")
	}
	startAt, endAt, err := parseSubmissionPeriod(raw.SubmissionPeriodDates)
	if err != nil {
		return reject("missing or invalid submission period: " + err.Error())
	}
	officialURL := validateHTTPURL(raw.URL)
	if officialURL == "" {
		return reject("missing or invalid hackathon URL")
	}
	imageURL := validateHTTPURL(raw.ThumbnailURL)
	location := cleanText(raw.DisplayedLocation.Location)
	format := normalizeFormat(location)
	organizer := cleanText(raw.OrganizationName)
	tags := make([]string, 0, len(raw.Themes))
	for _, theme := range raw.Themes {
		appendUnique(&tags, cleanText(theme.Name))
	}
	prize := cleanHTML(raw.PrizeAmount)
	descriptionParts := make([]string, 0, 3)
	if organizer != "" {
		descriptionParts = append(descriptionParts, "Хакатон от "+organizer+".")
	}
	descriptionParts = append(descriptionParts, "Приём работ: "+cleanText(raw.SubmissionPeriodDates)+".")
	if prize != "" {
		descriptionParts = append(descriptionParts, "Призовой фонд: "+prize+".")
	}
	description := strings.Join(descriptionParts, " ")

	fingerprintInput := strings.Join([]string{
		sourceEventID(raw.ID), title, raw.SubmissionPeriodDates, raw.OpenState,
		location, organizer, officialURL, imageURL, prize, strings.Join(tags, "|"),
	}, "\n")
	hash := sha256.Sum256([]byte(fingerprintInput))
	deadline := endAt
	listingExpiry := endAt
	return model.Event{
		ID:                     model.DevpostSourceID + ":" + sourceEventID(raw.ID),
		SourceID:               model.DevpostSourceID,
		SourceName:             "Devpost",
		SourceEventID:          sourceEventID(raw.ID),
		Title:                  title,
		Description:            description,
		StartAt:                &startAt,
		EndAt:                  &endAt,
		StartPrecision:         "date",
		RegistrationDeadlineAt: &deadline,
		ListingExpiresAt:       &listingExpiry,
		Format:                 format,
		Organizer:              organizer,
		OfficialURL:            officialURL,
		SourceURL:              officialURL,
		ImageURL:               imageURL,
		Tags:                   tags,
		ParsingNotes: []string{
			"record read from the public JSON endpoint declared by the Devpost hackathon catalog",
			"catalog request explicitly filtered to status=open",
			"registration deadline equals the end of the published submission period",
		},
		RawFingerprint: hex.EncodeToString(hash[:]),
		FirstSeenAt:    observedAt,
		LastSeenAt:     observedAt,
	}, nil
}

func parseSubmissionPeriod(value string) (time.Time, time.Time, error) {
	normalized := cleanText(value)
	normalized = strings.NewReplacer("–", "-", "—", "-").Replace(normalized)
	if match := crossMonthRE.FindStringSubmatch(normalized); match != nil {
		startMonth, monthErr := parseMonth(match[1])
		endMonth, endMonthErr := parseMonth(match[4])
		startDay, dayErr := strconv.Atoi(match[2])
		endDay, endDayErr := strconv.Atoi(match[5])
		endYear, yearErr := strconv.Atoi(match[6])
		if monthErr != nil || endMonthErr != nil || dayErr != nil || endDayErr != nil || yearErr != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("unsupported date range %q", value)
		}
		startYear := endYear
		if match[3] != "" {
			startYear, yearErr = strconv.Atoi(match[3])
			if yearErr != nil {
				return time.Time{}, time.Time{}, fmt.Errorf("unsupported date range %q", value)
			}
		} else if startMonth > endMonth {
			startYear--
		}
		return validatedDateRange(startYear, startMonth, startDay, endYear, endMonth, endDay, value)
	}
	if match := sameMonthRE.FindStringSubmatch(normalized); match != nil {
		month, monthErr := parseMonth(match[1])
		startDay, dayErr := strconv.Atoi(match[2])
		endDay, endDayErr := strconv.Atoi(match[3])
		year, yearErr := strconv.Atoi(match[4])
		if monthErr != nil || dayErr != nil || endDayErr != nil || yearErr != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("unsupported date range %q", value)
		}
		return validatedDateRange(year, month, startDay, year, month, endDay, value)
	}
	if match := singleDateRE.FindStringSubmatch(normalized); match != nil {
		month, monthErr := parseMonth(match[1])
		day, dayErr := strconv.Atoi(match[2])
		year, yearErr := strconv.Atoi(match[3])
		if monthErr != nil || dayErr != nil || yearErr != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("unsupported date %q", value)
		}
		return validatedDateRange(year, month, day, year, month, day, value)
	}
	return time.Time{}, time.Time{}, fmt.Errorf("unsupported date range %q", value)
}

func validatedDateRange(startYear int, startMonth time.Month, startDay, endYear int, endMonth time.Month, endDay int, raw string) (time.Time, time.Time, error) {
	start := time.Date(startYear, startMonth, startDay, 0, 0, 0, 0, time.UTC)
	end := time.Date(endYear, endMonth, endDay, 0, 0, 0, 0, time.UTC)
	if start.Year() != startYear || start.Month() != startMonth || start.Day() != startDay ||
		end.Year() != endYear || end.Month() != endMonth || end.Day() != endDay || end.Before(start) {
		return time.Time{}, time.Time{}, fmt.Errorf("invalid date range %q", raw)
	}
	return start, end, nil
}

func parseMonth(value string) (time.Month, error) {
	months := map[string]time.Month{
		"jan": time.January, "feb": time.February, "mar": time.March,
		"apr": time.April, "may": time.May, "jun": time.June,
		"jul": time.July, "aug": time.August, "sep": time.September,
		"oct": time.October, "nov": time.November, "dec": time.December,
	}
	month, ok := months[strings.ToLower(strings.TrimSpace(value))]
	if !ok {
		return 0, fmt.Errorf("unknown month %q", value)
	}
	return month, nil
}

func normalizeFormat(location string) string {
	if strings.Contains(strings.ToLower(location), "online") {
		return "online"
	}
	if strings.TrimSpace(location) != "" {
		return "offline"
	}
	return ""
}

func sourceEventID(id int64) string {
	if id <= 0 {
		return ""
	}
	return strconv.FormatInt(id, 10)
}

func validateHTTPURL(value string) string {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "//") {
		value = "https:" + value
	}
	parsed, err := url.Parse(value)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" {
		return ""
	}
	return parsed.String()
}

func cleanHTML(value string) string {
	return cleanText(tagsRE.ReplaceAllString(value, ""))
}

func cleanText(value string) string {
	value = stdhtml.UnescapeString(value)
	value = strings.NewReplacer("\u00a0", " ", "\u202f", " ").Replace(value)
	return strings.TrimSpace(spacesRE.ReplaceAllString(value, " "))
}

func appendUnique(values *[]string, value string) {
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

func (a *Adapter) getJSON(ctx context.Context, destination string) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if err := a.waitForSlot(ctx); err != nil {
			return nil, err
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, destination, nil)
		if err != nil {
			return nil, err
		}
		request.Header.Set("Accept", "application/json")
		request.Header.Set("User-Agent", "MeeteamRATPoC/0.1 (educational parser research)")
		response, err := a.client.Do(request)
		if err != nil {
			lastErr = err
			if !sleepContext(ctx, time.Duration(attempt+1)*350*time.Millisecond) {
				return nil, ctx.Err()
			}
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(response.Body, 8<<20))
		response.Body.Close()
		if readErr != nil {
			lastErr = readErr
			continue
		}
		if response.StatusCode >= 200 && response.StatusCode < 300 {
			if len(body) == 0 {
				return nil, errors.New("source returned an empty response")
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
