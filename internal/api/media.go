package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/yhw5231/fluxgate/internal/domain"
	"github.com/yhw5231/fluxgate/internal/proxy"
	"github.com/yhw5231/fluxgate/internal/store"
)

// Generation endpoints: images, videos, and embeddings.
//
// An embedding is an ordinary model-routed request whose answer happens to be a
// vector, so it needs nothing beyond a route of its own. A generation is not: the
// upstream spends seconds to minutes producing the artifact before it answers at
// all, and for a video it does not answer with the artifact but with the
// identifier of work it has only just started. Those two differences are what this
// file exists for.
//
// A request that follows up on a job names no model, so the routing table cannot
// say where it belongs: only the upstream that accepted the job can answer for it.
// The gateway is the one party that knows both the identifier and the line it was
// created on, so it records the pair and uses it to pin the follow-up request to
// that line.

// proxyEndpoint describes one OpenAI-compatible route the gateway proxies. The
// routes are otherwise alike — a body with a model in it, forwarded to the
// upstream that serves that model — so only what differs is stated here.
type proxyEndpoint struct {
	// method and path are the route as it is registered and as it is sent upstream.
	method string
	path   string
	// media marks an endpoint whose upstream produces an artifact and may hold the
	// connection open for as long as that takes. Such a request is bounded by the
	// gateway's media timeout and dispatched through the media client, because the
	// response-header timeout a chat completion is bounded by is shorter than a
	// generation takes to start answering.
	media bool
	// recordsJob marks an endpoint whose answer creates upstream-side work the
	// client will come back to. Its answer is read for the identifier of that work
	// and the pair is remembered, which is what routes the follow-up requests.
	recordsJob bool
}

// proxyEndpoints is the OpenAI-compatible surface the gateway serves.
var proxyEndpoints = []proxyEndpoint{
	{method: http.MethodPost, path: "/v1/chat/completions"},
	{method: http.MethodPost, path: "/v1/responses"},
	{method: http.MethodPost, path: "/v1/messages"},
	{method: http.MethodPost, path: "/v1/embeddings"},
	{method: http.MethodPost, path: "/v1/images/generations", media: true},
	// An image edit and a variation upload the image they work from as a multipart
	// form, with the model as one field beside it. The body is rebuilt for the
	// channel it is sent to — the model is rewritten and the form gets a new
	// boundary — while the uploaded parts travel unchanged.
	{method: http.MethodPost, path: "/v1/images/edits", media: true},
	{method: http.MethodPost, path: "/v1/images/variations", media: true},
	{method: http.MethodPost, path: "/v1/videos", media: true, recordsJob: true},
}

// defaultMediaRequestTimeout bounds a generation request when the operator has not
// said otherwise. It is deliberately far longer than the request timeout a chat
// completion is held to: a large image or a video is minutes of upstream work, and
// a gateway that gave up after the chat timeout would turn a slow generation into
// a failure.
const defaultMediaRequestTimeout = 10 * time.Minute

// mediaJobKeep is how many generation jobs the record holds. It is a constant
// rather than a setting because the record is a routing aid, not a store of
// results: the newest few thousand cover every job an upstream will still answer
// for, and a job that has aged out is answered as unknown — the same answer the
// client gets for an identifier its upstream has forgotten.
const mediaJobKeep = 2000

// maxJobBodyBytes bounds how much of a creation response is held in order to read
// the job identifier out of it. The answer is a small JSON document; an answer
// larger than this is not one the gateway needs to understand, and a job it cannot
// identify is one whose follow-up requests are answered as unknown rather than
// misrouted to a line that never heard of it.
const maxJobBodyBytes = int64(1 << 18)

// mediaTimeout is the bound the gateway puts on one generation request, or zero
// for an endpoint that is not a generation.
func (s *Server) mediaTimeout(endpoint proxyEndpoint) time.Duration {
	if !endpoint.media {
		return 0
	}
	if timeout := s.MediaRequestTimeout; timeout > 0 {
		return timeout
	}
	return defaultMediaRequestTimeout
}

