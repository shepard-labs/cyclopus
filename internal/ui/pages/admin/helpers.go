package admin

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/shepard-labs/cyclopus/internal/adapters/observability"
)

func selectedNode(nodes []observability.NodeSystemDetail, selectedID string) (observability.NodeSystemDetail, bool) {
	if len(nodes) == 0 {
		return observability.NodeSystemDetail{}, false
	}
	for _, n := range nodes {
		if n.ID == selectedID {
			return n, true
		}
	}
	return nodes[0], true
}

func resolveNodeID(nodes []observability.NodeSystemDetail, selectedID string) string {
	n, ok := selectedNode(nodes, selectedID)
	if !ok {
		return selectedID
	}
	return n.ID
}

// metricsRangeOption pairs a chart time-range ID with its trigger/menu label.
type metricsRangeOption struct {
	ID    string
	Label string
}

// metricsRangeOptions lists the chart time windows in menu order.
func metricsRangeOptions() []metricsRangeOption {
	return []metricsRangeOption{
		{ID: string(observability.MetricsRange1h), Label: "1 hour"},
		{ID: string(observability.MetricsRange12h), Label: "12 hours"},
		{ID: string(observability.MetricsRange24h), Label: "24 hours"},
		{ID: string(observability.MetricsRange1w), Label: "1 week"},
	}
}

// normalizeRangeID maps an arbitrary range string to a valid range ID,
// defaulting to the 24h window.
func normalizeRangeID(rangeID string) string {
	for _, option := range metricsRangeOptions() {
		if option.ID == rangeID {
			return rangeID
		}
	}
	return string(observability.DefaultMetricsTimeRange)
}

// rangeLabel returns the menu label for a range ID, defaulting as
// normalizeRangeID does.
func rangeLabel(rangeID string) string {
	id := normalizeRangeID(rangeID)
	for _, option := range metricsRangeOptions() {
		if option.ID == id {
			return option.Label
		}
	}
	return id
}

// formatNodeUptime matches Beszel's secondsToUptimeString: minutes under an hour,
// hours under 100 hours, otherwise whole days.
func formatNodeUptime(seconds float64) string {
	if seconds < 1 {
		return ""
	}
	switch {
	case seconds < 3600:
		n := int(seconds / 60)
		if n <= 1 {
			return "1 minute"
		}
		return fmt.Sprintf("%d minutes", n)
	case seconds < 360000:
		n := int(seconds / 3600)
		if n == 1 {
			return "1 hour"
		}
		return fmt.Sprintf("%d hours", n)
	default:
		n := int(seconds / 86400)
		if n <= 1 {
			return "1 day"
		}
		return fmt.Sprintf("%d days", n)
	}
}

// formatMemoryGB renders a Beszel system_details memory byte count as a
// human-friendly GB value ("15.7 GB"). Returns "" when unknown.
func formatMemoryGB(bytes int64) string {
	if bytes <= 0 {
		return ""
	}
	return fmt.Sprintf("%.1f GB", float64(bytes)/(1024*1024*1024))
}

// nodeCPUModelLabel builds the CPU segment for the node header: the model
// name when Beszel reports one, otherwise a core/thread count fallback.
func nodeCPUModelLabel(node observability.NodeSystemDetail) string {
	if node.CPUModel != "" {
		return node.CPUModel
	}
	switch {
	case node.Cores > 0 && node.Threads > 0 && node.Threads != node.Cores:
		return fmt.Sprintf("%d cores / %d threads", node.Cores, node.Threads)
	case node.Cores > 0:
		return fmt.Sprintf("%d cores", node.Cores)
	case node.Threads > 0:
		return fmt.Sprintf("%d threads", node.Threads)
	}
	return ""
}

// titleCase upper-cases the first letter of a status string for display.
func titleCase(value string) string {
	if value == "" {
		return ""
	}
	return strings.ToUpper(value[:1]) + value[1:]
}

func avgCPUNodes(nodes []observability.NodeSystemDetail) float64 {
	if len(nodes) == 0 {
		return 0
	}
	var total float64
	for _, n := range nodes {
		total += n.CPUPercent
	}
	return total / float64(len(nodes))
}

func avgMemoryNodes(nodes []observability.NodeSystemDetail) float64 {
	if len(nodes) == 0 {
		return 0
	}
	var total float64
	for _, n := range nodes {
		total += n.MemoryUsed
	}
	return total / float64(len(nodes))
}

