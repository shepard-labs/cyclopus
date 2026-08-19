package admin

import (
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/shepard-labs/cyclopus/internal/adapters/observability"
)

func TestFormatNodeUptime(t *testing.T) {
	t.Parallel()
	cases := []struct {
		seconds float64
		want    string
	}{
		{0, ""},
		{30, "1 minute"},
		{120, "2 minutes"},
		{3600, "1 hour"},
		{7200, "2 hours"},
		{359999, "99 hours"},
		{360000, "4 days"},
		{604800, "7 days"},
	}
	for _, tc := range cases {
		if got := formatNodeUptime(tc.seconds); got != tc.want {
			t.Errorf("formatNodeUptime(%v) = %q, want %q", tc.seconds, got, tc.want)
		}
	}
}

func TestFormatMemoryGB(t *testing.T) {
	t.Parallel()
	cases := []struct {
		bytes int64
		want  string
	}{
		{0, ""},
		{-5, ""},
		{12240656793, "11.4 GB"},
		{16818663424, "15.7 GB"},
	}
	for _, tc := range cases {
		if got := formatMemoryGB(tc.bytes); got != tc.want {
			t.Errorf("formatMemoryGB(%v) = %q, want %q", tc.bytes, got, tc.want)
		}
	}
}

func TestNodeCPUModelLabel(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		node observability.NodeSystemDetail
		want string
	}{
		{"model wins", observability.NodeSystemDetail{CPUModel: "Intel Core Processor (Haswell, no TSX)", Cores: 4, Threads: 8}, "Intel Core Processor (Haswell, no TSX)"},
		{"cores and threads", observability.NodeSystemDetail{Cores: 4, Threads: 8}, "4 cores / 8 threads"},
		{"equal cores and threads", observability.NodeSystemDetail{Cores: 16, Threads: 16}, "16 cores"},
		{"cores only", observability.NodeSystemDetail{Cores: 2}, "2 cores"},
		{"threads only", observability.NodeSystemDetail{Threads: 2}, "2 threads"},
		{"nothing", observability.NodeSystemDetail{}, ""},
	}
	for _, tc := range cases {
		if got := nodeCPUModelLabel(tc.node); got != tc.want {
			t.Errorf("%s: nodeCPUModelLabel() = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestTitleCase(t *testing.T) {
	t.Parallel()
	if got := titleCase("pending"); got != "Pending" {
		t.Errorf("titleCase = %q, want %q", got, "Pending")
	}
	if got := titleCase(""); got != "" {
		t.Errorf("titleCase(\"\") = %q, want empty", got)
	}
}

func TestPinnedAxisMax(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		values []float64
		want   float64
	}{
		{"empty", nil, 0},
		{"below 1 raw", []float64{0.2, 0.75, 0.5}, 0.75},
		{"round to 0.1 above 1", []float64{1.0, 4.27, 3.1}, 4.3},
		{"round integer just above 10", []float64{10.04}, 10},
		{"round integer above 10", []float64{45.2, 12.5}, 45},
		{"round integer above 10 up", []float64{45.6}, 46},
		{"zero values", []float64{0, 0}, 0},
	}
	for _, tc := range cases {
		if got := pinnedAxisMax(tc.values); math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("%s: pinnedAxisMax(%v) = %v, want %v", tc.name, tc.values, got, tc.want)
		}
	}
}

func TestToFixedFloat(t *testing.T) {
	t.Parallel()
	cases := []struct {
		v      float64
		digits int
		want   float64
	}{
		{13.256, 2, 13.26},
		{13.254, 2, 13.25},
		{13.0, 2, 13},
		{4.0, 2, 4},
		{3.999, 2, 4},
		{3.2, 0, 4}, // digits=0 uses ceil like Beszel
		{0.05, 2, 0.05},
	}
	for _, tc := range cases {
		if got := toFixedFloat(tc.v, tc.digits); got != tc.want {
			t.Errorf("toFixedFloat(%v, %d) = %v, want %v", tc.v, tc.digits, got, tc.want)
		}
	}
}

func TestDecimalString(t *testing.T) {
	t.Parallel()
	cases := []struct {
		v      float64
		digits int
		want   string
	}{
		{13.2, 2, "13.20"},
		{13, 2, "13.00"},
		{4.567, 2, "4.57"},
		{0.5, 2, "0.50"},
		{3.2, 0, "4"},
	}
	for _, tc := range cases {
		if got := decimalString(tc.v, tc.digits); got != tc.want {
			t.Errorf("decimalString(%v, %d) = %q, want %q", tc.v, tc.digits, got, tc.want)
		}
	}
}

