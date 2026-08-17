package lebro

import "github.com/tesh254/lebro/internal/runtime"

func NewScheduler(config SchedulerConfig) (*Scheduler, error) {
	return runtime.NewScheduler(config)
}

func ParseCronSpec(spec string) (CronSchedule, error) { return runtime.ParseCronSpec(spec) }
