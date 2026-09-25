package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yhw5231/fluxgate/internal/domain"
	"github.com/yhw5231/fluxgate/internal/proxy"
	"github.com/yhw5231/fluxgate/internal/router"
	"github.com/yhw5231/fluxgate/internal/store"
)

// Generation endpoints: embeddings, images, and videos.
//
// What each of these has to prove is that the request reaches an upstream that
// can serve it and that the answer comes back whole. For an image edit that means
// the uploaded form survives a rewrite it cannot be dropped by; for a generation it
// means the request is not cut off by a timeout meant for a chat completion; and
// for a video it means the follow-up requests reach the one upstream holding the
// job rather than a line chosen by the routing table.

// fakeMediaJobs is an in-memory record of generation jobs.
type fakeMediaJobs struct {
	mu        sync.Mutex
	jobs      map[string]store.MediaJob
	recordErr error
	lookupErr error
	deleted   []string
}

func newFakeMediaJobs() *fakeMediaJobs {
	return &fakeMediaJobs{jobs: map[string]store.MediaJob{}}
}

func (f *fakeMediaJobs) RecordMediaJob(_ context.Context, job store.MediaJob, _ int) error {
	if f.recordErr != nil {
		return f.recordErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.jobs[job.JobID] = job
	return nil
}

func (f *fakeMediaJobs) LookupMediaJob(_ context.Context, kind, jobID string, keyID int64) (store.MediaJob, bool, error) {
	if f.lookupErr != nil {
		return store.MediaJob{}, false, f.lookupErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	job, ok := f.jobs[jobID]
	if !ok || job.Kind != kind || job.KeyID != keyID {
		return store.MediaJob{}, false, nil
	}
	return job, true, nil
}

func (f *fakeMediaJobs) DeleteMediaJob(_ context.Context, jobID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, jobID)
	delete(f.jobs, jobID)
	return nil
}

func (f *fakeMediaJobs) channelOf(t *testing.T, jobID string) string {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	job, ok := f.jobs[jobID]
	if !ok {
		t.Fatalf("job %q was not recorded", jobID)
	}
	return job.ChannelID
}

// failingRoundTripper stands in for one of the engine's two clients, so a test can
// tell which of them carried a request.
type failingRoundTripper struct{ reason string }

func (t failingRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New(t.reason)
}

// multipartBody builds the form an image edit uploads: the model beside the image
// it works from.
func multipartBody(t *testing.T, fields map[string]string, fileName string, payload []byte) ([]byte, string) {
	t.Helper()
	var buffer bytes.Buffer
	writer := multipart.NewWriter(&buffer)
	for name, value := range fields {
		if err := writer.WriteField(name, value); err != nil {
			t.Fatalf("write %s field: %v", name, err)
		}
	}
	part, err := writer.CreateFormFile("image", fileName)
	if err != nil {
		t.Fatalf("create file part: %v", err)
	}
	if _, err := part.Write(payload); err != nil {
		t.Fatalf("write file part: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close form: %v", err)
	}
	return buffer.Bytes(), writer.FormDataContentType()
}

// testEngine builds an engine over the given routes with the given clients.
func testEngine(t *testing.T, routes []domain.Route, client, mediaClient *http.Client) *proxy.Engine {
	t.Helper()
	return &proxy.Engine{
		Selector:    router.NewMemorySelector(routes),
		Client:      client,
		MediaClient: mediaClient,
		Policy:      domain.RetryPolicy{MaxAttempts: 1, MaxAttemptsPerChannel: 1},
		Sleep:       func(context.Context, time.Duration) error { return nil },
	}
}

// An embedding is an ordinary model-routed request: it names a model, the route
// maps that name to the one the upstream knows, and the answer comes back as the
// upstream wrote it.
func TestEmbeddingsAreForwardedWithTheRoutedModel(t *testing.T) {
	var seen struct {
		path string
		body map[string]any
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.path = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&seen.body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0.25,-0.5]}]}`))
	}))
	defer upstream.Close()

	routes := []domain.Route{{
		ID:           1,
		ModelPattern: "text-embedding-3-small",
		ModelMapping: domain.ModelMapping{{Pattern: "text-embedding-3-small", Target: "bge-m3"}},
		Enabled:      true,
		Channels: []domain.Channel{{
			ID: "channel-embed", BaseURL: upstream.URL, Enabled: true, SiteID: 1,
		}},
	}}
	server := &Server{
		Engine:        testEngine(t, routes, nil, nil),
		Authenticator: allowTestAuthentication(),
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/embeddings",
		strings.NewReader(`{"model":"text-embedding-3-small","input":"hello"}`))
	request.Header.Set("Authorization", "Bearer test-key")
	response := httptest.NewRecorder()

	server.Handler().ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", response.Code, response.Body.String())
	}
	if seen.path != "/v1/embeddings" {
		t.Fatalf("upstream path = %q, want /v1/embeddings", seen.path)
	}
	if got := seen.body["model"]; got != "bge-m3" {
		t.Fatalf("upstream model = %v, want the mapped bge-m3", got)
	}
	if got := seen.body["input"]; got != "hello" {
		t.Fatalf("upstream input = %v, want hello", got)
	}
	if !strings.Contains(response.Body.String(), `"embedding":[0.25,-0.5]`) {
		t.Fatalf("body = %s, want the upstream's vectors", response.Body.String())
	}
}

// A generation is dispatched through the media client, and everything else through
// the ordinary one: the two differ in how long an upstream may take to start
// answering, so sending a generation down the ordinary path is what would make a
// slow image fail.
func TestGenerationsAreDispatchedThroughTheMediaClient(t *testing.T) {
	served := make(chan string, 2)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served <- r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"url":"https://cdn.example/image.png"}]}`))
	}))
	defer upstream.Close()

	routes := testRoutes(domain.Channel{ID: "channel-media", BaseURL: upstream.URL, Enabled: true, SiteID: 1})
	engine := testEngine(t, routes,
		&http.Client{Transport: failingRoundTripper{reason: "the ordinary client carried a generation"}},
		&http.Client{},
	)
	server := &Server{Engine: engine, Authenticator: allowTestAuthentication()}

	generation := httptest.NewRequest(http.MethodPost, "/v1/images/generations",
		strings.NewReader(`{"model":"gpt-image-1","prompt":"a lighthouse at dusk"}`))
	generation.Header.Set("Authorization", "Bearer test-key")
	generationResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(generationResponse, generation)

	if generationResponse.Code != http.StatusOK {
		t.Fatalf("generation status = %d, want 200; body = %s",
			generationResponse.Code, generationResponse.Body.String())
	}
	if got := <-served; got != "/v1/images/generations" {
		t.Fatalf("upstream path = %q, want /v1/images/generations", got)
	}
	if got := generationResponse.Header().Get("X-Fluxgate-Upstream-Channel"); got != "channel-media" {
		t.Fatalf("X-Fluxgate-Upstream-Channel = %q, want channel-media", got)
	}

	// The other direction: a chat completion is not a generation and must stay on
	// the ordinary client.
	chat := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-5","messages":[]}`))
	chat.Header.Set("Authorization", "Bearer test-key")
	chatResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(chatResponse, chat)

	if chatResponse.Code != http.StatusBadGateway {
		t.Fatalf("chat status = %d, want 502 from the ordinary client; body = %s",
			chatResponse.Code, chatResponse.Body.String())
	}
	select {
	case path := <-served:
		t.Fatalf("the media client carried %s, which is not a generation", path)
	default:
	}
}

