package devfolio

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"meeteam/internal/model"
)

const (
	defaultAPIURL   = "https://api.devfolio.co/api/search/hackathons"
	defaultPageSize = 10
	defaultMaxPages = 20
	maxResponseSize = 8 << 20
)

var (
	spacesRE    = regexp.MustCompile(`\s+`)
	subdomainRE = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
)

type Config struct {
	Client   *http.Client
	APIURL   string
	MinDelay time.Duration
	PageSize int
	MaxPages int
	Now      func() time.Time
}

type Adapter struct {
	client   *http.Client
	apiURL   string
	minDelay time.Duration
	pageSize int
	maxPages int
	now      func() time.Time

	limiterMu   sync.Mutex
	nextRequest time.Time
}

type searchRequest struct {
	Type string `json:"type"`
	From int    `json:"from"`
	Size int    `json:"size"`
}

type searchResponse struct {
	Hits struct {
		Total struct {
			Value int `json:"value"`
		} `json:"total"`
		Hits []searchHit `json:"hits"`
	} `json:"hits"`
}

type searchHit struct {
	Source rawHackathon `json:"_source"`
}

type rawHackathon struct {
	UUID              string            `json:"uuid"`
	Slug              string            `json:"slug"`
	Name              string            `json:"name"`
	Tagline           string            `json:"tagline"`
	StartsAt          string            `json:"starts_at"`
	EndsAt            string            `json:"ends_at"`
	IsOnline          bool              `json:"is_online"`
	Location          string            `json:"location"`
	City              string            `json:"city"`
	State             string            `json:"state"`
	Country           string            `json:"country"`
	CoverImage        string            `json:"cover_img"`
	ParticipantsCount int               `json:"participants_count"`
	Themes            []rawTheme        `json:"themes"`
	Setting           hackathonSettings `json:"hackathon_setting"`
}

type rawTheme struct {
	Name string `json:"name"`
}

type hackathonSettings struct {
	RegistrationEndsAt string `json:"reg_ends_at"`
	ExternalApplyURL   string `json:"external_apply_url"`
	Subdomain          string `json:"subdomain"`
	Review             bool   `json:"review"`
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
	if config.PageSize <= 0 {
		config.PageSize = defaultPageSize
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
		pageSize: config.PageSize,
		maxPages: config.MaxPages,
		now:      config.Now,
	}
}

func (a *Adapter) Source() model.SourceState {
	return model.SourceState{
		ID:             model.DevfolioSourceID,
		Name:           "Devfolio",
		BaseURL:        "https://devfolio.co/hackathons",
		Enabled:        true,
		Status:         "never",
		AdapterVersion: "devfolio-public-search-v1",
	}
}

