package pipeline

import "encoding/json"

// unmarshal isolates the json.Unmarshal call so other files in the package
// can rely on a single import point and tests can stub it if needed.
func unmarshal(raw string, dst any) error {
	return json.Unmarshal([]byte(raw), dst)
}