// A generation outlives the per-channel timeout a chat completion is bounded by.
// The upstream here takes longer than the channel's own timeout to start
// answering, which is what a real image generation does, and the request has to
// survive it.
func TestSlowGenerationSurvivesThePerChannelTimeout(t *testing.T) {
	const workTime = 120 * time.Millisecond
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(workTime)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"url":"https://cdn.example/slow.png"}]}`))
	}))
	defer upstream.Close()

	// The channel is bounded far below what the generation takes, so only the media
	// timeout can be what carries the request.
	routes := testRoutes(domain.Channel{
		ID: "channel-slow", BaseURL: upstream.URL, Enabled: true, SiteID: 1,
		RequestTimeout: 20 * time.Millisecond,
	})
	server := &Server{
		Engine:              testEngine(t, routes, &http.Client{}, &http.Client{}),
		Authenticator:       allowTestAuthentication(),
		MediaRequestTimeout: 5 * time.Second,
	}

	generation := httptest.NewRequest(http.MethodPost, "/v1/images/generations",
		strings.NewReader(`{"model":"gpt-image-1","prompt":"a slow one"}`))
	generation.Header.Set("Authorization", "Bearer test-key")
	generationResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(generationResponse, generation)

	if generationResponse.Code != http.StatusOK {
		t.Fatalf("generation status = %d, want 200; body = %s",
			generationResponse.Code, generationResponse.Body.String())
	}

	// The same upstream, the same channel, and a chat completion: the per-channel
	// timeout is what bounds it, and it is too short for this upstream.
	chat := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-5","messages":[]}`))
	chat.Header.Set("Authorization", "Bearer test-key")
	chatResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(chatResponse, chat)

	if chatResponse.Code != http.StatusBadGateway {
		t.Fatalf("chat status = %d, want 502 once the per-channel timeout expires; body = %s",
			chatResponse.Code, chatResponse.Body.String())
	}
}

