package crdsource

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/milo-os/assistant/internal/auth"
	appmetrics "github.com/milo-os/assistant/internal/metrics"
	capv1alpha1 "github.com/milo-os/assistant/pkg/apis/capabilities/v1alpha1"
)

// This file closes defect #3 of docs/enhancements/crd-capability-source.md:
// there is no feedback loop to whoever authored a binding. A document that
// fails Validate is dropped with a warn line INSIDE the assistant, where the
// provider who wrote it and the catalog operator who projected it will never
// see it. A capability can be silently absent for weeks.
//
// Kubernetes already solved this: the consumer writes status.conditions back
// onto the object and the producer reads them with `kubectl describe`. What
// follows is that writer, and almost all of its design is about what it must
// NOT do:
//
//   - It must not run on the request goroutine. The entire point of the cached
//     source is that the config plane left the latency path; an inline PATCH
//     would put it back, and a control-plane outage would then add its timeout
//     to every turn. Observations are enqueued without blocking and a single
//     background worker does the I/O.
//   - It must not write per observation. A cache entry lives 60s and one turn
//     consults a binding repeatedly; an uncoalesced writer is a hot loop
//     against the control plane. Writes happen on TRANSITION only.
//   - It must not grow without bound. Queue and state map are both capped, and
//     an outage degrades to "status not updated", never to backpressure on
//     chats or unbounded memory. A lost status update is a cosmetic
//     regression; a status update that blocks a chat turn is an outage.

const (
	// DefaultStatusWriteTimeout bounds ONE status PATCH. It matches
	// [DefaultFetchTimeout] and auth.DefaultSARTimeout rather than picking a
	// third number: the write is to the same control plane over the same
	// transport, so a budget that is right for reading configuration is right
	// for reporting on it. Nothing waits on this call, so the timeout exists
	// only to stop a hung connection from parking the single worker (and with
	// it every later write) behind one unresponsive request.
	DefaultStatusWriteTimeout = 5 * time.Second

	// defaultStatusQueueSize bounds the pending-write queue.
	//
	// The steady-state depth is ~0: writes only occur on transition, and
	// transitions only occur when a spec or an endpoint's reachability changes.
	// The queue exists for the burst — a restart, where every active project's
	// first LIST observes every one of its bindings at once — and for the
	// outage, where the worker is blocked on a timing-out PATCH while
	// observations keep arriving. 256 covers a few hundred bindings' worth of
	// first-observation burst at a few hundred bytes each (tens of kilobytes),
	// which is small enough to never be the memory problem and large enough
	// that a healthy deployment never reaches it. Beyond it, DROP THE OLDEST:
	// the pending writes are independent verdicts and the newest is the closest
	// to the truth, so shedding the stalest one loses the least.
	defaultStatusQueueSize = 256

	// maxStatusStateEntries bounds the coalescing map, which is keyed by
	// (project, binding) and therefore by TENANT COUNT — a number this
	// repository does not control. Same cap and same policy as
	// [maxCacheEntries] so an operator reasons about one eviction behavior, not
	// two. Evicting an entry costs at most one redundant PATCH the next time
	// that binding is observed, which is why a crude eviction is acceptable
	// here and would not be in the cache.
	maxStatusStateEntries = 4096

	// shutdownDrainTimeout caps how long Close spends flushing queued writes.
	// Draining is worth a moment — the writes queued at shutdown are the last
	// word on bindings this replica just observed — but a status update is
	// never worth delaying a pod's termination into a SIGKILL, so the drain is
	// best-effort inside a deadline well under any reasonable grace period.
	shutdownDrainTimeout = 2 * time.Second

	// statusPatchPathTemplate is appended to the project's control-plane
	// prefix. %[1]s group, %[2]s version, %[3]s object name.
	//
	// No namespace segment, matching [listPathTemplate]: the binding is
	// cluster-scoped and the control-plane prefix already names the project.
	// The write must address the object the LIST produced, so the two paths
	// have to agree — a namespaced write path would 404 on every condition.
	statusPatchPathTemplate = "/apis/%[1]s/%[2]s/capabilitybindings/%[3]s/status"

	// mergePatchContentType is RFC 7386 JSON merge patch — the patch type a
	// CustomResource's status subresource accepts (strategic merge patch is an
	// aggregated-apiserver feature and is not available on CRDs).
	//
	// Its one sharp edge governs the whole payload shape below: a merge patch
	// REPLACES an array wholesale, it does not merge by key, despite
	// status.conditions being declared listType=map. Sending only the condition
	// that changed would therefore delete the other one. Every patch carries
	// the full set of conditions this writer knows for that binding.
	mergePatchContentType = "application/merge-patch+json"

	// maxConditionMessageBytes truncates a condition message. metav1.Condition
	// permits 32Ki, but the message is read by a human in `kubectl describe`
	// and produced from an error string of unbounded length (a control-plane
	// error body, say). A path-qualified validation error is tens of bytes;
	// anything past 512 is not adding diagnosis, only patch size.
	maxConditionMessageBytes = 512
)

