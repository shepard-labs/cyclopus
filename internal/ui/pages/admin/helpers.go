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

type SVGChartData struct {
	ID          string
	Title       string
	Unit        string
	Series      []ChartSeries
	Times       []string
	MinVal      float64
	MaxVal      float64
	CurrentVal  string
	LinePaths   []string
	AreaPath    string
	FillColor   string
	StrokeColor string
	DataJSON    string
	YTicks      []float64
	XTicks      []int // indices into Times
}

// niceTicks returns a human-friendly set of tick values spanning [min, max]
// (inclusive), typically 3-5 ticks.
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
	if len(ticks) < 2 {
		ticks = []float64{min, max}
	}
	return ticks
}

// timeTickIndices picks up to 4 evenly spaced sample indices for the X axis.
func timeTickIndices(n int) []int {
	switch {
	case n <= 1:
		return []int{0}
	case n == 2:
		return []int{0, 1}
	case n == 3:
		return []int{0, 1, 2}
	default:
		return []int{0, n / 3, (2 * n) / 3, n - 1}
	}
}

func formatTickValue(v float64, unit string) string {
	if unit == "%" {
		return strconv.FormatFloat(math.Round(v), 'f', -1, 64) + "%"
	}
	return strconv.FormatFloat(math.Round(v*10)/10, 'f', -1, 64) + " " + unit
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

// xTickLeftPct maps a sample index to its horizontal position as a CSS left
// percentage of the plot width.
func xTickLeftPct(idx, n int) float64 {
	if n <= 1 {
		return 50
	}
	return float64(idx) / float64(n-1) * 100.0
}

var nonAlphanumeric = regexp.MustCompile(`[^a-zA-Z0-9]+`)

func chartDOMID(title string) string {
	slug := nonAlphanumeric.ReplaceAllString(strings.ToLower(title), "-")
	return strings.Trim(slug, "-")
}

func buildSingleSeriesChart(title, unit, strokeColor, fillColor string, times []string, values []float64, fixedMax float64) SVGChartData {
	if len(values) == 0 {
		return SVGChartData{ID: chartDOMID(title), Title: title, Unit: unit, CurrentVal: "0", StrokeColor: strokeColor, FillColor: fillColor}
	}
	minV := values[0]
	maxV := values[0]
	for _, v := range values {
		if v < minV {
			minV = v
		}
		if v > maxV {
			maxV = v
		}
	}
	if fixedMax > 0 {
		maxV = fixedMax
		minV = 0
	} else {
		if minV > 0 {
			minV = 0
		}
		if maxV == minV {
			maxV = minV + 1
		}
	}

	w := 300.0
	h := 80.0
	padTop := 8.0
	padBottom := 8.0
	effH := h - padTop - padBottom

	var points [][2]float64
	n := len(values)
	for i, v := range values {
		var x float64
		if n == 1 {
			x = w / 2.0
		} else {
			x = (float64(i) / float64(n-1)) * w
		}
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

	var linePath strings.Builder
	for i, pt := range points {
		if i == 0 {
			linePath.WriteString(fmt.Sprintf("M %.1f,%.1f", pt[0], pt[1]))
		} else {
			linePath.WriteString(fmt.Sprintf(" L %.1f,%.1f", pt[0], pt[1]))
		}
	}

	var areaPath strings.Builder
	if len(points) > 0 {
		areaPath.WriteString(fmt.Sprintf("M %.1f,%.1f", points[0][0], h-padBottom))
		for _, pt := range points {
			areaPath.WriteString(fmt.Sprintf(" L %.1f,%.1f", pt[0], pt[1]))
		}
		areaPath.WriteString(fmt.Sprintf(" L %.1f,%.1f Z", points[len(points)-1][0], h-padBottom))
	}

	curr := values[len(values)-1]
	currStr := fmt.Sprintf("%.1f %s", curr, unit)
	if unit == "%" {
		currStr = fmt.Sprintf("%.1f%%", curr)
	}

	payload := map[string]any{
		"times":  times,
		"unit":   unit,
		"series": []map[string]any{{"name": title, "color": strokeColor, "values": values}},
	}
	dataBytes, _ := json.Marshal(payload)

	return SVGChartData{
		ID:          chartDOMID(title),
		Title:       title,
		Unit:        unit,
		MinVal:      minV,
		MaxVal:      maxV,
		CurrentVal:  currStr,
		LinePaths:   []string{linePath.String()},
		AreaPath:    areaPath.String(),
		Times:       times,
		StrokeColor: strokeColor,
		FillColor:   fillColor,
		DataJSON:    string(dataBytes),
		YTicks:      niceTicks(minV, maxV),
		XTicks:      timeTickIndices(len(times)),
	}
}

func buildMultiSeriesChart(title, unit string, seriesList []ChartSeries, times []string, fixedMax float64) SVGChartData {
	if len(seriesList) == 0 || len(seriesList[0].Values) == 0 {
		return SVGChartData{ID: chartDOMID(title), Title: title, Unit: unit, CurrentVal: "-"}
	}
	minV := 0.0
	maxV := 0.0
	for _, s := range seriesList {
		for _, v := range s.Values {
			if v > maxV {
				maxV = v
			}
		}
	}
	if fixedMax > 0 {
		maxV = fixedMax
	} else if maxV <= minV {
		maxV = minV + 1.0
	}

	w := 300.0
	h := 80.0
	padTop := 8.0
	padBottom := 8.0
	effH := h - padTop - padBottom

	var paths []string
	for _, s := range seriesList {
		n := len(s.Values)
		var linePath strings.Builder
		for i, v := range s.Values {
			var x float64
			if n == 1 {
				x = w / 2.0
			} else {
				x = (float64(i) / float64(n-1)) * w
			}
			yRatio := (v - minV) / (maxV - minV)
			if yRatio < 0 {
				yRatio = 0
			}
			if yRatio > 1 {
				yRatio = 1
			}
			y := h - padBottom - (yRatio * effH)
			if i == 0 {
				linePath.WriteString(fmt.Sprintf("M %.1f,%.1f", x, y))
			} else {
				linePath.WriteString(fmt.Sprintf(" L %.1f,%.1f", x, y))
			}
		}
		paths = append(paths, linePath.String())
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
		"times":  times,
		"unit":   unit,
		"series": seriesPayload,
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
		DataJSON:   string(dataBytes),
		YTicks:     niceTicks(minV, maxV),
		XTicks:     timeTickIndices(len(times)),
	}
}

func extractNodeChartData(node observability.NodeSystemDetail) (cpu SVGChartData, mem SVGChartData, disk SVGChartData, diskIO SVGChartData, net SVGChartData, load SVGChartData, temp SVGChartData) {
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
				Bandwidth:   node.Bandwidth,
				Load1:       node.Load1,
				Load5:       node.Load5,
				Load15:      node.Load15,
				Temperature: node.Temperature,
			},
		}
	}

	times := make([]string, len(hist))
	cpuVals := make([]float64, len(hist))
	memVals := make([]float64, len(hist))
	diskVals := make([]float64, len(hist))
	diskRVals := make([]float64, len(hist))
	diskWVals := make([]float64, len(hist))
	bwVals := make([]float64, len(hist))
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
		cpuVals[i] = pt.CPU
		memVals[i] = pt.MemoryMB / memDiv
		diskVals[i] = pt.Disk
		diskRVals[i] = pt.DiskRead
		diskWVals[i] = pt.DiskWrite
		bwVals[i] = pt.Bandwidth
		l1Vals[i] = pt.Load1
		l5Vals[i] = pt.Load5
		l15Vals[i] = pt.Load15
		tempVals[i] = pt.Temperature
	}

	cpu = buildSingleSeriesChart("CPU Usage", "%", "var(--tw-color-primary)", "var(--tw-color-primary)", times, cpuVals, 100)
	mem = buildSingleSeriesChart("Memory Usage", memUnit, "var(--tw-color-info)", "var(--tw-color-info)", times, memVals, 0)
	disk = buildSingleSeriesChart("Disk Space", "%", "var(--tw-color-accent)", "var(--tw-color-accent)", times, diskVals, 100)
	diskIO = buildMultiSeriesChart("Disk I/O", "MB/s", []ChartSeries{
		{Name: "Read", Values: diskRVals, Color: "var(--tw-color-success)"},
		{Name: "Write", Values: diskWVals, Color: "var(--tw-color-warning)"},
	}, times, 0)
	net = buildSingleSeriesChart("Bandwidth Throughput", "MB/s", "var(--tw-color-primary)", "var(--tw-color-primary)", times, bwVals, 0)
	load = buildMultiSeriesChart("Load Average", "", []ChartSeries{
		{Name: "1m", Values: l1Vals, Color: "var(--tw-color-error)"},
		{Name: "5m", Values: l5Vals, Color: "var(--tw-color-warning)"},
		{Name: "15m", Values: l15Vals, Color: "var(--tw-color-accent)"},
	}, times, 0)
	temp = buildSingleSeriesChart("Core Temperature", "°C", "var(--tw-color-error)", "var(--tw-color-error)", times, tempVals, 100)
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
