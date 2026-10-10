package main

import (
	"embed"
	"encoding/json"
	"os"
	"strings"
)

//go:embed catalog/*.json
var helpFS embed.FS

// help translates one user-facing CLI string; the English text is the key.
// Logs and %v diagnostics deliberately stay English.
// ponytail: rereads the catalog per call — a CLI prints at most a handful of
// lines before exiting, so caching would only add state to test.
func help(s string) string {
	catalog := helpCatalog(helpLocale())
	if catalog == nil {
		return s
	}
	if translated := catalog[s]; translated != "" {
		return translated
	}
	return s
}

func helpLocale() string {
	for _, name := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		tag := strings.ToLower(os.Getenv(name))
		if tag == "" || tag == "c" || tag == "posix" {
			continue
		}
		base := tag
		for i, r := range tag {
			if r == '_' || r == '.' || r == '@' {
				base = tag[:i]
				break
			}
		}
		switch base {
		case "es", "pt", "ja", "ko", "ru":
			return base
		}
	}
	return ""
}

func helpCatalog(locale string) map[string]string {
	if locale == "" {
		return nil
	}
	raw, err := helpFS.ReadFile("catalog/" + locale + ".json")
	if err != nil {
		return nil
	}
	var catalog map[string]string
	if err := json.Unmarshal(raw, &catalog); err != nil {
		return nil
	}
	return catalog
}