func avgDiskNodes(nodes []observability.NodeSystemDetail) float64 {
	if len(nodes) == 0 {
		return 0
	}
	var total float64
	for _, n := range nodes {
		total += n.DiskPercent
	}
	return total / float64(len(nodes))
}

type ChartSeries struct {
	Name   string
	Values []float64
	Color  string
}

type HoverPoint struct {
	Time   string
	Values []float64
	X      float64
	Y      []float64
}

// TimeTick is a time-based X-axis tick: an ISO timestamp and its horizontal
// position as a CSS left percentage of the plot width (true time scale).
type TimeTick struct {
	ISO     string
	LeftPct float64
}

type SVGChartData struct {
	ID          string
	Title       string
	Unit        string
	Series      []ChartSeries
	Times       []string
	TimesMs     []int64 // per-point Unix-ms timestamps (true time scale)
	TimeMin     int64
	TimeMax     int64
	MinVal      float64
	MaxVal      float64
	CurrentVal  string
	LinePaths   []string
	AreaPath    string
	FillColor   string
	StrokeColor string
	DataJSON    string
	YTicks      []float64
	XTicks      []TimeTick
}

// niceTicks returns a human-friendly set of tick values spanning [min, max]
// (inclusive), typically 3-5 ticks. Ticks always include min and max and
// never extend outside the domain.
func niceTicks(min, max float64) []float64 {
	if max <= min {
		return []float64{min}
	}
	raw := (max - min) / 4.0
	mag := math.Pow(10, math.Floor(math.Log10(raw)))
	norm := raw / mag
	var step float64
	switch {
	case norm >= 5:
		step = 5 * mag
	case norm >= 2.5:
		step = 2 * mag
	case norm >= 1.5:
		step = 2 * mag
	default:
		step = mag
	}
	start := math.Ceil(min/step) * step
	var ticks []float64
	for v := start; v <= max+step*1e-9; v += step {
		ticks = append(ticks, v)
	}
	// Ticks must include both endpoints of the domain.
	if len(ticks) == 0 || ticks[0] > min {
		ticks = append([]float64{min}, ticks...)
	}
	if ticks[len(ticks)-1] < max-step*1e-9 {
		ticks = append(ticks, max)
	}
	if len(ticks) < 2 {
		ticks = []float64{min, max}
	}
	return ticks
}

// pinnedAxisMax ports Beszel's pinnedAxisDomain upper-bound rule: round to an
// integer above 10, round to one decimal above 1, otherwise keep the raw max.
func pinnedAxisMax(values []float64) float64 {
	dataMax := 0.0
	for _, v := range values {
		if v > dataMax {
			dataMax = v
		}
	}
	if dataMax > 10 {
		return math.Round(dataMax)
	}
	if dataMax > 1 {
		return math.Round(dataMax/0.1) * 0.1
	}
	return dataMax
}

// toFixedFloat ports Beszel's toFixedFloat: round to at most `digits` decimal
// places, trimming trailing zeros.
func toFixedFloat(v float64, digits int) float64 {
	if digits == 0 {
		return math.Ceil(v)
	}
	p := math.Pow(10, float64(digits))
	return math.Round(v*p) / p
}

// decimalString ports Beszel's decimalString: exactly `digits` decimal places,
// keeping trailing zeros.
func decimalString(v float64, digits int) string {
	if digits == 0 {
		return strconv.FormatFloat(math.Ceil(v), 'f', 0, 64)
	}
	return strconv.FormatFloat(v, 'f', digits, 64)
}

// formatTickValue renders an axis tick label using Beszel's toFixedFloat
// rounding (trim trailing zeros).
func formatTickValue(v float64, unit string) string {
	if unit == "%" {
		return strconv.FormatFloat(toFixedFloat(v, 2), 'f', -1, 64) + "%"
	}
	if unit == "" {
		return strconv.FormatFloat(toFixedFloat(v, 2), 'f', -1, 64)
	}
	return strconv.FormatFloat(toFixedFloat(v, 2), 'f', -1, 64) + " " + unit
}

// timeFraction returns the fraction of the way t is through [tMin, tMax],
// clamped to [0, 1]. Zero-width spans return 0.5.
func timeFraction(t, tMin, tMax int64) float64 {
	if tMax <= tMin {
		return 0.5
	}
	f := float64(t-tMin) / float64(tMax-tMin)
	if f < 0 {
		return 0
	}
	if f > 1 {
		return 1
	}
	return f
}

