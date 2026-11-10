package util

import "os/exec"

// clipProjectPage is cliproxyapi's project page: the install step on a machine
// with no Homebrew, the same place install.sh sends a person there.
const clipProjectPage = "https://github.com/router-for-me/CLIProxyAPI"

// ClipInstallStep is the one step that installs cliproxyapi on this machine
// (#369): the brew command where Homebrew is on PATH, else the project page —
// the same answer install.sh gives, so the installer, `ccmorph add`, `ccmorph
// run` and `ccmorph doctor` name one step for the same machine, never a
// command it cannot run.
func ClipInstallStep() string {
	if _, err := exec.LookPath("brew"); err == nil {
		return "brew install cliproxyapi"
	}
	return clipProjectPage
}
