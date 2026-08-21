package mcpdcmd

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ahodges22/mcpd/internal/update"
)

type fakeUpdater struct {
	checkResult *update.CheckResult
	checkErr    error
	updateTag   string
	updateErr   error
	updateOpts  update.Options
	updates     int
}

func (updater *fakeUpdater) Check(context.Context) (*update.CheckResult, error) {
	return updater.checkResult, updater.checkErr
}

func (updater *fakeUpdater) Update(_ context.Context, opts update.Options) (string, error) {
	updater.updates++
	updater.updateOpts = opts
	return updater.updateTag, updater.updateErr
}

func TestRunUpdateCheckDoesNotInstall(t *testing.T) {
	updater := &fakeUpdater{checkResult: &update.CheckResult{Current: "v1.0.0", Latest: "v1.1.0", Outdated: true}}
	var output strings.Builder

	if err := runUpdateWith(t.Context(), []string{"--check"}, updater, &output, &output, "linux"); err != nil {
		t.Fatal(err)
	}
	if updater.updates != 0 {
		t.Fatalf("Update called %d times during check", updater.updates)
	}
	if got := output.String(); !strings.Contains(got, "v1.0.0") || !strings.Contains(got, "v1.1.0") {
		t.Fatalf("check output = %q", got)
	}
}

func TestRunUpdateInstallsRequestedVersionAndPrintsLinuxRestart(t *testing.T) {
	updater := &fakeUpdater{updateTag: "v1.2.3"}
	var output strings.Builder

	if err := runUpdateWith(t.Context(), []string{"--version", "1.2.3"}, updater, &output, &output, "linux"); err != nil {
		t.Fatal(err)
	}
	if updater.updateOpts.Version != "1.2.3" {
		t.Fatalf("Update options = %+v", updater.updateOpts)
	}
	if got := output.String(); !strings.Contains(got, "Installed v1.2.3") || !strings.Contains(got, "systemctl --user restart mcpd") {
		t.Fatalf("update output = %q", got)
	}
}

func TestRunUpdatePrintsMacOSRestart(t *testing.T) {
	updater := &fakeUpdater{updateTag: "v1.2.3"}
	var output strings.Builder

	if err := runUpdateWith(t.Context(), nil, updater, &output, &output, "darwin"); err != nil {
		t.Fatal(err)
	}
	if got := output.String(); !strings.Contains(got, "launchctl kickstart -k gui/$(id -u)/dev.mcpd.daemon") {
		t.Fatalf("update output = %q", got)
	}
}

func TestRunUpdateReturnsUpdaterError(t *testing.T) {
	want := errors.New("failed")
	if err := runUpdateWith(t.Context(), nil, &fakeUpdater{updateErr: want}, &strings.Builder{}, &strings.Builder{}, "linux"); !errors.Is(err, want) {
		t.Fatalf("runUpdateWith() error = %v, want %v", err, want)
	}
}
