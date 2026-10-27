package garminkey

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stefiix92/coachd/internal/civil"
)

func date(s string) civil.Date {
	d, err := civil.Parse(s)
	if err != nil {
		panic(err)
	}
	return d
}

var configured = Key{Value: "configured-key", Expires: date("2026-11-22")}

// fakeGarmin accepts the keys in ok and records which key it uses.
type fakeGarmin struct {
	ok   map[string]bool
	err  error
	used string
}

func (f *fakeGarmin) UseKey(_ context.Context, key string, commit func() error) error {
	if f.err != nil {
		return f.err
	}
	if !f.ok[key] {
		return errors.New("refused")
	}
	if err := commit(); err != nil {
		return err
	}
	f.used = key
	return nil
}

func TestLoadWithoutFile(t *testing.T) {
	h, err := Load(t.TempDir(), configured)
	if err != nil {
		t.Fatal(err)
	}
	if h.Value() != "configured-key" || h.Expires() != configured.Expires {
		t.Fatalf("key = %q until %v", h.Value(), h.Expires())
	}
}

func TestRenewWritesAndSwitches(t *testing.T) {
	dir := t.TempDir()
	h, err := Load(dir, configured)
	if err != nil {
		t.Fatal(err)
	}
	g := &fakeGarmin{ok: map[string]bool{"new-key": true}}
	next := Key{Value: "new-key", Expires: date("2027-02-20")}
	if err := h.Renew(context.Background(), g, next); err != nil {
		t.Fatal(err)
	}
	if h.Value() != "new-key" || h.Expires() != next.Expires || g.used != "new-key" {
		t.Fatalf("after renew: %q until %v, client uses %q", h.Value(), h.Expires(), g.used)
	}
	info, err := os.Stat(filepath.Join(dir, FileName))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("key file: %v, %v", info, err)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".garmin-key-*")); len(left) != 0 {
		t.Fatalf("temporary files left: %v", left)
	}
	// After a restart the renewed key, which expires later, wins.
	again, err := Load(dir, configured)
	if err != nil || again.Value() != "new-key" || again.Expires() != next.Expires {
		t.Fatalf("reload: %q until %v, %v", again.Value(), again.Expires(), err)
	}
}

func TestRenewRefusedChangesNothing(t *testing.T) {
	dir := t.TempDir()
	h, _ := Load(dir, configured)
	for name, g := range map[string]*fakeGarmin{
		"refused":     {ok: map[string]bool{}},
		"unreachable": {err: errors.New("dial")},
	} {
		if err := h.Renew(context.Background(), g, Key{Value: "bad", Expires: date("2027-01-01")}); err == nil {
			t.Fatalf("%s: renew succeeded", name)
		}
		if h.Value() != "configured-key" || h.Expires() != configured.Expires {
			t.Fatalf("%s: key changed to %q", name, h.Value())
		}
		if _, err := os.Stat(filepath.Join(dir, FileName)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s: key file written: %v", name, err)
		}
	}
}

func TestRenewWriteFailureKeepsKey(t *testing.T) {
	h, _ := Load(filepath.Join(t.TempDir(), "missing-dir"), configured)
	g := &fakeGarmin{ok: map[string]bool{"new-key": true}}
	if err := h.Renew(context.Background(), g, Key{Value: "new-key", Expires: date("2027-01-01")}); err == nil {
		t.Fatal("renew succeeded without a data directory")
	}
	if h.Value() != "configured-key" || g.used != "" {
		t.Fatalf("key = %q, client uses %q", h.Value(), g.used)
	}
}

func TestNoKey(t *testing.T) {
	h := NoKey(date("2099-12-31"))
	if h.Value() != "" || h.Expires() != date("2099-12-31") {
		t.Fatalf("key = %q until %v", h.Value(), h.Expires())
	}
	g := &fakeGarmin{ok: map[string]bool{"new-key": true}}
	if err := h.Renew(context.Background(), g, Key{Value: "new-key", Expires: date("2027-01-01")}); err == nil || g.used != "" {
		t.Fatalf("renewed a keyless holder: %v, client uses %q", err, g.used)
	}
}

// The configured key wins when it expires later, or on the same day: the
// runner went back to editing the secret file.
func TestLoadLaterExpiryWins(t *testing.T) {
	for name, tc := range map[string]struct {
		stored string
		want   string
	}{
		"stored later":  {"2027-01-01", "stored-key"},
		"stored sooner": {"2026-11-01", "configured-key"},
		"same day":      {"2026-11-22", "configured-key"},
	} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, FileName), []byte(`{"key":"stored-key","expires":"`+tc.stored+`"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		h, err := Load(dir, configured)
		if err != nil || h.Value() != tc.want {
			t.Errorf("%s: key = %q, %v; want %q", name, h.Value(), err, tc.want)
		}
	}
}

func TestLoadRefusesBadFiles(t *testing.T) {
	for name, tc := range map[string]struct {
		body string
		mode os.FileMode
	}{
		"group readable": {`{"key":"secret-value","expires":"2027-01-01"}`, 0o640},
		"not json":       {`secret-value`, 0o600},
		"no key":         {`{"expires":"2027-01-01"}`, 0o600},
		"no expiry":      {`{"key":"secret-value"}`, 0o600},
	} {
		dir := t.TempDir()
		p := filepath.Join(dir, FileName)
		if err := os.WriteFile(p, []byte(tc.body), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, tc.mode); err != nil {
			t.Fatal(err)
		}
		_, err := Load(dir, configured)
		if err == nil {
			t.Errorf("%s: loaded", name)
			continue
		}
		if strings.Contains(err.Error(), "secret-value") {
			t.Errorf("%s: error quotes the key: %v", name, err)
		}
	}
}