// Condition reasons. Kubernetes requires reasons to be CamelCase tokens, and
// they are the field a coalescing decision keys on (see [StatusWriter.observe]),
// so they are deliberately COARSE: a reason that embedded the failing field
// would make every distinct validation error its own transition.
const (
	reasonValidated           = "Validated"
	reasonValidationFailed    = "ValidationFailed"
	reasonComposed            = "Composed"
	reasonCompositionDegraded = "CompositionDegraded"
)

// BindingObserver receives one verdict per binding, per observation. It is the
// seam between the read path (which sees the KRM object, and therefore
// metadata.generation) and the write path (which does the I/O).
//
// It is an interface rather than a bare callback on [Config] because there are
// two conditions with different lifetimes — Accepted is decided at conversion,
// Composed at compose time, in a different goroutine and a different package —
// and a single func value would have had to grow a discriminator argument to
// carry both.
//
// Implementations MUST NOT block: every method is called from a request
// goroutine.
type BindingObserver interface {
	// ObserveAccepted reports whether one binding parsed and validated.
	// validationErr nil means Accepted=True.
	ObserveAccepted(projectName, bindingName string, generation int64, validationErr error)

	// ObserveComposed reports whether the last composition of one binding could
	// actually fetch its knowledge, connect its MCP servers, and index its
	// skills. composeErr nil means Composed=True.
	//
	// projectName must be the project the documents were FETCHED for (the
	// turn's project), never anything read off the document: a cluster-scoped
	// binding carries no namespace, so a project derived from one would always
	// be empty and every status write would be addressed to no control plane at
	// all.
	//
	// Deliberately NO generation parameter, unlike ObserveAccepted. The
	// composition side holds a capability.CapabilityDocument, which carries
	// metadata.name but NOT metadata.generation — the
	// source drops the KRM envelope at conversion. Rather than thread a
	// generation through a type whose whole purpose is to be client-free, the
	// writer reuses the generation it recorded for this binding at
	// ObserveAccepted time, which is the same LIST the composed documents came
	// from. A binding never observed as accepted reports no observedGeneration
	// rather than a wrong one.
	ObserveComposed(projectName, bindingName string, composeErr error)
}

// StatusWriterConfig configures [NewStatusWriter]. The control-plane
// coordinates are the same ones [Config] takes, for the same reason: the write
// goes to the object the read came from, over the same connection, with the
// same client certificate.
type StatusWriterConfig struct {
	// APIURL is the control-plane API base URL. Required.
	APIURL string
	// BearerToken is the assistant's own service-account token — carried for a
	// plain Kubernetes apiserver (kind, dev, e2e). On Milo the client
	// certificate is the credential that works; see internal/auth/transport.go.
	BearerToken string
	// CACert verifies the SERVER. ClientCert/ClientKey prove who WE are.
	CACert     []byte
	ClientCert []byte
	ClientKey  []byte

	// Timeout bounds one PATCH. Zero uses [DefaultStatusWriteTimeout].
	Timeout time.Duration
	// QueueSize overrides [defaultStatusQueueSize] when > 0 (tests).
	QueueSize int
	// MaxStateEntries overrides [maxStatusStateEntries] when > 0 (tests).
	MaxStateEntries int

	// Logger records write failures. Nil discards them.
	Logger *slog.Logger
	// Metrics records assistant_capability_status_write_total. Nil disables.
	Metrics *appmetrics.Metrics

	// Transport overrides the control-plane transport (tests point it at an
	// httptest server). Nil builds the real one from the credentials above.
	Transport http.RoundTripper
	// now overrides the clock (tests). Nil uses time.Now.
	now func() time.Time
}

// bindingKey identifies one CapabilityBinding across the whole process.
type bindingKey struct{ project, name string }

