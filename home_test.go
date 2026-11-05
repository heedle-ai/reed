package reed_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/worker"

	"github.com/heedle-ai/reed"
	"github.com/heedle-ai/shuttle"
	"github.com/heedle-ai/shuttle/plan"
)

// TestInv_BrowserContainersSayTheirHome: a browse or act container
// started under a context carrying a home (WithHome, as a worker sets it
// for all its activities) is labelled heedle.home with it, after its run
// labels; a visit's own home label wins; the browse command's labels
// read it from the worker's context; and a home that isn't 12 lowercase
// hex digits is refused as ErrInvalid before Podman runs (REED-520,
// roadmap 9.2.1).
func TestInv_BrowserContainersSayTheirHome(t *testing.T) {
	const home, other = "0123456789ab", "ba9876543210"
	hctx := reed.WithHome(context.Background(), home)
	run := map[string]string{reed.LabelWorkflow: "loom-abc/plan", reed.LabelRun: "6f1c2a7d-3b8e", reed.LabelStep: "n4"}
	for _, c := range []struct {
		kind   string
		ctx    context.Context
		labels map[string]string
		want   []string
	}{
		{"browse", hctx, nil, []string{"heedle.kind=browse", "heedle.home=" + home}},
		{"browse", hctx, run, []string{"heedle.kind=browse", "heedle.workflow=loom-abc/plan", "heedle.run=6f1c2a7d-3b8e", "heedle.step=n4", "heedle.home=" + home}},
		{"act", hctx, run, []string{"heedle.kind=act", "heedle.workflow=loom-abc/plan", "heedle.run=6f1c2a7d-3b8e", "heedle.step=n4", "heedle.home=" + home}},
		{"act", hctx, map[string]string{reed.LabelHome: other}, []string{"heedle.kind=act", "heedle.home=" + other}},
		{"browse", context.Background(), nil, []string{"heedle.kind=browse"}},
	} {
		podman, log := runArgv(t)
		b := reed.Browser{Podman: podman, Image: img2, Network: "net1"}
		v := reed.Visit{URL: "https://a.example/", Labels: c.labels}
		var err error
		if c.kind == "browse" {
			_, err = b.Open(c.ctx, v)
		} else {
			_, err = b.Start(c.ctx, v)
		}
		if err == nil {
			t.Fatal("the fake podman's run should fail")
		}
		out, _ := os.ReadFile(log)
		got, placed := labelsOf(strings.Split(strings.TrimSuffix(string(out), "\n"), "\n"))
		if !reflect.DeepEqual(got, c.want) || !placed {
			t.Errorf("%s with %v: labels %q (placed: %v), want %q", c.kind, c.labels, got, placed, c.want)
		}
	}
	if len(run) != 3 {
		t.Errorf("the visit's own labels were changed: %v", run)
	}

	// The browse command's labels, under a worker whose activities start
	// from its home.
	var s testsuite.WorkflowTestSuite
	env := s.NewTestActivityEnvironment()
	env.SetWorkerOptions(worker.Options{BackgroundActivityContext: reed.WithHome(context.Background(), home)})
	var got reed.Visit
	env.RegisterActivityWithOptions(shuttle.ArrowAsActivity(reed.Command(labelling{&got})), activity.RegisterOptions{Name: reed.ActivityName})
	if _, err := env.ExecuteActivity(reed.ActivityName, plan.Input{NodeID: "n5", Args: []string{"https://a.example/"}}); err != nil {
		t.Fatal(err)
	}
	if got.Labels[reed.LabelHome] != home || got.Labels[reed.LabelStep] != "n5" || got.Labels[reed.LabelRun] == "" {
		t.Errorf("browse labels under a worker's home: %v", got.Labels)
	}
	if l := reed.StepLabels(reed.WithHome(context.Background(), "--privileged"), "n1"); l[reed.LabelHome] != "" {
		t.Errorf("StepLabels kept a bad home: %v", l)
	}

	// A home that isn't one is refused, given or carried.
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran")
	podman := filepath.Join(dir, "podman")
	os.WriteFile(podman, []byte("#!/bin/sh\n[ \"$1\" = ps ] && { echo '[]'; exit 0; }\ntouch "+marker+"\nexit 125\n"), 0o755)
	b := reed.Browser{Podman: podman, Image: img2, Network: "net1"}
	for _, h := range []string{"--privileged", "ABCDEF012345", "0123456789a", "/home/me/.heedle", "0123456789abc"} {
		v := reed.Visit{URL: "https://a.example/", Labels: map[string]string{reed.LabelHome: h}}
		if _, err := b.Open(context.Background(), v); !errors.Is(err, reed.ErrInvalid) {
			t.Errorf("browse with home %q: %v, want ErrInvalid", h, err)
		}
		if _, err := b.Start(reed.WithHome(context.Background(), h), reed.Visit{URL: "https://a.example/"}); !errors.Is(err, reed.ErrInvalid) {
			t.Errorf("act with context home %q: %v, want ErrInvalid", h, err)
		}
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("podman ran for a home that should have been refused")
	}
}

// TestInv_SweepsNeverCutAnotherHomesBrowser: Sweep removes browsers by
// reed's own labels, whichever home started them, so it only ever takes
// one past its own deadline: a browse visit's (VisitTimeout) is well
// under StaleAfter, and an act session's (SessionTimeout) under
// StaleActAfter. Home A starting can't cut home B's live browser
// (REED-521, review 55).
func TestInv_SweepsNeverCutAnotherHomesBrowser(t *testing.T) {
	if reed.VisitTimeout >= reed.StaleAfter || reed.SessionTimeout >= reed.StaleActAfter {
		t.Errorf("a sweep could remove a live browser: VisitTimeout %v, StaleAfter %v, SessionTimeout %v, StaleActAfter %v",
			reed.VisitTimeout, reed.StaleAfter, reed.SessionTimeout, reed.StaleActAfter)
	}
}

// TestInv_NoStrayFiles: nothing a test run leaves behind is tracked: no
// empty file at the repository's root (REED-522, review 55: an empty
// "oom" was committed).
func TestInv_NoStrayFiles(t *testing.T) {
	out, err := exec.Command("git", "ls-files").Output()
	if err != nil {
		t.Skip("not a git checkout")
	}
	for _, f := range strings.Fields(string(out)) {
		if strings.Contains(f, "/") {
			continue
		}
		if fi, err := os.Stat(f); err == nil && fi.Size() == 0 {
			t.Errorf("%s is an empty file at the root", f)
		}
	}
}
