package main

import (
	"database/sql"
	"os"
	"strings"
)

// ---- runtime settings, stored in dogear_config, with env fallbacks ----
//
// Precedence: value saved in the DB (set through the admin UI) wins; if unset,
// fall back to the environment variable; if that's unset too, the default.
// This lets an externally-hosted install be configured entirely from the UI.

func (s *Store) getConfig(key string) (string, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM dogear_config WHERE key=?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return v, err
}

func (s *Store) setConfig(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO dogear_config(key, value) VALUES(?, ?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

func (s *Store) deleteConfig(key string) error {
	_, err := s.db.Exec(`DELETE FROM dogear_config WHERE key=?`, key)
	return err
}

// config resolves key through DB -> env -> default.
func (s *Store) config(key, envVar, def string) string {
	if v, err := s.getConfig(key); err == nil && strings.TrimSpace(v) != "" {
		return v
	}
	if envVar != "" {
		if v := strings.TrimSpace(os.Getenv(envVar)); v != "" {
			return v
		}
	}
	return def
}

// Config keys used across the app.
const (
	cfgProwlarrURL = "prowlarr_url"
	cfgProwlarrKey = "prowlarr_api_key"
	cfgProwlarrOn  = "prowlarr_enabled"
	cfgAAKey       = "aa_donator_key"
	cfgAABaseURL   = "aa_base_url"
	cfgSourceOrder = "release_source_order"
	cfgTTSUrl      = "tts_url"
	cfgABSURL      = "abs_url"
	cfgABSKey      = "abs_api_key"
	cfgABSLibrary  = "abs_library_id"
	cfgABSOn       = "abs_enabled"
)

// Settings holds the resolved integration configuration.
type Settings struct {
	ProwlarrURL     string `json:"prowlarr_url"`
	ProwlarrKeySet  bool   `json:"prowlarr_api_key_set"`
	ProwlarrEnabled bool   `json:"prowlarr_enabled"`
	AAKeySet        bool   `json:"aa_donator_key_set"`
	AABaseURL       string `json:"aa_base_url"`
	HardcoverSet    bool   `json:"hardcover_token_set"`
	TTSUrl          string `json:"tts_url"`
	ABSURL          string `json:"abs_url"`
	ABSKeySet       bool   `json:"abs_api_key_set"`
	ABSLibrary      string `json:"abs_library_id"`
	ABSEnabled      bool   `json:"abs_enabled"`
}

func (s *Store) settings() Settings {
	return Settings{
		ProwlarrURL:     s.config(cfgProwlarrURL, "PROWLARR_URL", ""),
		ProwlarrKeySet:  s.config(cfgProwlarrKey, "PROWLARR_API_KEY", "") != "",
		ProwlarrEnabled: s.config(cfgProwlarrOn, "", "") == "true",
		AAKeySet:        s.config(cfgAAKey, "AA_DONATOR_KEY", "") != "",
		AABaseURL:       s.config(cfgAABaseURL, "AA_BASE_URL", "https://annas-archive.gd"),
		HardcoverSet:    s.config(cfgHardcoverToken, "", "") != "",
		TTSUrl:          s.config(cfgTTSUrl, "DOGEAR_TTS_URL", "http://dogear-tts:8097"),
		ABSURL:          s.config(cfgABSURL, "ABS_URL", ""),
		ABSKeySet:       s.config(cfgABSKey, "ABS_API_KEY", "") != "",
		ABSLibrary:      s.config(cfgABSLibrary, "ABS_LIBRARY_ID", ""),
		ABSEnabled:      s.config(cfgABSOn, "", "") == "true",
	}
}

func (s *Store) prowlarrURL() string { return s.config(cfgProwlarrURL, "PROWLARR_URL", "") }
func (s *Store) prowlarrKey() string { return s.config(cfgProwlarrKey, "PROWLARR_API_KEY", "") }
func (s *Store) prowlarrEnabled() bool {
	return s.config(cfgProwlarrOn, "", "") == "true"
}
func (s *Store) aaKey() string { return s.config(cfgAAKey, "AA_DONATOR_KEY", "") }
func (s *Store) aaBaseURL() string {
	return s.config(cfgAABaseURL, "AA_BASE_URL", "https://annas-archive.gd")
}
func (s *Store) ttsURL() string {
	return strings.TrimRight(s.config(cfgTTSUrl, "DOGEAR_TTS_URL", "http://dogear-tts:8097"), "/")
}
func (s *Store) absURL() string  { return strings.TrimRight(s.config(cfgABSURL, "ABS_URL", ""), "/") }
func (s *Store) absKey() string  { return s.config(cfgABSKey, "ABS_API_KEY", "") }
func (s *Store) absLibrary() string { return s.config(cfgABSLibrary, "ABS_LIBRARY_ID", "") }
func (s *Store) absEnabled() bool { return s.config(cfgABSOn, "", "") == "true" }
