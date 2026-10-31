package api

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type fakeExperience struct {
	value Experience
	err   error
}

func (f fakeExperience) Experience(context.Context) (Experience, error) { return f.value, f.err }
func TestExperienceRoute(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reader ExperienceReader
		status int
		want   string
	}{
		{"missing", nil, 503, `"error":"engine_unavailable"`},
		{"failure", fakeExperience{err: errors.New("failed")}, 502, `"error":"experience_unavailable"`},
		{"off", fakeExperience{value: Experience{Version: 1}}, 200, `{"version":1,"enabled":false}`},
		{"on", fakeExperience{value: Experience{Version: 1, Enabled: true, Week: &ExperienceWeek{}, Days: make([]bool, 35), Badges: []string{}}}, 200, `"streak":null`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, Execution{Experience: tc.reader})
			status, body := f.request(t, "GET", "/v1/experience", "", "", nil)
			if status != tc.status || !strings.Contains(string(body), tc.want) {
				t.Fatalf("%d %s", status, body)
			}
		})
	}
}