func TestNiceTicksDomain(t *testing.T) {
	t.Parallel()
	cases := []struct{ min, max float64 }{
		{0, 100},
		{0, 45},
		{0, 4.3},
		{0, 0.75},
		{0, 13},
	}
	for _, tc := range cases {
		ticks := niceTicks(tc.min, tc.max)
		if len(ticks) < 2 {
			t.Fatalf("niceTicks(%v, %v) returned %v, want at least 2 ticks", tc.min, tc.max, ticks)
		}
		if ticks[0] != tc.min {
			t.Errorf("niceTicks(%v, %v) first tick = %v, want min %v", tc.min, tc.max, ticks[0], tc.min)
		}
		if ticks[len(ticks)-1] != tc.max {
			t.Errorf("niceTicks(%v, %v) last tick = %v, want max %v", tc.min, tc.max, ticks[len(ticks)-1], tc.max)
		}
		for _, tick := range ticks {
			if tick < tc.min || tick > tc.max {
				t.Errorf("niceTicks(%v, %v) tick %v out of domain", tc.min, tc.max, tick)
			}
		}
	}
}

func TestTimeFractionMapping(t *testing.T) {
	t.Parallel()
	tMin := int64(1_000_000)
	tMax := int64(2_000_000)
	if got := timeFraction(tMin, tMin, tMax); got != 0 {
		t.Errorf("timeFraction(min) = %v, want 0", got)
	}
	if got := timeFraction(tMax, tMin, tMax); got != 1 {
		t.Errorf("timeFraction(max) = %v, want 1", got)
	}
	if got := timeFraction(1_500_000, tMin, tMax); got != 0.5 {
		t.Errorf("timeFraction(mid) = %v, want 0.5", got)
	}
	if got := timeFraction(tMin-500, tMin, tMax); got != 0 {
		t.Errorf("timeFraction(before) = %v, want clamped 0", got)
	}
	if got := timeFraction(tMax+500, tMin, tMax); got != 1 {
		t.Errorf("timeFraction(after) = %v, want clamped 1", got)
	}
	if got := timeFraction(42, 42, 42); got != 0.5 {
		t.Errorf("timeFraction(zero span) = %v, want 0.5", got)
	}
}

func TestTimeTicks(t *testing.T) {
	t.Parallel()
	base := time.Date(2025, 6, 15, 0, 0, 0, 0, time.UTC)

	t.Run("24h span with target 12 yields ~12 round-boundary ticks", func(t *testing.T) {
		tMin := base.UnixMilli()
		tMax := base.Add(24 * time.Hour).UnixMilli()
		ticks := timeTicks(tMin, tMax, 12)
		if len(ticks) < 8 || len(ticks) > 13 {
			t.Fatalf("timeTicks(24h, 12) returned %d ticks, want ~12: %+v", len(ticks), ticks)
		}
		for _, tick := range ticks {
			if tick.LeftPct < 0 || tick.LeftPct > 100 {
				t.Errorf("tick LeftPct %v out of [0,100]", tick.LeftPct)
			}
			parsed, err := time.Parse("2006-01-02T15:04:05Z", tick.ISO)
			if err != nil {
				t.Errorf("tick ISO %q not parseable: %v", tick.ISO, err)
			}
			ms := parsed.UnixMilli()
			if ms < tMin || ms > tMax {
				t.Errorf("tick time %q out of span", tick.ISO)
			}
			// 24h/12 = 2h -> 2h nice step; ticks must land on round hours.
			if parsed.Minute() != 0 || parsed.Second() != 0 {
				t.Errorf("tick %q not on a round hour", tick.ISO)
			}
		}
	})

	t.Run("24h target 12 works from a ragged now", func(t *testing.T) {
		// now mid-minute/mid-hour: ticks still anchor to round boundaries and
		// span the full selected window.
		tMax := base.Add(24*time.Hour + 7*time.Minute + 33*time.Second).UnixMilli()
		tMin := tMax - 24*3600*1000
		ticks := timeTicks(tMin, tMax, 12)
		if len(ticks) < 8 || len(ticks) > 13 {
			t.Fatalf("timeTicks(ragged 24h, 12) returned %d ticks, want ~12: %+v", len(ticks), ticks)
		}
		for _, tick := range ticks {
			parsed, _ := time.Parse("2006-01-02T15:04:05Z", tick.ISO)
			if parsed.Minute() != 0 || parsed.Second() != 0 {
				t.Errorf("tick %q not on a round hour", tick.ISO)
			}
		}
	})

	t.Run("1w span with target 7 yields ~7 day-boundary ticks", func(t *testing.T) {
		tMin := base.UnixMilli()
		tMax := base.Add(7 * 24 * time.Hour).UnixMilli()
		ticks := timeTicks(tMin, tMax, 7)
		if len(ticks) < 5 || len(ticks) > 8 {
			t.Fatalf("timeTicks(1w, 7) returned %d ticks, want ~7: %+v", len(ticks), ticks)
		}
		for _, tick := range ticks {
			parsed, _ := time.Parse("2006-01-02T15:04:05Z", tick.ISO)
			if parsed.Hour() != 0 || parsed.Minute() != 0 {
				t.Errorf("tick %q not on a day boundary", tick.ISO)
			}
		}
	})

	t.Run("1h span with target 12 stays in span and ticks ascending", func(t *testing.T) {
		tMin := base.UnixMilli()
		tMax := base.Add(time.Hour).UnixMilli()
		ticks := timeTicks(tMin, tMax, 12)
		if len(ticks) < 6 {
			t.Fatalf("timeTicks(1h, 12) returned %d ticks, want ~12: %+v", len(ticks), ticks)
		}
		prev := -1.0
		for _, tick := range ticks {
			if tick.LeftPct <= prev {
				t.Errorf("ticks not ascending: %v after %v", tick.LeftPct, prev)
			}
			prev = tick.LeftPct
		}
	})

	t.Run("zero span returns no ticks", func(t *testing.T) {
		if ticks := timeTicks(base.UnixMilli(), base.UnixMilli(), 12); len(ticks) != 0 {
			t.Errorf("timeTicks(zero span) = %+v, want empty", ticks)
		}
	})
}

