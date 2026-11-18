package store

import (
	"errors"
	"time"

	"github.com/stefiix92/coachd/internal/civil"
	"github.com/stefiix92/coachd/internal/fitdata"
)

// ErrNotFound is returned when a requested row does not exist.
var ErrNotFound = errors.New("not found")

type Sport string

const (
	SportRunning      Sport = "running"
	SportTrailRunning Sport = "trail_running"
	SportWalking      Sport = "walking"
	SportHiking       Sport = "hiking"
	SportCycling      Sport = "cycling"
	SportStrength     Sport = "strength"
	SportOther        Sport = "other"
)

type FITStatus string

const (
	FITPending     FITStatus = "pending"
	FITStored      FITStatus = "stored"
	FITUnavailable FITStatus = "unavailable"
	FITFailed      FITStatus = "failed"
)

// ActivityGarmin holds Garmin-proprietary derived values.
type ActivityGarmin struct {
	TrainingLoad, AerobicTE, AnaerobicTE, VO2Max, TSS *float64
}

// Source names where an activity came from.
type Source string

const (
	SourceGarmin    Source = "garmin"
	SourceIntervals Source = "intervals_icu"
)

type Activity struct {
	ID                                                  int64
	StartLocal, StartUTC                                time.Time
	Date, WeekStart                                     civil.Date
	TypeKey                                             string
	Sport                                               Sport
	NameUntrusted                                       string
	DistanceM, DurationS, MovingS, ElevGainM, ElevLossM float64
	AvgHR, MaxHR                                        *float64
	Manual                                              bool
	Garmin                                              ActivityGarmin
	// Perceived is the runner's own rating from the FIT session, set with the
	// activity's detail; UpsertActivity never writes it.
	Perceived   fitdata.Perceived
	FITPath     string // relative to DataDir
	FITStatus   FITStatus
	FITAttempts int
	// Source is where the activity came from; empty is SourceGarmin on upsert.
	Source Source
	// SourceID is the source's own ID; "" for Garmin (stored NULL).
	SourceID string
	// HasPower: the source's list entry reports average power above zero.
	HasPower bool
	// HiddenBy is the ID of the richer copy of the same session; nil when shown.
	// UpsertActivity never writes it, only SetHiddenBy.
	HiddenBy *int64
}

// DailyGarmin holds Garmin-proprietary derived daily values.
type DailyGarmin struct {
	SleepScore                                                                        *int
	HRVStatus                                                                         *string
	HRVBaselineLowMs, HRVBaselineHighMs                                               *int
	BodyBatteryMax, BodyBatteryMin, BodyBatteryCharged, BodyBatteryDrained, StressAvg *int
}

type DailyHealth struct {
	Date                                                       civil.Date
	SleepS, HRVOvernightMs, HRVWeeklyAvgMs, RestingHR, SpO2Avg *int
	WeightKg                                                   *float64
	Garmin                                                     DailyGarmin
}

type DayMetrics struct {
	Date                                                                                         civil.Date
	HRV7dMs, HRVBandLowMs, HRVBandHighMs                                                         *float64
	HRVPosition                                                                                  string // "below","within","above",""
	RestingHR7d, RestingHR60d, Sleep7dS                                                          *float64
	LongestRun7dM, LongestRun7dS, LongestRun30dM, LongestRun30dS, LongestRun90dM, LongestRun90dS float64
}

type SyncState struct {
	DataType                                     string // "activities","sleep","hrv","daily","weight", or SyncTypeIntervals
	LastAttemptAt, LastSuccessAt, FreshestDataAt *time.Time
	LastOutcome                                  string
	Cursor                                       string
	BackfillDone                                 bool
}

// SyncTypeIntervals is the data type of intervals.icu activities; every other
// data type is Garmin's.
const SyncTypeIntervals = "intervals_activities"

// FromGarmin reports whether the state is of one of Garmin's data types.
func (s SyncState) FromGarmin() bool { return s.DataType != SyncTypeIntervals }

