package intervals

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stefiix92/coachd/internal/civil"
)

const testKey = "k3y"

func gz(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func newTest(t *testing.T, h http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return New(srv.URL, testKey, "coachd-test/1", nil), srv
}

func TestListActivities(t *testing.T) {
	const e1 = `{"id":"i1","start_date":"2024-11-20T06:35:18Z","start_date_local":"2024-11-20T07:35:18","type":"Ride","name":"x","source":"ZWIFT","file_type":"fit","distance":1000.5,"moving_time":60,"elapsed_time":70,"total_elevation_gain":5,"average_heartrate":140,"max_heartrate":170,"icu_average_watts":200}`
	const e2 = `{"id":"i2","type":"Run"}`
	c, _ := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/athlete/0/activities" {
			t.Errorf("path %s", r.URL.Path)
		}
		q := r.URL.Query()
		if q.Get("oldest") != "2024-11-01" || q.Get("newest") != "2024-11-20" {
			t.Errorf("query %v", q)
		}
		if got, want := r.Header.Get("Authorization"), "Basic "+base64.StdEncoding.EncodeToString([]byte("API_KEY:"+testKey)); got != want {
			t.Errorf("auth %q", got)
		}
		if r.Header.Get("User-Agent") != "coachd-test/1" {
			t.Errorf("ua %q", r.Header.Get("User-Agent"))
		}
		w.Write([]byte("[" + e1 + "," + e2 + "]"))
	})
	from, _ := civil.Parse("2024-11-01")
	to, _ := civil.Parse("2024-11-20")
	items, err := c.ListActivities(context.Background(), from, to)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("len %d", len(items))
	}
	if string(items[0].Raw) != e1 || string(items[1].Raw) != e2 {
		t.Errorf("raw %s | %s", items[0].Raw, items[1].Raw)
	}
	a := items[0].V
	if a.ID != "i1" || a.StartDateLocal != "2024-11-20T07:35:18" || a.Source != "ZWIFT" || a.FileType != "fit" {
		t.Errorf("%+v", a)
	}
	if a.AverageWatts == nil || *a.AverageWatts != 200 || a.TotalElevationGain == nil || *a.TotalElevationGain != 5 {
		t.Errorf("%+v", a)
	}
	if items[1].V.Distance != nil || items[1].V.AverageWatts != nil {
		t.Errorf("absent fields not nil: %+v", items[1].V)
	}
}

func TestDownloadFileGzipBody(t *testing.T) {
	want := []byte("fit-bytes")
	c, _ := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/activity/i1/file" {
			t.Errorf("path %s", r.URL.Path)
		}
		w.Write(gz(t, want))
	})
	got, err := c.DownloadFile(context.Background(), "i1")
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("%q %v", got, err)
	}
}

func TestDownloadFileContentEncoding(t *testing.T) {
	want := []byte("fit-bytes")
	c, _ := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		w.Write(gz(t, want))
	})
	got, err := c.DownloadFile(context.Background(), "i1")
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("%q %v", got, err)
	}
}

func TestDownloadFilePlainBody(t *testing.T) {
	want := []byte("plain fit")
	c, _ := newTest(t, func(w http.ResponseWriter, r *http.Request) { w.Write(want) })
	got, err := c.DownloadFile(context.Background(), "i1")
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("%q %v", got, err)
	}
}

func TestDownloadGeneratedFIT(t *testing.T) {
	want := []byte("generated fit")
	for _, body := range [][]byte{want, gz(t, want)} {
		c, _ := newTest(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/v1/activity/i1/fit-file" {
				t.Errorf("path %s", r.URL.Path)
			}
			w.Write(body)
		})
		got, err := c.DownloadGeneratedFIT(context.Background(), "i1")
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("%q %v", got, err)
		}
	}
	c, _ := newTest(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) })
	if _, err := c.DownloadGeneratedFIT(context.Background(), "i1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("404: err = %v, want ErrNotFound", err)
	}
}

func TestDownloadFileTooLarge(t *testing.T) {
	c, _ := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write(gz(t, make([]byte, maxFileBody+1)))
	})
	got, err := c.DownloadFile(context.Background(), "i1")
	if err == nil || got != nil {
		t.Fatalf("want error and no bytes, got %d bytes, %v", len(got), err)
	}
	var se *StatusError
	if errors.As(err, &se) {
		t.Errorf("too-large must not be a StatusError")
	}
}

func TestDownloadFileEscapesID(t *testing.T) {
	c, _ := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != "/api/v1/activity/a%2Fb/file" {
			t.Errorf("escaped path %s", r.URL.EscapedPath())
		}
		w.Write([]byte("x"))
	})
	if _, err := c.DownloadFile(context.Background(), "a/b"); err != nil {
		t.Fatal(err)
	}
}

func TestActivityExists(t *testing.T) {
	c, _ := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/activity/ok":
			w.Write([]byte(`{}`))
		case "/api/v1/activity/gone":
			w.WriteHeader(404)
		default:
			w.WriteHeader(500)
		}
	})
	ctx := context.Background()
	if ok, err := c.ActivityExists(ctx, "ok"); !ok || err != nil {
		t.Errorf("200: %v %v", ok, err)
	}
	if ok, err := c.ActivityExists(ctx, "gone"); ok || err != nil {
		t.Errorf("404: %v %v", ok, err)
	}
	if ok, err := c.ActivityExists(ctx, "boom"); ok || err == nil {
		t.Errorf("500: %v %v", ok, err)
	}
}

