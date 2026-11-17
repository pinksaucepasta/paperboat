package main

// helpText is the reference text of one command: what it does, how it behaves, examples.
// An empty Long keeps the command's own Long and only adds the examples.
type helpText struct {
	Long    string
	Example string
}

// helpTextGroups holds one map per command group, keyed by command path. A path is
// owned by one map and never also by commandHelpDetails (TestHelpTextOwnership).
var helpTextGroups = []map[string]helpText{teamHelpTexts, machineHelpTexts, filesHelpTexts, tunnelHelpTexts, accessHelpTexts, coreHelpTexts}

func lookupHelpText(path string) (helpText, bool) {
	for _, group := range helpTextGroups {
		if text, ok := group[path]; ok {
			return text, true
		}
	}
	return helpText{}, false
}

// confirmNote explains the two-step confirmation every destructive command shares (confirmMutation).
const confirmNote = `Confirmation is two steps. Run the command without --confirm: it changes nothing, shows what
would happen and prints a six-character code (exit status 2). Run the same command again with
--confirm CODE within 5 minutes. The code works once and only for that exact command and target.`
