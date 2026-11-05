package main

import "testing"

func TestPowerShellPromptPattern(t *testing.T) {
	for _, text := range []string{
		"PS C:\\work> ",
		"PS C:\\中文 项目>\r\n",
		"PS C:\\work\\nested>   ",
	} {
		if !powershellPromptPattern.MatchString(text) {
			t.Errorf("prompt not matched: %q", text)
		}
	}
	for _, text := range []string{"PowerShell> ", "PS C:\\work", "xPS C:\\work>"} {
		if powershellPromptPattern.MatchString(text) {
			t.Errorf("non-prompt matched: %q", text)
		}
	}
}

func TestPowerShellPromptScreenFeedFragments(t *testing.T) {
	var screen screen
	screen.init(80, 24)
	screen.feed([]byte("\x1b[?25l\x1b[2J"))
	if powershellPromptPattern.MatchString(screen.text()) {
		t.Fatal("initial control sequences became a prompt")
	}
	for _, part := range []string{"PS C:\\中文 ", "项目>", "\r\n"} {
		screen.feed([]byte(part))
	}
	if screen.err != nil || !powershellPromptPattern.MatchString(screen.text()) {
		t.Fatalf("fragmented prompt not recognized: err=%v text=%q", screen.err, screen.text())
	}
	screen.feed([]byte("\x1b[2KPS C:\\work>\r\n"))
	if !powershellPromptPattern.MatchString(screen.text()) {
		t.Fatalf("redrawn prompt not recognized: %q", screen.text())
	}
}