// timeTickLeftPct maps a Unix-ms timestamp to a CSS left percentage of the
// plot width on the true time scale.
func timeTickLeftPct(t, tMin, tMax int64) float64 {
	return timeFraction(t, tMin, tMax) * 100.0
}

// timeTickSteps are the "nice" tick intervals (in ms), ascending, mirroring
// the round-boundary intervals d3-time chooses from.
var timeTickSteps = []float64{
	60e3, 2 * 60e3, 5 * 60e3, 10 * 60e3, 15 * 60e3, 30 * 60e3,
	3600e3, 2 * 3600e3, 3 * 3600e3, 6 * 3600e3, 12 * 3600e3,
	86400e3, 2 * 86400e3, 7 * 86400e3, 14 * 86400e3, 30 * 86400e3,
}

// timeTickIntervalMs returns a "nice" time interval (in ms) so that the
// [tMin, tMax] span yields roughly targetCount ticks.
func timeTickIntervalMs(tMin, tMax int64, targetCount int) float64 {
	span := float64(tMax - tMin)
	if span <= 0 || targetCount < 1 {
		return 1
	}
	raw := span / float64(targetCount)
	for _, s := range timeTickSteps {
		if s >= raw {
			return s
		}
	}
	return raw
}

// xTickTargetForRange maps a chart time range to the number of X-axis ticks
// Beszel targets for that window (chartTimeData.ticks). Default is 12 (the
// 24h window's target).
func xTickTargetForRange(rangeID string) int {
	switch observability.MetricsTimeRangeID(rangeID) {
	case observability.MetricsRange1w:
		return 7
	default:
		return 12
	}
}

// timeTicks returns ~targetCount time-based X-axis ticks across [tMin, tMax]
// at nice time intervals, anchored on interval boundaries (like d3-time
// timeTicks). Each tick carries its ISO timestamp and x position percentage.
// The span is the SELECTED window (window start -> now), so ticks cover the
// full range even when data is sparse.
func timeTicks(tMin, tMax int64, targetCount int) []TimeTick {
	if tMax <= tMin || targetCount < 1 {
		return nil
	}
	interval := timeTickIntervalMs(tMin, tMax, targetCount)
	// Anchor on interval boundaries so ticks land on round clock times.
	var ticks []TimeTick
	for t := int64(math.Ceil(float64(tMin)/interval) * interval); t <= tMax; t += int64(interval) {
		ticks = append(ticks, TimeTick{
			ISO:     time.UnixMilli(t).UTC().Format("2006-01-02T15:04:05Z"),
			LeftPct: timeTickLeftPct(t, tMin, tMax),
		})
	}
	// Nice steps round the raw interval up, so the count can only fall short
	// of the target; when it lands far short, drop one step finer to get
	// closer to it (d3's timeTicks picks an interval targeting ~N ticks).
	for stepIndex := len(timeTickSteps) - 1; stepIndex > 0; stepIndex-- {
		if timeTickSteps[stepIndex] != interval {
			continue
		}
		finer := timeTickSteps[stepIndex-1]
		finerCount := int(float64(tMax-tMin)/finer) + 1
		if len(ticks) < targetCount*3/4 && finerCount <= targetCount*2 {
			return timeTicks(tMin, tMax, finerCount-1)
		}
		break
	}
	return thinXTicks(ticks)
}

// thinXTicks drops ticks that would render too close together, mirroring
// Recharts' minTickGap: labels are centered (-translate-x-1/2) and need
// roughly 90px of horizontal room each. Tick LeftPct is a percentage of an
// ~1100px plot, so ~8% is the minimum readable spacing; we keep ticks at a
// multiple of 8% so survivors stay on round boundaries.
func thinXTicks(ticks []TimeTick) []TimeTick {
	const minGapPct = 8.0
	if len(ticks) < 2 {
		return ticks
	}
	kept := make([]TimeTick, 0, len(ticks))
	lastLeft := math.Inf(-1)
	for _, tk := range ticks {
		if tk.LeftPct-lastLeft < minGapPct {
			continue
		}
		kept = append(kept, tk)
		lastLeft = tk.LeftPct
	}
	return kept
}