func (a *Adapter) Fetch(ctx context.Context) (model.ImportResult, error) {
	started := a.now().UTC()
	result := model.ImportResult{SourceID: model.DevfolioSourceID, StartedAt: started}

	firstPage, err := a.fetchPage(ctx, 0)
	if err != nil {
		result.FinishedAt = a.now().UTC()
		return result, fmt.Errorf("fetch Devfolio offset 0: %w", err)
	}
	total := firstPage.Hits.Total.Value
	if total <= 0 || len(firstPage.Hits.Hits) == 0 {
		result.FinishedAt = a.now().UTC()
		return result, errors.New("contract error: Devfolio returned zero hackathons with open applications")
	}
	totalPages := (total + a.pageSize - 1) / a.pageSize
	if totalPages > a.maxPages {
		result.FinishedAt = a.now().UTC()
		return result, fmt.Errorf("contract error: Devfolio requires %d pages, above the safety limit %d", totalPages, a.maxPages)
	}

	all := append([]searchHit(nil), firstPage.Hits.Hits...)
	for page := 1; page < totalPages; page++ {
		offset := page * a.pageSize
		payload, fetchErr := a.fetchPage(ctx, offset)
		if fetchErr != nil {
			result.FinishedAt = a.now().UTC()
			return result, fmt.Errorf("fetch Devfolio offset %d: %w", offset, fetchErr)
		}
		if len(payload.Hits.Hits) == 0 {
			result.Warnings = append(result.Warnings, fmt.Sprintf("Devfolio offset %d became empty while the catalog was changing", offset))
			break
		}
		all = append(all, payload.Hits.Hits...)
	}

	seen := make(map[string]struct{}, len(all))
	for _, hit := range all {
		raw := hit.Source
		key := strings.TrimSpace(raw.UUID)
		if key != "" {
			if _, duplicate := seen[key]; duplicate {
				continue
			}
			seen[key] = struct{}{}
		}
		result.FetchedCount++
		event, rejected := normalizeEvent(raw, started)
		if rejected != nil {
			result.Rejected = append(result.Rejected, *rejected)
			continue
		}
		result.Events = append(result.Events, event)
	}
	if result.FetchedCount < total {
		result.Warnings = append(result.Warnings, fmt.Sprintf(
			"Devfolio reported %d hackathons with open applications, but %d unique records were received during pagination",
			total,
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
		return result, errors.New("contract error: no Devfolio record passed required-field validation")
	}
	return result, nil
}

func (a *Adapter) fetchPage(ctx context.Context, offset int) (searchResponse, error) {
	requestPayload := searchRequest{Type: "application_open", From: offset, Size: a.pageSize}
	body, err := json.Marshal(requestPayload)
	if err != nil {
		return searchResponse{}, fmt.Errorf("encode request: %w", err)
	}
	responseBody, err := a.postJSON(ctx, body)
	if err != nil {
		return searchResponse{}, err
	}
	var payload searchResponse
	if err := json.Unmarshal(responseBody, &payload); err != nil {
		return searchResponse{}, fmt.Errorf("decode API response: %w", err)
	}
	return payload, nil
}

func (a *Adapter) postJSON(ctx context.Context, body []byte) ([]byte, error) {
	if _, err := url.ParseRequestURI(a.apiURL); err != nil {
		return nil, fmt.Errorf("parse API URL: %w", err)
	}
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		if err := a.waitForRequest(ctx); err != nil {
			return nil, err
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, a.apiURL, bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("create request: %w", err)
		}
		request.Header.Set("Accept", "application/json")
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("User-Agent", "Meeteam-RAT-PoC/1.0 (+local research prototype)")
		response, err := a.client.Do(request)
		if err != nil {
			lastErr = err
		} else {
			payload, readErr := io.ReadAll(io.LimitReader(response.Body, maxResponseSize+1))
			_ = response.Body.Close()
			if readErr != nil {
				lastErr = readErr
			} else if len(payload) > maxResponseSize {
				return nil, errors.New("response exceeds 8 MiB safety limit")
			} else if response.StatusCode >= 200 && response.StatusCode < 300 {
				return payload, nil
			} else {
				lastErr = fmt.Errorf("HTTP %d", response.StatusCode)
				if response.StatusCode != http.StatusTooManyRequests && response.StatusCode < 500 {
					return nil, lastErr
				}
			}
		}
		if attempt < 3 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt) * 300 * time.Millisecond):
			}
		}
	}
	return nil, fmt.Errorf("request failed after retries: %w", lastErr)
}

