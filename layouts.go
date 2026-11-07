package main

// Named layout profiles.
//
// "Default" is layout.json (the original single layout, so existing setups
// keep working). Any other profile is stored as layouts/<name>.json.
//
//   GET    /api/layouts               -> {"profiles": ["Default", ...]}
//   GET    /api/layout?name=X         -> layout JSON ("{}" if not saved yet)
//   PUT    /api/layout?name=X         -> save layout JSON
//   DELETE /api/layout?name=X         -> delete a profile ("Default" can't be deleted)

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const (
	defaultProfile = "Default"
	profilesDir    = "layouts"
)

var (
	profileNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 _-]{0,31}$`)
	errBadProfile = errors.New("invalid profile name")
)

// profileFile maps a profile name to its file. Names are restricted to a
// safe character set, so they can't escape profilesDir.
func profileFile(name string) (string, error) {
	if name == "" || name == defaultProfile {
		return layoutPath, nil
	}
	if !profileNameRe.MatchString(name) {
		return "", errBadProfile
	}
	return filepath.Join(profilesDir, name+".json"), nil
}

func listProfiles() []string {
	names := []string{defaultProfile}
	entries, _ := os.ReadDir(profilesDir)
	var extra []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".json")
		if profileNameRe.MatchString(name) {
			extra = append(extra, name)
		}
	}
	sort.Strings(extra)
	return append(names, extra...)
}

func profilesHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"error":"method not allowed"}`, 405)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"profiles": listProfiles()})
}

func layoutHandler(w http.ResponseWriter, r *http.Request) {
	path, err := profileFile(r.URL.Query().Get("name"))
	if err != nil {
		http.Error(w, `{"error":"invalid profile name"}`, 400)
		return
	}

	switch r.Method {
	case http.MethodGet:
		data, err := os.ReadFile(path)
		if err != nil {
			w.Write([]byte("{}"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(data)

	case http.MethodPut:
		body, err := io.ReadAll(io.LimitReader(r.Body, maxLayoutBytes+1))
		if err != nil || len(body) > maxLayoutBytes {
			http.Error(w, `{"error":"read or too large"}`, 400)
			return
		}
		// Must be a JSON object; the client reads it as a map of control positions.
		var probe map[string]json.RawMessage
		if err := json.Unmarshal(body, &probe); err != nil || probe == nil {
			http.Error(w, `{"error":"layout must be a JSON object"}`, 400)
			return
		}
		if path != layoutPath {
			if err := os.MkdirAll(profilesDir, 0755); err != nil {
				http.Error(w, `{"error":"write"}`, 500)
				return
			}
		}
		if err := os.WriteFile(path, body, 0644); err != nil {
			http.Error(w, `{"error":"write"}`, 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "saved"})

	case http.MethodDelete:
		if path == layoutPath {
			http.Error(w, `{"error":"the Default layout can't be deleted"}`, 400)
			return
		}
		if err := os.Remove(path); err != nil {
			http.Error(w, `{"error":"not found"}`, 404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "deleted"})

	default:
		http.Error(w, `{"error":"method not allowed"}`, 405)
	}
}
