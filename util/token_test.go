package util

import (
	"testing"
)

func TestTokenShaped(t *testing.T) {
	const tail20 = "0123456789abcdefghij" // the shortest rest that is still a token
	cases := []struct {
		in   string
		want bool
		why  string
	}{
		{"sk-ant-oat01-" + tail20, true, "a real token's shape"},
		{"sk-ant-oat01-" + tail20 + "XYZ-_", true, "a longer token, all URL-safe characters"},
		{"abc", false, "the issue's junk paste"},
		{"sk-ant-oat01-" + tail20[:19], false, "a rest shorter than 20 characters is no token"},
		{"sk-ant-api03-" + tail20, false, "an API key is not a token"},
		{"sk-ant-oat01-" + tail20 + "!", false, "a character outside the token alphabet"},
		{" sk-ant-oat01-" + tail20, false, "a leading space is not the shape"},
		{"sk-ant-oat01-" + tail20 + " ", false, "a trailing space is not the shape"},
		{"", false, "empty"},
	}
	for _, c := range cases {
		if got := TokenShaped(c.in); got != c.want {
			t.Errorf("TokenShaped(%q) = %v, want %v (%s)", c.in, got, c.want, c.why)
		}
	}
}
