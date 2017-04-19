package server

import (
	"fmt"
	"log"
	"time"

	"github.com/armon/go-metrics"
	"udup/internal/server/models"
	"udup/internal/server/scheduler"
)

const (
	// backoffBaseline is the baseline time for exponential backoff
	backoffBaseline = 20 * time.Millisecond

	// backoffLimit is the limit of the exponential backoff
	backoffLimit = 5 * time.Second

	// dequeueTimeout is used to timeout an evaluation dequeue so that
	// we can check if there is a shutdown event
	dequeueTimeout = 500 * time.Millisecond

	// raftSyncLimit is the limit of time we will wait for Raft replication
	// to catch up to the evaluation. This is used to fast Nack and
	// allow another scheduler to pick it up.
	raftSyncLimit = 5 * time.Second
)

// Worker is a single threaded scheduling worker. There may be multiple
// running per server (leader or follower). They are responsible for dequeuing
// pending evaluations, invoking schedulers, plan submission and the
// lifecycle around making task allocations. They bridge the business logic
// of the scheduler with the plumbing required to make it all work.
type Worker struct {
	srv    *Server
	logger *log.Logger

	failures uint
}

// NewWorker starts a new worker associated with the given server
func NewWorker(srv *Server) (*Worker, error) {
	w := &Worker{
		srv:    srv,
		logger: srv.logger,
	}
	go w.run()
	return w, nil
}

// run is the long-lived goroutine which is used to run the worker
func (w *Worker) run() {
	for {
		// Dequeue a pending evaluation
		eval, shutdown := w.dequeueEvaluation()
		if shutdown {
			return
		}

		// Check for a shutdown
		if w.srv.IsShutdown() {
			w.sendAck(eval.ID, false)
			return
		}

		// Wait for the the raft log to catchup to the evaluation
		if err := w.waitForIndex(eval.ModifyIndex, raftSyncLimit); err != nil {
			w.sendAck(eval.ID, false)
			continue
		}

		// Invoke the scheduler to determine placements
		if err := w.invokeScheduler(eval); err != nil {
			w.sendAck(eval.ID, false)
			continue
		}

		// Complete the evaluation
		w.sendAck(eval.ID, true)
	}
}

// dequeueEvaluation is used to fetch the next ready evaluation.
// This blocks until an evaluation is available or a timeout is reached.
func (w *Worker) dequeueEvaluation() (*models.Evaluation, bool) {
	// Setup the request
	req := models.EvalDequeueRequest{
		Schedulers: w.srv.config.EnabledSchedulers,
		Timeout:    dequeueTimeout,
		WriteRequest: models.WriteRequest{
			Region: w.srv.config.Region,
		},
	}
	var resp models.SingleEvalResponse

REQ:
	// Make a blocking RPC
	err := w.srv.RPC("Eval.Dequeue", &req, &resp)
	if err != nil {
		w.logger.Printf("[ERR] worker: failed to dequeue evaluation: %v", err)
		if w.backoffErr() {
			return nil, true
		}
		goto REQ
	}
	w.backoffReset()

	// Check if we got a response
	if resp.Eval != nil {
		return resp.Eval, false
	}

	// Check for potential shutdown
	if w.srv.IsShutdown() {
		return nil, true
	}
	goto REQ
}

// sendAck makes a best effort to ack or nack the evaluation.
// Any errors are logged but swallowed.
func (w *Worker) sendAck(evalID string, ack bool) {
	// Setup the request
	req := models.EvalSpecificRequest{
		EvalID: evalID,
		WriteRequest: models.WriteRequest{
			Region: w.srv.config.Region,
		},
	}
	var resp models.GenericResponse

	// Determine if this is an Ack or Nack
	verb := "ack"
	endpoint := "Eval.Ack"
	if !ack {
		verb = "nack"
		endpoint = "Eval.Nack"
	}

	// Make the RPC call
	err := w.srv.RPC(endpoint, &req, &resp)
	if err != nil {
		w.logger.Printf("[ERR] worker: failed to %s evaluation '%s': %v",
			verb, evalID, err)
	}
}