// smoothPath builds an SVG path with a Fritsch–Carlson monotone cubic
// interpolation between points (equivalent to Recharts type="monotoneX").
// The resulting curve never overshoots the sample min/max.
func smoothPath(points [][2]float64) string {
	n := len(points)
	if n == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("M %.1f,%.1f", points[0][0], points[0][1]))
	if n == 1 {
		return b.String()
	}

	// Secant slopes.
	dx := make([]float64, n-1)
	m := make([]float64, n-1)
	for i := 0; i < n-1; i++ {
		dx[i] = points[i+1][0] - points[i][0]
		if dx[i] <= 0 {
			dx[i] = 1e-9 // guard against duplicate/unsorted x
		}
		m[i] = (points[i+1][1] - points[i][1]) / dx[i]
	}

	// Tangents (d3-curve-monotone / Fritsch–Carlson).
	t := make([]float64, n)
	t[0] = m[0]
	t[n-1] = m[n-2]
	for i := 1; i < n-1; i++ {
		if m[i-1]*m[i] <= 0 {
			t[i] = 0
		} else {
			t[i] = (dx[i-1]*m[i] + dx[i]*m[i-1]) / (dx[i-1] + dx[i])
		}
	}

	// Monotone clamping.
	for i := 0; i < n-1; i++ {
		if m[i] == 0 {
			t[i] = 0
			t[i+1] = 0
			continue
		}
		a := t[i] / m[i]
		beta := t[i+1] / m[i]
		s := a*a + beta*beta
		if s > 9 {
			tau := 3 / math.Sqrt(s)
			t[i] = tau * a * m[i]
			t[i+1] = tau * beta * m[i]
		}
	}

	for i := 0; i < n-1; i++ {
		h := dx[i] / 3
		c1x := points[i][0] + h
		c1y := points[i][1] + h*t[i]
		c2x := points[i+1][0] - h
		c2y := points[i+1][1] - h*t[i+1]
		b.WriteString(fmt.Sprintf(" C %.1f,%.1f %.1f,%.1f %.1f,%.1f",
			c1x, c1y, c2x, c2y, points[i+1][0], points[i+1][1]))
	}
	return b.String()
}

// tickTopPct maps a data value to its vertical position within the plot,
// expressed as a CSS top percentage. Mirrors the Go path builder: the SVG
// viewBox is 300x80 with 8 units of padding top and bottom.
func tickTopPct(min, max, v float64) float64 {
	if max <= min {
		return 50
	}
	ratio := (v - min) / (max - min)
	if ratio < 0 {
		ratio = 0
	}
	if ratio > 1 {
		ratio = 1
	}
	y := 80.0 - 8.0 - ratio*64.0
	return y / 80.0 * 100.0
}

// synthTimesMs pads or synthesizes a Unix-ms timestamp array to length n. It
// preserves any provided timestamps and extends evenly spaced points at
// 1-minute intervals when timestamps are missing.
func synthTimesMs(timesMs []int64, n int) []int64 {
	out := make([]int64, n)
	copy(out, timesMs)
	if len(timesMs) < n {
		var start int64
		if len(timesMs) > 0 {
			start = timesMs[0]
		} else {
			start = time.Now().UnixMilli() - int64(n-1)*60_000
		}
		for i := len(timesMs); i < n; i++ {
			out[i] = start + int64(i)*60_000
		}
	}
	return out
}

// synthTimes pads or synthesizes ISO time strings to length n from the ms
// timestamps.
func synthTimes(timesMs []int64, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = time.UnixMilli(timesMs[i]).UTC().Format("2006-01-02T15:04:05Z")
	}
	return out
}

var nonAlphanumeric = regexp.MustCompile(`[^a-zA-Z0-9]+`)

func chartDOMID(title string) string {
	slug := nonAlphanumeric.ReplaceAllString(strings.ToLower(title), "-")
	return strings.Trim(slug, "-")
}

// chartOpts carries per-chart axis options: an explicit X window (the
// selected time range, [start, now] in Unix-ms), the X tick target count,
// and an optional fixed Y domain override (e.g. Disk Usage's [0, total]).
// Zero fields fall back to the data-derived behavior.
type chartOpts struct {
	xMinMs     int64
	xMaxMs     int64
	xTickCount int
	fixedMin   float64
	fixedMax   float64
	fixed      bool
}

