package fitdata

import (
	"bytes"
	"math"
	"runtime"
	"testing"
	"time"

	"github.com/stefiix92/coachd/internal/fitdata/fittest"
)

var start = time.Date(2026, 10, 20, 6, 0, 0, 0, time.UTC)

func build(t *testing.T, laps, records int, hr uint8, speed float64) []byte {
	t.Helper()
	b, err := fittest.Build(fittest.Options{Start: start, Laps: laps, Records: records, HR: hr, SpeedMS: speed})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestDecodeLapsAndPoints(t *testing.T) {
	a, err := Decode(bytes.NewReader(build(t, 2, 120, 140, 3.0)))
	laps, points := a.Laps, a.Points
	if err != nil {
		t.Fatal(err)
	}
	if len(laps) != 2 || len(points) != 120 {
		t.Fatalf("laps=%d points=%d", len(laps), len(points))
	}
	if p := points[0].PaceSPerKm; p == nil || math.Abs(*p-333.3) > 0.1 {
		t.Fatalf("pace = %v", p)
	}
	if h := points[0].HRBpm; h == nil || *h != 140 {
		t.Fatalf("hr = %v", h)
	}
	if points[5].OffsetS != 5 {
		t.Fatalf("offset = %v", points[5].OffsetS)
	}
	if d := points[2].DistanceKm; d == nil || math.Abs(*d-0.006) > 1e-9 {
		t.Fatalf("distance = %v", d)
	}
	if a := points[0].AltitudeM; a == nil || math.Abs(*a-200) > 0.01 {
		t.Fatalf("altitude = %v", a)
	}
	l := laps[1]
	if l.Index != 1 || l.StartOffsetS != 60 || l.DurationS != 60 || math.Abs(l.DistanceKm-0.18) > 1e-9 {
		t.Fatalf("lap = %+v", l)
	}
	if l.AvgHRBpm == nil || *l.AvgHRBpm != 140 || l.MaxHRBpm == nil || *l.MaxHRBpm != 140 {
		t.Fatalf("lap hr = %+v", l)
	}
	if l.AvgPaceSPerKm == nil || math.Abs(*l.AvgPaceSPerKm-333.3) > 0.1 {
		t.Fatalf("lap pace = %v", l.AvgPaceSPerKm)
	}
	if l.ElevGainM == nil || *l.ElevGainM != 5 {
		t.Fatalf("lap elev = %v", l.ElevGainM)
	}
}

func TestDecodeInvalidHRIsNil(t *testing.T) {
	a, err := Decode(bytes.NewReader(build(t, 1, 10, 0xFF, 3.0)))
	laps, points := a.Laps, a.Points
	if err != nil {
		t.Fatal(err)
	}
	if points[0].HRBpm != nil || laps[0].AvgHRBpm != nil || laps[0].MaxHRBpm != nil {
		t.Fatal("invalid HR must decode to nil")
	}
}

func TestDecodeSlowSpeedNoPace(t *testing.T) {
	a, err := Decode(bytes.NewReader(build(t, 1, 10, 120, 0.2)))
	laps, points := a.Laps, a.Points
	if err != nil {
		t.Fatal(err)
	}
	if points[0].PaceSPerKm != nil || laps[0].AvgPaceSPerKm != nil {
		t.Fatal("speed below 0.3 m/s must give nil pace")
	}
}

func TestDecodeGarbageErrors(t *testing.T) {
	if _, err := Decode(bytes.NewReader([]byte("definitely not a fit file"))); err == nil {
		t.Fatal("want error")
	}
}

func TestDownsampleLongActivity(t *testing.T) {
	ps := make([]Point, 36000)
	for i := range ps {
		ps[i].OffsetS = float64(i)
	}
	out := Downsample(ps, 600)
	if len(out) != 600 {
		t.Fatalf("len = %d", len(out))
	}
	if out[0].OffsetS != 0 || out[599] != ps[35999] {
		t.Fatalf("ends wrong: %v %v", out[0], out[599])
	}
	for i := 1; i < len(out); i++ {
		if out[i].OffsetS <= out[i-1].OffsetS {
			t.Fatal("not ordered")
		}
	}
}

func TestDownsampleShortUnchanged(t *testing.T) {
	ps := []Point{{OffsetS: 1}, {OffsetS: 2}}
	if out := Downsample(ps, 600); len(out) != 2 {
		t.Fatal("short input changed")
	}
	if out := Downsample(ps, 0); len(out) != 2 {
		t.Fatal("max<=0 must return input")
	}
}

// BenchmarkDecode36k decodes a 10-hour, 1 Hz activity (36,000 records).
func BenchmarkDecode36k(b *testing.B) {
	data, err := fittest.Build(fittest.Options{Start: start, Laps: 36, Records: 36000, HR: 150, SpeedMS: 3})
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		a, err := Decode(bytes.NewReader(data))
		laps, points := a.Laps, a.Points
		if err != nil || len(laps) != 36 || len(points) != 36000 {
			b.Fatalf("laps=%d points=%d err=%v", len(laps), len(points), err)
		}
	}
}