type TokenRecord struct {
	ID, Name              string
	Hash                  []byte
	Scopes                []string
	CreatedAt, ExpiresAt  time.Time
	LastUsedAt, RevokedAt *time.Time
}

type AuditEvent struct {
	At                                          time.Time
	Lane, Actor, Client, Event, Target, Outcome string
}

type ActivityQuery struct {
	From, To civil.Date
	Sport    Sport
	Limit    int
	Cursor   string
}

type WeekVolume struct {
	WeekStart                     civil.Date
	Sport                         Sport
	DistanceM, MovingS, ElevGainM float64
	Count                         int
}

type Coverage struct {
	FirstDate          *civil.Date
	WeeksOfHistory     int
	DailyHealthGaps90d int
	// Days in [today−89, today], on/after FirstDate, whose field is NULL or
	// whose daily_health row is missing.
	HRVGaps90d, SleepGaps90d, RestingHRGaps90d int
}

// Goal is the runner's single active target event; older goals are archived.
type Goal struct {
	ID                         int64
	Name                       string
	Date                       civil.Date
	Sport                      Sport
	DistanceKm, ElevationGainM *float64
	Target, Notes, CreatedBy   string
	CreatedAt                  time.Time
	ArchivedAt                 *time.Time
}

type NoteCategory string

const (
	NoteInjury       NoteCategory = "injury"
	NoteAvailability NoteCategory = "availability"
	NotePreference   NoteCategory = "preference"
	NoteOther        NoteCategory = "other"
)

type Note struct {
	ID                   int64
	Category             NoteCategory
	Text, CreatedBy      string
	CreatedAt, UpdatedAt time.Time
	// ValidUntil is the last day the note holds, in the runner's timezone;
	// nil for a durable note.
	ValidUntil *civil.Date
}

// Expired reports whether the note's last day is before today.
func (n Note) Expired(today civil.Date) bool {
	return n.ValidUntil != nil && n.ValidUntil.Before(today)
}

type ProposalStatus string

const (
	ProposalPending        ProposalStatus = "pending"
	ProposalApproved       ProposalStatus = "approved"
	ProposalDelivering     ProposalStatus = "delivering"
	ProposalDelivered      ProposalStatus = "delivered"
	ProposalDeliveryFailed ProposalStatus = "delivery_failed"
	ProposalWithdrawn      ProposalStatus = "withdrawn"
	ProposalDeclined       ProposalStatus = "declined"
	ProposalCancelled      ProposalStatus = "cancelled"
	ProposalSuperseded     ProposalStatus = "superseded"
	ProposalExpired        ProposalStatus = "expired"
)

// ProposalStatuses lists every status in lifecycle order. Read-only.
var ProposalStatuses = []ProposalStatus{ProposalPending, ProposalApproved, ProposalDelivering, ProposalDelivered,
	ProposalDeliveryFailed, ProposalWithdrawn, ProposalDeclined, ProposalCancelled, ProposalSuperseded, ProposalExpired}

type Proposal struct {
	ID                                    string
	Date                                  civil.Date
	Sport                                 Sport
	Name                                  string
	ContentJSON                           []byte
	ContentHash                           string
	Rationale                             string
	Replaces                              *string
	WarningsJSON                          []byte
	Status                                ProposalStatus
	StatusAt, CreatedAt                   time.Time
	CreatedLane, CreatedBy, CreatedClient string
	// TraceID and SpanID are the hex IDs of the span that proposed it; empty when unknown.
	TraceID, SpanID string
}

// Transition is a compare-and-swap status change; the audit event is "proposal_<To>".
type Transition struct {
	From                []ProposalStatus
	To                  ProposalStatus
	At                  time.Time
	Lane, Actor, Client string
}

// ErrConflict means a compare-and-swap failed: the status was not in From.
var ErrConflict = errors.New("conflict")

// Note limits.
const (
	MaxNotes     = 50
	MaxNoteRunes = 500
)

// ErrNotesFull means the notes cap is reached.
var ErrNotesFull = errors.New("notes full")

