package policy

import (
	"encoding/json"
	"reflect"
	"slices"

	"github.com/pinksaucepasta/paperboat/shared/schemagen"
)

// SchemaDefs are the schemas of the names a policy rule and a recipe's reach share. The recipe schema
// uses them too, so one definition of an action or a target serves both files.
func SchemaDefs() map[string]any {
	scope := slices.Concat([]string{"*"}, MachineActions, ENVActions)
	return map[string]any{
		"user": map[string]any{"type": "string", "pattern": "^user:.+@.+$", "description": "A workspace member: user:<email>.", "examples": []string{"user:ana@example.test"}},
		"subject": map[string]any{
			"type":        "string",
			"description": "Who a rule covers: user:<email>, group:<name>, role:owner, role:admin (owners and admins), role:billing, role:auditor (exact role), role:member (everyone) or * (every member).",
			"pattern":     "^(\\*|user:.+@.+|group:[a-z0-9][a-z0-9._-]{0,62}|role:(owner|admin|billing|auditor|member))$",
			"examples":    []string{"user:ana@example.test", "group:platform", "role:admin", "*"},
		},
		"target": map[string]any{
			"type":        "string",
			"description": "What a rule covers: machine:<alias>, machine:*, tag:<name>, preview:<name>, tunnel:<name> or env:team.",
			"pattern":     "^(machine:.+|tag:[a-z0-9][a-z0-9._-]{0,62}|preview:.+|tunnel:.+|env:team)$",
			"examples":    []string{"machine:web", "machine:*", "tag:ci", "preview:demo", "tunnel:db", "env:team"},
		},
		"configSyncTarget": map[string]any{
			"type":        "string",
			"description": "machine:* or a tag defined in tags.",
			"pattern":     "^(machine:\\*|tag:[a-z0-9][a-z0-9._-]{0,62})$",
			"examples":    []string{"machine:*", "tag:ci"},
		},
		"action": map[string]any{
			"type": "string",
			"description": "Machine actions (machine:, tag:): terminal, exec, managed_ssh, files, preview_manage, tunnel_manage, private_access, inspect, replay, or * for all of these. " +
				"Team recordings (machine:, tag:; never part of *): record, recording_view. Preview and tunnel actions: inspect, replay. ENV actions (env:team): env_read, env_write.",
			"enum":     slices.Concat([]string{"*"}, MachineActions, RecordingActions, ENVActions),
			"examples": []string{"terminal", "exec", "private_access"},
		},
		"scopeAction": map[string]any{
			"type":        "string",
			"description": "An action a token or recipe may be given: terminal, exec, managed_ssh, files, preview_manage, tunnel_manage, private_access, inspect, replay, env_read, env_write, or * for every machine action.",
			"enum":        scope,
			"examples":    []string{"exec", "files"},
		},
		"port": map[string]any{
			"description": "A port (5432) or an inclusive range (\"8000-8100\"), from 1 to 65535.",
			"oneOf": []any{
				map[string]any{"type": "integer", "minimum": 1, "maximum": 65535},
				map[string]any{"type": "string", "pattern": "^[0-9]{1,5}(-[0-9]{1,5})?$"},
			},
			"examples": []any{5432, "8000-8100"},
		},
	}
}

// PortsNeedPrivateAccess is the constraint "ports apply only when the actions are exactly private_access".
func PortsNeedPrivateAccess(keys ...string) map[string]any {
	var options []any
	for _, key := range keys {
		options = append(options, map[string]any{"required": []string{key}, "properties": map[string]any{key: map[string]any{"const": []string{ActionPrivateAccess}}}})
	}
	return map[string]any{"if": map[string]any{"required": []string{"ports"}}, "then": map[string]any{"anyOf": options}}
}

func merge(base map[string]any, more map[string]any) map[string]any {
	for k, v := range more {
		base[k] = v
	}
	return base
}

// SchemaOptions describe the policy file for schemagen; schema.json is generated from them
// (go test ./shared/policy -run TestSchemaIsGenerated -update).
func SchemaOptions() schemagen.Options {
	ruleExtra := merge(PortsNeedPrivateAccess("allow", "deny"), map[string]any{
		"description": "One rule: who (src) may or may not (allow or deny) do what (actions) on which targets (dst).",
		"oneOf":       []any{map[string]any{"required": []string{"allow"}}, map[string]any{"required": []string{"deny"}}},
		"examples": []any{
			map[string]any{"src": []string{"group:platform"}, "dst": []string{"tag:ci"}, "allow": []string{"terminal", "exec"}},
			map[string]any{"src": []string{"role:member"}, "dst": []string{"machine:db"}, "allow": []string{"private_access"}, "ports": []any{5432}},
		},
	})
	return schemagen.Options{
		Title:       "Paperboat workspace policy",
		Description: "Who may use what on which machines of one workspace. Rules allow or deny; a deny wins over an allow; anything no rule allows is denied. The owner of a personal machine always keeps access to it. Read and written with pb policy get and pb policy set; the file may hold // comments and trailing commas (HuJSON).",
		Examples: []json.RawMessage{json.RawMessage(`{
  "groups": {"group:platform": ["user:ana@example.test"]},
  "tags": {"tag:ci": ["machine:ci-1"]},
  "rules": [{"src": ["group:platform"], "dst": ["tag:ci"], "allow": ["terminal", "exec", "files"]}],
  "tests": [{"src": "user:ana@example.test", "dst": "machine:ci-1", "allow": ["exec"]}]
}`)},
		Defs:  SchemaDefs(),
		Types: map[reflect.Type]string{reflect.TypeOf(PortRange{}): "port"},
		Extra: map[reflect.Type]map[string]any{
			reflect.TypeOf(Rule{}): ruleExtra,
			reflect.TypeOf(Test{}): {
				"description": "An expectation checked on every save: this member can (allow) or cannot (deny) do these actions on this target.",
				"anyOf":       []any{map[string]any{"required": []string{"allow"}}, map[string]any{"required": []string{"deny"}}},
				"examples":    []any{map[string]any{"src": "user:ana@example.test", "dst": "machine:ci-1", "deny": []string{"files"}}},
			},
			reflect.TypeOf(ConfigSyncRule{}): {
				"description": "Chooses the config sync repository for the machines in dst.",
			},
			reflect.TypeOf(ScopeEntry{}): merge(PortsNeedPrivateAccess("allow"), map[string]any{
				"description": "One part of a token's or recipe's reach: these actions on these targets.",
				"examples":    []any{map[string]any{"dst": []string{"tag:ci"}, "allow": []string{"exec", "files"}}},
			}),
		},
	}
}