func errCases(t *testing.T) map[string]error {
	t.Helper()
	fixed := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	status := func(code int, hdr string) error {
		c, _ := newTest(t, func(w http.ResponseWriter, r *http.Request) {
			if hdr != "" {
				w.Header().Set("Retry-After", hdr)
			}
			w.WriteHeader(code)
			w.Write([]byte("body mentions " + testKey))
		})
		c.now = func() time.Time { return fixed }
		_, err := c.ListActivities(context.Background(), civil.Date{Year: 2026, Month: 11, Day: 1}, civil.Date{Year: 2026, Month: 11, Day: 2})
		return err
	}
	closed := httptest.NewServer(http.NotFoundHandler())
	c := New(closed.URL, testKey, "ua", nil)
	closed.Close()
	_, terr := c.ListActivities(context.Background(), civil.Date{Year: 2026, Month: 11, Day: 1}, civil.Date{Year: 2026, Month: 11, Day: 2})
	return map[string]error{
		"401": status(401, ""), "403": status(403, ""), "404": status(404, ""),
		"429": status(429, "120"), "429none": status(429, ""), "500": status(500, ""), "transport": terr,
	}
}

func TestErrors(t *testing.T) {
	e := errCases(t)
	if !errors.Is(e["401"], ErrUnauthorized) || !errors.Is(e["403"], ErrUnauthorized) {
		t.Errorf("401/403: %v %v", e["401"], e["403"])
	}
	if !errors.Is(e["404"], ErrNotFound) {
		t.Errorf("404: %v", e["404"])
	}
	fixed := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	var rl *RateLimitedError
	if !errors.As(e["429"], &rl) || !rl.RetryAt.Equal(fixed.Add(120*time.Second)) {
		t.Errorf("429: %v", e["429"])
	}
	if !errors.As(e["429none"], &rl) || !rl.RetryAt.Equal(fixed.Add(15*time.Minute)) {
		t.Errorf("429 none: %v", e["429none"])
	}
	var se *StatusError
	if !errors.As(e["500"], &se) || se.Status != 500 || se.Endpoint != "activities" {
		t.Errorf("500: %v", e["500"])
	}
	if !errors.As(e["transport"], &se) || se.Status != 0 {
		t.Errorf("transport: %v", e["transport"])
	}
}

func TestKeyNeverInErrors(t *testing.T) {
	b64 := base64.StdEncoding.EncodeToString([]byte("API_KEY:" + testKey))
	for name, err := range errCases(t) {
		if err == nil {
			t.Errorf("%s: nil error", name)
			continue
		}
		if s := err.Error(); strings.Contains(s, testKey) || strings.Contains(s, b64) {
			t.Errorf("%s leaks key: %s", name, s)
		}
	}
}

// TestListActivitiesRealShape decodes entries shaped like a live response of
// 2026-11-20 (testdata/list_real.json): a MyWhoosh ride sent by its own link,
// a Strava stub and a GPX upload. Field names, value types and nulls are the
// API's; identifiers, names and health values are made up.
func TestListActivitiesRealShape(t *testing.T) {
	body, err := os.ReadFile("testdata/list_real.json")
	if err != nil {
		t.Fatal(err)
	}
	c, _ := newTest(t, func(w http.ResponseWriter, r *http.Request) { w.Write(body) })
	from, _ := civil.Parse("2026-11-10")
	to, _ := civil.Parse("2026-11-20")
	items, err := c.ListActivities(context.Background(), from, to)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 {
		t.Fatalf("len %d", len(items))
	}
	ride, stub, gpx := items[0].V, items[1].V, items[2].V
	if ride.ID != "i900000001" || ride.Source != "OAUTH_CLIENT" || ride.FileType != "fit" || ride.Type != "VirtualRide" ||
		ride.StartDate != "2026-11-20T15:33:48Z" || ride.StartDateLocal != "2026-11-20T16:33:48" {
		t.Errorf("ride %+v", ride)
	}
	if ride.AverageWatts == nil || *ride.AverageWatts != 151 || ride.AverageHeartrate == nil || *ride.AverageHeartrate != 143 ||
		ride.MaxHeartrate == nil || *ride.MaxHeartrate != 171 || ride.ElapsedTime == nil || *ride.ElapsedTime != 3483 ||
		ride.MovingTime == nil || *ride.MovingTime != 3483 || ride.Distance == nil || *ride.Distance != 7327.28 ||
		ride.TotalElevationGain == nil || *ride.TotalElevationGain != 586 {
		t.Errorf("ride numbers %+v", ride)
	}
	if stub.ID != "90000000002" || stub.Source != "STRAVA" || stub.StartDate != "" || stub.ElapsedTime != nil {
		t.Errorf("stub %+v", stub)
	}
	if gpx.FileType != "gpx" || gpx.Source != "UPLOAD" || gpx.AverageWatts != nil || gpx.ElapsedTime == nil || *gpx.ElapsedTime != 5487 {
		t.Errorf("gpx %+v", gpx)
	}
}
