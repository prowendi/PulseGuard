package web

import (
	_ "embed"
	"encoding/json"
	"sync"
)

// templateDemo is a single preset shown on the Templates page Gallery.
// The body is plain text — the front-end pastes it verbatim into the
// editor when the user clicks "复制到编辑器".
type templateDemo struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description"`
	ParseMode   string `json:"parse_mode"`
	Body        string `json:"body"`
}

//go:embed template_demos.json
var templateDemosJSON []byte

var (
	templateDemosOnce sync.Once
	templateDemosList []templateDemo
)

// templateDemos returns the static preset list used by both the page
// renderer and the in-page JS that copies a preset into the editor.
// Parse failures degenerate to an empty slice so the UI still loads.
func templateDemos() []templateDemo {
	templateDemosOnce.Do(func() {
		var out []templateDemo
		if err := json.Unmarshal(templateDemosJSON, &out); err == nil {
			templateDemosList = out
		}
	})
	return templateDemosList
}