// waitForIndex ensures that the local state is at least as fresh
// as the given index. This is used before starting an evaluation,
// but also potentially mid-stream. If a Plan fails because of stale
// state (attempt to allocate to a failed/dead node), we may need
// to sync our state again and do the planning with more recent data.
func (w *Worker) waitForIndex(index uint64, timeout time.Duration) error {
	start := time.Now()
	defer metrics.MeasureSince([]string{"server", "worker", "wait_for_index"}, time.Now())
CHECK:
	// We only need the FSM state to be as recent as the given index
	appliedIndex := w.srv.raft.AppliedIndex()
	if index <= appliedIndex {
		w.backoffReset()
		return nil
	}

	// Check if we've reached our limit
	if time.Now().Sub(start) > timeout {
		return fmt.Errorf("sync wait limit reached")
	}

	// Exponential back off if we haven't yet reached it
	if w.backoffErr() {
		return fmt.Errorf("shutdown while waiting for state sync")
	}
	goto CHECK
}

// invokeScheduler is used to invoke the business logic of the scheduler
func (w *Worker) invokeScheduler(eval *models.Evaluation) error {
	// Snapshot the current state
	snap, err := w.srv.fsm.State().Snapshot()
	if err != nil {
		return fmt.Errorf("failed to snapshot state: %v", err)
	}

	// Create the scheduler
	sched, err := scheduler.NewScheduler(eval.Type, snap, w)
	if err != nil {
		return fmt.Errorf("failed to instantiate scheduler: %v", err)
	}

	// Process the evaluation
	err = sched.Process(eval)
	if err != nil {
		return fmt.Errorf("failed to process evaluation: %v", err)
	}
	return nil
}

// SubmitPlan is used to submit a plan for consideration. This allows
// the worker to act as the planner for the scheduler.
func (w *Worker) SubmitPlan(plan *models.Plan) (*models.PlanResult, scheduler.State, error) {
	defer metrics.MeasureSince([]string{"server", "worker", "submit_plan"}, time.Now())
	// Setup the request
	req := models.PlanRequest{
		Plan: plan,
		WriteRequest: models.WriteRequest{
			Region: w.srv.config.Region,
		},
	}
	var resp models.PlanResponse

	// Make the RPC call
	if err := w.srv.RPC("Plan.Submit", &req, &resp); err != nil {
		w.logger.Printf("[ERR] worker: failed to submit plan for evaluation %s: %v",
			plan.EvalID, err)
		return nil, nil, err
	} else {
		w.logger.Printf("[DEBUG] worker: submitted plan for evaluation %s", plan.EvalID)
	}

	// Look for a result
	result := resp.Result
	if result == nil {
		return nil, nil, fmt.Errorf("missing result")
	}

	// Check if a state update is required. This could be required if we
	// planning based on stale data, which is causing issues. For example, a
	// node failure since the time we've started planning or conflicting task
	// allocations.
	var state scheduler.State
	if result.RefreshIndex != 0 {
		// Wait for the the raft log to catchup to the evaluation
		if err := w.waitForIndex(result.RefreshIndex, raftSyncLimit); err != nil {
			return nil, nil, err
		}

		// Snapshot the current state
		snap, err := w.srv.fsm.State().Snapshot()
		if err != nil {
			return nil, nil, fmt.Errorf("failed to snapshot state: %v", err)
		}
		state = snap
	}

	// Return the result and potential state update
	return result, state, nil
}

// backoffErr is used to do an exponential back off on error. This is
// maintained statefully for the worker. Returns if attempts should be
// abandoneded due to shutdown.
// be made or abandoned.
func (w *Worker) backoffErr() bool {
	backoff := (1 << (2 * w.failures)) * backoffBaseline
	if backoff > backoffLimit {
		backoff = backoffLimit
	} else {
		w.failures++
	}
	select {
	case <-time.After(backoff):
		return false
	case <-w.srv.shutdownCh:
		return true
	}
}

// backoffReset is used to reset the failure count for
// exponential backoff
func (w *Worker) backoffReset() {
	w.failures = 0
}
