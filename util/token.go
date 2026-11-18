package util

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/akleizor/ccmorph/ctxwindow"
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

var readSigninKeychain = ReadSigninKeychain

// StoredSignin reads the Claude-account sign-in without changing it.
func StoredSignin(home string) (access, refresh string, known bool) {
	raw, err := []byte(nil), error(nil)
	if runtime.GOOS == "darwin" {
		raw, err = ReadSigninCredentials(home, readSigninKeychain)
	}
	if runtime.GOOS != "darwin" || err != nil {
		// Claude Code falls back to the profile home's file where it has no keychain;
		// anything but a regular file there holds no sign-in to read.
		path := filepath.Join(home, ".credentials.json")
		if fi, e := os.Lstat(path); e != nil || !fi.Mode().IsRegular() {
			return "", "", false // never signed in, or kept elsewhere: only an emptied sign-in is known
		}
		if raw, err = os.ReadFile(path); err != nil {
			return "", "", false
		}
	}
	var doc struct {
		OAuth struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
		} `json:"claudeAiOauth"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return "", "", false
	}
	return doc.OAuth.AccessToken, doc.OAuth.RefreshToken, true
}

// ProbeToken is the never-failing check of a Claude OAuth token: the HTTP
// status of a Bearer-only listing request to Anthropic, "000" when nothing
// answered. The token reaches curl through its stdin config, never a command
// line; an x-api-key header is never sent, because the launch that spends this
// token sends none.
func ProbeToken(token string) (code string) {
	body, err := curlBearer(token, []string{"-sS", "--connect-timeout", "2", "--max-time", "5",
		"-w", `\n` + statusMarker + `%{http_code}`, "-H", "anthropic-version: 2023-06-01",
		ctxwindow.APIURL("https://api.anthropic.com", "/v1/models?limit=1000")}, io.Discard)
	_, code = splitProbeAnswer(body, err)
	return code
}
