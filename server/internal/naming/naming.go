// Package naming owns Paperboat's randomly allocated readable names.
// Callers persist each name and reserve it in the appropriate namespace.
package naming

import (
	"crypto/rand"
	"fmt"
	"io"
	"math/big"
	"strings"
)

var qualities = [...]string{
	"agile", "alert", "bold", "brave", "bright", "calm", "careful", "cheerful",
	"clean", "clear", "clever", "cool", "cozy", "crisp", "curious", "daring",
	"direct", "eager", "easy", "fair", "faithful", "fast", "fine", "firm",
	"focused", "frank", "free", "fresh", "friendly", "gentle", "glad", "graceful",
	"grand", "happy", "hardy", "helpful", "honest", "humble", "jolly", "keen",
	"kind", "lively", "loyal", "lucky", "merry", "mild", "neat", "nimble",
	"open", "patient", "peaceful", "playful", "polite", "proud", "quick", "quiet",
	"ready", "sharp", "simple", "smart", "smooth", "steady", "strong", "warm",
}

var atmospheres = [...]string{
	"alpine", "amber", "arctic", "autumn", "blue", "breezy", "bronze", "cloudy",
	"coastal", "coral", "crimson", "dawn", "desert", "dewy", "dusk", "dusty",
	"emerald", "evening", "foggy", "forest", "frosty", "golden", "gray", "green",
	"hazy", "icy", "indigo", "island", "ivory", "jade", "lake", "lunar",
	"marine", "meadow", "midnight", "misty", "morning", "mountain", "northern", "ocean",
	"orange", "peach", "pearl", "pink", "polar", "purple", "rainy", "red",
	"river", "rocky", "rosy", "sandy", "shaded", "silver", "snowy", "solar",
	"spring", "starlit", "stormy", "summer", "sunny", "tropical", "violet", "winter",
}

var waypoints = [...]string{
	"anchor", "arch", "barn", "bay", "beach", "beacon", "boat", "bridge",
	"brook", "cabin", "camp", "canal", "cape", "castle", "cave", "channel",
	"cliff", "coast", "compass", "corner", "creek", "crossing", "deck", "dock",
	"ferry", "field", "garden", "gate", "harbor", "haven", "hill", "home",
	"horizon", "inlet", "journey", "landing", "lantern", "lookout", "map", "marker",
	"mast", "mill", "orchard", "park", "path", "pier", "pond", "port",
	"ranch", "reef", "ridge", "road", "route", "sail", "shore", "station",
	"summit", "tower", "trail", "valley", "village", "voyage", "waypoint", "wharf",
}

// Session allocates a three-word candidate. A nil reader uses crypto/rand.
func Session(random io.Reader) (string, error) {
	if random == nil {
		random = rand.Reader
	}
	words := make([]string, 0, 3)
	for _, list := range [][]string{qualities[:], atmospheres[:], waypoints[:]} {
		index, err := rand.Int(random, big.NewInt(int64(len(list))))
		if err != nil {
			return "", fmt.Errorf("generate readable name: %w", err)
		}
		words = append(words, list[index.Int64()])
	}
	return strings.Join(words, "-"), nil
}

// PublicSlug allocates a three-word candidate with a uniformly random four-digit suffix.
// Leading zeroes are retained; callers must reserve the complete hostname atomically.
func PublicSlug(random io.Reader) (string, error) {
	if random == nil {
		random = rand.Reader
	}
	name, err := Session(random)
	if err != nil {
		return "", err
	}
	digits, err := rand.Int(random, big.NewInt(10000))
	if err != nil {
		return "", fmt.Errorf("generate readable name suffix: %w", err)
	}
	return fmt.Sprintf("%s-%04d", name, digits.Int64()), nil
}