// bindingState is this writer's memory of what it last successfully wrote for
// one binding. It is what makes the writer coalescing and idempotent: a
// condition whose status, reason, and observedGeneration all match what is
// recorded produces no API call at all.
type bindingState struct {
	// generation is the last metadata.generation seen for this binding, used
	// by ObserveComposed, which has no generation of its own.
	generation int64
	// conds is the full condition set last written, keyed by type. Every patch
	// sends all of it — merge patch replaces arrays (see mergePatchContentType).
	conds map[string]metav1.Condition
}

// statusWrite is one ready-to-send PATCH. The body is rendered at ENQUEUE time,
// under the same lock that made the coalescing decision, so the worker is a
// dumb sender and there is no window in which the queued intent and the
// recorded state can disagree.
type statusWrite struct {
	key      bindingKey
	condType string // the condition whose transition caused this write
	body     []byte
}

// StatusWriter writes CapabilityBinding status conditions back to the project
// control plane, off the request path, coalescing on transition.
//
// # Multiple replicas
//
// The catalog is specified never to write status (docs/enhancements/crd-capability-source.md:
// "Two writers on one status block is a hot loop"), so the assistant is the
// sole writer of this block — but the assistant is replicated, and every
// replica independently observes and writes. Three consequences, all accepted:
//
//   - No conflict errors. The patch carries no resourceVersion precondition and
//     the status subresource is patched, not the spec, so two replicas writing
//     the same binding are last-writer-wins rather than 409-and-retry. There is
//     no conflict-retry loop here on purpose: a retry loop on a status write is
//     exactly the hot loop the design warns about, and the next observation is
//     a better retry than an immediate one.
//   - Agreement is the common case. Accepted is a pure function of the spec, so
//     every replica computes the same verdict from the same generation and the
//     redundant writes are byte-identical.
//   - Composed can legitimately disagree — one replica may reach an MCP
//     endpoint another cannot — and would then flap between replicas. The
//     coalescing state is per replica, so the flap rate is bounded by the
//     observation rate (once per cache TTL per project for Accepted, once per
//     turn for Composed), not amplified by it. A genuinely partitioned replica
//     shows up as a condition whose message names what it could not reach,
//     which is the diagnosis anyway.
//   - A replica that has only observed Accepted overwrites the array with just
//     Accepted, dropping a Composed another replica wrote. That condition
//     returns at that replica's next Composed transition. Merge patch cannot
//     express "merge this array by key"; server-side apply can, and is the
//     upgrade path if per-condition ownership ever matters more than the
//     simplicity of one PATCH shape.
type StatusWriter struct {
	baseURL string
	token   string
	client  *http.Client
	logger  *slog.Logger
	metrics *appmetrics.Metrics
	now     func() time.Time

	queueMax  int
	stateMax  int
	mu        sync.Mutex
	queue     []statusWrite
	states    map[bindingKey]*bindingState
	notify    chan struct{}
	done      chan struct{}
	stopped   chan struct{}
	closeOnce sync.Once
}

// NewStatusWriter builds the writer and starts its single worker goroutine.
// Call [StatusWriter.Close] on shutdown.
//
// Like [New] it fails only on misconfiguration, because once constructed this
// writer never fails anything: every runtime error is recorded and dropped.
func NewStatusWriter(cfg StatusWriterConfig) (*StatusWriter, error) {
	if strings.TrimSpace(cfg.APIURL) == "" {
		return nil, fmt.Errorf("crdsource: status writer APIURL is required (the Milo control-plane base URL)")
	}

	transport := cfg.Transport
	if transport == nil {
		t, err := auth.NewControlPlaneTransport(cfg.CACert, cfg.ClientCert, cfg.ClientKey)
		if err != nil {
			return nil, fmt.Errorf("crdsource: status writer: %w", err)
		}
		transport = t
	}

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultStatusWriteTimeout
	}
	queueMax := cfg.QueueSize
	if queueMax <= 0 {
		queueMax = defaultStatusQueueSize
	}
	stateMax := cfg.MaxStateEntries
	if stateMax <= 0 {
		stateMax = maxStatusStateEntries
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	now := cfg.now
	if now == nil {
		now = time.Now
	}

	w := &StatusWriter{
		baseURL:  strings.TrimRight(cfg.APIURL, "/"),
		token:    cfg.BearerToken,
		client:   &http.Client{Transport: transport, Timeout: timeout},
		logger:   logger,
		metrics:  cfg.Metrics,
		now:      now,
		queueMax: queueMax,
		stateMax: stateMax,
		states:   make(map[bindingKey]*bindingState),
		notify:   make(chan struct{}, 1),
		done:     make(chan struct{}),
		stopped:  make(chan struct{}),
	}
	go w.run()
	return w, nil
}