// TestDecodeLongActivityStreams pins streaming decode: a 36,000-record file
// must not keep every decoded message (the old decode-all path allocated
// ~32 MiB here; streaming allocates ~8 MiB, mostly the returned points).
func TestDecodeLongActivityStreams(t *testing.T) {
	data := build(t, 36, 36000, 150, 3.0)
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	a, err := Decode(bytes.NewReader(data))
	laps, points := a.Laps, a.Points
	runtime.ReadMemStats(&after)
	if err != nil || len(laps) != 36 || len(points) != 36000 {
		t.Fatalf("laps=%d points=%d err=%v", len(laps), len(points), err)
	}
	// 14 MiB: Point grew by the power pointer; the race detector adds more.
	const maxBytes = 14 << 20
	if got := after.TotalAlloc - before.TotalAlloc; got > maxBytes {
		t.Fatalf("Decode allocated %.1f MiB, want <= %d MiB", float64(got)/(1<<20), maxBytes>>20)
	}
}

func TestDecodePerceived(t *testing.T) {
	u8 := func(v uint8) *uint8 { return &v }
	for name, tc := range map[string]struct {
		feel, rpe *uint8
		wantFeel  *int
		wantRPE   *float64
	}{
		"both":         {u8(75), u8(60), ptr(75), ptr(6.0)},
		"weak, max":    {u8(0), u8(100), ptr(0), ptr(10.0)},
		"not asked":    {nil, nil, nil, nil},
		"out of range": {u8(101), u8(0xFF), nil, nil},
	} {
		b, err := fittest.Build(fittest.Options{Start: start, Laps: 1, Records: 10, HR: 140, SpeedMS: 3, Feel: tc.feel, RPE: tc.rpe})
		if err != nil {
			t.Fatal(err)
		}
		a, err := Decode(bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		p := a.Perceived
		if (p.FeelPct == nil) != (tc.wantFeel == nil) || p.FeelPct != nil && *p.FeelPct != *tc.wantFeel ||
			(p.RPE == nil) != (tc.wantRPE == nil) || p.RPE != nil && *p.RPE != *tc.wantRPE {
			t.Errorf("%s: perceived = %v %v", name, p.FeelPct, p.RPE)
		}
	}
}

func TestDecodePower(t *testing.T) {
	watts := []uint16{180, 220}
	b, err := fittest.Build(fittest.Options{Start: start, Laps: 1, Records: 2, HR: 140, SpeedMS: 3,
		PowerAt: func(i int) uint16 { return watts[i] }, LapPower: func(int) uint16 { return 200 }})
	if err != nil {
		t.Fatal(err)
	}
	a, err := Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Points) != 2 || a.Points[0].PowerW == nil || *a.Points[0].PowerW != 180 ||
		a.Points[1].PowerW == nil || *a.Points[1].PowerW != 220 {
		t.Errorf("points power = %v %v", a.Points[0].PowerW, a.Points[1].PowerW)
	}
	if len(a.Laps) != 1 || a.Laps[0].AvgPowerW == nil || *a.Laps[0].AvgPowerW != 200 {
		t.Errorf("lap power = %v", a.Laps[0].AvgPowerW)
	}
}

func TestDecodeNoPower(t *testing.T) {
	a, err := Decode(bytes.NewReader(build(t, 1, 5, 140, 3)))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range a.Points {
		if p.PowerW != nil {
			t.Fatalf("point power = %d, want nil", *p.PowerW)
		}
	}
	if a.Laps[0].AvgPowerW != nil {
		t.Errorf("lap power = %d, want nil", *a.Laps[0].AvgPowerW)
	}
}