// MediaJobStore remembers where upstream-side generation work was created, so that
// a request naming only a job identifier can reach the line that holds it. It is
// satisfied by the SQLite store.
type MediaJobStore interface {
	RecordMediaJob(ctx context.Context, job store.MediaJob, keep int) error
	LookupMediaJob(ctx context.Context, kind, jobID string, keyID int64) (store.MediaJob, bool, error)
	DeleteMediaJob(ctx context.Context, jobID string) error
}

// captureMediaJob reads the identifier of the work an upstream has just created
// and records the line it was created on. The response body is put back exactly as
// it arrived, so the client receives the upstream's own answer.
//
// A gateway with no job record, or a creation response that names no identifier,
// leaves the request to finish on its own: the client still gets its answer, and
// only a follow-up request discovers there is nothing to route it by.
func (s *Server) captureMediaJob(r *http.Request, result proxy.Result, key store.DownstreamAPIKey, record domain.RequestRecord) io.ReadCloser {
	body := result.Response.Body
	// A creation that failed created nothing, so there is no identifier to keep.
	if s.MediaJobs == nil || body == nil ||
		result.Response.StatusCode < http.StatusOK || result.Response.StatusCode >= http.StatusMultipleChoices {
		return body
	}

	// The beginning of the answer is all that is read: a creation answer is a small
	// document, and an answer that is longer than the bound is not one the gateway
	// needs to understand. ReadFull is what makes a body shorter than the bound
	// come back whole rather than waiting for bytes that will never arrive.
	prefix := make([]byte, maxJobBodyBytes)
	count, _ := io.ReadFull(body, prefix)
	prefix = prefix[:count]
	rebuilt := &prefixedReadCloser{Reader: io.MultiReader(bytes.NewReader(prefix), body), Closer: body}

	jobID, ok := parseJobID(prefix)
	if !ok {
		return rebuilt
	}
	// The record is written while the request's context is still live, which it is
	// here because the response has not been handed over yet. A record that cannot
	// be written is reported in the process log and never changes what the client is
	// told: the creation succeeded, and it is the follow-up request that will have
	// to be explained.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
	defer cancel()
	if err := s.MediaJobs.RecordMediaJob(ctx, store.MediaJob{
		JobID:     jobID,
		Kind:      store.JobKindVideo,
		ChannelID: result.Attempt.ChannelID,
		KeyID:     key.ID,
		Model:     record.Model,
		CreatedAt: time.Now().UTC(),
	}, mediaJobKeep); err != nil {
		s.logger().Warn("media_job_record_failed", "error", err.Error(), "job_id", jobID)
	}
	return rebuilt
}

// jobIdentifierKeys are the field names an upstream names newly created work by.
// Providers agree on the shape of the answer — a JSON object with an identifier in
// it — but not on what the identifier is called, so the names that mean the same
// thing are all read rather than making an operator rename a working upstream's
// output.
var jobIdentifierKeys = []string{"id", "task_id", "video_id", "job_id"}

// parseJobID reads the identifier of created work out of an upstream's answer. The
// identifier sits at the top level for most providers and under a container for
// the rest, so both are read.
func parseJobID(body []byte) (string, bool) {
	var decoded any
	if err := json.Unmarshal(body, &decoded); err != nil {
		return "", false
	}
	return findJobID(decoded, 0)
}