// ObserveAccepted implements [BindingObserver].
func (w *StatusWriter) ObserveAccepted(projectName, bindingName string, generation int64, validationErr error) {
	if w == nil {
		return
	}
	cond := metav1.Condition{
		Type:               capv1alpha1.CapabilityBindingConditionAccepted,
		Status:             metav1.ConditionTrue,
		Reason:             reasonValidated,
		Message:            "Spec validated; this binding is composed for its project.",
		ObservedGeneration: generation,
	}
	if validationErr != nil {
		cond.Status = metav1.ConditionFalse
		cond.Reason = reasonValidationFailed
		// The same path-qualified error Validate returns today
		// ("spec.skills[0].source: required") — the exact string the assistant
		// currently logs to itself at capability.crd.entry_skipped, delivered
		// to the one person who can fix it.
		cond.Message = truncate(validationErr.Error(), maxConditionMessageBytes)
	}
	w.observe(bindingKey{projectName, bindingName}, generation, true, cond)
}

// ObserveComposed implements [BindingObserver].
func (w *StatusWriter) ObserveComposed(projectName, bindingName string, composeErr error) {
	if w == nil {
		return
	}
	key := bindingKey{projectName, bindingName}
	cond := metav1.Condition{
		Type:    capv1alpha1.CapabilityBindingConditionComposed,
		Status:  metav1.ConditionTrue,
		Reason:  reasonComposed,
		Message: "Knowledge fetched, MCP servers connected, and skills indexed on the last turn that used this binding.",
	}
	if composeErr != nil {
		cond.Status = metav1.ConditionFalse
		cond.Reason = reasonCompositionDegraded
		cond.Message = truncate(composeErr.Error(), maxConditionMessageBytes)
	}
	w.observe(key, 0, false, cond)
}

// observe applies one condition to a binding's recorded state and enqueues a
// patch IF AND ONLY IF something a reader would act on changed.
//
// The comparison is over (status, reason, observedGeneration) and deliberately
// NOT over the message. Composed is observed once per turn and its message
// names whatever happened to be unreachable at that moment; including it would
// make an intermittent endpoint a per-turn API write, which is the hot loop
// this writer exists to avoid. The message that ships is the one from the
// observation that caused the transition.
func (w *StatusWriter) observe(key bindingKey, generation int64, haveGeneration bool, cond metav1.Condition) {
	// Refuse an unkeyed observation. Both halves of the key are path segments:
	// the project selects the virtual control plane and the name selects the
	// object. An empty project would send the PATCH to a control-plane path for
	// no project; an empty name would PATCH ".../capabilitybindings//status",
	// an object that cannot exist. Neither can be recovered from downstream, and
	// both would otherwise be silent — hence a hard drop with a log line rather
	// than a request nobody reads the answer to. The live hazard is a caller
	// that derives the project from a document's namespace, which is always
	// empty now that the binding is cluster-scoped.
	if key.project == "" || key.name == "" {
		w.metrics.RecordCapabilityStatusWrite("dropped")
		w.logger.Warn("capability.crd.status_unkeyed",
			"projectName", key.project, "name", key.name, "condition", cond.Type,
			"effect", "status not written for this observation",
			"hint", "the project must come from the LIST/turn, never from a document's namespace")
		return
	}

	w.mu.Lock()

	state := w.states[key]
	if state == nil {
		w.evictStatesLocked()
		state = &bindingState{conds: make(map[string]metav1.Condition, 2)}
		w.states[key] = state
	}
	if haveGeneration {
		state.generation = generation
	} else {
		// Composed has no generation of its own; it describes the spec the last
		// LIST observed. Zero (a binding never seen accepted) is omitted from
		// the patch rather than written as a wrong answer.
		cond.ObservedGeneration = state.generation
	}

	prev, seen := state.conds[cond.Type]
	if seen && prev.Status == cond.Status && prev.Reason == cond.Reason &&
		prev.ObservedGeneration == cond.ObservedGeneration {
		w.mu.Unlock()
		return
	}
	// lastTransitionTime moves only when the STATUS changes, per the
	// metav1.Condition contract: a reason or generation change is an update to
	// the same state, not a new transition, and a reader uses this field to ask
	// "how long has it been broken".
	if seen && prev.Status == cond.Status {
		cond.LastTransitionTime = prev.LastTransitionTime
	} else {
		cond.LastTransitionTime = metav1.NewTime(w.now())
	}
	state.conds[cond.Type] = cond

	body, err := renderStatusPatch(state)
	w.mu.Unlock()

	if err != nil {
		// Unreachable in practice (metav1.Condition always marshals); recorded
		// rather than ignored so an impossible thing is still visible.
		w.metrics.RecordCapabilityStatusWrite("error")
		w.logger.Warn("capability.crd.status_encode_failed",
			"projectName", key.project, "name", key.name, "error", err.Error())
		return
	}
	w.enqueue(statusWrite{key: key, condType: cond.Type, body: body})
}

