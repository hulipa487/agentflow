package shelljobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"agentflow/internal/core/runtime"
	"agentflow/internal/core/workspace"
)

// Job states. A job is running until it has a result or an error.
const (
	JobRunning   = "running"
	JobFinished  = "finished"  // the command ended; see ExitCode and TimedOut
	JobFailed    = "failed"    // there is no result; see Error
	JobCancelled = "cancelled" // stopped by shell.cancel or because its session ended
)

const (
	// tailBytes is how much of each stream a background job keeps. A long
	// build's interesting output is its end.
	tailBytes = 16 << 10
	// beatEvery is how often a running job proves its instance is alive.
	beatEvery = 15 * time.Second
	// lostAfter is how stale a beat may be before the job is taken for lost.
	lostAfter = 4 * beatEvery
	// keepFor is how long a finished job can still be asked about.
	keepFor = 24 * time.Hour
	// maxPerOwner bounds one session's running background jobs.
	maxPerOwner = 4

	jobRowPrefix = "shelljob|"
)

// Job is a background job as a loop sees it, and as it is recorded so that a
// session which moved to another instance can still ask about it.
type Job struct {
	ID        string `json:"id"`
	Owner     string `json:"owner"`
	Project   string `json:"project"`
	Command   string `json:"command"`
	State     string `json:"state"`
	Instance  string `json:"instance,omitempty"`
	StartedAt int64  `json:"started_at"`
	EndedAt   int64  `json:"ended_at,omitempty"`
	Beat      int64  `json:"beat,omitempty"`

	ExitCode   int                `json:"exit_code"`
	TimedOut   bool               `json:"timed_out"`
	Truncated  bool               `json:"truncated"`
	DurationMS int64              `json:"duration_ms"`
	Stdout     string             `json:"stdout"` // the tail
	Stderr     string             `json:"stderr"` // the tail
	Error      string             `json:"error,omitempty"`
	Changes    *workspace.Changes `json:"changes,omitempty"`
}

// tail keeps the last bytes written to it.
type tail struct {
	mu  sync.Mutex
	buf []byte
}

func (t *tail) write(p []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - tailBytes; over > 0 {
		t.buf = append(t.buf[:0], t.buf[over:]...)
	}
}

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(bytesToValidUTF8(t.buf))
}

type liveJob struct {
	job    Job
	cancel context.CancelFunc
	out    tail
	errOut tail
	done   chan struct{}
}

// background runs jobs that outlive the op that started them.
type background struct {
	svc      *Service
	rows     runtime.Rows // nil: jobs are known to this process only
	instance string
	base     context.Context

	mu   sync.Mutex
	live map[string]*liveJob
}

func newBackground(svc *Service) *background {
	return &background{svc: svc, base: context.Background(), live: map[string]*liveJob{}}
}

// SetStore records background jobs in the runtime store, under this
// instance's id, so that any instance can report on them. ctx bounds every
// background job: when the engine shuts down, they stop with it.
func (s *Service) SetStore(ctx context.Context, rows runtime.Rows, instance string) {
	s.bg.mu.Lock()
	defer s.bg.mu.Unlock()
	s.bg.rows, s.bg.instance = rows, instance
	if ctx != nil {
		s.bg.base = ctx
	}
}

// Start queues a command and returns at once. done is called once, from
// another goroutine, with the job's final state; the job's result is also
// kept for Status. The request is checked first, so a mistake in it is an
// error here and not a failed job a moment later.
func (s *Service) Start(ctx context.Context, req Request, done func(Job)) (Job, error) {
	if err := s.Available(); err != nil {
		return Job{}, err
	}
	req.Background = true
	p, err := s.plan(ctx, req)
	if err != nil {
		return Job{}, err
	}
	b := s.bg
	b.mu.Lock()
	running := 0
	for _, lj := range b.live {
		if lj.job.Owner == req.Owner {
			running++
		}
	}
	if running >= maxPerOwner {
		b.mu.Unlock()
		return Job{}, fmt.Errorf("shell: this session already has %d background jobs running; wait for one or cancel it", running)
	}
	// Detached from the op that started it, and from the turn: the point of a
	// background job is to outlive both. It still ends with the engine.
	jobCtx, cancel := context.WithCancel(b.base)
	now := time.Now().Unix()
	lj := &liveJob{
		job: Job{
			ID: newID(), Owner: req.Owner, Project: p.project, Command: clip(req.Command, 512),
			State: JobRunning, Instance: b.instance, StartedAt: now, Beat: now,
		},
		cancel: cancel,
		done:   make(chan struct{}),
	}
	b.live[lj.job.ID] = lj
	started := lj.job
	b.mu.Unlock()
	b.save(started, 0)

	go b.run(jobCtx, lj, req, done)
	return started, nil
}

