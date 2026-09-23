package main

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/preferences"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

var preferenceTemplate = regexp.MustCompile(`\{(args|[1-9][0-9]*)\}`)
var numericShortcut = regexp.MustCompile(`^[0-9]+$`)

type preferenceResolution struct {
	Arguments []string `json:"arguments"`
	Shortcut  string   `json:"shortcut,omitempty"`
	Command   string   `json:"command,omitempty"`
	Defaults  []string `json:"defaults,omitempty"`
}

func preparePreferences(root *cobra.Command, args []string, ctx context.Context) ([]string, context.Context, error) {
	if preferenceFlag(args, "no-customization") || preferenceCustomizeInvocation(root, args) || preferenceRecoveryInvocation(root, args) {
		resolved, err := normalizeRawGlobalFlags(root, args)
		return resolved, preferences.WithContext(ctx, preferences.Default()), err
	}
	configPath := preferenceFlagValue(root, args, "config")
	path, err := preferences.Path(configPath)
	if err != nil {
		return nil, ctx, err
	}
	doc, err := preferences.Load(path)
	if err != nil {
		return nil, ctx, err
	}
	if err := validatePreferencesCommands(root, doc); err != nil {
		return nil, ctx, err
	}
	resolved, _, err := resolvePreferences(root, doc, args)
	if err != nil {
		return nil, ctx, err
	}
	resolved, err = normalizeRawGlobalFlags(root, resolved)
	return resolved, preferences.WithContext(ctx, doc), err
}

func explainPreferences(root *cobra.Command, doc preferences.Document, args []string) (preferenceResolution, error) {
	_, result, err := resolvePreferences(root, doc, args)
	return result, err
}

func validatePreferencesCommands(root *cobra.Command, doc preferences.Document) error {
	root.InitDefaultHelpCmd()
	root.InitDefaultCompletionCmd()
	reserved := map[string]bool{}
	for _, command := range root.Commands() {
		reserved[command.Name()] = true
		for _, alias := range command.Aliases {
			reserved[alias] = true
		}
	}
	for name, shortcut := range doc.Shortcuts {
		if reserved[name] {
			return fmt.Errorf("shortcut %q conflicts with a built-in command", name)
		}
		if numericShortcut.MatchString(name) {
			return fmt.Errorf("shortcut %q conflicts with port syntax", name)
		}
		if len(shortcut.Command) == 0 {
			return fmt.Errorf("shortcut %q has no command", name)
		}
		command, remaining, err := root.Find(shortcut.Command)
		if err != nil || command == root || len(remaining) != 0 || !command.Runnable() || command.Hidden || strings.HasPrefix(command.Name(), "__") {
			return fmt.Errorf("shortcut %q does not name a configurable built-in command", name)
		}
		canonical := strings.Fields(strings.TrimPrefix(command.CommandPath(), root.Name()+" "))
		if !slices.Equal(canonical, shortcut.Command) {
			return fmt.Errorf("shortcut %q must name a canonical command", name)
		}
		if err := validateShortcutTemplates(name, shortcut.Args); err != nil {
			return err
		}
	}
	for path, values := range doc.Defaults {
		command, remaining, err := root.Find(strings.Fields(path))
		if err != nil || command == root || len(remaining) != 0 || !command.Runnable() || command.Hidden || strings.HasPrefix(command.Name(), "__") {
			return fmt.Errorf("defaults name unknown command %q", path)
		}
		if strings.TrimPrefix(command.CommandPath(), root.Name()+" ") != path {
			return fmt.Errorf("defaults must name canonical command %q", path)
		}
		for name := range values {
			flag := command.Flags().Lookup(name)
			if flag == nil || !preferenceConfigurableFlag(flag) {
				return fmt.Errorf("flag --%s cannot be configured for %q", name, path)
			}
			if err := validatePreferenceFlagValue(flag, values[name]); err != nil {
				return fmt.Errorf("default --%s for %q is invalid", name, path)
			}
		}
	}
	return nil
}

