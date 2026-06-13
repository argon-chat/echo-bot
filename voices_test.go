package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestNormalizeLang(t *testing.T) {
	cases := map[string]string{
		"":        "",
		"  ":      "",
		"ru":      "ru",
		"RU":      "ru",
		"en-US":   "en",
		"ru_RU":   "ru",
		"  ja  ":  "ja",
		"zh-Hans": "zh",
	}
	for in, want := range cases {
		if got := normalizeLang(in); got != want {
			t.Errorf("normalizeLang(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseRarity(t *testing.T) {
	cases := map[string]Rarity{
		"":          RarityCommon,
		"common":    RarityCommon,
		"COMMON":    RarityCommon,
		"uncommon":  RarityUncommon,
		"rare":      RarityRare,
		"legendary": RarityLegendary,
		"bogus":     RarityCommon,
		" rare ":    RarityRare,
	}
	for in, want := range cases {
		if got := parseRarity(in); got != want {
			t.Errorf("parseRarity(%q) = %q, want %q", in, got, want)
		}
	}
}

// testPool builds a pool directly from in-memory assets, bypassing file I/O.
func testPool(fallback string, byLang map[string][]pooledVoice) *VoicePool {
	return &VoicePool{byLang: byLang, fallback: normalizeLang(fallback)}
}

func voice(name string) *voiceAsset { return &voiceAsset{Name: name} }

func TestPickResolvesLanguageBuckets(t *testing.T) {
	ru := voice("ru_a")
	en := voice("en_a")
	pool := testPool("en", map[string][]pooledVoice{
		"ru": {{asset: ru, lang: "ru", rarity: RarityCommon, weight: 100}},
		"en": {{asset: en, lang: "en", rarity: RarityCommon, weight: 100}},
	})

	// Exact match.
	if a, lang, _ := pool.Pick("ru"); a != ru || lang != "ru" {
		t.Errorf("Pick(ru) = %v/%q, want ru_a/ru", a, lang)
	}
	// BCP-47 region subtag is stripped.
	if a, lang, _ := pool.Pick("ru-RU"); a != ru || lang != "ru" {
		t.Errorf("Pick(ru-RU) = %v/%q, want ru_a/ru", a, lang)
	}
	// Unknown locale → fallback language (en).
	if a, lang, _ := pool.Pick("de"); a != en || lang != "en" {
		t.Errorf("Pick(de) = %v/%q, want en_a/en", a, lang)
	}
	// Empty locale → fallback language (en).
	if a, lang, _ := pool.Pick(""); a != en || lang != "en" {
		t.Errorf("Pick('') = %v/%q, want en_a/en", a, lang)
	}
}

func TestPickFallsBackToAnyBucketWhenFallbackMissing(t *testing.T) {
	ru := voice("ru_a")
	// Fallback is "en" but there is no en bucket — must still answer with ru.
	pool := testPool("en", map[string][]pooledVoice{
		"ru": {{asset: ru, lang: "ru", rarity: RarityCommon, weight: 100}},
	})
	if a, lang, _ := pool.Pick("fr"); a != ru || lang != "ru" {
		t.Errorf("Pick(fr) = %v/%q, want ru_a/ru", a, lang)
	}
}

func TestPickEmptyPool(t *testing.T) {
	pool := testPool("en", map[string][]pooledVoice{})
	if a, lang, rarity := pool.Pick("ru"); a != nil || lang != "" || rarity != "" {
		t.Errorf("Pick on empty pool = %v/%q/%q, want nil/''/''", a, lang, rarity)
	}
}

// TestWeightedPickRespectsRarity is a statistical check: a rare preset should
// be picked far less often than a common one sharing the same bucket.
func TestWeightedPickRespectsRarity(t *testing.T) {
	common := voice("common")
	rare := voice("rare")
	bucket := []pooledVoice{
		{asset: common, lang: "ru", rarity: RarityCommon, weight: rarityWeights[RarityCommon]},
		{asset: rare, lang: "ru", rarity: RarityLegendary, weight: rarityWeights[RarityLegendary]},
	}

	const iters = 20000
	counts := map[string]int{}
	for i := 0; i < iters; i++ {
		counts[weightedPick(bucket).asset.Name]++
	}
	if counts["common"] <= counts["rare"] {
		t.Errorf("expected common picked more than rare: common=%d rare=%d",
			counts["common"], counts["rare"])
	}
	// Legendary (weight 1) vs common (weight 100): rare share should be tiny.
	if rareShare := float64(counts["rare"]) / iters; rareShare > 0.05 {
		t.Errorf("rare share %.3f too high (expected ~0.01)", rareShare)
	}
}

// TestLoadVoicePoolFromConfig is an end-to-end check that the shipped
// config.json + voices/<lang>/ layout actually decodes every preset.
func TestLoadVoicePoolFromConfig(t *testing.T) {
	data, err := os.ReadFile("config.json")
	if err != nil {
		t.Fatalf("read config.json: %v", err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("parse config.json: %v", err)
	}

	dir := cfg.VoicesDir
	if dir == "" {
		dir = "voices"
	}
	pathFor := func(lang, name string) string { return filepath.Join(dir, lang, name) }

	pool, err := LoadVoicePool(cfg.Voices, cfg.FallbackLanguage, pathFor)
	if err != nil {
		t.Fatalf("LoadVoicePool: %v", err)
	}

	for lang, want := range map[string]int{"ru": 4, "en": 9} {
		if got := len(pool.byLang[lang]); got != want {
			t.Errorf("%s presets loaded = %d, want %d", lang, got, want)
		}
	}

	// A ru caller must get a voice from the ru folder, and vice versa.
	if a, lang, _ := pool.Pick("ru-RU"); a == nil || lang != "ru" {
		t.Errorf("Pick(ru-RU) = %v/%q, want a ru voice", a, lang)
	}
	if a, lang, _ := pool.Pick("en-US"); a == nil || lang != "en" {
		t.Errorf("Pick(en-US) = %v/%q, want an en voice", a, lang)
	}
	// Unknown locale resolves to the fallback language (en).
	if _, lang, _ := pool.Pick("fr"); lang != "en" {
		t.Errorf("Pick(fr) resolved to %q, want en (fallback)", lang)
	}
}

func TestWeightedPickZeroWeightsUniform(t *testing.T) {
	a, b := voice("a"), voice("b")
	bucket := []pooledVoice{
		{asset: a, weight: 0},
		{asset: b, weight: 0},
	}
	// Should not panic and should return a valid entry.
	for i := 0; i < 100; i++ {
		got := weightedPick(bucket).asset
		if got != a && got != b {
			t.Fatalf("weightedPick returned unknown asset %v", got)
		}
	}
}