func TestXTickTargetForRange(t *testing.T) {
	t.Parallel()
	cases := []struct {
		rangeID string
		want    int
	}{
		{"1h", 12},
		{"12h", 12},
		{"24h", 12},
		{"1w", 7},
		{"bogus", 12},
		{"", 12},
	}
	for _, tc := range cases {
		if got := xTickTargetForRange(tc.rangeID); got != tc.want {
			t.Errorf("xTickTargetForRange(%q) = %d, want %d", tc.rangeID, got, tc.want)
		}
	}
}

func TestExtractNodeChartData24hTicksSpanSelectedWindow(t *testing.T) {
	// Sparse data (two samples minutes apart) must still yield ~12 X ticks
	// spanning the full 24h selected window: the chart's X axis is pinned to
	// the window even though the data covers only minutes of it.
	hist := []observability.SystemStatPoint{
		{Timestamp: time.Now().Add(-3 * time.Minute), CPU: 10, DiskUsed: 20, DiskTotal: 100},
		{Timestamp: time.Now(), CPU: 12, DiskUsed: 21, DiskTotal: 100},
	}
	node := observability.NodeSystemDetail{History: hist, DiskTotal: 100}
	cpu, _, _, _, _, _, _ := extractNodeChartData(node, "24h")
	if len(cpu.XTicks) < 8 || len(cpu.XTicks) > 13 {
		t.Fatalf("24h CPU chart has %d X ticks, want ~12: %+v", len(cpu.XTicks), cpu.XTicks)
	}
	span := time.UnixMilli(cpu.TimeMax).Sub(time.UnixMilli(cpu.TimeMin))
	if span < 23*time.Hour || span > 25*time.Hour {
		t.Errorf("chart X axis span = %v, want ~24h (selected window, not data extent)", span)
	}
	// Ticks anchor on round boundaries: the first tick is within one tick
	// interval of the window start, and the first/last ticks sit near the
	// window edges.
	first := time.UnixMilli(cpu.TimeMin).Add(time.Duration(cpu.XTicks[0].LeftPct / 100 * float64(span)))
	if windowStart := time.UnixMilli(cpu.TimeMin); first.Sub(windowStart) > 3*time.Hour {
		t.Errorf("first tick %v is >3h after window start %v; ticks do not span the selected window", first, windowStart)
	}
	if cpu.XTicks[0].LeftPct > 15 || cpu.XTicks[len(cpu.XTicks)-1].LeftPct < 85 {
		t.Errorf("ticks %+v...%+v do not cover the window edges", cpu.XTicks[0], cpu.XTicks[len(cpu.XTicks)-1])
	}
}