// evictStatesLocked keeps the coalescing map bounded. Caller holds w.mu.
func (w *StatusWriter) evictStatesLocked() {
	if len(w.states) < w.stateMax {
		return
	}
	// Arbitrary victim: unlike the document cache there is no freshness axis to
	// prefer on, and the cost of a wrong choice is one redundant PATCH.
	for k := range w.states {
		delete(w.states, k)
		break
	}
}

// enqueue appends a pending write, dropping the OLDEST on overflow, and never
// blocks. Not blocking is the whole contract: this runs on the goroutine
// serving a chat turn.
func (w *StatusWriter) enqueue(write statusWrite) {
	var dropped *statusWrite
	w.mu.Lock()
	if len(w.queue) >= w.queueMax {
		oldest := w.queue[0]
		w.queue = w.queue[1:]
		// The dropped write's state is forgotten so the binding is re-observed
		// as a transition later, rather than being recorded as written when it
		// never was.
		w.forgetLocked(oldest.key, oldest.condType)
		dropped = &oldest
	}
	w.queue = append(w.queue, write)
	w.mu.Unlock()

	if dropped != nil {
		w.metrics.RecordCapabilityStatusWrite("dropped")
		w.logger.Warn("capability.crd.status_write_dropped",
			"projectName", dropped.key.project, "name", dropped.key.name,
			"condition", dropped.condType, "queueSize", w.queueMax,
			"effect", "status not updated for this binding; chats are unaffected")
	}

	select {
	case w.notify <- struct{}{}:
	default: // a wake-up is already pending; the worker drains the whole queue
	}
}

// forgetLocked drops the recorded state for one condition so the next
// observation counts as a transition and retries. Caller holds w.mu.
func (w *StatusWriter) forgetLocked(key bindingKey, condType string) {
	if state, ok := w.states[key]; ok {
		delete(state.conds, condType)
	}
}

func (w *StatusWriter) forget(key bindingKey, condType string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.forgetLocked(key, condType)
}

func (w *StatusWriter) pop() (statusWrite, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.queue) == 0 {
		return statusWrite{}, false
	}
	write := w.queue[0]
	w.queue = w.queue[1:]
	return write, true
}

// run is the single worker. One goroutine, not a pool: status writes have no
// latency requirement, and serializing them means a control-plane outage costs
// one stuck request rather than N.
func (w *StatusWriter) run() {
	defer close(w.stopped)
	for {
		select {
		case <-w.notify:
			w.drain(context.Background())
		case <-w.done:
			// Best-effort flush of what this replica has already decided,
			// inside a deadline that cannot delay termination materially.
			ctx, cancel := context.WithTimeout(context.Background(), shutdownDrainTimeout)
			w.drain(ctx)
			cancel()
			w.discardQueued()
			return
		}
	}
}

func (w *StatusWriter) drain(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		write, ok := w.pop()
		if !ok {
			return
		}
		w.write(ctx, write)
	}
}

// discardQueued accounts for anything left after the shutdown deadline, so a
// slow drain is visible as dropped writes rather than as silence.
func (w *StatusWriter) discardQueued() {
	w.mu.Lock()
	remaining := w.queue
	w.queue = nil
	w.mu.Unlock()
	for range remaining {
		w.metrics.RecordCapabilityStatusWrite("dropped")
	}
}

// Close stops the worker after a bounded drain. Safe to call more than once;
// blocks until the worker has exited.
//
// Worst-case wait is one in-flight write's timeout plus [shutdownDrainTimeout],
// because a write already on the wire when Close arrives is allowed to finish
// under its own budget rather than being torn out mid-request. Observations
// that arrive after Close are still enqueued and simply never sent — the queue
// is bounded, so they cost nothing beyond their own memory.
func (w *StatusWriter) Close() {
	if w == nil {
		return
	}
	w.closeOnce.Do(func() { close(w.done) })
	<-w.stopped
}

