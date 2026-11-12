package util

import (
	"io"
	"os"
	"path/filepath"
	"strings"
)

// TokenShaped reports whether s has the shape every Claude OAuth token has:
// "sk-ant-oat01-" followed by a long run of URL-safe characters. It is the
// paste-time judgment — the one that needs no network; only Anthropic can
// judge a well-shaped token.
func TokenShaped(s string) bool {
	const prefix = "sk-ant-oat01-"
	if !strings.HasPrefix(s, prefix) || len(s) < len(prefix)+20 {
		return false
	}
	for _, c := range s[len(prefix):] {
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// StoredSignin is the Claude-account sign-in a profile home keeps in
// .credentials.json, "" when the home holds none there. Claude Code's own
// secure storage is the other place a sign-in can live (macOS keeps it in the
// login keychain), and a sign-in it does not hand over is not doctor's to guess
// at. It never writes, so doctor can call it.
func StoredSignin(home string) string {
	path := filepath.Join(home, ".credentials.json")
	// Anything but a regular file — missing, symlinked, a directory — holds no
	// sign-in to read.
	if fi, err := os.Lstat(path); err != nil || !fi.Mode().IsRegular() {
		return ""
	}
	doc, err := loadObj(path, nil)
	if err != nil || doc == nil {
		return ""
	}
	v, ok := doc.Get("claudeAiOauth")
	if !ok {
		return ""
	}
	oauth := asObj(v)
	if oauth == nil {
		return ""
	}
	av, _ := oauth.Get("accessToken")
	s, _ := str(av)
	return s
}

// ProbeToken is the never-failing check of a Claude OAuth token: the HTTP
// status of a Bearer-only listing request to Anthropic, "000" when nothing
// answered. The token reaches curl through its stdin config, never a command
// line; an x-api-key header is never sent, because the launch that spends this
// token sends none.
func ProbeToken(token string) (code string) {
	body, err := curlBearer(token, []string{"-sS", "--connect-timeout", "2", "--max-time", "5",
		"-w", `\n` + statusMarker + `%{http_code}`, "-H", "anthropic-version: 2023-06-01",
		apiURL("https://api.anthropic.com", "/v1/models?limit=1000")}, io.Discard)
	_, code = splitProbeAnswer(body, err)
	return code
}
