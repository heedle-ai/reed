package reed

import (
	"context"
	"fmt"
	"regexp"

	"go.temporal.io/sdk/activity"
)

// heedle's labels (roadmap 9.2), as bobbin writes them: every browser
// container reed starts carries heedle.kind (browse or act), and one
// started for a step of a run carries that run and step too. heedle
// finds, stops and removes only containers that carry them. One started
// by a worker that knows its home carries heedle.home too (roadmap
// 9.2.1): bobbin's HomeOf the worker's store, 12 hex digits.
const (
	LabelKind     = "heedle.kind"
	LabelWorkflow = "heedle.workflow"
	LabelRun      = "heedle.run"
	LabelStep     = "heedle.step"
	LabelHome     = "heedle.home"
)

// runLabels are the labels a Visit may set; reed sets heedle.kind itself.
var runLabels = []string{LabelWorkflow, LabelRun, LabelStep, LabelHome}

// homeValue is what heedle.home may be: 12 lowercase hex digits.
var homeValue = regexp.MustCompile(`^[0-9a-f]{12}$`)

type homeKey struct{}

// WithHome is ctx carrying the worker's home label, for every browser
// started under it that its visit doesn't label (roadmap 9.2.1). A worker
// sets it on the context all its activities start from.
func WithHome(ctx context.Context, home string) context.Context {
	return context.WithValue(ctx, homeKey{}, home)
}

// HomeFrom is the home label ctx carries (WithHome), if any.
func HomeFrom(ctx context.Context) string {
	h, _ := ctx.Value(homeKey{}).(string)
	return h
}

// withHome is m, or a copy of it with the home ctx carries when m names
// none (checked as a visit's own labels are).
func withHome(ctx context.Context, m map[string]string) map[string]string {
	h := HomeFrom(ctx)
	if h == "" || m[LabelHome] != "" {
		return m
	}
	out := map[string]string{LabelHome: h}
	for k, v := range m {
		out[k] = v
	}
	return out
}

// labelValue is what a label's value may be: printable ASCII with no
// space, quote, comma or equals sign, starting with a letter or digit, at
// most 255 bytes (bobbin's rule).
var labelValue = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@+-]{0,254}$`)

// checkLabels reports whether a visit's labels can be given to podman:
// only the run labels, each a valid value.
func checkLabels(m map[string]string) error {
	for k, v := range m {
		ok := false
		for _, name := range runLabels {
			ok = ok || k == name
		}
		if !ok {
			return fmt.Errorf("%w: label %q isn't one a visit sets (%v)", ErrInvalid, k, runLabels)
		}
		if !labelValue.MatchString(v) {
			return fmt.Errorf("%w: label %s=%q has characters a label can't", ErrInvalid, k, v)
		}
		if k == LabelHome && !homeValue.MatchString(v) {
			return fmt.Errorf("%w: label %s=%q isn't 12 hex digits", ErrInvalid, k, v)
		}
	}
	return nil
}

// labelArgs are the --label flags for a container of kind with the run
// labels m, in a fixed order: after the fixed flags, so none can be one.
func labelArgs(kind string, m map[string]string) []string {
	argv := []string{"--label", LabelKind + "=" + kind}
	for _, k := range runLabels {
		if v := m[k]; v != "" {
			argv = append(argv, "--label", k+"="+v)
		}
	}
	return argv
}

// StepLabels are the run labels of the step node, read from Temporal's
// activity info when ctx is an activity's: the workflow that scheduled it
// and that workflow's run (bobbin.StepLabels, as reed's labels); and the
// home ctx carries (WithHome). A value a label can't hold is left out, so
// labelling never fails a step.
func StepLabels(ctx context.Context, step string) map[string]string {
	m := map[string]string{LabelStep: step}
	if activity.IsActivity(ctx) {
		e := activity.GetInfo(ctx).WorkflowExecution
		m[LabelWorkflow], m[LabelRun] = e.ID, e.RunID
	}
	if h := HomeFrom(ctx); h != "" {
		m[LabelHome] = h
	}
	for k, v := range m {
		if !labelValue.MatchString(v) || k == LabelHome && !homeValue.MatchString(v) {
			delete(m, k)
		}
	}
	return m
}