func resolvePreferences(root *cobra.Command, doc preferences.Document, args []string) ([]string, preferenceResolution, error) {
	work, tail := args, []string(nil)
	if dash := slices.Index(args, "--"); dash >= 0 {
		work, tail = args[:dash], slices.Clone(args[dash:])
	}
	result := preferenceResolution{Arguments: slices.Clone(work)}
	finish := func() ([]string, preferenceResolution, error) {
		result.Arguments = append(result.Arguments, tail...)
		return result.Arguments, result, nil
	}
	index := preferenceCommandIndex(root, work)
	if index < 0 {
		return finish()
	}
	name := work[index]
	if port, err := strconv.ParseUint(name, 10, 16); err == nil && port > 0 {
		if doc.PortAction == "tunnel" {
			result.Arguments = append(append(slices.Clone(work[:index]), "tunnel", "create", "--port", name), work[index+1:]...)
			result.Shortcut, result.Command = name, "tunnel create"
		} else {
			result.Arguments = append(append(slices.Clone(work[:index]), "preview", name), work[index+1:]...)
			result.Shortcut, result.Command = name, "preview"
		}
	} else if shortcut, ok := doc.Shortcuts[name]; ok {
		target, _, _ := root.Find(shortcut.Command)
		expanded, err := expandPreferenceShortcut(name, shortcut, target, work[index+1:])
		if err != nil {
			return nil, result, err
		}
		result.Arguments = append(slices.Clone(work[:index]), expanded...)
		result.Shortcut, result.Command = name, strings.Join(shortcut.Command, " ")
	}
	command, _, err := root.Find(result.Arguments)
	if err != nil || command == nil {
		return finish()
	}
	path := strings.TrimPrefix(command.CommandPath(), root.Name()+" ")
	defaults := doc.Defaults[path]
	if len(defaults) == 0 {
		return finish()
	}
	names := make([]string, 0, len(defaults))
	for name := range defaults {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		flag := command.Flags().Lookup(name)
		if flag == nil || preferenceFlagExplicit(command, result.Arguments, flag) {
			continue
		}
		result.Arguments = append(result.Arguments, "--"+name+"="+defaults[name])
		result.Defaults = append(result.Defaults, name)
	}
	return finish()
}

func expandPreferenceShortcut(name string, shortcut preferences.Shortcut, command *cobra.Command, supplied []string) ([]string, error) {
	positional := []int(nil)
	indexed := false
	for _, template := range shortcut.Args {
		for _, token := range preferenceTemplate.FindAllString(template, -1) {
			if token != "{args}" {
				indexed = true
			}
		}
	}
	if indexed {
		var err error
		positional, err = preferencePositionalIndexes(command, supplied)
		if err != nil {
			return nil, err
		}
	}
	used := map[int]bool{}
	argsToken := false
	result := slices.Clone(shortcut.Command)
	for _, template := range shortcut.Args {
		for _, token := range preferenceTemplate.FindAllString(template, -1) {
			key := token[1 : len(token)-1]
			if key == "args" {
				continue
			}
			n, _ := strconv.Atoi(key)
			if n < 1 || n > len(positional) {
				return nil, fmt.Errorf("shortcut %q is missing a required argument", name)
			}
			used[positional[n-1]] = true
		}
	}
	for _, template := range shortcut.Args {
		if strings.Contains(template, "{") && !preferenceTemplate.MatchString(template) {
			return nil, fmt.Errorf("shortcut %q has a malformed template", name)
		}
		if template == "{args}" {
			if argsToken {
				return nil, fmt.Errorf("shortcut %q repeats {args}", name)
			}
			argsToken = true
			for i, v := range supplied {
				if !used[i] {
					result = append(result, v)
				}
			}
			continue
		}
		expanded := preferenceTemplate.ReplaceAllStringFunc(template, func(token string) string {
			key := token[1 : len(token)-1]
			if key == "args" {
				return ""
			}
			n, _ := strconv.Atoi(key)
			return supplied[positional[n-1]]
		})
		result = append(result, expanded)
	}
	for i := range supplied {
		if !used[i] && !argsToken {
			return nil, fmt.Errorf("shortcut %q does not accept all supplied arguments", name)
		}
	}
	return result, nil
}

func preferencePositionalIndexes(command *cobra.Command, supplied []string) ([]int, error) {
	result := []int{}
	raw := command != nil && command.DisableFlagParsing
	for i := 0; i < len(supplied); i++ {
		value := supplied[i]
		if value == "--" {
			for j := i + 1; j < len(supplied); j++ {
				result = append(result, j)
			}
			break
		}
		if raw && strings.HasPrefix(value, "-") && value != "-" {
			consumes, known := rawToolOption(command.Name(), value)
			if !known {
				return nil, fmt.Errorf("shortcut cannot determine whether %s option %q consumes a value; use a shortcut without indexed operands", command.Name(), value)
			}
			if consumes && !strings.Contains(value, "=") {
				if i+1 >= len(supplied) {
					return nil, fmt.Errorf("%s option %q requires a value", command.Name(), value)
				}
				i++
			}
			continue
		}
		if strings.HasPrefix(value, "--") {
			name := strings.TrimPrefix(strings.SplitN(value, "=", 2)[0], "--")
			if flag := command.Flags().Lookup(name); flag != nil && flag.NoOptDefVal == "" && !strings.Contains(value, "=") && i+1 < len(supplied) {
				i++
			}
			continue
		}
		if strings.HasPrefix(value, "-") && value != "-" {
			name := strings.TrimPrefix(value, "-")
			if flag := command.Flags().ShorthandLookup(name); flag != nil && flag.NoOptDefVal == "" && i+1 < len(supplied) {
				i++
			}
			continue
		}
		result = append(result, i)
	}
	return result, nil
}