// statusPatchURL is the status subresource of one binding, addressed through
// the SAME project control-plane prefix the LIST uses — the write goes to the
// project that owns the object, with the assistant's own identity, and needs no
// cluster-wide anything.
func (w *StatusWriter) statusPatchURL(key bindingKey) string {
	gv := capv1alpha1.SchemeGroupVersion
	return auth.ProjectControlPlaneURL(w.baseURL, key.project) +
		fmt.Sprintf(statusPatchPathTemplate, gv.Group, gv.Version, url.PathEscape(key.name))
}

// write performs one PATCH. Every failure path ends in a log line and a metric,
// never in an error returned anywhere: by the time execution reaches here the
// turn that produced the observation is long finished.
func (w *StatusWriter) write(ctx context.Context, item statusWrite) {
	endpoint := w.statusPatchURL(item.key)

	ctx, cancel := context.WithTimeout(ctx, w.client.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, endpoint, bytes.NewReader(item.body))
	if err != nil {
		w.fail(item, "error", "capability.crd.status_write_failed", endpoint, err.Error())
		return
	}
	req.Header.Set("Content-Type", mergePatchContentType)
	req.Header.Set("Accept", "application/json")
	if w.token != "" {
		req.Header.Set("Authorization", "Bearer "+w.token)
	}

	resp, err := w.client.Do(req)
	if err != nil {
		w.fail(item, "error", "capability.crd.status_write_failed", endpoint, err.Error())
		return
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		w.metrics.RecordCapabilityStatusWrite("ok")
	case resp.StatusCode == http.StatusForbidden:
		// NOT folded into the generic error path, on purpose. Milo's IAM
		// permission list has no subresource axis, so
		// config/milo/iam/resources/capabilitybindings.yaml assumes a status
		// write evaluates as `capabilitybindings.patch` on the parent. If Milo
		// instead expects a distinct subresource permission, EVERY status write
		// in production 403s and nothing else changes — chats keep working,
		// bindings keep composing, and the only symptom is that conditions
		// never appear. That is a question for the platform team, and this log
		// line is what turns it from a multi-hour mystery into a grep.
		w.logger.Warn("capability.crd.status_forbidden",
			"url", endpoint, "projectName", item.key.project, "name", item.key.name,
			"condition", item.condType, "error", strings.TrimSpace(string(raw)),
			"hint", "the assistant's identity lacks patch on capabilitybindings/status in this project; "+
				"Milo IAM has no subresource axis, so this may need a distinct subresource permission "+
				"rather than capabilitybindings.patch on the parent",
			"effect", "status conditions will not appear on any binding; chats are unaffected")
		w.forget(item.key, item.condType)
		w.metrics.RecordCapabilityStatusWrite("forbidden")
	default:
		w.fail(item, "error", "capability.crd.status_write_failed", endpoint,
			fmt.Sprintf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(raw))))
	}
}

// fail records a failed write and forgets the recorded condition so the next
// observation retries it. Without the forget, an optimistic state update would
// make one failed write permanent: the condition would read as "already
// written" forever and the binding would never be reported again.
//
// The 403 path above does the same three things inline rather than calling
// this, because its log line carries the subresource-permission hypothesis and
// must not be flattened into the generic write-failure shape.
func (w *StatusWriter) fail(item statusWrite, outcome, event, endpoint, detail string) {
	w.logger.Warn(event,
		"url", endpoint, "projectName", item.key.project, "name", item.key.name,
		"condition", item.condType, "error", detail)
	w.forget(item.key, item.condType)
	w.metrics.RecordCapabilityStatusWrite(outcome)
}

// renderStatusPatch builds the merge patch body: observedGeneration plus the
// FULL condition set (see mergePatchContentType for why full). Conditions are
// sorted by type so an identical state always produces identical bytes, which
// is what makes a redundant write a no-op at the server rather than a new
// resourceVersion.
func renderStatusPatch(state *bindingState) ([]byte, error) {
	conds := make([]metav1.Condition, 0, len(state.conds))
	for _, c := range state.conds {
		conds = append(conds, c)
	}
	sort.Slice(conds, func(i, j int) bool { return conds[i].Type < conds[j].Type })

	status := capv1alpha1.CapabilityBindingStatus{
		ObservedGeneration: state.generation,
		Conditions:         conds,
	}
	return json.Marshal(map[string]any{"status": status})
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

var _ BindingObserver = (*StatusWriter)(nil)