// An image edit uploads the image it works from. The model is rewritten for the
// channel, and the upload has to reach the upstream as a form — not re-labelled as
// JSON, and not rebuilt without its parts.
func TestImageEditRewritesTheModelAndKeepsTheUpload(t *testing.T) {
	payload := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0x01, 0x02, 0x03}
	var (
		seenModel       string
		seenPrompt      string
		seenImage       []byte
		seenFileName    string
		seenContentType string
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenContentType = r.Header.Get("Content-Type")
		reader, err := r.MultipartReader()
		if err != nil {
			t.Errorf("upstream could not read the form: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		for {
			part, err := reader.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Errorf("read part: %v", err)
				break
			}
			body, _ := io.ReadAll(part)
			switch part.FormName() {
			case "model":
				seenModel = string(body)
			case "prompt":
				seenPrompt = string(body)
			case "image":
				seenImage = body
				seenFileName = part.FileName()
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"url":"https://cdn.example/edited.png"}]}`))
	}))
	defer upstream.Close()

	routes := []domain.Route{{
		ID:           1,
		ModelPattern: "gpt-image-1",
		ModelMapping: domain.ModelMapping{{Pattern: "gpt-image-1", Target: "gpt-image-1-2026"}},
		Enabled:      true,
		Channels: []domain.Channel{{
			ID: "channel-image", BaseURL: upstream.URL, Enabled: true, SiteID: 1,
		}},
	}}
	server := &Server{
		Engine:        testEngine(t, routes, nil, &http.Client{}),
		Authenticator: allowTestAuthentication(),
	}

	body, contentType := multipartBody(t, map[string]string{
		"model":  "gpt-image-1",
		"prompt": "add a red kite to the sky",
	}, "photo.png", payload)
	request := httptest.NewRequest(http.MethodPost, "/v1/images/edits", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer test-key")
	request.Header.Set("Content-Type", contentType)
	response := httptest.NewRecorder()

	server.Handler().ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", response.Code, response.Body.String())
	}
	if !strings.HasPrefix(seenContentType, "multipart/form-data") {
		t.Fatalf("upstream Content-Type = %q, want the form's own", seenContentType)
	}
	if seenModel != "gpt-image-1-2026" {
		t.Fatalf("upstream model field = %q, want the mapped gpt-image-1-2026", seenModel)
	}
	if seenPrompt != "add a red kite to the sky" {
		t.Fatalf("upstream prompt field = %q, want it preserved", seenPrompt)
	}
	if !bytes.Equal(seenImage, payload) {
		t.Fatalf("upstream image = %v, want the uploaded bytes %v", seenImage, payload)
	}
	if seenFileName != "photo.png" {
		t.Fatalf("upstream filename = %q, want photo.png", seenFileName)
	}
}

// A generation endpoint still answers a request that names no model with the same
// refusal a chat completion does, rather than forwarding a request it cannot route.
func TestGenerationEndpointRequiresAModel(t *testing.T) {
	server := &Server{
		Engine:        testEngine(t, testRoutes(), nil, nil),
		Authenticator: allowTestAuthentication(),
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/images/generations",
		strings.NewReader(`{"prompt":"a lighthouse"}`))
	request.Header.Set("Authorization", "Bearer test-key")
	response := httptest.NewRecorder()

	server.Handler().ServeHTTP(response, request)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", response.Code)
	}
	if !strings.Contains(response.Body.String(), `"code":"missing_model"`) {
		t.Fatalf("body = %s, want missing_model", response.Body.String())
	}
}

// The point of the job record: a video is created on one line, and the requests
// that follow name no model at all. They have to reach that same line even when
// the routing table would now prefer another upstream, because no other upstream
// has ever heard of the job.
func TestVideoFollowUpReachesTheLineThatHoldsTheJob(t *testing.T) {
	var (
		mu       sync.Mutex
		requests []string
	)
	record := func(prefix string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			requests = append(requests, prefix+" "+r.URL.Path)
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			if r.Method == http.MethodDelete {
				_, _ = w.Write([]byte(`{"id":"video_1","deleted":true}`))
				return
			}
			if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/content") {
				w.Header().Set("Content-Type", "video/mp4")
				_, _ = w.Write([]byte("mp4-bytes"))
				return
			}
			if r.Method == http.MethodGet {
				_, _ = w.Write([]byte(`{"id":"video_1","status":"completed"}`))
				return
			}
			_, _ = w.Write([]byte(`{"id":"video_1","object":"video","status":"queued"}`))
		}
	}
	first := httptest.NewServer(record("first"))
	defer first.Close()
	second := httptest.NewServer(record("second"))
	defer second.Close()

	routes := []domain.Route{{
		ID:           1,
		ModelPattern: "sora-2",
		Enabled:      true,
		Channels: []domain.Channel{
			{ID: "channel-first", BaseURL: first.URL, Enabled: true, SiteID: 1, SitePriority: 10},
			{ID: "channel-second", BaseURL: second.URL, Enabled: true, SiteID: 2, SitePriority: 0},
		},
	}}
	selector := router.NewMemorySelector(routes)
	engine := &proxy.Engine{
		Selector: selector,
		Policy:   domain.RetryPolicy{MaxAttempts: 1, MaxAttemptsPerChannel: 1},
		Sleep:    func(context.Context, time.Duration) error { return nil },
	}
	jobs := newFakeMediaJobs()
	server := &Server{
		Engine:        engine,
		Authenticator: allowTestAuthentication(),
		MediaJobs:     jobs,
		Routes:        routes,
	}

	create := httptest.NewRequest(http.MethodPost, "/v1/videos",
		strings.NewReader(`{"model":"sora-2","prompt":"a paper boat crossing a puddle"}`))
	create.Header.Set("Authorization", "Bearer test-key")
	createResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(createResponse, create)

	if createResponse.Code != http.StatusOK {
		t.Fatalf("create status = %d, want 200; body = %s", createResponse.Code, createResponse.Body.String())
	}
	if got := createResponse.Header().Get("X-Fluxgate-Upstream-Channel"); got != "channel-first" {
		t.Fatalf("create served by %q, want the preferred channel-first", got)
	}
	if got := jobs.channelOf(t, "video_1"); got != "channel-first" {
		t.Fatalf("recorded channel = %q, want channel-first", got)
	}
	if !strings.Contains(createResponse.Body.String(), `"status":"queued"`) {
		t.Fatalf("create body = %s, want the upstream's own answer", createResponse.Body.String())
	}

	// The other upstream becomes the preferred one, so a follow-up request that was
	// routed by model would go somewhere the job does not exist.
	preferred := append([]domain.Route(nil), routes...)
	preferred[0].Channels = []domain.Channel{
		{ID: "channel-first", BaseURL: first.URL, Enabled: true, SiteID: 1, SitePriority: 0},
		{ID: "channel-second", BaseURL: second.URL, Enabled: true, SiteID: 2, SitePriority: 99},
	}
	selector.SetRoutes(preferred)

	poll := httptest.NewRequest(http.MethodGet, "/v1/videos/video_1", nil)
	poll.Header.Set("Authorization", "Bearer test-key")
	pollResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(pollResponse, poll)

	if pollResponse.Code != http.StatusOK {
		t.Fatalf("poll status = %d, want 200; body = %s", pollResponse.Code, pollResponse.Body.String())
	}
	if got := pollResponse.Header().Get("X-Fluxgate-Upstream-Channel"); got != "channel-first" {
		t.Fatalf("poll served by %q, want the recorded channel-first", got)
	}
	if !strings.Contains(pollResponse.Body.String(), `"status":"completed"`) {
		t.Fatalf("poll body = %s, want the upstream's own answer", pollResponse.Body.String())
	}

	content := httptest.NewRequest(http.MethodGet, "/v1/videos/video_1/content", nil)
	content.Header.Set("Authorization", "Bearer test-key")
	contentResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(contentResponse, content)

	if contentResponse.Code != http.StatusOK {
		t.Fatalf("content status = %d, want 200; body = %s", contentResponse.Code, contentResponse.Body.String())
	}
	if got := contentResponse.Header().Get("X-Fluxgate-Upstream-Channel"); got != "channel-first" {
		t.Fatalf("content served by %q, want the recorded channel-first", got)
	}
	if got := contentResponse.Header().Get("Content-Type"); got != "video/mp4" {
		t.Fatalf("content Content-Type = %q, want video/mp4", got)
	}
	if contentResponse.Body.String() != "mp4-bytes" {
		t.Fatalf("content body = %q, want the artifact", contentResponse.Body.String())
	}

	mu.Lock()
	defer mu.Unlock()
	for _, seen := range requests {
		if strings.HasPrefix(seen, "second ") {
			t.Fatalf("the follow-up requests reached the wrong upstream: %v", requests)
		}
	}
	wanted := []string{"first /v1/videos", "first /v1/videos/video_1", "first /v1/videos/video_1/content"}
	if strings.Join(requests, ",") != strings.Join(wanted, ",") {
		t.Fatalf("upstream requests = %v, want %v", requests, wanted)
	}
}

// A video belongs to the caller that created it, so another key asking for the
// same identifier is told there is no such video rather than being routed to
// someone else's work.
func TestVideoJobIsNotVisibleToAnotherKey(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"video_9","status":"queued"}`))
	}))
	defer upstream.Close()

	jobs := newFakeMediaJobs()
	server := &Server{
		Engine: testEngine(t, testRoutes(domain.Channel{
			ID: "channel-video", BaseURL: upstream.URL, Enabled: true, SiteID: 1,
		}), nil, nil),
		Authenticator: &fakeAuthenticator{
			wantCredential: "test-key",
			key:            store.DownstreamAPIKey{ID: 7, Name: "team-a"},
		},
		MediaJobs: jobs,
	}

	create := httptest.NewRequest(http.MethodPost, "/v1/videos", strings.NewReader(`{"model":"sora-2"}`))
	create.Header.Set("Authorization", "Bearer test-key")
	createResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(createResponse, create)
	if createResponse.Code != http.StatusOK {
		t.Fatalf("create status = %d, want 200; body = %s", createResponse.Code, createResponse.Body.String())
	}

	// The same callers' own key finds it.
	owner := httptest.NewRequest(http.MethodGet, "/v1/videos/video_9", nil)
	owner.Header.Set("Authorization", "Bearer test-key")
	ownerResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(ownerResponse, owner)
	if ownerResponse.Code != http.StatusOK {
		t.Fatalf("owner poll status = %d, want 200; body = %s", ownerResponse.Code, ownerResponse.Body.String())
	}

	// A different key does not.
	other := httptest.NewRequest(http.MethodGet, "/v1/videos/video_9", nil)
	other.Header.Set("Authorization", "Bearer other-key")
	otherResponse := httptest.NewRecorder()
	server.Authenticator = &fakeAuthenticator{
		wantCredential: "other-key",
		key:            store.DownstreamAPIKey{ID: 8, Name: "team-b"},
	}
	server.Handler().ServeHTTP(otherResponse, other)
	if otherResponse.Code != http.StatusNotFound {
		t.Fatalf("other key status = %d, want 404", otherResponse.Code)
	}
	if !strings.Contains(otherResponse.Body.String(), `"code":"video_not_found"`) {
		t.Fatalf("body = %s, want video_not_found", otherResponse.Body.String())
	}
}

