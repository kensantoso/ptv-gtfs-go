// Package realtime reads Victoria's GTFS-Realtime feeds.
//
// Separate from the schedule package because the two halves have opposite
// shapes: the schedule is a 268 MB download that changes weekly and needs no
// credential; realtime is 45 KB that changes every thirty seconds and needs a
// key. A client that only wants live delays should not pull in an indexer, and
// a server that only serves timetables should not pull in protobuf.
package realtime

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

// BaseURL is the gateway fronting Victoria's GTFS-Realtime feeds.
const BaseURL = "https://api.opendata.transport.vic.gov.au/opendata/public-transport/gtfs/realtime/v1"

// Feed identifies one published feed. Each is network-wide for its
// mode, which is the property that makes realtime cheap to serve: one request
// covers every service, rather than one request per stop.
type Feed struct {
	Path string
	Kind string // "trip-updates", "vehicle-positions", "service-alerts"
}

var (
	MetroTrainTripUpdates      = Feed{"/metro/trip-updates", "trip-updates"}
	MetroTrainVehiclePositions = Feed{"/metro/vehicle-positions", "vehicle-positions"}
	MetroTrainServiceAlerts    = Feed{"/metro/service-alerts", "service-alerts"}
	TramTripUpdates            = Feed{"/tram/trip-updates", "trip-updates"}
	TramVehiclePositions       = Feed{"/tram/vehicle-positions", "vehicle-positions"}
	TramServiceAlerts          = Feed{"/tram/service-alerts", "service-alerts"}
	BusTripUpdates             = Feed{"/bus/trip-updates", "trip-updates"}
	BusVehiclePositions        = Feed{"/bus/vehicle-positions", "vehicle-positions"}
)

// Client fetches GTFS-Realtime feeds.
//
// Authentication is a key in a KeyID header. Note this contradicts the
// published OpenAPI documents, which declare Ocp-Apim-Subscription-Key; that
// header returns 401 against the live gateway while KeyID succeeds. Verified
// empirically, not assumed.
type Client struct {
	key     string
	baseURL string
	http    *http.Client
}

// Option configures a Client.
type Option func(*Client)

// WithHTTPClient supplies the HTTP client, for a custom transport or
// timeout. The default has a 30s timeout.
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) { c.http = h }
}

// WithBaseURL overrides the gateway, mainly for tests.
func WithBaseURL(u string) Option {
	return func(c *Client) { c.baseURL = u }
}

// NewClient returns a client for the realtime feeds. Register for a key
// at https://opendata.transport.vic.gov.au — signup is self-service, unlike the
// Timetable API which is issued by email.
func NewClient(key string, opts ...Option) (*Client, error) {
	if key == "" {
		return nil, fmt.Errorf("realtime: API key is required")
	}
	c := &Client{
		key:     key,
		baseURL: BaseURL,
		http:    &http.Client{Timeout: 30 * time.Second},
	}
	for _, o := range opts {
		o(c)
	}
	return c, nil
}

// Fetch returns the raw protobuf body of a feed.
//
// The bytes are returned undecoded so callers can unmarshal with the official
// gtfs-realtime bindings without this package taking on that dependency. The
// gateway caches for 30 seconds and permits 24 requests per minute per mode, so
// polling faster than every 30s gains nothing.
func (c *Client) Fetch(ctx context.Context, feed Feed) ([]byte, error) {
	url := c.baseURL + feed.Path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("realtime: request: %w", err)
	}
	req.Header.Set("KeyID", c.key)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("realtime: %s: %w", feed.Path, err)
	}
	defer resp.Body.Close()

	// Feeds are a few hundred KB; the cap guards against a malformed response.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, fmt.Errorf("realtime: %s: read: %w", feed.Path, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &Error{Status: resp.StatusCode, Path: feed.Path, Body: string(body)}
	}
	return body, nil
}

// Error is a non-200 from the realtime gateway.
type Error struct {
	Status int
	Path   string
	Body   string
}

func (e *Error) Error() string {
	body := e.Body
	if len(body) > 200 {
		body = body[:200] + "…"
	}
	return fmt.Sprintf("realtime: %s: status %d: %s", e.Path, e.Status, body)
}
