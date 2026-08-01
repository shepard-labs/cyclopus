package admin

import (
	"encoding/json"
	"fmt"
	"time"
)

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