// An identifier the gateway never recorded cannot be routed anywhere: no upstream
// is contacted, and the client is told the video is unknown rather than being sent
// to an upstream that would answer for someone else's job.
func TestUnknownVideoJobIsNotFound(t *testing.T) {
	var contacted bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contacted = true
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	server := &Server{
		Engine: testEngine(t, testRoutes(domain.Channel{
			ID: "channel-video", BaseURL: upstream.URL, Enabled: true, SiteID: 1,
		}), nil, nil),
		Authenticator: allowTestAuthentication(),
		MediaJobs:     newFakeMediaJobs(),
	}
	for _, path := range []string{"/v1/videos/video_unknown", "/v1/videos/video_unknown/content"} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.Header.Set("Authorization", "Bearer test-key")
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)

		if response.Code != http.StatusNotFound {
			t.Fatalf("%s status = %d, want 404", path, response.Code)
		}
		if !strings.Contains(response.Body.String(), `"code":"video_not_found"`) {
			t.Fatalf("%s body = %s, want video_not_found", path, response.Body.String())
		}
	}
	if contacted {
		t.Fatal("an unknown job identifier reached an upstream")
	}
}

// Deleting a video removes it at the upstream that holds it, and the record is
// dropped with it: what is left is a gateway that no longer knows the job existed.
func TestVideoDeleteRemovesTheVideoAndForgetsTheJob(t *testing.T) {
	var (
		mu        sync.Mutex
		methods   []string
		paths     []string
		deleteGap bool
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		methods = append(methods, r.Method)
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodDelete {
			if !deleteGap {
				// A deletion the upstream refuses must leave the record alone, so
				// this answer is the one that fails.
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(`{"error":{"message":"the video is still being generated"}}`))
				return
			}
			_, _ = w.Write([]byte(`{"id":"video_5","deleted":true}`))
			return
		}
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"id":"video_5","status":"completed"}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"video_5","status":"queued"}`))
	}))
	defer upstream.Close()

	jobs := newFakeMediaJobs()
	server := &Server{
		Engine: testEngine(t, testRoutes(domain.Channel{
			ID: "channel-video", BaseURL: upstream.URL, Enabled: true, SiteID: 1,
		}), nil, nil),
		Authenticator: allowTestAuthentication(),
		MediaJobs:     jobs,
	}

	create := httptest.NewRequest(http.MethodPost, "/v1/videos", strings.NewReader(`{"model":"sora-2"}`))
	create.Header.Set("Authorization", "Bearer test-key")
	createResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(createResponse, create)
	if createResponse.Code != http.StatusOK {
		t.Fatalf("create status = %d, want 200; body = %s", createResponse.Code, createResponse.Body.String())
	}

	// The upstream refuses the deletion, so the job stays findable: the client can
	// see the upstream's own reason on its next request.
	refused := httptest.NewRequest(http.MethodDelete, "/v1/videos/video_5", nil)
	refused.Header.Set("Authorization", "Bearer test-key")
	refusedResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(refusedResponse, refused)
	if refusedResponse.Code != http.StatusConflict {
		t.Fatalf("refused delete status = %d, want 409; body = %s",
			refusedResponse.Code, refusedResponse.Body.String())
	}
	if len(jobs.deleted) != 0 {
		t.Fatalf("a refused deletion forgot the job: %v", jobs.deleted)
	}

	deleteGap = true
	accepted := httptest.NewRequest(http.MethodDelete, "/v1/videos/video_5", nil)
	accepted.Header.Set("Authorization", "Bearer test-key")
	acceptedResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(acceptedResponse, accepted)
	if acceptedResponse.Code != http.StatusOK {
		t.Fatalf("delete status = %d, want 200; body = %s",
			acceptedResponse.Code, acceptedResponse.Body.String())
	}
	if len(jobs.deleted) != 1 || jobs.deleted[0] != "video_5" {
		t.Fatalf("deleted = %v, want the recorded job to be forgotten", jobs.deleted)
	}

	gone := httptest.NewRequest(http.MethodGet, "/v1/videos/video_5", nil)
	gone.Header.Set("Authorization", "Bearer test-key")
	goneResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(goneResponse, gone)
	if goneResponse.Code != http.StatusNotFound {
		t.Fatalf("poll after delete status = %d, want 404", goneResponse.Code)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(methods) != 3 || methods[0] != http.MethodPost || methods[1] != http.MethodDelete || methods[2] != http.MethodDelete {
		t.Fatalf("upstream methods = %v, want the creation and the two deletions", methods)
	}
	if paths[1] != "/v1/videos/video_5" {
		t.Fatalf("delete path = %q, want /v1/videos/video_5", paths[1])
	}
}

// A gateway without a job record cannot route a follow-up request anywhere, and
// saying so is better than choosing an upstream at random for work it cannot
// identify.
func TestVideoFollowUpWithoutAJobRecordIsUnavailable(t *testing.T) {
	server := &Server{
		Engine:        testEngine(t, testRoutes(), nil, nil),
		Authenticator: allowTestAuthentication(),
	}
	request := httptest.NewRequest(http.MethodGet, "/v1/videos/video_1", nil)
	request.Header.Set("Authorization", "Bearer test-key")
	response := httptest.NewRecorder()

	server.Handler().ServeHTTP(response, request)

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", response.Code)
	}
	if !strings.Contains(response.Body.String(), `"code":"media_jobs_unavailable"`) {
		t.Fatalf("body = %s, want media_jobs_unavailable", response.Body.String())
	}
}

// The upstream path a follow-up request is sent to carries the identifier as one
// segment, so an identifier holding a slash cannot turn a request about a job into
// a request for something else on that upstream.
func TestVideoUpstreamPathEscapesTheIdentifier(t *testing.T) {
	for _, test := range []struct {
		name     string
		jobID    string
		content  bool
		expected string
	}{
		{name: "status", jobID: "video_1", expected: "/v1/videos/video_1"},
		{name: "content", jobID: "video_1", content: true, expected: "/v1/videos/video_1/content"},
		{name: "slash", jobID: "video/../../admin", expected: "/v1/videos/video%2F..%2F..%2Fadmin"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := videoUpstreamPath(test.jobID, test.content); got != test.expected {
				t.Fatalf("videoUpstreamPath(%q, %v) = %q, want %q", test.jobID, test.content, got, test.expected)
			}
		})
	}
}

// An upstream names newly created work the way it likes. The identifier is read
// from the shapes providers use, because one the gateway failed to read would be a
// job whose follow-up requests could not be routed at all.
func TestJobIdentifierIsReadFromTheShapesProvidersUse(t *testing.T) {
	for _, test := range []struct {
		name     string
		body     string
		expected string
	}{
		{name: "openai id", body: `{"id":"video_1","status":"queued"}`, expected: "video_1"},
		{name: "task id", body: `{"task_id":"task-9"}`, expected: "task-9"},
		{name: "video id", body: `{"video_id":"vid-3"}`, expected: "vid-3"},
		{name: "nested", body: `{"data":{"id":"nested-1"}}`, expected: "nested-1"},
		{name: "result", body: `{"result":{"task_id":"res-2"}}`, expected: "res-2"},
		{name: "no identifier", body: `{"status":"queued"}`},
		{name: "not an object", body: `queued`},
		{name: "empty identifier", body: `{"id":""}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, ok := parseJobID([]byte(test.body))
			if test.expected == "" {
				if ok {
					t.Fatalf("parseJobID(%s) = %q, want no identifier", test.body, got)
				}
				return
			}
			if !ok || got != test.expected {
				t.Fatalf("parseJobID(%s) = %q (found = %v), want %q", test.body, got, ok, test.expected)
			}
		})
	}
}

