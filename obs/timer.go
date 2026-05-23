package obs

import (
	"context"
	"errors"
	"time"
)

type Timer struct {
	op    string
	tags  []string
	start time.Time
	ctx   context.Context
}

func Time(ctx context.Context, op string, tags ...string) *Timer {
	return &Timer{op: op, tags: tags, start: time.Now(), ctx: ctx}
}

// Done emits a duration histogram and a count counter for the op.
// Pass a pointer to your named error return so Done can tag the result.
//
//	func foo() (err error) {
//	    t := obs.Time(ctx, obs.MetricCreate, "provider:aws")
//	    defer t.Done(&err)
//	    ...
//	}
func (t *Timer) Done(errp *error) {
	if t == nil {
		return
	}
	dur := time.Since(t.start)
	result := "ok"
	var errClass string
	if errp != nil && *errp != nil {
		result = "err"
		errClass = classify(*errp)
	}
	tags := append([]string{}, t.tags...)
	tags = append(tags, "result:"+result)
	if errClass != "" {
		tags = append(tags, "err_class:"+errClass)
	}
	_ = M().Histogram(t.op, float64(dur.Milliseconds()), tags, 1)
	_ = M().Incr(stripDuration(t.op)+MetricCountSuffix, tags, 1)
	L(t.ctx).Info("op",
		"op", t.op,
		"duration_ms", dur.Milliseconds(),
		"result", result,
		"tags", tags,
	)
}

func stripDuration(op string) string {
	const suf = ".duration_ms"
	if len(op) > len(suf) && op[len(op)-len(suf):] == suf {
		return op[:len(op)-len(suf)]
	}
	return op
}

func classify(err error) string {
	var c interface{ ErrorCode() string }
	if errors.As(err, &c) {
		return c.ErrorCode()
	}
	return "generic"
}
