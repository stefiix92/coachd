// Package garmin is a client for the Garmin Connect Personal API.
//
// Errors returned from this package carry only the endpoint name and HTTP
// status code. They never contain the API key, bearer token, request URLs or
// response bodies.
package garmin

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Sentinel errors.
var (
	ErrNotFound     = errors.New("garmin: not found")
	ErrUnauthorized = errors.New("garmin: unauthorized")
)

// RateLimitedError is returned on HTTP 429.
type RateLimitedError struct{ RetryAt time.Time }

func (e *RateLimitedError) Error() string {
	return "garmin: rate limited, retry at " + e.RetryAt.UTC().Format(time.RFC3339)
}

// StatusError reports an unexpected HTTP status (or a transport failure when
// Status is 0) for an endpoint.
type StatusError struct {
	Endpoint string
	Status   int
}

func (e *StatusError) Error() string {
	if e.Status == 0 {
		return fmt.Sprintf("garmin: %s: transport error", e.Endpoint)
	}
	return fmt.Sprintf("garmin: %s: unexpected status %d", e.Endpoint, e.Status)
}

// Item pairs a decoded DTO with the raw JSON it came from.
type Item[T any] struct {
	V   T
	Raw json.RawMessage
}

// ActivityDTO is one entry of the activity search. Times use the format
// "2006-01-02 15:04:05". Units: metres, seconds.
type ActivityDTO struct {
	ActivityID     int64  `json:"activityId"`
	ActivityName   string `json:"activityName"` // untrusted free text
	StartTimeLocal string `json:"startTimeLocal"`
	StartTimeGMT   string `json:"startTimeGMT"`
	ActivityType   *struct {
		TypeKey string `json:"typeKey"`
	} `json:"activityType"`
	Distance                *float64 `json:"distance"`
	Duration                *float64 `json:"duration"`
	MovingDuration          *float64 `json:"movingDuration"`
	ElevationGain           *float64 `json:"elevationGain"`
	ElevationLoss           *float64 `json:"elevationLoss"`
	AverageHR               *float64 `json:"averageHR"`
	MaxHR                   *float64 `json:"maxHR"`
	AvgPower                *float64 `json:"avgPower"` // watts
	ActivityTrainingLoad    *float64 `json:"activityTrainingLoad"`
	AerobicTrainingEffect   *float64 `json:"aerobicTrainingEffect"`
	AnaerobicTrainingEffect *float64 `json:"anaerobicTrainingEffect"`
	VO2MaxValue             *float64 `json:"vO2MaxValue"`
	TrainingStressScore     *float64 `json:"trainingStressScore"`
	ManualActivity          bool     `json:"manualActivity"`
}

// SleepDTO is one daily sleep summary.
type SleepDTO struct {
	CalendarDate     string `json:"calendarDate"`
	SleepTimeSeconds *int64 `json:"sleepTimeSeconds"`
	SleepScores      map[string]struct {
		Value *int `json:"value"`
	} `json:"sleepScores"`
	AverageSpO2Value *float64 `json:"averageSpO2Value"`
}

// HRVSummaryDTO is one daily HRV summary.
type HRVSummaryDTO struct {
	CalendarDate string   `json:"calendarDate"`
	LastNightAvg *float64 `json:"lastNightAvg"`
	WeeklyAvg    *float64 `json:"weeklyAvg"`
	Status       string   `json:"status"`
	Baseline     *struct {
		BalancedLow   *float64 `json:"balancedLow"`
		BalancedUpper *float64 `json:"balancedUpper"`
	} `json:"baseline"`
}

// DailySummaryV2DTO is one v2 daily summary.
type DailySummaryV2DTO struct {
	CalendarDate string `json:"calendarDate"`
	HeartRate    *struct {
		RestingValue *int `json:"restingValue"`
	} `json:"heartRate"`
	Stress *struct {
		AvgLevel *int `json:"avgLevel"`
	} `json:"stress"`
	BodyBattery *struct {
		MaxValue     *int `json:"maxValue"`
		MinValue     *int `json:"minValue"`
		ChargedValue *int `json:"chargedValue"`
		DrainedValue *int `json:"drainedValue"`
	} `json:"bodyBattery"`
	PulseOx *struct {
		AvgValue *int `json:"avgValue"`
	} `json:"pulseOx"`
}

// WeightDTO is one daily weight summary (WeightRangeDTO.dailyWeightSummaries
// element). Unit per the OpenAPI spec is kilograms; ingest keeps a >= 1000 ->
// grams rule as a safety net.
type WeightDTO struct {
	SummaryDate  string       `json:"summaryDate"`
	LatestWeight *WeightEntry `json:"latestWeight"`
}

// WeightEntry is the latest weigh-in of a day (DateWeightDTO subset).
type WeightEntry struct {
	CalendarDate string   `json:"calendarDate"`
	Weight       *float64 `json:"weight"`
}
