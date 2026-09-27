package basispoints

import (
	"context"
	"testing"
)

// 模拟检查过程中截止时间到达，避免用睡眠或随机调度复现竞态。
type deadlineBetweenObservations struct {
	context.Context
	expired bool
}

func (c *deadlineBetweenObservations) Err() error {
	if !c.expired {
		c.expired = true
		return nil
	}
	return context.DeadlineExceeded
}

func TestRunningStreamDeadlineIsNotClientCancellation(t *testing.T) {
	run := &runningStream{
		service: NewService(),
		ctx:     &deadlineBetweenObservations{Context: context.Background()},
	}
	// 本次观察尚未超时；下一次观察才应分类为 504，不能凭第二次读取误报 499。
	if err := run.contextError(); err != nil {
		t.Fatalf("active request was misclassified during deadline transition: %v", err)
	}
	err := run.contextError()
	api, ok := err.(*APIError)
	if !ok || api.Status != 504 || api.Kind != "upstream_timeout" {
		t.Fatalf("expired request was not classified as upstream timeout: %v", err)
	}
}
