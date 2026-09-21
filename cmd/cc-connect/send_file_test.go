package main

import "testing"

func TestResolveSendFileTargetUsesAgentEnvironment(t *testing.T) {
	t.Setenv("CC_PROJECT", " diffus-intel ")
	t.Setenv("CC_SESSION_KEY", " feishu:group:user ")

	project, sessionKey := resolveSendFileTarget("", "")
	if project != "diffus-intel" {
		t.Fatalf("project = %q, want diffus-intel", project)
	}
	if sessionKey != "feishu:group:user" {
		t.Fatalf("sessionKey = %q, want feishu:group:user", sessionKey)
	}
}

func TestResolveSendFileTargetExplicitValuesOverrideEnvironment(t *testing.T) {
	t.Setenv("CC_PROJECT", "env-project")
	t.Setenv("CC_SESSION_KEY", "env-session")

	project, sessionKey := resolveSendFileTarget(" explicit-project ", " explicit-session ")
	if project != "explicit-project" {
		t.Fatalf("project = %q, want explicit-project", project)
	}
	if sessionKey != "explicit-session" {
		t.Fatalf("sessionKey = %q, want explicit-session", sessionKey)
	}
}
