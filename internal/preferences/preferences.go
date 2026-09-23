package preferences

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/pinksaucepasta/paperboat/internal/atomicfile"
	"github.com/pinksaucepasta/paperboat/internal/config"
)

const maxFileBytes = 128 << 10

type Shortcut struct {
	Command []string `json:"command"`
	Args    []string `json:"args,omitempty"`
}

type TUI struct {
	Theme         string              `json:"theme,omitempty"`
	Accent        string              `json:"accent,omitempty"`
	Density       string              `json:"density,omitempty"`
	Keys          map[string]string   `json:"keys,omitempty"`
	HomeOrder     []string            `json:"home_order,omitempty"`
	HomeHidden    []string            `json:"home_hidden,omitempty"`
	Favorites     []string            `json:"favorites,omitempty"`
	Columns       map[string][]string `json:"columns,omitempty"`
	PreviewPanels []string            `json:"preview_panels"`
}

type Document struct {
	Version    int                          `json:"version"`
	Shortcuts  map[string]Shortcut          `json:"shortcuts,omitempty"`
	PortAction string                       `json:"port_action,omitempty"`
	Defaults   map[string]map[string]string `json:"defaults,omitempty"`
	TUI        TUI                          `json:"tui,omitempty"`
}

func Default() Document {
	return Document{Version: 1, PortAction: "preview", TUI: TUI{
		Theme: "terminal", Density: "comfortable",
		Keys:          map[string]string{"up": "up", "down": "down", "select": "enter", "back": "esc", "preview_open": "o", "preview_background": "b", "preview_tunnel": "t", "preview_stop": "s"},
		Columns:       map[string][]string{"machines": {"status", "platform"}, "sessions": {"machine", "state"}, "previews": {"state", "access"}, "tunnels": {"state", "access", "endpoint"}},
		PreviewPanels: []string{"target", "access", "expiry", "domains"},
	}}
}

var homeIDs = []string{"machines", "sessions", "previews", "environment-variables", "inbox", "team", "transfer", "config", "doctor", "account", "customize", "commands"}
var columnIDs = map[string][]string{"machines": {"status", "platform", "id"}, "sessions": {"machine", "state", "id"}, "previews": {"state", "access", "id"}, "tunnels": {"state", "access", "endpoint"}}
var previewPanelIDs = []string{"target", "access", "expiry", "domains"}

func HomeIDs() []string                  { return slices.Clone(homeIDs) }
func ColumnIDs(resource string) []string { return slices.Clone(columnIDs[resource]) }
func PreviewPanelIDs() []string          { return slices.Clone(previewPanelIDs) }

func Effective(doc Document) Document {
	result := Clone(doc)
	defaults := Default()
	if result.PortAction == "" {
		result.PortAction = defaults.PortAction
	}
	if result.TUI.Theme == "" {
		result.TUI.Theme = defaults.TUI.Theme
	}
	if result.TUI.Density == "" {
		result.TUI.Density = defaults.TUI.Density
	}
	if result.TUI.Keys == nil {
		result.TUI.Keys = defaults.TUI.Keys
	} else {
		for k, v := range defaults.TUI.Keys {
			if _, ok := result.TUI.Keys[k]; !ok {
				result.TUI.Keys[k] = v
			}
		}
	}
	if result.TUI.Columns == nil {
		result.TUI.Columns = defaults.TUI.Columns
	} else {
		for k, v := range defaults.TUI.Columns {
			if _, ok := result.TUI.Columns[k]; !ok {
				result.TUI.Columns[k] = v
			}
		}
	}
	if result.TUI.PreviewPanels == nil {
		result.TUI.PreviewPanels = defaults.TUI.PreviewPanels
	}
	return result
}

func Path(configPath string) (string, error) {
	if configPath == "" {
		var err error
		configPath, err = config.DefaultPath()
		if err != nil {
			return "", err
		}
	}
	if strings.HasSuffix(configPath, ".json") {
		configPath = strings.TrimSuffix(configPath, ".json")
	}
	return configPath + ".preferences.json", nil
}

func Load(path string) (Document, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return Default(), nil
	}
	if err != nil {
		return Document{}, fmt.Errorf("read preferences: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > maxFileBytes {
		return Document{}, errors.New("preferences file must be a regular file no larger than 128 KiB")
	}
	f, err := os.Open(path)
	if err != nil {
		return Document{}, fmt.Errorf("read preferences: %w", err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxFileBytes+1))
	if err != nil || len(data) > maxFileBytes {
		return Document{}, errors.New("preferences file cannot be read within the 128 KiB limit")
	}
	var doc Document
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&doc); err != nil {
		return Document{}, fmt.Errorf("parse preferences: %w", err)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return Document{}, errors.New("parse preferences: trailing JSON value")
	}
	if err := Validate(doc); err != nil {
		return Document{}, err
	}
	return Effective(doc), nil
}