// findJobID walks a decoded answer for an identifier, descending into the
// containers an answer is known to put one in. It descends a bounded number of
// levels, so a deeply nested document cannot make the walk expensive, and it stops
// at the first identifier it finds rather than collecting every field that happens
// to carry one.
func findJobID(value any, depth int) (string, bool) {
	if depth > 3 {
		return "", false
	}
	switch typed := value.(type) {
	case map[string]any:
		for _, key := range jobIdentifierKeys {
			text, ok := typed[key].(string)
			if !ok {
				continue
			}
			if trimmed := strings.TrimSpace(text); trimmed != "" {
				return trimmed, true
			}
		}
		for _, key := range []string{"data", "result", "output"} {
			if nested, present := typed[key]; present {
				if found, ok := findJobID(nested, depth+1); ok {
					return found, true
				}
			}
		}
	case []any:
		for _, entry := range typed {
			if found, ok := findJobID(entry, depth+1); ok {
				return found, true
			}
		}
	}
	return "", false
}

// prefixedReadCloser re-emits bytes that were read ahead and put back, so a
// response whose beginning was read for a job identifier still reaches the client
// whole.
type prefixedReadCloser struct {
	io.Reader
	io.Closer
}

// handleVideoJob answers a client asking how the work it started is going.
func (s *Server) handleVideoJob(w http.ResponseWriter, r *http.Request) {
	s.serveVideoJob(w, r, false)
}

// handleVideoJobContent serves the finished artifact.
func (s *Server) handleVideoJobContent(w http.ResponseWriter, r *http.Request) {
	s.serveVideoJob(w, r, true)
}

// handleVideoJobDelete removes a video at its upstream and forgets where it was.
//
// The record is forgotten only once the upstream has agreed the video is gone: a
// deletion the upstream refused leaves the job findable, so the client sees the
// upstream's own reason on the next request instead of losing the trail.
func (s *Server) handleVideoJobDelete(w http.ResponseWriter, r *http.Request) {
	jobID := strings.TrimSpace(r.PathValue("id"))
	status := s.serveVideoJob(w, r, false)
	switch status {
	case http.StatusOK, http.StatusNoContent, http.StatusAccepted, http.StatusNotFound:
	default:
		return
	}
	if s.MediaJobs == nil {
		return
	}
	// The deletion is reported as done, so the record is dropped. The row is not
	// worth keeping: a follow-up request for a video the upstream has forgotten is
	// answered by the upstream itself, which is the party that decides whether the
	// video exists.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
	defer cancel()
	if err := s.MediaJobs.DeleteMediaJob(ctx, jobID); err != nil {
		s.logger().Warn("media_job_forget_failed", "error", err.Error(), "job_id", jobID)
	}
}

// serveVideoJob dispatches one request about a recorded video job, returning the
// status the client was answered with. content selects the artifact download,
// which is a different upstream path under the same job.
func (s *Server) serveVideoJob(w http.ResponseWriter, r *http.Request, content bool) int {
	jobID := r.PathValue("id")
	return s.serveMediaJob(w, r, mediaJobRequest{
		kind:     store.JobKindVideo,
		jobID:    jobID,
		upstream: videoUpstreamPath(jobID, content),
		missing:  "video_not_found",
		unknown:  "no video with that identifier was created through this gateway for this API key",
	})
}

// mediaJobRequest describes one request about recorded upstream-side work.
type mediaJobRequest struct {
	// kind is the endpoint family the job belongs to, which keeps an identifier
	// from one family from reaching another family's work.
	kind string
	// jobID is the identifier the client is asking about.
	jobID string
	// upstream is the path the request is sent to on the upstream.
	upstream string
	// missing and unknown phrase the answer for an identifier this gateway did not
	// record.
	missing string
	unknown string
}

