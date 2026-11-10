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

// fzfProjectPage is fzf's project page: the install step on a machine whose
// PATH holds none of the package managers install.sh's table knows.
const fzfProjectPage = "https://github.com/junegunn/fzf"

// fzfSteps is install.sh's _ccm_pkg_hint table for fzf: the first manager on
// PATH, in the installer's own probe order, names the command it takes.
var fzfSteps = []struct{ probe, step string }{
	{"brew", "brew install fzf"},
	{"apt-get", "sudo apt-get install -y fzf"},
	{"dnf", "sudo dnf install -y fzf"},
	{"apk", "sudo apk add fzf"},
	{"pacman", "sudo pacman -S fzf"},
	{"zypper", "sudo zypper install -y fzf"},
}

// FzfInstallStep is the one step that installs fzf on this machine (#387):
// the first package-manager command the PATH answers, else the project page —
// the same answer install.sh's own hint gives, so the picker's hint and the
// installer name one step for the same machine, never a command it cannot
// run (#369).
func FzfInstallStep() string {
	for _, pm := range fzfSteps {
		if _, err := exec.LookPath(pm.probe); err == nil {
			return pm.step
		}
	}
	return fzfProjectPage
}
