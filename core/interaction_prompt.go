package core

import "strings"

// interactionRequestMarker separates InteractionOptionsPrompt from the user's
// request in prompts built by WrapInteractionOptions.
const interactionRequestMarker = "\n\nUser request:\n"

// interactionReminderMarker introduces InteractionOptionsPrompt after the
// user's request in prompts built by AppendInteractionOptions.
const interactionReminderMarker = "\n\n(echo-client reminder)\n"

// WrapInteractionOptions prefixes a prompt with InteractionOptionsPrompt, for
// agents that have no system prompt flag.
func WrapInteractionOptions(prompt string) string {
	hint := strings.TrimSpace(InteractionOptionsPrompt())
	if hint == "" {
		return prompt
	}
	return hint + interactionRequestMarker + prompt
}

// AppendInteractionOptions puts InteractionOptionsPrompt after the prompt, so
// agents that store a truncated preview of the first message (agy) still
// show the user's words.
func AppendInteractionOptions(prompt string) string {
	hint := strings.TrimSpace(InteractionOptionsPrompt())
	if hint == "" {
		return prompt
	}
	return prompt + interactionReminderMarker + hint
}

// StripInteractionOptions returns the user's request from a prompt built by
// WrapInteractionOptions or AppendInteractionOptions, so session lists and
// history show what the user wrote. Other text is returned unchanged.
func StripInteractionOptions(text string) string {
	if request, _, ok := strings.Cut(text, interactionReminderMarker); ok {
		return request
	}
	// A preview truncated inside the marker ("…\n\n(echo-cl").
	for n := len(interactionReminderMarker) - 1; n >= 3; n-- {
		if strings.HasSuffix(text, interactionReminderMarker[:n]) {
			return text[:len(text)-n]
		}
	}
	hint := strings.TrimSpace(InteractionOptionsPrompt())
	if hint == "" || !strings.HasPrefix(strings.TrimSpace(text), hint) {
		return text
	}
	if _, request, ok := strings.Cut(text, interactionRequestMarker); ok {
		return request
	}
	return text
}