// parseSmoothPathSegments extracts the numbers from each C segment of a
// smoothPath output: [c1x, c1y, c2x, c2y, x, y] per segment.
func parseSmoothPathSegments(t *testing.T, path string) [][6]float64 {
	t.Helper()
	if !strings.HasPrefix(path, "M ") {
		t.Fatalf("path does not start with M: %q", path)
	}
	var segs [][6]float64
	for _, part := range strings.Split(path, " C ") {
		fields := strings.Fields(strings.ReplaceAll(strings.TrimPrefix(strings.TrimSpace(part), "M "), ",", " "))
		switch len(fields) {
		case 2: // the M move
		case 6: // a C segment
			var seg [6]float64
			for i, f := range fields {
				v, err := strconv.ParseFloat(f, 64)
				if err != nil {
					t.Fatalf("unparseable number %q in path %q", f, path)
				}
				seg[i] = v
			}
			segs = append(segs, seg)
		default:
			t.Fatalf("unexpected segment %q in path %q", part, path)
		}
	}
	return segs
}

func TestSmoothPathMonotoneNoOvershoot(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		points [][2]float64
	}{
		{"spike", [][2]float64{{0, 10}, {50, 90}, {100, 10}, {150, 10}, {200, 60}, {250, 30}}},
		{"flat", [][2]float64{{0, 40}, {100, 40}, {200, 40}}},
		{"two points", [][2]float64{{0, 10}, {300, 70}}},
		{"descending", [][2]float64{{0, 70}, {100, 50}, {200, 60}, {300, 10}}},
		{"constant then rise", [][2]float64{{0, 20}, {50, 20}, {100, 20}, {150, 80}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := smoothPath(tc.points)
			segs := parseSmoothPathSegments(t, path)
			if len(segs) != len(tc.points)-1 {
				t.Fatalf("smoothPath returned %d C segments, want %d (path %q)", len(segs), len(tc.points)-1, path)
			}
			// Sample each Bezier segment densely; the y value must stay within
			// the global sample min/max (no overshoot).
			yMin, yMax := tc.points[0][1], tc.points[0][1]
			for _, p := range tc.points {
				yMin = math.Min(yMin, p[1])
				yMax = math.Max(yMax, p[1])
			}
			prev := tc.points[0]
			for _, seg := range segs {
				p1 := [2]float64{seg[0], seg[1]}
				p2 := [2]float64{seg[2], seg[3]}
				p3 := [2]float64{seg[4], seg[5]}
				for i := 0; i <= 50; i++ {
					u := float64(i) / 50
					v := 1 - u
					y := v*v*v*prev[1] + 3*v*v*u*p1[1] + 3*v*u*u*p2[1] + u*u*u*p3[1]
					if y < yMin-1e-6 || y > yMax+1e-6 {
						t.Fatalf("curve overshoots: y=%v outside [%v,%v] in path %q", y, yMin, yMax, path)
					}
				}
				prev = p3
			}
		})
	}
}

func TestSmoothPathEdgeCases(t *testing.T) {
	t.Parallel()
	if got := smoothPath(nil); got != "" {
		t.Errorf("smoothPath(nil) = %q, want empty", got)
	}
	single := smoothPath([][2]float64{{10, 20}})
	if !strings.HasPrefix(single, "M ") || strings.Contains(single, "C ") {
		t.Errorf("smoothPath(single) = %q, want move-only path", single)
	}
}

