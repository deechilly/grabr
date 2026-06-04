package crawler

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

const userAgent = "grabr/0.1 (+https://github.com/deechilly/grabr)"

// Adaptive rate-limit controller bounds.
const (
	minRPS              = 0.1
	maxRPS              = 20.0
	rampSuccessStreak   = 10  // bump rate up after this many consecutive 2xx/3xx
	rampUpFactor        = 1.25
	rampDownFactor      = 0.5
	maxRetryAfterWindow = 60 * time.Second // ignore absurd Retry-After values
)

// Fetcher is a per-site HTTP client with adaptive rate limiting. The limit
// starts at the configured RPS, ramps up on a streak of healthy responses,
// halves on 429/5xx, and honors Retry-After headers.
type Fetcher struct {
	client *http.Client
	tag    string // log prefix (e.g. site slug)

	mu            sync.Mutex
	limiter       *rate.Limiter
	currentRate   float64
	maxAllowedRPS float64 // dynamic ceiling; defaults to maxRPS, can be lowered by robots.txt Crawl-delay
	successStreak int
	retryUntil    time.Time
}

func NewFetcher(rps float64) *Fetcher {
	return NewFetcherWithTag(rps, "")
}

func NewFetcherWithTag(rps float64, tag string) *Fetcher {
	if rps <= 0 {
		rps = 1
	}
	if rps < minRPS {
		rps = minRPS
	}
	if rps > maxRPS {
		rps = maxRPS
	}
	return &Fetcher{
		client:        &http.Client{Timeout: 30 * time.Second},
		tag:           tag,
		limiter:       rate.NewLimiter(rate.Limit(rps), 1),
		currentRate:   rps,
		maxAllowedRPS: maxRPS,
	}
}

// SetMaxRate clamps both the dynamic ceiling and the current rate. Used by
// the crawler when robots.txt specifies a Crawl-delay tighter than maxRPS.
func (f *Fetcher) SetMaxRate(rps float64) {
	if rps <= 0 {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.maxAllowedRPS = rps
	if f.currentRate > rps {
		f.currentRate = rps
		f.limiter.SetLimit(rate.Limit(rps))
		if f.tag != "" {
			log.Printf("fetcher[%s]: clamped rate to %.3f rps (robots.txt Crawl-delay)", f.tag, rps)
		}
	}
}

// CurrentRate returns the limiter's current RPS (for logging/diagnostics).
func (f *Fetcher) CurrentRate() float64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.currentRate
}

type Response struct {
	StatusCode  int
	ContentType string
	Body        []byte
	FinalURL    string
	RetryAfter  string // raw header value if present
}

func (f *Fetcher) Get(ctx context.Context, url string) (*Response, error) {
	// Honor any pending Retry-After window before consulting the token bucket.
	f.mu.Lock()
	wait := time.Until(f.retryUntil)
	f.mu.Unlock()
	if wait > 0 {
		t := time.NewTimer(wait)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return nil, ctx.Err()
		}
	}
	if err := f.limiter.Wait(ctx); err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "*/*")
	resp, err := f.client.Do(req)
	if err != nil {
		f.observeError()
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		f.observeError()
		return nil, fmt.Errorf("read body: %w", err)
	}
	out := &Response{
		StatusCode:  resp.StatusCode,
		ContentType: resp.Header.Get("Content-Type"),
		Body:        body,
		FinalURL:    resp.Request.URL.String(),
		RetryAfter:  resp.Header.Get("Retry-After"),
	}
	f.observe(out)
	return out, nil
}

func (f *Fetcher) observe(resp *Response) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case resp.StatusCode == 429 || resp.StatusCode >= 500:
		f.applyRetryAfterLocked(resp.RetryAfter)
		f.decreaseRateLocked(resp.StatusCode)
		f.successStreak = 0
	case resp.StatusCode >= 200 && resp.StatusCode < 400:
		f.successStreak++
		if f.successStreak >= rampSuccessStreak {
			f.increaseRateLocked()
			f.successStreak = 0
		}
	default:
		// 4xx (non-429) — don't blame the server's capacity, no rate change.
		f.successStreak = 0
	}
}

func (f *Fetcher) observeError() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.decreaseRateLocked(-1)
	f.successStreak = 0
}

func (f *Fetcher) applyRetryAfterLocked(header string) {
	if header == "" {
		return
	}
	var wait time.Duration
	if secs, err := strconv.Atoi(header); err == nil && secs > 0 {
		wait = time.Duration(secs) * time.Second
	} else if t, err := http.ParseTime(header); err == nil {
		wait = time.Until(t)
	}
	if wait <= 0 {
		return
	}
	if wait > maxRetryAfterWindow {
		wait = maxRetryAfterWindow
	}
	deadline := time.Now().Add(wait)
	if deadline.After(f.retryUntil) {
		f.retryUntil = deadline
	}
	if f.tag != "" {
		log.Printf("fetcher[%s]: Retry-After=%s, pausing %s", f.tag, header, wait)
	}
}

func (f *Fetcher) decreaseRateLocked(status int) {
	prev := f.currentRate
	f.currentRate *= rampDownFactor
	if f.currentRate < minRPS {
		f.currentRate = minRPS
	}
	f.limiter.SetLimit(rate.Limit(f.currentRate))
	if f.tag != "" {
		log.Printf("fetcher[%s]: back off rate %.3f -> %.3f rps (status=%d)", f.tag, prev, f.currentRate, status)
	}
}

func (f *Fetcher) increaseRateLocked() {
	prev := f.currentRate
	ceiling := f.maxAllowedRPS
	if ceiling <= 0 || ceiling > maxRPS {
		ceiling = maxRPS
	}
	f.currentRate *= rampUpFactor
	if f.currentRate > ceiling {
		f.currentRate = ceiling
	}
	if f.currentRate == prev {
		return
	}
	f.limiter.SetLimit(rate.Limit(f.currentRate))
	if f.tag != "" {
		log.Printf("fetcher[%s]: ramp up rate %.3f -> %.3f rps", f.tag, prev, f.currentRate)
	}
}
