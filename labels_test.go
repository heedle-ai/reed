package reed_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/testsuite"

	"github.com/heedle-ai/reed"
	"github.com/heedle-ai/shuttle"
	"github.com/heedle-ai/shuttle/plan"
)

// runArgv is the arguments of the podman run a fake podman logged: one
// per line, the run failing so nothing more happens.
func runArgv(t *testing.T) (podman, log string) {
	dir := t.TempDir()
	log = filepath.Join(dir, "log")
	podman = filepath.Join(dir, "podman")
	os.WriteFile(podman, []byte("#!/bin/sh\ncase \"$1\" in\n  run) for a in \"$@\"; do printf '%s\\n' \"$a\"; done >> "+log+"; echo 'Error: no' >&2; exit 125;;\n  ps) echo '[]';;\nesac\n"), 0o755)
	return podman, log
}

// labelsOf are the heedle labels in argv, and whether each comes after
// every fixed flag (the last is --log-driver=none, then --network) and
// before the image.
func labelsOf(argv []string) (labels []string, placed bool) {
	last, img := -1, -1
	for i, a := range argv {
		switch {
		case a == "--log-driver=none" || strings.HasPrefix(a, "--network="):
			last = i
		case a == img2 && img < 0:
			img = i
		}
	}
	placed = last > 0 && img > last
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == "--label" && strings.HasPrefix(argv[i+1], "heedle.") {
			labels = append(labels, argv[i+1])
			placed = placed && i > last && i < img
		}
	}
	return labels, placed
}

var img2 = strings.Repeat("ab", 32)

// TestInv_BrowserContainersAreLabelled: every browse and act container
// carries heedle.kind (browse or act) and its visit's run labels, as
// --label flags after every fixed flag and before the image; the old
// reed labels stay (REED-500).
func TestInv_BrowserContainersAreLabelled(t *testing.T) {
	run := map[string]string{reed.LabelWorkflow: "loom-abc/plan", reed.LabelRun: "6f1c2a7d-3b8e", reed.LabelStep: "n4"}
	for _, c := range []struct {
		kind   string
		labels map[string]string
		want   []string
	}{
		{"browse", nil, []string{"heedle.kind=browse"}},
		{"browse", run, []string{"heedle.kind=browse", "heedle.workflow=loom-abc/plan", "heedle.run=6f1c2a7d-3b8e", "heedle.step=n4"}},
		{"act", nil, []string{"heedle.kind=act"}},
		{"act", run, []string{"heedle.kind=act", "heedle.workflow=loom-abc/plan", "heedle.run=6f1c2a7d-3b8e", "heedle.step=n4"}},
	} {
		podman, log := runArgv(t)
		b := reed.Browser{Podman: podman, Image: img2, Network: "net1"}
		v := reed.Visit{URL: "https://a.example/", Labels: c.labels}
		var err error
		if c.kind == "browse" {
			_, err = b.Open(context.Background(), v)
		} else {
			_, err = b.Start(context.Background(), v)
		}
		if err == nil {
			t.Fatal("the fake podman's run should fail")
		}
		out, _ := os.ReadFile(log)
		argv := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
		got, placed := labelsOf(argv)
		if !reflect.DeepEqual(got, c.want) || !placed {
			t.Errorf("%s with %v: labels %q (placed: %v), want %q\n%q", c.kind, c.labels, got, placed, c.want, argv)
		}
		if old := map[string]string{"browse": reed.Label, "act": reed.ActLabel}[c.kind]; !strings.Contains(string(out), "--label\n"+old+"\n") {
			t.Errorf("%s lost its %s label", c.kind, old)
		}
	}
}

// TestInv_VisitLabelsCantInjectFlags: a visit's labels are only the run
// labels, each a plain value; anything else is refused as ErrInvalid
// before podman runs, and reed alone sets heedle.kind (REED-501).
func TestInv_VisitLabelsCantInjectFlags(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran")
	podman := filepath.Join(dir, "podman")
	os.WriteFile(podman, []byte("#!/bin/sh\n[ \"$1\" = ps ] && { echo '[]'; exit 0; }\ntouch "+marker+"\nexit 125\n"), 0o755)
	b := reed.Browser{Podman: podman, Image: img2, Network: "net1"}
	for _, l := range []map[string]string{
		{reed.LabelKind: "agent"}, {"other": "x"}, {"--privileged": "x"}, {reed.LabelStep: "--privileged"},
		{reed.LabelStep: "-v"}, {reed.LabelWorkflow: "w --volume=/:/h"}, {reed.LabelRun: "a=b"}, {reed.LabelStep: "n\n0"},
		{reed.LabelStep: strings.Repeat("s", 256)}, {reed.LabelStep: ""},
	} {
		v := reed.Visit{URL: "https://a.example/", Labels: l}
		if _, err := b.Open(context.Background(), v); !errors.Is(err, reed.ErrInvalid) {
			t.Errorf("browse with labels %q: %v, want ErrInvalid", l, err)
		}
		if _, err := b.Start(context.Background(), v); !errors.Is(err, reed.ErrInvalid) {
			t.Errorf("act with labels %q: %v, want ErrInvalid", l, err)
		}
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("podman ran for labels that should have been refused")
	}
	if got := reed.StepLabels(context.Background(), "--privileged"); len(got) != 0 {
		t.Errorf("StepLabels kept a bad step: %v", got)
	}
}

// labelling opens nothing: it keeps the visit it was given.
type labelling struct{ got *reed.Visit }

func (o labelling) Open(_ context.Context, v reed.Visit) (reed.Page, error) {
	*o.got = v
	return reed.Page{Reed: 1}, nil
}

// TestInv_BrowseLabelsComeFromTemporal: the browse command labels its
// visit with Temporal's activity info (the workflow that scheduled the
// step and that workflow's run) and the step's node ID (REED-502).
func TestInv_BrowseLabelsComeFromTemporal(t *testing.T) {
	var s testsuite.WorkflowTestSuite
	env := s.NewTestActivityEnvironment()
	var got reed.Visit
	env.RegisterActivityWithOptions(shuttle.ArrowAsActivity(reed.Command(labelling{&got})), activity.RegisterOptions{Name: reed.ActivityName})
	info := func(ctx context.Context) (map[string]string, error) {
		e := activity.GetInfo(ctx).WorkflowExecution
		return map[string]string{reed.LabelWorkflow: e.ID, reed.LabelRun: e.RunID}, nil
	}
	env.RegisterActivityWithOptions(info, activity.RegisterOptions{Name: "Info"})
	if _, err := env.ExecuteActivity(reed.ActivityName, plan.Input{NodeID: "n5", Args: []string{"https://a.example/"}}); err != nil {
		t.Fatal(err)
	}
	v, err := env.ExecuteActivity("Info")
	if err != nil {
		t.Fatal(err)
	}
	var want map[string]string
	v.Get(&want)
	want[reed.LabelStep] = "n5"
	if want[reed.LabelWorkflow] == "" || want[reed.LabelRun] == "" || !reflect.DeepEqual(got.Labels, want) {
		t.Errorf("browse labels %v, want %v", got.Labels, want)
	}
}