// serveMediaJob resolves a recorded job, pins the request to the line that holds
// it, and forwards it, returning the status the client was answered with.
//
// It is the whole of what a follow-up endpoint does, and it is written once
// because a status poll, an artifact download and a deletion differ only in the
// method they carry and the path they are sent to.
func (s *Server) serveMediaJob(w http.ResponseWriter, r *http.Request, request mediaJobRequest) int {
	if s.Engine == nil {
		writeError(w, http.StatusServiceUnavailable, "gateway_not_ready", "gateway engine is not configured")
		return http.StatusServiceUnavailable
	}
	key, ok := s.authenticate(w, r)
	if !ok {
		return http.StatusUnauthorized
	}

	started := time.Now()
	record := domain.RequestRecord{
		RequestID: newRequestID(),
		At:        started.UTC(),
		Method:    r.Method,
		Path:      r.URL.Path,
		ClientIP:  clientIP(r),
		KeyID:     key.ID,
		KeyName:   key.Name,
	}
	// refuse answers the client, records why, and reports the status it used, so a
	// caller acting on the outcome of the request has it in hand.
	refuse := func(status int, code, message string, details map[string]any) int {
		record.Status = status
		record.ErrorCode = code
		record.ErrorMessage = message
		writeErrorDetails(w, status, code, message, details)
		return status
	}
	w.Header().Set("X-Fluxgate-Request-Id", record.RequestID)
	defer func() {
		record.DurationMS = time.Since(started).Milliseconds()
		s.recordProxiedRequest(r, record)
	}()

	if s.MediaJobs == nil {
		return refuse(http.StatusServiceUnavailable, "media_jobs_unavailable",
			"this gateway does not keep the record a video job would be found by", nil)
	}
	jobID := strings.TrimSpace(request.jobID)
	if jobID == "" {
		return refuse(http.StatusBadRequest, "missing_job_id", "a video identifier is required", nil)
	}
	job, found, err := s.MediaJobs.LookupMediaJob(r.Context(), request.kind, jobID, key.ID)
	if err != nil {
		s.logger().Error("media_job_unreadable", "error", err.Error())
		return refuse(http.StatusInternalServerError, "media_jobs_unreadable", "failed to read the record of the video jobs", nil)
	}
	if !found {
		// An identifier this gateway never recorded, and one recorded for another
		// caller, are answered the same way: whether some other key's job exists is
		// not this caller's business.
		return refuse(http.StatusNotFound, request.missing, request.unknown, nil)
	}
	record.Model = job.Model

	// A follow-up is a generation in every way that matters to dispatch: its
	// upstream may take as long to answer as the request that created the job did,
	// and the artifact it eventually produces is a multi-megabyte transfer.
	followUp := proxyEndpoint{media: true}
	result, err := s.Engine.Forward(r.Context(), domain.Request{
		Method:      r.Method,
		Path:        request.upstream,
		Headers:     sanitizedHeaders(r.Header),
		ContentType: r.Header.Get("Content-Type"),
		Model:       job.Model,
		Policy:      key.Policy(),
		OnlyChannel: job.ChannelID,
		Media:       followUp.media,
		Timeout:     s.mediaTimeout(followUp),
		RequestID:   record.RequestID,
	})
	record.Attempts = result.Trace.Attempts
	if err != nil {
		failure := classifyDispatchError(err, result.Trace, record.RequestID)
		return refuse(failure.Status, failure.Code, failure.Message, failure.Details)
	}
	defer result.Response.Body.Close()

	record.Status = result.Response.StatusCode
	copyResponseHeaders(w.Header(), result.Response.Header)
	w.Header().Set("X-Fluxgate-Upstream-Channel", result.Attempt.ChannelID)
	w.WriteHeader(result.Response.StatusCode)
	if strings.Contains(strings.ToLower(result.Response.Header.Get("Content-Type")), "text/event-stream") {
		streamResponse(w, result.Response.Body)
		return result.Response.StatusCode
	}
	_, _ = io.Copy(w, result.Response.Body)
	return result.Response.StatusCode
}

// videoUpstreamPath is the upstream path a request about a video job is sent to.
// The identifier is escaped as a single path segment, so an identifier carrying a
// slash cannot turn a request about a job into a request for something else.
func videoUpstreamPath(jobID string, content bool) string {
	path := "/v1/videos/" + url.PathEscape(strings.TrimSpace(jobID))
	if content {
		path += "/content"
	}
	return path
}
