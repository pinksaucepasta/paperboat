package recipe

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/policy"
	"github.com/pinksaucepasta/paperboat/shared/schemagen"
)

// DurationPattern is a regular expression for the durations ParseDuration reads ("30m", "12h", "7d")
// between from and to, so the schema refuses the same values the parser does.
func DurationPattern(from, to time.Duration) string {
	var alts []string
	for _, unit := range []struct {
		suffix string
		length time.Duration
	}{{"m", time.Minute}, {"h", time.Hour}, {"d", 24 * time.Hour}} {
		lo := int((from + unit.length - 1) / unit.length)
		hi := int(to / unit.length)
		lo, hi = max(lo, 1), min(hi, 999999)
		if lo <= hi {
			alts = append(alts, "(?:"+numberRange(lo, hi)+")"+unit.suffix)
		}
	}
	return "^(?:" + strings.Join(alts, "|") + ")$"
}

// numberRange matches the decimal numbers from lo to hi (no leading zeros).
func numberRange(lo, hi int) string {
	var alts []string
	for digits := len(strconv.Itoa(lo)); digits <= len(strconv.Itoa(hi)); digits++ {
		from, to := max(lo, pow10(digits-1)), min(hi, pow10(digits)-1)
		alts = append(alts, sameLength(strconv.Itoa(from), strconv.Itoa(to)))
	}
	return strings.Join(alts, "|")
}

func pow10(n int) int {
	p := 1
	for ; n > 0; n-- {
		p *= 10
	}
	return p
}

// sameLength matches the digit strings from lo to hi, which have the same length.
func sameLength(lo, hi string) string {
	if lo == hi {
		return lo
	}
	if lo[0] == hi[0] {
		return lo[:1] + "(?:" + sameLength(lo[1:], hi[1:]) + ")"
	}
	rest := len(lo) - 1
	if rest == 0 {
		return "[" + lo + "-" + hi + "]"
	}
	zeros, nines := strings.Repeat("0", rest), strings.Repeat("9", rest)
	from, to := lo[0], hi[0]
	var alts []string
	if lo[1:] != zeros {
		alts = append(alts, lo[:1]+"(?:"+sameLength(lo[1:], nines)+")")
		from++
	}
	highEdge := hi[1:] != nines
	if highEdge {
		to--
	}
	if from <= to {
		alts = append(alts, "["+string(from)+"-"+string(to)+"][0-9]{"+strconv.Itoa(rest)+"}")
	}
	if highEdge {
		alts = append(alts, hi[:1]+"(?:"+sameLength(zeros, hi[1:])+")")
	}
	return strings.Join(alts, "|")
}

func duration(from, to time.Duration, what string, examples ...string) map[string]any {
	return map[string]any{
		"type":        "string",
		"pattern":     DurationPattern(from, to),
		"description": fmt.Sprintf("%s: a whole number and m, h or d (minutes, hours, days), from %s to %s.", what, label(from), label(to)),
		"examples":    examples,
	}
}

func label(d time.Duration) string {
	if d%(24*time.Hour) == 0 {
		return fmt.Sprintf("%dd", d/(24*time.Hour))
	}
	if d%time.Hour == 0 {
		return fmt.Sprintf("%dh", d/time.Hour)
	}
	return fmt.Sprintf("%dm", d/time.Minute)
}

// SchemaOptions describe the recipe file for schemagen; schema.json is generated from them
// (go test ./shared/recipe -run TestSchemaIsGenerated -update).
func SchemaOptions() schemagen.Options {
	defs := policy.SchemaDefs()
	defs["durationExpires"] = duration(time.Minute, MaxExpires, "How long a recipe lasts", "30d", "365d")
	defs["durationOffline"] = duration(MinOffline, MaxOffline, "How long a machine may be offline", "30m", "7d")
	defs["durationLifetime"] = duration(MinLifetime, MaxLifetime, "How long a machine may live", "8h", "7d")
	defs["durationTTL"] = duration(time.Minute, MaxTTL, "How long a preview stays open", "30m", "2h")
	defs["durationTunnel"] = duration(time.Minute, MaxLifetime, "How long a tunnel stays open", "2h", "7d")
	return schemagen.Options{
		Title:       "Paperboat recipe",
		Description: "What happens on a machine when it pairs with this recipe's token: its tags, what it may reach, which ENV vaults it reads, which repository it pulls config from, which previews and tunnels it opens, and when it is removed. The file is named <recipe name>.jsonc (lowercase letters, digits and -, up to 63) and may hold // comments and trailing commas (HuJSON). Create it with pb recipe create -f <file>.",
		Examples: []json.RawMessage{
			json.RawMessage(`{"tags": ["ci"], "max_machines": 20, "remove_after_offline": "30m", "env": {"vaults": ["canary-env"]}, "preview": [{"port": 3000, "ttl": "2h"}]}`),
			json.RawMessage(`{"max_machines": 1, "remove_after_offline": "10m", "machine_lifetime": "8h", "reach": [{"dst": ["tag:ci"], "allow": ["exec", "managed_ssh", "files"]}], "tunnel": [{"port": 8123, "private": true}]}`),
		},
		Defs:  defs,
		Types: map[reflect.Type]string{reflect.TypeOf(policy.PortRange{}): "port"},
		Extra: map[reflect.Type]map[string]any{
			reflect.TypeOf(policy.ScopeEntry{}): policyScopeExtra(),
			reflect.TypeOf(Env{}):               {"description": "The ENV vaults a recipe reads."},
			reflect.TypeOf(Config{}):            {"description": "The config sync repository a recipe pulls."},
			reflect.TypeOf(Preview{}):           {"description": "One preview opened on each machine."},
			reflect.TypeOf(Tunnel{}): {
				"description": "One tunnel opened on each machine: it has port or from, not both.",
				"oneOf":       []any{map[string]any{"required": []string{"port"}}, map[string]any{"required": []string{"from"}}},
			},
		},
	}
}

func policyScopeExtra() map[string]any {
	return policy.SchemaOptions().Extra[reflect.TypeOf(policy.ScopeEntry{})]
}
