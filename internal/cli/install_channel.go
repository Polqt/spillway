package cli

import (
	"path/filepath"
	"regexp"
	"strings"
)

const releasesPage = "https://github.com/Polqt/spillway/releases"

var releaseVersionPattern = regexp.MustCompile(`^v\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$`)

type installChannel struct {
	name        string
	instruction string
}

func detectInstallChannel(goos, executable, version, moduleVersion, vcsModified, assets string) installChannel {
	path := strings.ToLower(filepath.ToSlash(filepath.Clean(executable)))
	switch {
	case goos == "linux" && path == "/spillway":
		return installChannel{name: "Docker image", instruction: "to upgrade offline, pull ghcr.io/polqt/spillway:X.Y.Z and update the tag in your docker run command or compose file"}
	case strings.Contains(path, "/scoop/apps/") || strings.Contains(path, "/scoop/shims/"):
		return installChannel{name: "scoop", instruction: "run scoop update spillway; scoop is not an official release channel"}
	case strings.HasPrefix(path, "/opt/homebrew/") || strings.HasPrefix(path, "/usr/local/cellar/") || strings.HasPrefix(path, "/home/linuxbrew/.linuxbrew/"):
		return installChannel{name: "Homebrew", instruction: "run brew upgrade spillway; Homebrew is not an official release channel"}
	case version == "dev" && releaseVersionPattern.MatchString(moduleVersion):
		return installChannel{name: "go install", instruction: "clone the chosen release tag, then run go install ./apps/spillway from the repository root"}
	case releaseVersionPattern.MatchString(version) && vcsModified == "false" && assets == "embedded":
		return installChannel{name: "release archive", instruction: "download a newer archive from " + releasesPage + ", verify checksums.txt and gh attestation verify as described in docs/install.md, then replace the printed executable path"}
	default:
		return installChannel{name: "local or unknown build", instruction: "rebuild from source or install a release archive from " + releasesPage}
	}
}