func (a *Adapter) waitForRequest(ctx context.Context) error {
	a.limiterMu.Lock()
	wait := time.Until(a.nextRequest)
	if wait < 0 {
		wait = 0
	}
	a.nextRequest = time.Now().Add(wait + a.minDelay)
	a.limiterMu.Unlock()
	if wait == 0 {
		return nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func normalizeEvent(raw rawHackathon, observedAt time.Time) (model.Event, *model.RejectedEvent) {
	sourceID := cleanText(raw.UUID)
	title := cleanText(raw.Name)
	reject := func(reason string) (model.Event, *model.RejectedEvent) {
		return model.Event{}, &model.RejectedEvent{SourceEventID: sourceID, Title: title, Reason: reason}
	}
	if sourceID == "" {
		return reject("missing source event id")
	}
	if title == "" {
		return reject("missing title")
	}
	startAt, err := time.Parse(time.RFC3339, strings.TrimSpace(raw.StartsAt))
	if err != nil {
		return reject("missing or invalid start date")
	}
	endAt, err := time.Parse(time.RFC3339, strings.TrimSpace(raw.EndsAt))
	if err != nil || endAt.Before(startAt) {
		return reject("missing or invalid end date")
	}
	deadline, err := time.Parse(time.RFC3339, strings.TrimSpace(raw.Setting.RegistrationEndsAt))
	if err != nil {
		return reject("missing or invalid registration deadline")
	}
	if !deadline.After(observedAt) {
		return reject("registration deadline has already passed")
	}
	subdomain := strings.ToLower(cleanText(raw.Setting.Subdomain))
	if subdomain == "" {
		subdomain = strings.ToLower(cleanText(raw.Slug))
	}
	if !subdomainRE.MatchString(subdomain) {
		return reject("missing or invalid Devfolio subdomain")
	}
	sourceURL := "https://" + subdomain + ".devfolio.co/"
	officialURL := validateHTTPURL(raw.Setting.ExternalApplyURL)
	if officialURL == "" {
		officialURL = sourceURL
	}

	format := "offline"
	if raw.IsOnline || raw.Setting.Review {
		format = "online"
	}
	location := cleanText(raw.Location)
	if location == "" {
		location = joinNonEmpty(", ", cleanText(raw.City), cleanText(raw.State), cleanText(raw.Country))
	}
	description := cleanText(raw.Tagline)
	if description == "" {
		description = "Открытая регистрация на Devfolio."
	}
	if format == "offline" && location != "" && !strings.Contains(strings.ToLower(description), strings.ToLower(location)) {
		description = strings.TrimSpace(description + " Место: " + location + ".")
	}
	tags := make([]string, 0, len(raw.Themes))
	for _, theme := range raw.Themes {
		appendUnique(&tags, cleanText(theme.Name))
	}
	imageURL := validateHTTPURL(raw.CoverImage)

	fingerprintInput := strings.Join([]string{
		sourceID, title, description, raw.StartsAt, raw.EndsAt, raw.Setting.RegistrationEndsAt,
		format, location, officialURL, sourceURL, imageURL, strings.Join(tags, "|"),
	}, "\n")
	hash := sha256.Sum256([]byte(fingerprintInput))
	listingExpiry := deadline
	return model.Event{
		ID:                     model.DevfolioSourceID + ":" + sourceID,
		SourceID:               model.DevfolioSourceID,
		SourceName:             "Devfolio",
		SourceEventID:          sourceID,
		Title:                  title,
		Description:            description,
		StartAt:                &startAt,
		EndAt:                  &endAt,
		StartPrecision:         "datetime",
		RegistrationDeadlineAt: &deadline,
		ListingExpiresAt:       &listingExpiry,
		Format:                 format,
		OfficialURL:            officialURL,
		SourceURL:              sourceURL,
		ImageURL:               imageURL,
		Tags:                   tags,
		ParsingNotes: []string{
			"record read from the public search endpoint used by the Devfolio open-applications catalog",
			"catalog request explicitly filtered to type=application_open",
			"RFC3339 timestamps and registration deadline were supplied by Devfolio",
		},
		RawFingerprint: hex.EncodeToString(hash[:]),
		FirstSeenAt:    observedAt,
		LastSeenAt:     observedAt,
	}, nil
}

func validateHTTPURL(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	parsed, err := url.Parse(value)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return ""
	}
	return parsed.String()
}

func cleanText(value string) string {
	return strings.TrimSpace(spacesRE.ReplaceAllString(value, " "))
}

func joinNonEmpty(separator string, values ...string) string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value != "" {
			result = append(result, value)
		}
	}
	return strings.Join(result, separator)
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
