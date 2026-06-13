package main

import (
	"fmt"
	"log/slog"
	"math/rand"
	"sort"
	"strings"
	"time"
)

// Rarity controls how often a voice preset is picked within its language
// bucket. Rarer tiers carry a lower selection weight, so a "legendary" preset
// surfaces only occasionally — handy for easter-egg voices.
type Rarity string

const (
	RarityCommon    Rarity = "common"
	RarityUncommon  Rarity = "uncommon"
	RarityRare      Rarity = "rare"
	RarityLegendary Rarity = "legendary"
)

// rarityWeights maps each tier to its relative selection weight. A common
// preset is ~100× more likely to be chosen than a legendary one.
var rarityWeights = map[Rarity]int{
	RarityCommon:    100,
	RarityUncommon:  30,
	RarityRare:      8,
	RarityLegendary: 1,
}

// parseRarity normalizes a config rarity string. Unknown/empty → common.
func parseRarity(s string) Rarity {
	switch Rarity(strings.ToLower(strings.TrimSpace(s))) {
	case RarityUncommon:
		return RarityUncommon
	case RarityRare:
		return RarityRare
	case RarityLegendary:
		return RarityLegendary
	default:
		return RarityCommon
	}
}

// VoicePreset is one configured voice within a language bucket.
type VoicePreset struct {
	Name   string `json:"name"`
	Rarity string `json:"rarity,omitempty"` // common|uncommon|rare|legendary (default common)
	Weight int    `json:"weight,omitempty"` // explicit weight override; >0 wins over rarity tier
}

// voiceAsset is the decoded audio + phase markers for one preset file.
// Loaded once and shared across every language bucket that references it.
type voiceAsset struct {
	Name    string
	Packets []OpusPacket
	Markers ParsedMarkers
}

// pooledVoice is a single preset inside a language bucket, pointing at a
// shared decoded asset.
type pooledVoice struct {
	asset  *voiceAsset
	lang   string
	rarity Rarity
	weight int
}

// VoicePool holds voice presets grouped by language and selects one per call
// using a rarity-weighted random pick, with locale-aware fallback.
type VoicePool struct {
	byLang   map[string][]pooledVoice
	fallback string
}

// normalizeLang reduces a BCP-47 tag (e.g. "en-US", "ru_RU") to its lowercase
// primary language subtag ("en", "ru"). Returns "" for empty input.
func normalizeLang(tag string) string {
	tag = strings.TrimSpace(tag)
	if tag == "" {
		return ""
	}
	base := strings.FieldsFunc(tag, func(r rune) bool { return r == '-' || r == '_' })
	if len(base) == 0 {
		return ""
	}
	return strings.ToLower(base[0])
}

