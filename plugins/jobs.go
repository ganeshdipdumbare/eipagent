//go:build ignore

package plugin

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"eipagent/sdk"
)

// Scheduled and background agent runs. Gives the model schedule_create,
// schedule_list and schedule_cancel tools. Each job runs a prompt on its
// own thread. Job definitions persist in "store" when one is loaded.
//
// Schedules: "every 30m" | "daily 09:00" | "weekly fri 17:00" | "once 2026-11-01T10:00:00Z"
//
// Config: timezone ("UTC"), max_jobs (20 per user), jobs (predefined:
// [{name, schedule, prompt, user}]), tick_seconds (15).
var Inject = []string{"agent", "?store"}

type job struct {
	ID, Name, Schedule, Prompt, User string
	Next                             time.Time
	LastRun                          time.Time
	LastResult                       string
	Running                          bool
}

func Apply(ctx *sdk.Context) error {
	agent := ctx.Service("agent").(*sdk.AgentService)
	store, _ := ctx.Service("store").(*sdk.Store)
	loc, err := time.LoadLocation(ctx.ConfigString("timezone", "UTC"))
	if err != nil {
		return err
	}
	maxJobs := ctx.ConfigInt("max_jobs", 20)
	tick := time.Duration(ctx.ConfigInt("tick_seconds", 15)) * time.Second
	ns := []string{"jobs"}
	var mu sync.Mutex
	jobs := map[string]*job{}

	save := func(j *job) {
		if store != nil {
			store.Put(context.Background(), ns, j.ID, map[string]any{"name": j.Name, "schedule": j.Schedule, "prompt": j.Prompt, "user": j.User})
		}
	}
	add := func(name, sched, prompt, user string, persist bool) (*job, error) {
		next, err := nextRun(sched, time.Now(), loc)
		if err != nil {
			return nil, err
		}
		mu.Lock()
		defer mu.Unlock()
		n := 0
		for _, j := range jobs {
			if j.User == user {
				n++
			}
		}
		if n >= maxJobs {
			return nil, fmt.Errorf("limit of %d jobs reached", maxJobs)
		}
		j := &job{ID: fmt.Sprintf("job-%d", time.Now().UnixNano()), Name: name, Schedule: sched, Prompt: prompt, User: user, Next: next}
		jobs[j.ID] = j
		if persist {
			save(j)
		}
		return j, nil
	}
	if store != nil {
		items, _ := store.Search(context.Background(), ns, "", 1000)
		for _, it := range items {
			s := func(k string) string { v, _ := it.Value[k].(string); return v }
			if next, err := nextRun(s("schedule"), time.Now(), loc); err == nil {
				jobs[it.Key] = &job{ID: it.Key, Name: s("name"), Schedule: s("schedule"), Prompt: s("prompt"), User: s("user"), Next: next}
			}
		}
	}
	for _, j := range ctx.ConfigList("jobs") {
		s := func(k string) string { v, _ := j[k].(string); return v }
		if _, err := add(s("name"), s("schedule"), s("prompt"), s("user"), false); err != nil {
			return fmt.Errorf("jobs: %s: %w", s("name"), err)
		}
	}

	// In-flight runs are cancelled and awaited on unload.
	runCtx, cancelRuns := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	runJob := func(j *job) {
		defer wg.Done()
		res, err := agent.Run(runCtx, j.ID, j.User, j.Prompt)
		mu.Lock()
		defer mu.Unlock()
		j.Running, j.LastRun = false, time.Now()
		if err != nil {
			j.LastResult = "error: " + err.Error()
		} else {
			j.LastResult = res.Text
		}
		ctx.Logf("job %s (%s) finished: %.80s", j.Name, j.ID, j.LastResult)
	}
	ctx.Effect(func() func() {
		stop := make(chan struct{})
		go func() {
			t := time.NewTicker(tick)
			defer t.Stop()
			for {
				select {
				case <-stop:
					return
				case now := <-t.C:
					var due []*job
					mu.Lock()
					for id, j := range jobs {
						if j.Running || now.Before(j.Next) {
							continue
						}
						j.Running = true
						due = append(due, j)
						if next, err := nextRun(j.Schedule, now, loc); err == nil {
							j.Next = next
						} else { // "once" jobs are done
							delete(jobs, id)
							if store != nil {
								store.Delete(context.Background(), ns, id)
							}
						}
					}
					mu.Unlock()
					for _, j := range due {
						wg.Add(1)
						go runJob(j)
					}
				}
			}
		}()
		return func() {
			close(stop)
			cancelRuns()
			wg.Wait()
		}
	})

	ctx.AddTool(sdk.Tool{
		Name:        "schedule_create",
		Description: "Schedule a recurring or one-time task. schedule: 'every 30m', 'daily 09:00', 'weekly fri 17:00' or 'once <RFC3339 time>' (timezone " + loc.String() + ")",
		Schema:      sdk.Object(map[string]any{"name": sdk.Str("short name"), "schedule": sdk.Str("when"), "prompt": sdk.Str("what to do, self-contained")}, "name", "schedule", "prompt"),
		Meta:        map[string]any{"risk": "medium"},
		Run: func(c context.Context, a map[string]any) (string, error) {
			j, err := add(sdk.ArgString(a, "name"), sdk.ArgString(a, "schedule"), sdk.ArgString(a, "prompt"), sdk.UserFrom(c), true)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("scheduled %s (%s); next run %s", j.ID, j.Schedule, j.Next.In(loc).Format(time.RFC1123)), nil
		},
	})
	ctx.AddTool(sdk.Tool{
		Name: "schedule_list", Description: "List your scheduled tasks", Schema: sdk.Object(map[string]any{}),
		Meta: map[string]any{"readonly": true},
		Run: func(c context.Context, a map[string]any) (string, error) {
			mu.Lock()
			defer mu.Unlock()
			var rows []string
			for _, j := range jobs {
				if j.User != sdk.UserFrom(c) {
					continue
				}
				rows = append(rows, fmt.Sprintf("%s  %s  [%s] next %s  last: %.60s", j.ID, j.Name, j.Schedule, j.Next.In(loc).Format(time.RFC1123), j.LastResult))
			}
			sort.Strings(rows)
			if len(rows) == 0 {
				return "no scheduled tasks", nil
			}
			return strings.Join(rows, "\n"), nil
		},
	})
	ctx.AddTool(sdk.Tool{
		Name: "schedule_cancel", Description: "Cancel a scheduled task", Schema: sdk.Object(map[string]any{"id": sdk.Str("job id")}, "id"),
		Run: func(c context.Context, a map[string]any) (string, error) {
			id := sdk.ArgString(a, "id")
			mu.Lock()
			defer mu.Unlock()
			j := jobs[id]
			if j == nil || j.User != sdk.UserFrom(c) {
				return "", fmt.Errorf("no job %s", id)
			}
			delete(jobs, id)
			if store != nil {
				store.Delete(c, ns, id)
			}
			return "cancelled " + id, nil
		},
	})
	return ctx.Provide("jobs", map[string]any{"count": func() int { mu.Lock(); defer mu.Unlock(); return len(jobs) }})
}