func buildSingleSeriesChart(title, unit, strokeColor, fillColor string, times []string, timesMs []int64, values []float64, opts ...chartOpts) SVGChartData {
	if len(values) == 0 {
		return SVGChartData{ID: chartDOMID(title), Title: title, Unit: unit, CurrentVal: "0", StrokeColor: strokeColor, FillColor: fillColor}
	}
	if len(timesMs) < len(values) {
		timesMs = synthTimesMs(timesMs, len(values))
	}
	if len(times) < len(values) {
		times = synthTimes(timesMs, len(values))
	}

	var opt chartOpts
	if len(opts) > 0 {
		opt = opts[0]
	}

	// Data-driven domain pinned at 0 (Beszel pinnedAxisDomain), unless the
	// caller pins an explicit domain (e.g. Disk Usage's [0, total GB]).
	maxV := pinnedAxisMax(values)
	if maxV <= 0 {
		maxV = 1
	}
	minV := 0.0
	if opt.fixed && opt.fixedMax > opt.fixedMin {
		minV, maxV = opt.fixedMin, opt.fixedMax
	}

	// X axis spans the SELECTED window (window start -> now) when supplied,
	// falling back to the data span. Ticks target the range's Beszel count.
	tMin, tMax := timesMs[0], timesMs[len(timesMs)-1]
	if opt.xMaxMs > opt.xMinMs {
		tMin, tMax = opt.xMinMs, opt.xMaxMs
	}
	tickTarget := opt.xTickCount
	if tickTarget < 1 {
		tickTarget = 4
	}

	w := 300.0
	h := 80.0
	padTop := 8.0
	padBottom := 8.0
	effH := h - padTop - padBottom

	points := make([][2]float64, 0, len(values))
	for i, v := range values {
		x := timeFraction(timesMs[i], tMin, tMax) * w
		yRatio := (v - minV) / (maxV - minV)
		if yRatio < 0 {
			yRatio = 0
		}
		if yRatio > 1 {
			yRatio = 1
		}
		y := h - padBottom - (yRatio * effH)
		points = append(points, [2]float64{x, y})
	}

	linePath := smoothPath(points)

	// Area = smooth top edge, then close straight down to the baseline. When
	// the data doesn't reach the left edge of the X window, carry the first
	// value horizontally to x=0 so the fill spans the full axis (like Beszel)
	// instead of leaving an empty gap under the leftmost gridlines.
	var areaPath strings.Builder
	if len(points) > 0 {
		if points[0][0] > 0 {
			if len(points) == 1 {
				// Single point: flat carry across the whole axis at that value.
				areaPath.WriteString(fmt.Sprintf("M 0.0,%.1f L %.1f,%.1f", points[0][1], points[0][0], points[0][1]))
			} else {
				areaPath.WriteString(fmt.Sprintf("M 0.0,%.1f L ", points[0][1]))
				areaPath.WriteString(strings.TrimPrefix(linePath, "M "))
			}
		} else {
			areaPath.WriteString(linePath)
		}
		areaPath.WriteString(fmt.Sprintf(" L %.1f,%.1f", points[len(points)-1][0], h-padBottom))
		areaPath.WriteString(" L 0.0," + fmt.Sprintf("%.1f", h-padBottom) + " Z")
	}

	curr := values[len(values)-1]
	currStr := fmt.Sprintf("%.1f %s", curr, unit)
	if unit == "%" {
		currStr = fmt.Sprintf("%.1f%%", curr)
	}

	payload := map[string]any{
		"times":   times,
		"timesMs": timesMs,
		"unit":    unit,
		"series":  []map[string]any{{"name": title, "color": strokeColor, "values": values}},
	}
	dataBytes, _ := json.Marshal(payload)

	return SVGChartData{
		ID:          chartDOMID(title),
		Title:       title,
		Unit:        unit,
		MinVal:      minV,
		MaxVal:      maxV,
		CurrentVal:  currStr,
		LinePaths:   []string{linePath},
		AreaPath:    areaPath.String(),
		Times:       times,
		TimesMs:     timesMs,
		TimeMin:     tMin,
		TimeMax:     tMax,
		StrokeColor: strokeColor,
		FillColor:   fillColor,
		DataJSON:    string(dataBytes),
		YTicks:      niceTicks(minV, maxV),
		XTicks:      timeTicks(tMin, tMax, tickTarget),
	}
}

