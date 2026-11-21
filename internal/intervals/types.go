package intervals

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

var (
	// ErrUnauthorized is a 401 or 403: the key is wrong or lacks access.
	ErrUnauthorized = errors.New("intervals: unauthorized")
	// ErrNotFound is a 404.
	ErrNotFound = errors.New("intervals: not found")
)

// RateLimitedError reports a 429; RetryAt is when to try again.
type RateLimitedError struct{ RetryAt time.Time }

func (e *RateLimitedError) Error() string {
	return "intervals: rate limited, retry at " + e.RetryAt.UTC().Format(time.RFC3339)
}

// StatusError reports an unexpected HTTP status (or a transport failure when
// Status is 0) for an endpoint. It never carries a URL, key or response body.
type StatusError struct {
	Endpoint string
	Status   int
}

func (e *StatusError) Error() string {
	if e.Status == 0 {
		return fmt.Sprintf("intervals: %s: transport error", e.Endpoint)
	}
	return fmt.Sprintf("intervals: %s: unexpected status %d", e.Endpoint, e.Status)
}

// Activity is one list entry. Pointer fields are nil when absent. JSON names
// are those of the Activity schema in https://intervals.icu/api/v1/docs.
type Activity struct {
	ID             string `json:"id"`
	StartDate      string `json:"start_date"`       // UTC
	StartDateLocal string `json:"start_date_local"` // "2024-11-20T07:35:18"
	Type           string `json:"type"`
	Name           string `json:"name"` // untrusted free text
	Source         string `json:"source"`
	FileType       string `json:"file_type"`

	Distance           *float64 `json:"distance"`
	MovingTime         *float64 `json:"moving_time"`
	ElapsedTime        *float64 `json:"elapsed_time"`
	TotalElevationGain *float64 `json:"total_elevation_gain"`
	AverageHeartrate   *float64 `json:"average_heartrate"`
	MaxHeartrate       *float64 `json:"max_heartrate"`
	AverageWatts       *float64 `json:"icu_average_watts"`
	WeightedAvgWatts   *float64 `json:"icu_weighted_avg_watts"` // normalized power
	TrainingLoad       *float64 `json:"icu_training_load"`
	Intensity          *float64 `json:"icu_intensity"` // intensity factor in percent
}

// Item pairs a decoded Activity with the raw JSON it came from.
type Item struct {
	V   Activity
	Raw json.RawMessage
}

// SportSettings is one entry of the athlete's per-sport settings. Pointer
// fields are nil when the API sends null. Zone names are not decoded.
type SportSettings struct {
	ID            int64    `json:"id"`
	Types         []string `json:"types"`
	FTP           *int     `json:"ftp"`
	IndoorFTP     *int     `json:"indoor_ftp"`
	LTHR          *int     `json:"lthr"`
	MaxHR         *int     `json:"max_hr"`
	ThresholdPace *float64 `json:"threshold_pace"`
	PowerZones    []int    `json:"power_zones"`
	HRZones       []int    `json:"hr_zones"`
	Updated       string   `json:"updated"`
}
