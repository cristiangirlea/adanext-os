package api

import (
	"context"
	"encoding/json"
	"net/http"
)

type Experience struct {
	Version     int                    `json:"version"`
	Enabled     bool                   `json:"enabled"`
	Level       int                    `json:"level"`
	XP          int                    `json:"xp"`
	WeekStart   string                 `json:"week_start"`
	Week        *ExperienceWeek        `json:"week"`
	Streak      *int                   `json:"streak"`
	Days        []bool                 `json:"days"`
	Badges      []string               `json:"badges"`
	Exercise    *ExperienceExercise    `json:"exercise"`
	Familiarity *ExperienceFamiliarity `json:"familiarity"`
}

func (e Experience) MarshalJSON() ([]byte, error) {
	if !e.Enabled {
		return json.Marshal(struct {
			Version int  `json:"version"`
			Enabled bool `json:"enabled"`
		}{e.Version, false})
	}
	type wire Experience
	return json.Marshal(wire(e))
}

type ExperienceWeek struct {
	Delivery int `json:"delivery"`
	Practice int `json:"practice"`
}
type ExperienceExercise struct {
	Kind     string  `json:"kind"`
	Source   string  `json:"source"`
	Material *string `json:"material,omitempty"`
	Answered bool    `json:"answered"`
}
type ExperienceArea struct {
	Repo  string `json:"repo"`
	Area  string `json:"area"`
	Count int    `json:"count"`
}
type ExperienceFamiliarity struct {
	Known  []ExperienceArea `json:"known"`
	Unseen []ExperienceArea `json:"unseen"`
}
type ExperienceReader interface {
	Experience(context.Context) (Experience, error)
}

func (h *handler) experience(w http.ResponseWriter, r *http.Request) {
	user, ok := h.actor(w, r)
	if !ok {
		return
	}
	exists, err := h.store.UserExists(r.Context(), user)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if !exists {
		writeError(w, 403, "forbidden")
		return
	}
	if h.exec.Experience == nil {
		writeError(w, 503, "engine_unavailable")
		return
	}
	value, err := h.exec.Experience.Experience(r.Context())
	if err != nil {
		writeError(w, 502, "experience_unavailable")
		return
	}
	writeJSON(w, 200, value)
}