func buildMultiSeriesChart(title, unit string, seriesList []ChartSeries, times []string, timesMs []int64, opts ...chartOpts) SVGChartData {
	if len(seriesList) == 0 || len(seriesList[0].Values) == 0 {
		return SVGChartData{ID: chartDOMID(title), Title: title, Unit: unit, CurrentVal: "-"}
	}
	n := len(seriesList[0].Values)
	if len(timesMs) < n {
		timesMs = synthTimesMs(timesMs, n)
	}
	if len(times) < n {
		times = synthTimes(timesMs, n)
	}

	var opt chartOpts
	if len(opts) > 0 {
		opt = opts[0]
	}

	// Data-driven domain pinned at 0 (Beszel pinnedAxisDomain).
	var all []float64
	for _, s := range seriesList {
		all = append(all, s.Values...)
	}
	minV := 0.0
	maxV := pinnedAxisMax(all)
	if maxV <= minV {
		maxV = minV + 1.0
	}

	// X axis spans the SELECTED window (window start -> now) when supplied,
	// falling back to the data span. Ticks target the range's Beszel count.
	tMin, tMax := timesMs[0], timesMs[len(timesMs)-1]
	if opt.xMaxMs > opt.xMinMs {
		tMin, tMax = opt.xMinMs, opt.xMaxMs
	}
	tickTarget := opt.xTickCount
	if tickTarget < 1 {
		tickTarget = 4
	}

	w := 300.0
	h := 80.0
	padTop := 8.0
	padBottom := 8.0
	effH := h - padTop - padBottom

	var paths []string
	for _, s := range seriesList {
		points := make([][2]float64, 0, len(s.Values))
		for i, v := range s.Values {
			x := timeFraction(timesMs[i], tMin, tMax) * w
			yRatio := (v - minV) / (maxV - minV)
			if yRatio < 0 {
				yRatio = 0
			}
			if yRatio > 1 {
				yRatio = 1
			}
			y := h - padBottom - (yRatio * effH)
			points = append(points, [2]float64{x, y})
		}
		paths = append(paths, smoothPath(points))
	}

	lastVals := make([]string, 0, len(seriesList))
	seriesPayload := make([]map[string]any, 0, len(seriesList))
	for _, s := range seriesList {
		last := s.Values[len(s.Values)-1]
		lastVals = append(lastVals, fmt.Sprintf("%.2f", last))
		seriesPayload = append(seriesPayload, map[string]any{
			"name":   s.Name,
			"color":  s.Color,
			"values": s.Values,
		})
	}

	payload := map[string]any{
		"times":   times,
		"timesMs": timesMs,
		"unit":    unit,
		"series":  seriesPayload,
	}
	dataBytes, _ := json.Marshal(payload)

	return SVGChartData{
		ID:         chartDOMID(title),
		Title:      title,
		Unit:       unit,
		MinVal:     minV,
		MaxVal:     maxV,
		CurrentVal: strings.Join(lastVals, " / "),
		Series:     seriesList,
		LinePaths:  paths,
		Times:      times,
		TimesMs:    timesMs,
		TimeMin:    tMin,
		TimeMax:    tMax,
		DataJSON:   string(dataBytes),
		YTicks:     niceTicks(0, maxV),
		XTicks:     timeTicks(tMin, tMax, tickTarget),
	}
}