func TestBuildSingleSeriesChartTimeScale(t *testing.T) {
	t.Parallel()
	base := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)
	timesMs := []int64{
		base.UnixMilli(),
		base.Add(time.Hour).UnixMilli(),
		base.Add(3 * time.Hour).UnixMilli(), // uneven gap: exercises time scale
	}
	times := []string{
		base.UTC().Format("2006-01-02T15:04:05Z"),
		base.Add(time.Hour).UTC().Format("2006-01-02T15:04:05Z"),
		base.Add(3 * time.Hour).UTC().Format("2006-01-02T15:04:05Z"),
	}
	chart := buildSingleSeriesChart("CPU Usage", "%", "red", "red", times, timesMs, []float64{10, 50, 30})

	if chart.TimeMin != timesMs[0] || chart.TimeMax != timesMs[2] {
		t.Errorf("TimeMin/TimeMax = %d/%d, want %d/%d", chart.TimeMin, chart.TimeMax, timesMs[0], timesMs[2])
	}
	// Data-driven domain: max 50 -> pinned 50, not fixed 100.
	if chart.MaxVal != 50 {
		t.Errorf("MaxVal = %v, want 50 (data-driven, not fixed 100)", chart.MaxVal)
	}
	if chart.MinVal != 0 {
		t.Errorf("MinVal = %v, want 0 floor", chart.MinVal)
	}
	// Line path must position the middle point by time fraction (1/3 of the
	// width), not by index (1/2).
	if !strings.Contains(chart.LinePaths[0], "100.0") {
		t.Errorf("line path missing time-scaled x=100.0 for middle point: %q", chart.LinePaths[0])
	}
	if strings.Contains(chart.LinePaths[0], " L ") {
		t.Errorf("line path still uses straight L segments: %q", chart.LinePaths[0])
	}
	if !strings.Contains(chart.LinePaths[0], "C ") {
		t.Errorf("line path missing monotone C segments: %q", chart.LinePaths[0])
	}
	if len(chart.XTicks) == 0 {
		t.Errorf("expected time-based XTicks, got none")
	}
	if len(chart.YTicks) < 2 || chart.YTicks[len(chart.YTicks)-1] != 50 {
		t.Errorf("YTicks = %v, want ticks ending at domain max 50", chart.YTicks)
	}
}

func TestBuildSingleSeriesChartSynthesizesTimes(t *testing.T) {
	t.Parallel()
	chart := buildSingleSeriesChart("CPU Usage", "%", "red", "red", nil, nil, []float64{1, 2, 3})
	if len(chart.TimesMs) != 3 || len(chart.Times) != 3 {
		t.Fatalf("synth times missing: TimesMs=%v Times=%v", chart.TimesMs, chart.Times)
	}
	if chart.TimesMs[0] >= chart.TimesMs[2] {
		t.Errorf("synth TimesMs not ascending: %v", chart.TimesMs)
	}
}

func TestExtractNodeChartDataDiskUsageAndBandwidth(t *testing.T) {
	t.Parallel()
	hist := []observability.SystemStatPoint{
		{Timestamp: time.Now().Add(-time.Minute), DiskUsed: 30.5, DiskTotal: 100, NetSent: 1.5, NetRecv: 2.5},
		{Timestamp: time.Now(), DiskUsed: 31.0, DiskTotal: 100, NetSent: 2.0, NetRecv: 3.0},
	}
	node := observability.NodeSystemDetail{History: hist, DiskTotal: 100}
	_, _, disk, _, net, _, _ := extractNodeChartData(node, "1h")

	if disk.Title != "Disk Usage" {
		t.Errorf("disk chart title = %q, want %q", disk.Title, "Disk Usage")
	}
	if disk.Unit != "GB" {
		t.Errorf("disk chart unit = %q, want GB", disk.Unit)
	}
	if disk.MinVal != 0 || disk.MaxVal != 100 {
		t.Errorf("disk chart domain = [%v, %v], want [0, 100] (disk total)", disk.MinVal, disk.MaxVal)
	}
	if disk.CurrentVal != "31.0 GB" {
		t.Errorf("disk chart CurrentVal = %q, want %q", disk.CurrentVal, "31.0 GB")
	}

	if net.Title != "Bandwidth" {
		t.Errorf("bandwidth chart title = %q, want %q", net.Title, "Bandwidth")
	}
	if len(net.Series) != 2 || net.Series[0].Name != "Sent" || net.Series[1].Name != "Received" {
		t.Fatalf("bandwidth series = %+v, want [Sent, Received]", net.Series)
	}
	if net.Series[0].Values[0] != 1.5 || net.Series[1].Values[0] != 2.5 {
		t.Errorf("bandwidth values = %v / %v, want sent 1.5 recv 2.5", net.Series[0].Values, net.Series[1].Values)
	}
}

func TestFormatTickValue(t *testing.T) {
	t.Parallel()
	cases := []struct {
		v    float64
		unit string
		want string
	}{
		{13.25, "%", "13.25%"},
		{13.0, "%", "13%"},
		{4.3, "MB/s", "4.3 MB/s"},
		{2.5, "", "2.5"},
	}
	for _, tc := range cases {
		if got := formatTickValue(tc.v, tc.unit); got != tc.want {
			t.Errorf("formatTickValue(%v, %q) = %q, want %q", tc.v, tc.unit, got, tc.want)
		}
	}
}
