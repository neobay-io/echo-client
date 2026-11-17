package core

import "testing"

// P8: every builtin command has a menu description (attach, loop, tts and
// agent-upgrade had none, so Telegram showed bare names or dropped them).
func TestEveryBuiltinCommandHasADescription(t *testing.T) {
	for _, lang := range []Language{LangEnglish, LangChinese} {
		e := NewEngine("p8", &stubAgent{}, nil, "", lang)
		for _, c := range e.GetAllCommands() {
			if c.Description == "" || c.Description == c.Command {
				t.Errorf("%s: /%s has no description", lang, c.Command)
			}
		}
		if got := e.builtinCommandDescription("agent-upgrade"); got == "" {
			t.Errorf("%s: agent-upgrade description not found under agent_upgrade", lang)
		}
	}
}

// P8: the picker lists the current model even when the agent does not, on
// every platform (it was Feishu-only).
func TestModelPickerShowsCurrentModelEverywhere(t *testing.T) {
	available := []ModelOption{{Name: "a"}, {Name: "b"}}
	for _, platform := range []string{"telegram", "feishu", "slack"} {
		got := modelPickerOptions(platform, "codex", "custom-model", available)
		if len(got) != 3 || got[0].Name != "custom-model" {
			t.Errorf("%s: options = %+v", platform, got)
		}
		if got := modelPickerOptions(platform, "codex", "b", available); len(got) != 2 {
			t.Errorf("%s: listed current model duplicated: %+v", platform, got)
		}
	}
	if len(available) != 2 {
		t.Fatal("input slice modified")
	}
}
