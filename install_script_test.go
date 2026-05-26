package tunnel_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0755); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestInstallScriptWindowsZipInstallsExe(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires sh-compatible shell")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	fakeBin := t.TempDir()
	installDir := t.TempDir()

	writeExecutable(t, filepath.Join(fakeBin, "uname"), `#!/bin/sh
if [ "$1" = "-s" ]; then
  echo MINGW64_NT
else
  echo x86_64
fi
`)
	writeExecutable(t, filepath.Join(fakeBin, "curl"), `#!/bin/sh
out=""
prev=""
for arg in "$@"; do
  if [ "$prev" = "-o" ]; then
    out="$arg"
  fi
  prev="$arg"
done
case "$*" in
  *api.github.com*) printf '{"tag_name":"v1.0.0"}' ;;
  *checksums.txt*) printf 'abc  tunnel_windows_amd64.zip\n' > "$out" ;;
  *) printf 'zip' > "$out" ;;
esac
`)
	writeExecutable(t, filepath.Join(fakeBin, "sha256sum"), `#!/bin/sh
printf 'abc  %s\n' "$1"
`)
	writeExecutable(t, filepath.Join(fakeBin, "unzip"), `#!/bin/sh
dest=""
prev=""
for arg in "$@"; do
  if [ "$prev" = "-d" ]; then
    dest="$arg"
  fi
  prev="$arg"
done
printf 'exe' > "$dest/tunnel.exe"
`)

	cmd := exec.Command("sh", "install.sh")
	cmd.Env = append(os.Environ(),
		"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"INSTALL_DIR="+installDir,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("install.sh failed: %v\n%s", err, out)
	}

	if _, err := os.Stat(filepath.Join(installDir, "tunnel.exe")); err != nil {
		t.Fatalf("expected tunnel.exe to be installed: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(installDir, "tunnel")); !os.IsNotExist(err) {
		t.Fatalf("did not expect extensionless tunnel binary, stat err=%v", err)
	}
}