func rawToolOption(tool, value string) (bool, bool) {
	name := strings.SplitN(value, "=", 2)[0]
	longValue := map[string]bool{"--exclude": true, "--include": true, "--filter": true, "--rsh": true, "--rsync-path": true, "--files-from": true, "--chmod": true, "--max-size": true, "--min-size": true, "--temp-dir": true, "--backup-dir": true, "--suffix": true, "--timeout": true, "--contimeout": true, "--port": true}
	longBool := map[string]bool{"--archive": true, "--recursive": true, "--verbose": true, "--compress": true, "--progress": true, "--delete": true, "--partial": true, "--checksum": true, "--dry-run": true, "--human-readable": true, "--itemize-changes": true, "--protect-args": true}
	if strings.HasPrefix(value, "--") {
		if longValue[name] {
			return !strings.Contains(value, "="), true
		}
		return false, longBool[name]
	}
	valueOpts := map[string]string{"scp": "cFiJoPSX", "sftp": "BbcDFiJloPRSsX", "rsync": "eBf"}[tool]
	boolOpts := map[string]string{"scp": "346Cprqv", "sftp": "46Aaq", "rsync": "avzrtpoglncPhinqs"}[tool]
	letters := strings.TrimPrefix(value, "-")
	if letters == "" {
		return false, false
	}
	for index, r := range letters {
		if strings.ContainsRune(valueOpts, r) {
			return index == len([]rune(letters))-1, true
		}
		if !strings.ContainsRune(boolOpts, r) {
			return false, false
		}
	}
	return false, true
}

func normalizeRawGlobalFlags(root *cobra.Command, args []string) ([]string, error) {
	dash := slices.Index(args, "--")
	if dash < 0 {
		dash = len(args)
	}
	commandIndex := preferenceCommandIndex(root, args[:dash])
	if commandIndex < 0 {
		return slices.Clone(args), nil
	}
	command, _, err := root.Find(args[commandIndex:dash])
	if err != nil || command == nil || !command.DisableFlagParsing {
		return slices.Clone(args), nil
	}
	allowed := map[string]bool{"config": true, "server": true, "json": true, "no-customization": true}
	raw := []string{args[commandIndex]}
	for i := 0; i < dash; i++ {
		if i == commandIndex {
			continue
		}
		value := args[i]
		name := strings.TrimPrefix(strings.SplitN(value, "=", 2)[0], "--")
		flag := root.PersistentFlags().Lookup(name)
		if strings.HasPrefix(value, "--") && flag != nil && allowed[name] {
			flagValue := ""
			if strings.Contains(value, "=") {
				flagValue = strings.SplitN(value, "=", 2)[1]
			} else if flag.NoOptDefVal != "" {
				flagValue = flag.NoOptDefVal
			} else {
				if i+1 >= dash || i+1 == commandIndex {
					return nil, fmt.Errorf("--%s requires a value", name)
				}
				i++
				flagValue = args[i]
			}
			if err := root.PersistentFlags().Set(name, flagValue); err != nil {
				return nil, fmt.Errorf("invalid --%s value", name)
			}
			continue
		}
		raw = append(raw, value)
		if i > commandIndex {
			if takesValue, known := rawToolOption(command.Name(), value); known && takesValue && i+1 < dash {
				i++
				raw = append(raw, args[i])
			}
		}
	}
	raw = append(raw, args[dash:]...)
	return raw, nil
}

func validateShortcutTemplates(name string, values []string) error {
	argsTokens := 0
	for _, value := range values {
		matches := preferenceTemplate.FindAllString(value, -1)
		stripped := preferenceTemplate.ReplaceAllString(value, "")
		if strings.ContainsAny(stripped, "{}") {
			return fmt.Errorf("shortcut %q has a malformed template", name)
		}
		for _, token := range matches {
			key := token[1 : len(token)-1]
			if key == "args" {
				if value != "{args}" {
					return fmt.Errorf("shortcut %q must use {args} as a whole argument", name)
				}
				argsTokens++
				continue
			}
			n, _ := strconv.Atoi(key)
			if n > 64 {
				return fmt.Errorf("shortcut %q argument template exceeds the limit", name)
			}
		}
	}
	if argsTokens > 1 {
		return fmt.Errorf("shortcut %q repeats {args}", name)
	}
	return nil
}