// ErrNotOwned means the note exists but was not written by an agent, so the
// agent variants (UpdateAgentNote, DeleteAgentNote) may not change it.
var ErrNotOwned = errors.New("note not written by an agent")

// ProposalSummary is the id, date and name of a proposal.
type ProposalSummary struct {
	ID   string
	Date civil.Date
	Name string
}

// Passkey is a registered runner WebAuthn credential. CredentialJSON is the
// webauthn.Credential as JSON; the store does not interpret it.
type Passkey struct {
	ID                    int64
	CredentialID          []byte
	CredentialJSON        []byte
	Label                 string
	CreatedAt             time.Time
	LastUsedAt, RevokedAt *time.Time
}

// WebSession is a runner web session bound to one passkey.
type WebSession struct {
	PasskeyID            int64
	CreatedAt, ExpiresAt time.Time
}

// EnrollParams carries everything EnrollPasskey does in one transaction.
type EnrollParams struct {
	EnrollmentID int64
	Nonce        string
	NonceExpires time.Time
	Passkey      Passkey // ID ignored
	At           time.Time
	Lane         string // audit: actor "runner", client the new passkey
}

// ErrNonceUsed means a single-use nonce was already consumed.
var ErrNonceUsed = errors.New("nonce already used")

// Device is a paired watch. Its token is stored only as a hash.
type Device struct {
	ID                    int64
	Label, Model          string
	CreatedAt             time.Time
	LastSeenAt, RevokedAt *time.Time
}

// Pairing is a watch's pending request to be paired.
type Pairing struct {
	ID, Code, Model        string
	CreatedAt, ExpiresAt   time.Time
	ConfirmedAt, ClaimedAt *time.Time
	Label                  string // set at confirmation
}

// ErrTooManyPairings is returned when the pending pairing cap is reached.
var ErrTooManyPairings = errors.New("too many pending pairings")

// Sentinel errors of the OAuth store.
var (
	ErrCodeReused  = errors.New("authorization code reused")
	ErrTokenReused = errors.New("refresh token reused")
	ErrRevoked     = errors.New("grant revoked")
	ErrExpired     = errors.New("expired")
	ErrScope       = errors.New("scope not granted")
)

// OAuthClient is a CIMD cache entry or a consented DCR client. Kind is "cimd" or "dcr".
type OAuthClient struct {
	ID                               int64
	ClientID, Kind, Name             string
	RedirectURIs                     []string
	Metadata                         []byte
	FetchedAt, CacheUntil, CreatedAt time.Time
}

type OAuthGrant struct {
	ID, ClientRowID, PasskeyID         int64
	ClientID                           string
	Scopes                             []string
	Resource                           string
	CreatedAt                          time.Time
	ActivatedAt, LastUsedAt, RevokedAt *time.Time
}

type OAuthCode struct {
	GrantID          int64
	ClientID         string
	RedirectURI      string
	RedirectExplicit bool
	CodeChallenge    string
	ExpiresAt        time.Time
	Used             bool
}

// NewTokens carries the hashes and expiries of a freshly issued access and refresh token.
type NewTokens struct {
	AccessHash, RefreshHash       []byte
	AccessExpires, RefreshExpires time.Time
}

// ConsentGrant is everything GrantConsent writes in one transaction.
type ConsentGrant struct {
	Client           OAuthClient // ID 0: insert it (a DCR client at first consent)
	PasskeyID        int64
	CredentialJSON   []byte
	Nonce            string
	NonceExpires     time.Time
	Scopes           []string
	Resource         string
	CodeHash         []byte
	RedirectURI      string
	RedirectExplicit bool
	CodeChallenge    string
	CodeExpires      time.Time
	At               time.Time
}

type Redeemed struct {
	Grant    OAuthGrant
	FirstUse bool
}

type Rotated struct {
	Grant   OAuthGrant
	Scopes  []string
	Sibling bool
}

type ConnectedApp struct {
	GrantID              int64
	ClientID, Kind, Name string
	RedirectURIs, Scopes []string
	CreatedAt            time.Time
	LastUsedAt           *time.Time
}