var weekdays = map[string]time.Weekday{"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6}

// nextRun returns the next time after now; "once" schedules in the past error.
func nextRun(s string, now time.Time, loc *time.Location) (time.Time, error) {
	f := strings.Fields(strings.ToLower(strings.TrimSpace(s)))
	clock := func(hm string) (int, int, error) {
		h, m, ok := strings.Cut(hm, ":")
		if !ok {
			return 0, 0, fmt.Errorf("bad time %q (want HH:MM)", hm)
		}
		hh, err1 := strconv.Atoi(h)
		mm, err2 := strconv.Atoi(m)
		if err1 != nil || err2 != nil || hh > 23 || mm > 59 {
			return 0, 0, fmt.Errorf("bad time %q", hm)
		}
		return hh, mm, nil
	}
	switch {
	case len(f) == 2 && f[0] == "every":
		d, err := time.ParseDuration(f[1])
		if err != nil || d < time.Minute {
			return time.Time{}, fmt.Errorf("bad interval %q (minimum 1m)", f[1])
		}
		return now.Add(d), nil
	case len(f) == 2 && f[0] == "daily":
		h, m, err := clock(f[1])
		if err != nil {
			return time.Time{}, err
		}
		n := now.In(loc)
		t := time.Date(n.Year(), n.Month(), n.Day(), h, m, 0, 0, loc)
		if !t.After(now) {
			t = t.AddDate(0, 0, 1)
		}
		return t, nil
	case len(f) == 3 && f[0] == "weekly":
		day := f[1]
		if len(day) > 3 { // Yaegi has no min builtin
			day = day[:3]
		}
		wd, ok := weekdays[day]
		if !ok {
			return time.Time{}, fmt.Errorf("bad weekday %q", f[1])
		}
		h, m, err := clock(f[2])
		if err != nil {
			return time.Time{}, err
		}
		n := now.In(loc)
		t := time.Date(n.Year(), n.Month(), n.Day(), h, m, 0, 0, loc)
		t = t.AddDate(0, 0, (int(wd)-int(t.Weekday())+7)%7)
		if !t.After(now) {
			t = t.AddDate(0, 0, 7)
		}
		return t, nil
	case len(f) == 2 && f[0] == "once":
		t, err := time.Parse(time.RFC3339, strings.Fields(s)[1])
		if err != nil {
			return time.Time{}, err
		}
		if !t.After(now) {
			return time.Time{}, fmt.Errorf("once: time is in the past")
		}
		return t, nil
	}
	return time.Time{}, fmt.Errorf("unknown schedule %q", s)
}