var forbiddenPreferenceFlags = map[string]bool{"config": true, "server": true, "json": true, "yes": true, "confirm": true, "confirmation": true, "token": true, "password": true, "env": true, "no-customization": true, "force": true}

func preferenceConfigurableFlag(flag *pflag.Flag) bool {
	return flag != nil && !flag.Hidden && !forbiddenPreferenceFlags[flag.Name] && !strings.Contains(flag.Name, "auth") && !strings.Contains(flag.Name, "secret") && !strings.Contains(flag.Name, "credential") && !strings.Contains(flag.Name, "password") && !strings.Contains(flag.Name, "token") && !strings.Contains(flag.Name, "confirm") && !strings.Contains(flag.Name, "config")
}

func validatePreferenceFlagValue(flag *pflag.Flag, value string) error {
	switch flag.Value.Type() {
	case "bool":
		_, err := strconv.ParseBool(value)
		return err
	case "duration":
		_, err := time.ParseDuration(value)
		return err
	case "int", "int64":
		_, err := strconv.ParseInt(value, 10, 64)
		return err
	case "int32":
		_, err := strconv.ParseInt(value, 10, 32)
		return err
	case "uint", "uint64":
		_, err := strconv.ParseUint(value, 10, 64)
		return err
	case "uint16":
		_, err := strconv.ParseUint(value, 10, 16)
		return err
	case "uint32":
		_, err := strconv.ParseUint(value, 10, 32)
		return err
	case "float32", "float64":
		_, err := strconv.ParseFloat(value, 64)
		return err
	case "string", "stringArray", "stringSlice":
		return nil
	default:
		return errors.New("unsupported configurable flag type")
	}
}

func preferenceFlagExplicit(command *cobra.Command, args []string, flag *pflag.Flag) bool {
	for _, value := range args {
		if value == "--" {
			break
		}
		if value == "--"+flag.Name || strings.HasPrefix(value, "--"+flag.Name+"=") || flag.Shorthand != "" && (value == "-"+flag.Shorthand || strings.HasPrefix(value, "-"+flag.Shorthand+"=") || flag.NoOptDefVal == "" && strings.HasPrefix(value, "-"+flag.Shorthand) && len(value) > len(flag.Shorthand)+1) {
			return true
		}
	}
	return false
}

func preferenceFlag(args []string, name string) bool {
	requested := false
	for _, v := range args {
		if v == "--" {
			break
		}
		if v == "--"+name || v == "--"+name+"=true" {
			requested = true
		} else if v == "--"+name+"=false" {
			requested = false
		}
	}
	return requested
}
func preferenceFlagValue(root *cobra.Command, args []string, name string) string {
	flag := root.PersistentFlags().Lookup(name)
	result := ""
	for i := 0; i < len(args); i++ {
		v := args[i]
		if v == "--" {
			break
		}
		if strings.HasPrefix(v, "--"+name+"=") {
			result = strings.TrimPrefix(v, "--"+name+"=")
			continue
		}
		if v == "--"+name && i+1 < len(args) {
			i++
			result = args[i]
			continue
		}
		if flag != nil && flag.Shorthand != "" && v == "-"+flag.Shorthand && i+1 < len(args) {
			i++
			result = args[i]
		}
	}
	return result
}
func preferenceCommandIndex(root *cobra.Command, args []string) int {
	valueFlags := map[string]bool{}
	root.PersistentFlags().VisitAll(func(f *pflag.Flag) {
		valueFlags["--"+f.Name] = f.NoOptDefVal == ""
		if f.Shorthand != "" {
			valueFlags["-"+f.Shorthand] = f.NoOptDefVal == ""
		}
	})
	for i := 0; i < len(args); i++ {
		v := args[i]
		if v == "--" {
			return -1
		}
		if strings.HasPrefix(v, "-") {
			if valueFlags[v] {
				i++
			}
			continue
		}
		return i
	}
	return -1
}
func preferenceCustomizeInvocation(root *cobra.Command, args []string) bool {
	i := preferenceCommandIndex(root, args)
	return i >= 0 && args[i] == "config" && i+1 < len(args) && args[i+1] == "customize"
}

func preferenceRecoveryInvocation(root *cobra.Command, args []string) bool {
	for _, value := range args {
		if value == "--" {
			break
		}
		if value == "--help" || value == "-h" || value == "--version" || value == "-v" {
			return true
		}
	}
	i := preferenceCommandIndex(root, args)
	if i < 0 {
		return false
	}
	// Enrollment guidance must remain available even when local preferences are broken.
	return args[i] == "help" || args[i] == "login" ||
		(args[i] == "auth" && i+1 < len(args) && (args[i+1] == "login" || args[i+1] == "switch"))
}