// LoadVoicePool decodes every referenced preset file once and groups presets
// into per-language buckets. pathFor resolves a (language, preset name) pair to
// a file path without extension — ".opus" and ".json" are appended. Presets
// that fail to load are skipped with a warning; the pool fails only when
// nothing at all could be loaded.
func LoadVoicePool(voices map[string][]VoicePreset, fallback string, pathFor func(lang, name string) string) (*VoicePool, error) {
	fb := normalizeLang(fallback)
	if fb == "" {
		fb = "en"
	}
	pool := &VoicePool{
		byLang:   make(map[string][]pooledVoice),
		fallback: fb,
	}

	// Decode each file at most once. Keyed by the resolved path, so the same
	// preset name in different languages (different recordings) stays distinct.
	assets := make(map[string]*voiceAsset)
	loadAsset := func(path string) (*voiceAsset, error) {
		if a, ok := assets[path]; ok {
			return a, nil
		}
		markers, err := loadMarkers(path + ".json")
		if err != nil {
			return nil, fmt.Errorf("markers: %w", err)
		}
		packets, err := LoadOpusPackets(path + ".opus")
		if err != nil {
			return nil, fmt.Errorf("opus: %w", err)
		}
		var dur time.Duration
		for _, p := range packets {
			dur += p.Duration
		}
		slog.Info("voice asset loaded", "path", path, "packets", len(packets), "duration", dur.Round(time.Millisecond))
		a := &voiceAsset{Name: path, Packets: packets, Markers: markers}
		assets[path] = a
		return a, nil
	}

	// Iterate language keys in a stable order for deterministic logging.
	langs := make([]string, 0, len(voices))
	for lang := range voices {
		langs = append(langs, lang)
	}
	sort.Strings(langs)

	for _, lang := range langs {
		nl := normalizeLang(lang)
		if nl == "" {
			slog.Warn("skipping voice bucket with empty language key")
			continue
		}
		for _, p := range voices[lang] {
			if p.Name == "" {
				slog.Warn("skipping voice preset with empty name", "lang", nl)
				continue
			}
			path := pathFor(nl, p.Name)
			asset, err := loadAsset(path)
			if err != nil {
				slog.Warn("skipping voice preset", "lang", nl, "name", p.Name, "path", path, "error", err)
				continue
			}
			rarity := parseRarity(p.Rarity)
			weight := p.Weight
			if weight <= 0 {
				weight = rarityWeights[rarity]
			}
			pool.byLang[nl] = append(pool.byLang[nl], pooledVoice{
				asset:  asset,
				lang:   nl,
				rarity: rarity,
				weight: weight,
			})
			slog.Info("voice preset registered",
				"lang", nl, "name", p.Name, "rarity", rarity, "weight", weight)
		}
	}

	if pool.Empty() {
		return nil, fmt.Errorf("no voice presets loaded")
	}
	if len(pool.byLang[pool.fallback]) == 0 {
		slog.Warn("fallback language has no presets; unknown locales will use any available bucket",
			"fallback", pool.fallback)
	}
	return pool, nil
}

// Empty reports whether the pool has no loadable presets in any language.
func (p *VoicePool) Empty() bool {
	for _, vs := range p.byLang {
		if len(vs) > 0 {
			return false
		}
	}
	return true
}

// Languages returns the languages that have at least one preset, sorted.
func (p *VoicePool) Languages() []string {
	out := make([]string, 0, len(p.byLang))
	for lang, vs := range p.byLang {
		if len(vs) > 0 {
			out = append(out, lang)
		}
	}
	sort.Strings(out)
	return out
}

// Pick selects a voice for the caller's locale. It resolves the locale to a
// language bucket (falling back to the configured fallback language, then to
// any available bucket), then performs a rarity-weighted random choice.
// Returns (nil, "", "") only if the pool is empty.
func (p *VoicePool) Pick(locale string) (*voiceAsset, string, Rarity) {
	bucket, resolved := p.resolveBucket(normalizeLang(locale))
	if len(bucket) == 0 {
		return nil, "", ""
	}
	choice := weightedPick(bucket)
	return choice.asset, resolved, choice.rarity
}

// resolveBucket returns the preset bucket for lang, falling back to the
// fallback language and finally to any non-empty bucket. The second return
// value is the language that was actually resolved.
func (p *VoicePool) resolveBucket(lang string) ([]pooledVoice, string) {
	if lang != "" {
		if b := p.byLang[lang]; len(b) > 0 {
			return b, lang
		}
	}
	if b := p.byLang[p.fallback]; len(b) > 0 {
		return b, p.fallback
	}
	// Last resort: pick the first non-empty bucket (sorted for determinism) so
	// the bot always answers, even if both the locale and fallback are missing.
	for _, lang := range p.Languages() {
		return p.byLang[lang], lang
	}
	return nil, ""
}

// weightedPick chooses one voice from bucket with probability proportional to
// each entry's weight.
func weightedPick(bucket []pooledVoice) pooledVoice {
	total := 0
	for _, v := range bucket {
		total += v.weight
	}
	if total <= 0 {
		return bucket[rand.Intn(len(bucket))]
	}
	n := rand.Intn(total)
	for _, v := range bucket {
		if n < v.weight {
			return v
		}
		n -= v.weight
	}
	return bucket[len(bucket)-1] // unreachable when total > 0
}