func Save(path string, doc Document) error {
	data, err := encoded(doc)
	if err != nil {
		return err
	}
	path, unlock, err := lockPreferences(path)
	if err != nil {
		return err
	}
	defer unlock()
	return write(path, data)
}

func encoded(doc Document) ([]byte, error) {
	if err := Validate(doc); err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil || len(data)+1 > maxFileBytes {
		return nil, errors.New("preferences exceed the 128 KiB limit")
	}
	data = append(data, '\n')
	return data, nil
}

func write(path string, data []byte) error {
	return atomicfile.Write(path, data, atomicfile.Options{Mode: 0600, OwnerUID: -1, OwnerGID: -1})
}

var namePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)
var hexColor = regexp.MustCompile(`^#[0-9A-Fa-f]{6}$`)
var argumentTemplate = regexp.MustCompile(`\{(args|[1-9][0-9]*)\}`)
var ErrChanged = errors.New("preferences changed since they were loaded")
var ErrBusy = errors.New("preferences are being edited by another process")

func Revision(path string) (string, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return "missing", nil
	}
	if err != nil {
		return "", fmt.Errorf("inspect preferences revision: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > maxFileBytes {
		return "", errors.New("preferences revision requires a regular file no larger than 128 KiB")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("read preferences revision: %w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxFileBytes+1))
	if err != nil {
		return "", fmt.Errorf("read preferences revision: %w", err)
	}
	if len(data) > maxFileBytes {
		return "", errors.New("preferences exceed the 128 KiB limit")
	}
	return fmt.Sprintf("sha256:%x", sha256.Sum256(data)), nil
}

func SaveIfUnchanged(path string, doc Document, revision string) error {
	data, err := encoded(doc)
	if err != nil {
		return err
	}
	path, unlock, err := lockPreferences(path)
	if err != nil {
		return err
	}
	defer unlock()
	current, err := Revision(path)
	if err != nil {
		return err
	}
	if current != revision {
		return ErrChanged
	}
	return write(path, data)
}

func Validate(doc Document) error {
	if doc.Version != 1 {
		return errors.New("preferences version must be 1")
	}
	if doc.PortAction != "" && doc.PortAction != "preview" && doc.PortAction != "tunnel" {
		return errors.New("port_action must be preview or tunnel")
	}
	if len(doc.Shortcuts) > 64 || len(doc.Defaults) > 64 {
		return errors.New("preferences contain too many shortcuts or command defaults")
	}
	for name, shortcut := range doc.Shortcuts {
		if !namePattern.MatchString(name) || len(shortcut.Command) == 0 || len(shortcut.Command)+len(shortcut.Args) > 64 {
			return fmt.Errorf("shortcut %q is invalid", name)
		}
		for _, value := range shortcut.Command {
			if value == "" {
				return fmt.Errorf("shortcut %q contains an empty command word", name)
			}
		}
		for _, value := range append(append([]string{}, shortcut.Command...), shortcut.Args...) {
			if len(value) > 1024 {
				return fmt.Errorf("shortcut %q contains an invalid argument", name)
			}
		}
		argsTokens := 0
		for _, value := range shortcut.Args {
			matches := argumentTemplate.FindAllString(value, -1)
			if strings.ContainsAny(argumentTemplate.ReplaceAllString(value, ""), "{}") {
				return fmt.Errorf("shortcut %q has a malformed template", name)
			}
			for _, token := range matches {
				key := token[1 : len(token)-1]
				if key == "args" {
					if value != "{args}" {
						return fmt.Errorf("shortcut %q must use {args} as a whole argument", name)
					}
					argsTokens++
				} else if n, _ := strconv.Atoi(key); n > 64 {
					return fmt.Errorf("shortcut %q argument template exceeds the limit", name)
				}
			}
		}
		if argsTokens > 1 {
			return fmt.Errorf("shortcut %q repeats {args}", name)
		}
	}
	for command, flags := range doc.Defaults {
		if strings.TrimSpace(command) != command || command == "" || len(command) > 1024 || len(flags) > 32 {
			return errors.New("command defaults are invalid")
		}
		for flag, value := range flags {
			if !namePattern.MatchString(flag) || len(value) > 1024 {
				return fmt.Errorf("defaults for %q are invalid", command)
			}
		}
	}
	if !slices.Contains([]string{"", "terminal", "dark", "light", "mono"}, doc.TUI.Theme) {
		return errors.New("tui theme is invalid")
	}
	accent, accentErr := strconv.Atoi(doc.TUI.Accent)
	if doc.TUI.Accent != "" && !hexColor.MatchString(doc.TUI.Accent) && (accentErr != nil || accent < 0 || accent > 255 || strconv.Itoa(accent) != doc.TUI.Accent) {
		return errors.New("tui accent is invalid")
	}
	if !slices.Contains([]string{"", "comfortable", "compact"}, doc.TUI.Density) {
		return errors.New("tui density is invalid")
	}
	if err := validateTUI(Effective(doc)); err != nil {
		return err
	}
	return nil
}

func validateTUI(doc Document) error {
	home := map[string]bool{}
	for _, id := range homeIDs {
		home[id] = true
	}
	for _, values := range [][]string{doc.TUI.HomeOrder, doc.TUI.HomeHidden} {
		seen := map[string]bool{}
		for _, v := range values {
			if !home[v] {
				return fmt.Errorf("unknown home item %q", v)
			}
			if seen[v] {
				return fmt.Errorf("home item %q is repeated", v)
			}
			seen[v] = true
		}
	}
	for _, v := range doc.TUI.HomeHidden {
		if v == "commands" || v == "customize" {
			return fmt.Errorf("home item %q cannot be hidden", v)
		}
	}
	seenFavorites := map[string]bool{}
	for _, v := range doc.TUI.Favorites {
		if _, ok := doc.Shortcuts[v]; !ok {
			return fmt.Errorf("favorite %q is not a shortcut", v)
		}
		if seenFavorites[v] {
			return fmt.Errorf("favorite %q is repeated", v)
		}
		seenFavorites[v] = true
	}
	allowedColumns := map[string]map[string]bool{}
	for resource, ids := range columnIDs {
		allowedColumns[resource] = map[string]bool{}
		for _, id := range ids {
			allowedColumns[resource][id] = true
		}
	}
	for table, values := range doc.TUI.Columns {
		allowed, ok := allowedColumns[table]
		if !ok {
			return fmt.Errorf("unknown column group %q", table)
		}
		seen := map[string]bool{}
		for _, v := range values {
			if !allowed[v] {
				return fmt.Errorf("unknown %s column %q", table, v)
			}
			if seen[v] {
				return fmt.Errorf("%s column %q is repeated", table, v)
			}
			seen[v] = true
		}
	}
	seenPanels := map[string]bool{}
	for _, v := range doc.TUI.PreviewPanels {
		if !slices.Contains(previewPanelIDs, v) {
			return fmt.Errorf("unknown preview panel %q", v)
		}
		if seenPanels[v] {
			return fmt.Errorf("preview panel %q is repeated", v)
		}
		seenPanels[v] = true
	}
	actions := map[string]bool{"up": true, "down": true, "select": true, "back": true, "preview_open": true, "preview_background": true, "preview_tunnel": true, "preview_stop": true}
	used := map[string]string{"enter": "select", "esc": "back", "ctrl+c": "quit", "ctrl+e": "environment", "ctrl+f": "favorites", "ctrl+p": "commands", "ctrl+k": "up", "ctrl+n": "down", "up": "up", "down": "down"}
	for action, key := range doc.TUI.Keys {
		if !actions[action] || !validKey(key, strings.HasPrefix(action, "preview_")) {
			return fmt.Errorf("invalid key binding for %q", action)
		}
		if prior, ok := used[key]; ok && prior != action {
			return fmt.Errorf("key binding conflicts with %q", prior)
		}
		used[key] = action
	}
	return nil
}

func validKey(key string, printable bool) bool {
	if slices.Contains([]string{"enter", "esc", "ctrl+c", "up", "down", "left", "right", "pgup", "pgdown", "home", "end"}, key) || regexp.MustCompile(`^(ctrl|alt)\+([a-z0-9]|up|down|left|right)$`).MatchString(key) || regexp.MustCompile(`^f([1-9]|1[0-2])$`).MatchString(key) {
		return true
	}
	return printable && len([]rune(key)) == 1 && key[0] >= 0x21 && key[0] <= 0x7e
}

func Clone(doc Document) Document {
	data, _ := json.Marshal(doc)
	var out Document
	_ = json.Unmarshal(data, &out)
	return out
}

type contextKey struct{}

func WithContext(ctx context.Context, doc Document) context.Context {
	return context.WithValue(ctx, contextKey{}, Effective(doc))
}
func FromContext(ctx context.Context) Document {
	if ctx != nil {
		if doc, ok := ctx.Value(contextKey{}).(Document); ok {
			return Effective(doc)
		}
	}
	return Default()
}