// A creation the upstream refused created nothing, so an answer that happens to
// carry an identifier must not be recorded as work the gateway can route by.
func TestFailedCreationIsNotRemembered(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = w.Write([]byte(`{"id":"video_never","error":{"message":"insufficient balance"}}`))
	}))
	defer upstream.Close()

	jobs := newFakeMediaJobs()
	server := &Server{
		Engine: testEngine(t, testRoutes(domain.Channel{
			ID: "channel-video", BaseURL: upstream.URL, Enabled: true, SiteID: 1,
		}), nil, nil),
		Authenticator: allowTestAuthentication(),
		MediaJobs:     jobs,
	}

	request := httptest.NewRequest(http.MethodPost, "/v1/videos", strings.NewReader(`{"model":"sora-2"}`))
	request.Header.Set("Authorization", "Bearer test-key")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)

	if response.Code != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want the upstream's own 402; body = %s", response.Code, response.Body.String())
	}
	if _, ok, _ := jobs.LookupMediaJob(context.Background(), store.JobKindVideo, "video_never", 0); ok {
		t.Fatal("a creation the upstream refused was recorded as a job")
	}
	// The upstream's own words still reach the client, which is what the request
	// record exists to explain.
	if !strings.Contains(response.Body.String(), "insufficient balance") {
		t.Fatalf("body = %s, want the upstream's message", response.Body.String())
	}
}

// The image endpoints are registered under the media types a client actually
// sends, and the JSON ones reach the upstream as JSON rather than being rebuilt.
func TestMediaEndpointContentTypesReachTheUpstream(t *testing.T) {
	var seenContentType string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenContentType = r.Header.Get("Content-Type")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"created":1}`))
	}))
	defer upstream.Close()

	server := &Server{
		Engine: testEngine(t, testRoutes(domain.Channel{
			ID: "channel-media", BaseURL: upstream.URL, Enabled: true, SiteID: 1,
		}), nil, &http.Client{}),
		Authenticator: allowTestAuthentication(),
	}

	request := httptest.NewRequest(http.MethodPost, "/v1/images/generations",
		strings.NewReader(`{"model":"gpt-image-1","prompt":"a lighthouse"}`))
	request.Header.Set("Authorization", "Bearer test-key")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", response.Code, response.Body.String())
	}
	mediaType, _, err := mime.ParseMediaType(seenContentType)
	if err != nil {
		t.Fatalf("upstream Content-Type = %q, want a valid media type: %v", seenContentType, err)
	}
	if mediaType != "application/json" {
		t.Fatalf("upstream Content-Type = %q, want application/json", seenContentType)
	}
}
