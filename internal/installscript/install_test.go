package installscript

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const archiveName = "mcpd_linux_amd64.tar.gz"

func TestInstallScriptSelectsVerifiesAndInstallsRelease(t *testing.T) {
	fixture, fakeBin, logPath := installFixture(t, false)
	installDir := filepath.Join(t.TempDir(), "bin")
	command := exec.Command("sh", filepath.Join("..", "..", "install.sh"))
	command.Env = append(os.Environ(),
		"PATH="+fakeBin+":/usr/bin:/bin",
		"FIXTURE_DIR="+fixture,
		"COSIGN_LOG="+logPath,
		"MCPD_INSTALL_DIR="+installDir,
		"MCPD_SKIP_SETUP=1",
	)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("install script: %v\n%s", err, output)
	}
	target := filepath.Join(installDir, "mcpd")
	body, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "mcpd test binary\n" {
		t.Fatalf("installed binary = %q", body)
	}
	if info, err := os.Stat(target); err != nil || info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("installed mode: info=%v err=%v", info, err)
	}
	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(log), "@refs/tags/v9.9.9") || !strings.Contains(string(log), "checksums.txt.cosign") {
		t.Fatalf("cosign invocation = %q", log)
	}
}

func TestInstallScriptRefusesChecksumMismatch(t *testing.T) {
	fixture, fakeBin, logPath := installFixture(t, true)
	installDir := filepath.Join(t.TempDir(), "bin")
	command := exec.Command("sh", filepath.Join("..", "..", "install.sh"))
	command.Env = append(os.Environ(),
		"PATH="+fakeBin+":/usr/bin:/bin",
		"FIXTURE_DIR="+fixture,
		"COSIGN_LOG="+logPath,
		"MCPD_INSTALL_DIR="+installDir,
		"MCPD_SKIP_SETUP=1",
	)
	if output, err := command.CombinedOutput(); err == nil {
		t.Fatalf("install accepted a checksum mismatch:\n%s", output)
	}
	if _, err := os.Stat(filepath.Join(installDir, "mcpd")); !os.IsNotExist(err) {
		t.Fatalf("binary exists after refused install: %v", err)
	}
}

func TestInstallScriptWithoutTerminalDefersSetup(t *testing.T) {
	fixture, fakeBin, logPath := installFixture(t, false)
	installDir := filepath.Join(t.TempDir(), "bin")
	command := exec.Command("sh", filepath.Join("..", "..", "install.sh"))
	command.Env = append(os.Environ(),
		"PATH="+fakeBin+":/usr/bin:/bin",
		"FIXTURE_DIR="+fixture,
		"COSIGN_LOG="+logPath,
		"MCPD_INSTALL_DIR="+installDir,
	)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("noninteractive install: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "No interactive terminal is available") {
		t.Fatalf("noninteractive install output = %q", output)
	}
}

func installFixture(t *testing.T, corruptChecksum bool) (fixture, fakeBin, logPath string) {
	t.Helper()
	fixture = t.TempDir()
	archivePath := filepath.Join(fixture, archiveName)
	file, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	zipper := gzip.NewWriter(file)
	archive := tar.NewWriter(zipper)
	body := []byte("mcpd test binary\n")
	if err := archive.WriteHeader(&tar.Header{Name: "mcpd", Mode: 0o755, Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	if _, err := archive.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zipper.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	archiveBytes, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(archiveBytes))
	if corruptChecksum {
		digest = strings.Repeat("0", 64)
	}
	if err := os.WriteFile(filepath.Join(fixture, "checksums.txt"), []byte(digest+"  "+archiveName+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture, "checksums.txt.cosign"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	fakeBin = t.TempDir()
	writeExecutable(t, filepath.Join(fakeBin, "uname"), `#!/bin/sh
case "$1" in
  -s) printf 'Linux\n' ;;
  -m) printf 'x86_64\n' ;;
esac
`)
	writeExecutable(t, filepath.Join(fakeBin, "curl"), `#!/bin/sh
out=
url=
while [ "$#" -gt 0 ]; do
  case "$1" in
    -o) out=$2; shift 2 ;;
    -w) shift 2 ;;
    -*) shift ;;
    *) url=$1; shift ;;
  esac
done
case "$url" in
  */releases/latest) printf 'https://github.com/ahodges22/mcpd/releases/tag/v9.9.9' ;;
  *) cp "$FIXTURE_DIR/${url##*/}" "$out" ;;
esac
`)
	logPath = filepath.Join(t.TempDir(), "cosign.log")
	writeExecutable(t, filepath.Join(fakeBin, "cosign"), `#!/bin/sh
printf '%s\n' "$*" >"$COSIGN_LOG"
`)
	return fixture, fakeBin, logPath
}

func writeExecutable(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}
