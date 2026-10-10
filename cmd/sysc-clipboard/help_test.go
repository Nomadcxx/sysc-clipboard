package main

import (
	"strings"
	"testing"
)

func TestHelpLocalePrecedence(t *testing.T) {
	t.Setenv("LC_ALL", "")
	t.Setenv("LC_MESSAGES", "")
	t.Setenv("LANG", "ja_JP.UTF-8")
	if got := helpLocale(); got != "ja" {
		t.Fatalf("helpLocale() = %q, want ja", got)
	}
	t.Setenv("LC_ALL", "C")
	if got := helpLocale(); got != "ja" {
		t.Fatalf("C should fall through to LANG, got %q", got)
	}
	t.Setenv("LC_ALL", "es_MX@variant")
	if got := helpLocale(); got != "es" {
		t.Fatalf("helpLocale() = %q, want es", got)
	}
	t.Setenv("LC_ALL", "")
	t.Setenv("LANG", "C")
	t.Setenv("LC_MESSAGES", "de_DE.UTF-8")
	if got := helpLocale(); got != "" {
		t.Fatalf("unsupported tags should resolve empty, got %q", got)
	}
}

func TestHelpFallsBackToEnglish(t *testing.T) {
	t.Setenv("LC_ALL", "ru_RU.UTF-8")
	if got := help("resolve clipboard socket: %w"); got != "resolve clipboard socket: %w" {
		t.Fatalf("uncatalogued string changed: %q", got)
	}
	t.Setenv("LC_ALL", "")
	t.Setenv("LANG", "en_US.UTF-8")
	if got := help("unexpected argument %q"); !strings.Contains(got, "%q") {
		t.Fatalf("English fallback lost placeholder: %q", got)
	}
}

func TestHelpTranslatesGuidance(t *testing.T) {
	for _, locale := range []string{"es", "pt", "ja", "ko", "ru"} {
		catalog := helpCatalog(locale)
		if catalog == nil {
			t.Fatalf("%s catalog missing", locale)
		}
		if len(catalog) != 6 {
			t.Fatalf("%s catalog has %d keys, want 5", locale, len(catalog))
		}
		for english, translated := range catalog {
			if translated == "" {
				t.Fatalf("%s: empty value for %q", locale, english)
			}
			if strings.Count(english, "%q")+strings.Count(english, "%d") != strings.Count(translated, "%q")+strings.Count(translated, "%d") {
				t.Fatalf("%s: %q lost %%q placeholders", locale, english)
			}
			if strings.HasSuffix(english, "\n") != strings.HasSuffix(translated, "\n") {
				t.Fatalf("%s: %q newline drift", locale, english)
			}
		}
	}
}
