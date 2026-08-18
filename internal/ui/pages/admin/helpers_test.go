package admin

import "testing"

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
