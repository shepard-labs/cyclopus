package admin

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/shepard-labs/cyclopus/internal/adapters/observability"
)

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

	for i, pt := range hist {
		times[i] = pt.Timestamp.UTC().Format("2006-01-02T15:04:05Z")
		cpuVals[i] = pt.CPU
		memVals[i] = pt.Memory
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
	mem = buildSingleSeriesChart("Memory Usage", "%", "var(--tw-color-info)", "var(--tw-color-info)", times, memVals, 100)
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