func (b *background) run(ctx context.Context, lj *liveJob, req Request, done func(Job)) {
	defer lj.cancel()
	stop := make(chan struct{})
	go b.beat(lj, stop)

	res, err := b.svc.run(ctx, req, func(stream string, p []byte) {
		if stream == "stderr" {
			lj.errOut.write(p)
		} else {
			lj.out.write(p)
		}
	})
	close(stop)

	b.mu.Lock()
	job := lj.job
	job.EndedAt = time.Now().Unix()
	job.Stdout, job.Stderr = lj.out.String(), lj.errOut.String()
	switch {
	case err != nil && ctx.Err() != nil:
		job.State = JobCancelled
		job.Error = "the job was cancelled"
		if b.base.Err() != nil {
			job.State, job.Error = JobFailed, "the engine shut down while the job was running"
		}
	case err != nil:
		job.State, job.Error = JobFailed, err.Error()
	default:
		job.State = JobFinished
		job.ExitCode, job.TimedOut, job.Truncated = res.ExitCode, res.TimedOut, res.Truncated
		job.DurationMS = res.Duration.Milliseconds()
		job.Changes = res.Changes
	}
	lj.job = job
	delete(b.live, job.ID)
	b.mu.Unlock()

	b.save(job, keepFor)
	close(lj.done)
	if done != nil {
		done(job)
	}
}

// beat refreshes a running job's record, which is how another instance tells
// a job that is still running from one whose instance died.
func (b *background) beat(lj *liveJob, stop <-chan struct{}) {
	t := time.NewTicker(beatEvery)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			b.mu.Lock()
			lj.job.Beat = time.Now().Unix()
			job := lj.job
			b.mu.Unlock()
			b.save(job, 0)
		}
	}
}

// save writes a job's record. ttl 0 is for a running job: it expires only if
// nothing refreshes it for long enough that it is certainly lost.
func (b *background) save(job Job, ttl time.Duration) {
	b.mu.Lock()
	rows := b.rows
	b.mu.Unlock()
	if rows == nil {
		return
	}
	if ttl == 0 {
		ttl = b.svc.cfg.BackgroundMaxTimeout + keepFor
	}
	raw, err := json.Marshal(job)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := rows.PutRow(ctx, jobRowPrefix+job.ID, string(raw), time.Now().Add(ttl)); err != nil {
		b.svc.log.Warn("background job not recorded", "job", job.ID, "err", err)
	}
}

// errNoJob is returned for an id that names no job of the calling session.
// An id that belongs to another session reads the same: whether it exists is
// not the caller's to know.
var errNoJob = errors.New("shell: no such background job (it may have finished more than a day ago)")

// Status reports a job of the given session, live or recorded.
func (s *Service) Status(ctx context.Context, owner, id string) (Job, error) {
	b := s.bg
	b.mu.Lock()
	if lj, ok := b.live[id]; ok && lj.job.Owner == owner {
		job := lj.job
		b.mu.Unlock()
		job.Stdout, job.Stderr = lj.out.String(), lj.errOut.String()
		return job, nil
	}
	rows := b.rows
	b.mu.Unlock()
	if rows == nil {
		return Job{}, errNoJob
	}
	row, ok, err := rows.GetRow(ctx, jobRowPrefix+id)
	if err != nil {
		return Job{}, fmt.Errorf("shell: read job %s: %w", id, err)
	}
	var job Job
	if !ok || json.Unmarshal([]byte(row.Value), &job) != nil || job.Owner != owner {
		return Job{}, errNoJob
	}
	if job.State == JobRunning && time.Since(time.Unix(job.Beat, 0)) > lostAfter {
		// Nobody is refreshing it and it is not ours: the instance that ran it
		// is gone, and its sandbox with it. The job is not started again —
		// only its author knows whether running it twice is safe.
		job.State = JobFailed
		job.Error = "the instance running it was lost; the job was not completed"
	}
	return job, nil
}

// Cancel stops a running job of the given session. A job on another instance
// cannot be stopped from here, and saying so beats pretending.
func (s *Service) Cancel(ctx context.Context, owner, id string) (Job, error) {
	b := s.bg
	b.mu.Lock()
	lj, ok := b.live[id]
	if ok && lj.job.Owner != owner {
		ok = false
	}
	b.mu.Unlock()
	if ok {
		lj.cancel()
		select {
		case <-lj.done:
		case <-ctx.Done():
			return Job{}, ctx.Err()
		}
		// From the job itself, not the record: a deployment without a shared
		// store has no record to read.
		b.mu.Lock()
		job := lj.job
		b.mu.Unlock()
		return job, nil
	}
	job, err := s.Status(ctx, owner, id)
	if err != nil {
		return Job{}, err
	}
	if job.State == JobRunning {
		return Job{}, fmt.Errorf("shell: job %s is running on another instance (%s) and cannot be cancelled from this one", id, job.Instance)
	}
	return job, nil
}

// Running reports how many background jobs a session has running here. The
// supervisor asks before unloading an idle session: one that is waiting for a
// job's result is not idle.
func (s *Service) Running(owner string) int {
	s.bg.mu.Lock()
	defer s.bg.mu.Unlock()
	n := 0
	for _, lj := range s.bg.live {
		if lj.job.Owner == owner {
			n++
		}
	}
	return n
}

// CancelOwner stops every background job of a session that has ended: there
// is no inbox left to deliver their results to.
func (s *Service) CancelOwner(owner string) {
	s.bg.mu.Lock()
	var mine []*liveJob
	for _, lj := range s.bg.live {
		if lj.job.Owner == owner {
			mine = append(mine, lj)
		}
	}
	s.bg.mu.Unlock()
	for _, lj := range mine {
		lj.cancel()
	}
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return string(bytesToValidUTF8([]byte(s[:n]))) + "…"
}