// extractNodeChartData builds the seven node charts. rangeID selects the
// Beszel-matching X tick count and window ([start, now]) that every chart's
// X axis spans, so ticks cover the full selected window even when data is
// sparse within it.
func extractNodeChartData(node observability.NodeSystemDetail, rangeID string) (cpu SVGChartData, mem SVGChartData, disk SVGChartData, diskIO SVGChartData, net SVGChartData, load SVGChartData, temp SVGChartData) {
	hist := node.History
	if len(hist) == 0 {
		// Fallback single point from live values
		now := time.Now()
		hist = []observability.SystemStatPoint{
			{
				Timestamp:   now,
				CPU:         node.CPUPercent,
				Memory:      node.MemoryUsed,
				Disk:        node.DiskPercent,
				DiskUsed:    node.DiskUsed,
				DiskTotal:   node.DiskTotal,
				Bandwidth:   node.Bandwidth,
				Load1:       node.Load1,
				Load5:       node.Load5,
				Load15:      node.Load15,
				Temperature: node.Temperature,
			},
		}
	}

	// The X axis spans the SELECTED window, not the data's own extent.
	opt := chartOpts{xTickCount: xTickTargetForRange(rangeID)}
	if window, err := observability.MetricsTimeRange(observability.MetricsTimeRangeID(normalizeRangeID(rangeID)), time.Now()); err == nil {
		opt.xMinMs = window.Start.UnixMilli()
		opt.xMaxMs = window.End.UnixMilli()
	}

	times := make([]string, len(hist))
	timesMs := make([]int64, len(hist))
	cpuVals := make([]float64, len(hist))
	memVals := make([]float64, len(hist))
	diskUsedVals := make([]float64, len(hist))
	diskRVals := make([]float64, len(hist))
	diskWVals := make([]float64, len(hist))
	sentVals := make([]float64, len(hist))
	recvVals := make([]float64, len(hist))
	l1Vals := make([]float64, len(hist))
	l5Vals := make([]float64, len(hist))
	l15Vals := make([]float64, len(hist))
	tempVals := make([]float64, len(hist))

	memUnit, memDiv := "MB", 1.0
	memMax := 0.0
	for _, pt := range hist {
		if pt.MemoryMB > memMax {
			memMax = pt.MemoryMB
		}
	}
	if memMax >= 1024 {
		memUnit, memDiv = "GB", 1024.0
	}

	for i, pt := range hist {
		times[i] = pt.Timestamp.UTC().Format("2006-01-02T15:04:05Z")
		timesMs[i] = pt.Timestamp.UTC().UnixMilli()
		cpuVals[i] = pt.CPU
		memVals[i] = pt.MemoryMB / memDiv
		diskUsedVals[i] = pt.DiskUsed
		diskRVals[i] = pt.DiskRead
		diskWVals[i] = pt.DiskWrite
		sentVals[i] = pt.NetSent
		recvVals[i] = pt.NetRecv
		l1Vals[i] = pt.Load1
		l5Vals[i] = pt.Load5
		l15Vals[i] = pt.Load15
		tempVals[i] = pt.Temperature
	}

	cpu = buildSingleSeriesChart("CPU Usage", "%", "var(--tw-color-primary)", "var(--tw-color-primary)", times, timesMs, cpuVals, opt)
	mem = buildSingleSeriesChart("Memory Usage", memUnit, "var(--tw-color-info)", "var(--tw-color-info)", times, timesMs, memVals, opt)
	// Disk Usage plots absolute used GB against the disk's total (Beszel
	// DiskUsageChart); fall back to the last sample's total when the live
	// info is missing it.
	diskTotal := node.DiskTotal
	if diskTotal <= 0 {
		diskTotal = hist[len(hist)-1].DiskTotal
	}
	diskOpt := opt
	if diskTotal > 0 {
		diskOpt.fixed = true
		diskOpt.fixedMin = 0
		diskOpt.fixedMax = diskTotal
	}
	disk = buildSingleSeriesChart("Disk Usage", "GB", "var(--tw-color-accent)", "var(--tw-color-accent)", times, timesMs, diskUsedVals, diskOpt)
	diskIO = buildMultiSeriesChart("Disk I/O", "MB/s", []ChartSeries{
		{Name: "Read", Values: diskRVals, Color: "var(--tw-color-success)"},
		{Name: "Write", Values: diskWVals, Color: "var(--tw-color-warning)"},
	}, times, timesMs, opt)
	net = buildMultiSeriesChart("Bandwidth", "MB/s", []ChartSeries{
		{Name: "Sent", Values: sentVals, Color: "var(--tw-color-primary)"},
		{Name: "Received", Values: recvVals, Color: "var(--tw-color-info)"},
	}, times, timesMs, opt)
	load = buildMultiSeriesChart("Load Average", "", []ChartSeries{
		{Name: "1m", Values: l1Vals, Color: "var(--tw-color-error)"},
		{Name: "5m", Values: l5Vals, Color: "var(--tw-color-warning)"},
		{Name: "15m", Values: l15Vals, Color: "var(--tw-color-accent)"},
	}, times, timesMs, opt)
	temp = buildSingleSeriesChart("Core Temperature", "°C", "var(--tw-color-error)", "var(--tw-color-error)", times, timesMs, tempVals, opt)
	return
}

func relTimeAdmin(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// findingCount extracts the findings count from a reconciliation run's report
// JSON. The admin table renders only the count, so the finding detail is not
// decoded further.
func findingCount(report json.RawMessage) int {
	if len(report) == 0 || string(report) == "null" {
		return 0
	}
	var value struct {
		Findings []json.RawMessage `json:"findings"`
	}
	if err := json.Unmarshal(report, &value); err != nil {
		return 0
	}
	return len(value.Findings)
}
