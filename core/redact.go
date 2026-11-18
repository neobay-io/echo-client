package core

import (
	"fmt"
	"strings"
)

// RedactArgs returns a copy of args with values after sensitive flag names masked.
// Sensitive flags: --api-key, --api_key, --token, --secret, -k, etc.
func RedactArgs(args []string) []string {
	out := make([]string, len(args))
	copy(out, args)

	sensitiveFlags := []string{
		"--api-key", "--api_key", "--apikey",
		"--token", "--secret", "--password",
		"-k",
	}

	for i := 0; i < len(out); i++ {
		arg := strings.ToLower(out[i])

		// --flag=value format
		for _, f := range sensitiveFlags {
			if strings.HasPrefix(arg, f+"=") {
				out[i] = out[i][:strings.Index(out[i], "=")+1] + "***"
				break
			}
		}

		// --flag value format
		for _, f := range sensitiveFlags {
			if arg == f && i+1 < len(out) {
				out[i+1] = "***"
				i++
				break
			}
		}
	}
	redactPrompts(out)
	return out
}

// promptFlags carry the user's prompt as their value (`agy -p <prompt>`,
// `gemini -p <prompt>`, `--prompt=…`). Logs keep only its length: prompts can
// hold pasted secrets. -p and --prompt always take the next argument, even one
// starting with "-" (a prompt like "- my token is …"); --print is a switch in
// some CLIs (claude, cursor, qoder), so a flag after it is left alone.
var promptFlags = []struct {
	name       string
	takesValue bool
}{
	{"-p", true},
	{"--prompt", true},
	{"--print", false},
}

func redactPrompts(args []string) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		for _, f := range promptFlags {
			if v, ok := strings.CutPrefix(arg, f.name+"="); ok {
				args[i] = f.name + "=" + promptPlaceholder(v)
				break
			}
			if arg == f.name && i+1 < len(args) && (f.takesValue || !strings.HasPrefix(args[i+1], "-")) {
				args[i+1] = promptPlaceholder(args[i+1])
				i++
				break
			}
		}
	}
}

func promptPlaceholder(prompt string) string {
	return fmt.Sprintf("<prompt: %d chars>", len([]rune(prompt)))
}
