package transform

import (
	"bytes"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"

	"github.com/yhw5231/fluxgate/internal/domain"
)

// imagePayload is the part of an image edit that must travel untouched: the form
// is rebuilt to replace the model, and a rebuild that lost or altered the upload
// would be worse than not rewriting the model at all.
var imagePayload = []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x01, 0x02, 0xff, 0xfe}

// buildForm writes a multipart form the way a client does: a model field beside
// the file it is editing. An empty model leaves the field out.
func buildForm(t *testing.T, model string, extraFields map[string]string) ([]byte, string) {
	t.Helper()
	var buffer bytes.Buffer
	writer := multipart.NewWriter(&buffer)
	if model != "" {
		if err := writer.WriteField("model", model); err != nil {
			t.Fatalf("write model field: %v", err)
		}
	}
	if err := writer.WriteField("prompt", "make the sky blue"); err != nil {
		t.Fatalf("write prompt field: %v", err)
	}
	for name, value := range extraFields {
		if err := writer.WriteField(name, value); err != nil {
			t.Fatalf("write %s field: %v", name, err)
		}
	}
	part, err := writer.CreateFormFile("image", "source.png")
	if err != nil {
		t.Fatalf("create file part: %v", err)
	}
	if _, err := part.Write(imagePayload); err != nil {
		t.Fatalf("write file part: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close form: %v", err)
	}
	return buffer.Bytes(), writer.FormDataContentType()
}

// readForm returns the fields and files a rebuilt form carries.
func readForm(t *testing.T, raw []byte, contentType string) (map[string]string, map[string][]byte) {
	t.Helper()
	_, parameters, err := mime.ParseMediaType(contentType)
	if err != nil {
		t.Fatalf("parse content type %q: %v", contentType, err)
	}
	reader := multipart.NewReader(bytes.NewReader(raw), parameters["boundary"])
	fields := map[string]string{}
	files := map[string][]byte{}
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read part: %v", err)
		}
		payload, err := io.ReadAll(part)
		if err != nil {
			t.Fatalf("read part body: %v", err)
		}
		if part.FileName() != "" {
			files[part.FormName()+"/"+part.FileName()] = payload
			continue
		}
		fields[part.FormName()] = string(payload)
	}
	return fields, files
}

// The model is the one thing the gateway rewrites in an uploaded form: an image
// edit is routed by the model it names, and the channel has to be asked for the
// upstream's own name for it. Everything else the client sent — the prompt, the
// file, its filename — has to arrive unchanged.
func TestMultipartFormKeepsItsPartsAndRewritesTheModel(t *testing.T) {
	raw, contentType := buildForm(t, "dall-e-2", nil)

	body, err := NewBody(contentType, raw).Rewrite("dall-e-2-v2", domain.TransformRules{})
	if err != nil {
		t.Fatalf("Rewrite() error = %v", err)
	}

	fields, files := readForm(t, body.Raw, body.ContentType)
	if got := fields["model"]; got != "dall-e-2-v2" {
		t.Fatalf("model field = %q, want the mapped model", got)
	}
	if got := fields["prompt"]; got != "make the sky blue" {
		t.Fatalf("prompt field = %q, want it preserved", got)
	}
	if got := files["image/source.png"]; !bytes.Equal(got, imagePayload) {
		t.Fatalf("image part = %v, want the uploaded bytes", got)
	}
	if body.ContentType == contentType {
		t.Fatalf("content type = %q, want one describing the rebuilt form", body.ContentType)
	}
}

// A form that names no model still has to be routed, and the route resolved one:
// the field is added so the channel is asked for the model the gateway selected
// rather than for whatever its own default is.
func TestMultipartFormWithoutAModelGetsTheMappedOne(t *testing.T) {
	raw, contentType := buildForm(t, "", nil)

	body, err := NewBody(contentType, raw).Rewrite("gpt-image-1", domain.TransformRules{})
	if err != nil {
		t.Fatalf("Rewrite() error = %v", err)
	}

	fields, files := readForm(t, body.Raw, body.ContentType)
	if got := fields["model"]; got != "gpt-image-1" {
		t.Fatalf("model field = %q, want gpt-image-1", got)
	}
	if got := files["image/source.png"]; !bytes.Equal(got, imagePayload) {
		t.Fatalf("image part = %v, want the uploaded bytes", got)
	}
}

// Reading the model has to work for both shapes a request body comes in, because
// an endpoint that cannot attribute a request to a model cannot route it.
func TestModelReadsJSONAndMultipartBodies(t *testing.T) {
	jsonBody := NewBody("application/json; charset=utf-8", []byte(`{"model":"text-embedding-3-small","input":"hello"}`))
	model, err := jsonBody.Model()
	if err != nil {
		t.Fatalf("JSON Model() error = %v", err)
	}
	if model != "text-embedding-3-small" {
		t.Fatalf("JSON model = %q, want text-embedding-3-small", model)
	}

	raw, contentType := buildForm(t, "gpt-image-1", nil)
	form := NewBody(contentType, raw)
	model, err = form.Model()
	if err != nil {
		t.Fatalf("multipart Model() error = %v", err)
	}
	if model != "gpt-image-1" {
		t.Fatalf("multipart model = %q, want gpt-image-1", model)
	}
}

// A content type the gateway did not recognise is read as JSON, which is the shape
// every proxy endpoint has always assumed: a client that sends no media type gets
// the same treatment it always did rather than a new refusal.
func TestUnknownContentTypeIsReadAsJSON(t *testing.T) {
	body := NewBody("text/plain", []byte(`{"model":"gpt-5"}`))
	if body.Shape != ShapeJSON {
		t.Fatalf("Shape = %q, want %q", body.Shape, ShapeJSON)
	}
	model, err := body.Model()
	if err != nil {
		t.Fatalf("Model() error = %v", err)
	}
	if model != "gpt-5" {
		t.Fatalf("model = %q, want gpt-5", model)
	}
}

// A request that carries no body names no model, which is what asking after a
// video job looks like. There is nothing to rewrite, and reading the empty body as
// an empty JSON document would only fail.
func TestBodylessRequestIsLeftAlone(t *testing.T) {
	rewritten, err := NewBody("", nil).Rewrite("sora-2", domain.TransformRules{})
	if err != nil {
		t.Fatalf("Rewrite() error = %v", err)
	}
	if len(rewritten.Raw) != 0 {
		t.Fatalf("body = %q, want it left empty", rewritten.Raw)
	}
}

// A rebuilt form carries a new boundary, so the header has to describe the body
// that is actually sent. A client's own content type is otherwise kept, and JSON
// is supplied only when the client named none.
func TestApplyHeadersCarriesTheContentTypeOfTheBodySent(t *testing.T) {
	multipartType := "multipart/form-data; boundary=client-boundary"
	source := http.Header{"Content-Type": []string{multipartType}}

	kept := ApplyHeaders(source, domain.TransformRules{}, "upstream-key", "")
	if got := kept.Get("Content-Type"); got != multipartType {
		t.Fatalf("Content-Type = %q, want the client's own %q", got, multipartType)
	}

	replaced := ApplyHeaders(source, domain.TransformRules{}, "upstream-key", "multipart/form-data; boundary=rebuilt")
	if got := replaced.Get("Content-Type"); got != "multipart/form-data; boundary=rebuilt" {
		t.Fatalf("Content-Type = %q, want the rebuilt body's boundary", got)
	}

	absent := ApplyHeaders(http.Header{}, domain.TransformRules{}, "upstream-key", "")
	if got := absent.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
}

// A file part's own content type is part of the upload, so a rebuild has to keep
// it: an upstream that reads the declared type of the image would otherwise be
// told it received something else.
func TestRebuiltFormKeepsTheFileContentType(t *testing.T) {
	var buffer bytes.Buffer
	writer := multipart.NewWriter(&buffer)
	if err := writer.WriteField("model", "gpt-image-1"); err != nil {
		t.Fatalf("write model field: %v", err)
	}
	header := make(map[string][]string)
	header["Content-Disposition"] = []string{`form-data; name="image"; filename="photo.jpg"`}
	header["Content-Type"] = []string{"image/jpeg"}
	part, err := writer.CreatePart(header)
	if err != nil {
		t.Fatalf("create part: %v", err)
	}
	if _, err := part.Write([]byte("jpeg-bytes")); err != nil {
		t.Fatalf("write part: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close form: %v", err)
	}

	body, err := NewBody(writer.FormDataContentType(), buffer.Bytes()).Rewrite("gpt-image-1-2026", domain.TransformRules{})
	if err != nil {
		t.Fatalf("Rewrite() error = %v", err)
	}

	_, parameters, err := mime.ParseMediaType(body.ContentType)
	if err != nil {
		t.Fatalf("parse content type: %v", err)
	}
	reader := multipart.NewReader(bytes.NewReader(body.Raw), parameters["boundary"])
	for {
		read, err := reader.NextPart()
		if err == io.EOF {
			t.Fatal("the file part did not survive the rewrite")
		}
		if err != nil {
			t.Fatalf("read part: %v", err)
		}
		if read.FormName() != "image" {
			continue
		}
		if got := read.Header.Get("Content-Type"); got != "image/jpeg" {
			t.Fatalf("file Content-Type = %q, want image/jpeg", got)
		}
		if got := read.FileName(); got != "photo.jpg" {
			t.Fatalf("filename = %q, want photo.jpg", got)
		}
		payload, err := io.ReadAll(read)
		if err != nil {
			t.Fatalf("read file part: %v", err)
		}
		if string(payload) != "jpeg-bytes" {
			t.Fatalf("file payload = %q, want jpeg-bytes", payload)
		}
		return
	}
}

// A multipart form with no boundary is not a form the gateway can read, and
// saying so is the only honest answer: guessing at one would forward a body whose
// model was never rewritten.
func TestMultipartWithoutBoundaryIsReported(t *testing.T) {
	body := NewBody("multipart/form-data", []byte("--nope--"))
	if _, err := body.Model(); err == nil {
		t.Fatal("Model() accepted a multipart form with no boundary")
	}
	if _, err := body.Rewrite("gpt-image-1", domain.TransformRules{}); err == nil {
		t.Fatal("Rewrite() accepted a multipart form with no boundary")
	}
}

// A JSON body is re-encoded for the channel, so it is described as JSON however
// the client labelled it. This is the media type the gateway has always sent an
// upstream, and a client's own label is not what the channel is about to receive.
func TestRewrittenJSONBodyIsDescribedAsJSON(t *testing.T) {
	for _, contentType := range []string{"", "application/json; charset=utf-8", "text/plain"} {
		body, err := NewBody(contentType, []byte(`{"model":"gpt-5"}`)).Rewrite("gpt-5-mini", domain.TransformRules{})
		if err != nil {
			t.Fatalf("Rewrite() from %q error = %v", contentType, err)
		}
		if body.ContentType != "application/json" {
			t.Fatalf("Rewrite() from %q gave ContentType %q, want application/json", contentType, body.ContentType)
		}
	}
}

// The JSON endpoints still apply the configured body mutations, which is what the
// console offers an operator per upstream.
func TestJSONRewriteStillAppliesConfiguredMutations(t *testing.T) {
	body, err := NewBody("application/json", []byte(`{"model":"gpt-4o","temperature":0.5,"logprobs":true}`)).
		Rewrite("gpt-4o-mini", domain.TransformRules{
			DeleteJSON:   []string{"logprobs"},
			OverrideJSON: map[string]any{"temperature": 0.1},
		})
	if err != nil {
		t.Fatalf("Rewrite() error = %v", err)
	}
	text := string(body.Raw)
	if !strings.Contains(text, `"model":"gpt-4o-mini"`) {
		t.Fatalf("body = %s, want the mapped model", text)
	}
	if strings.Contains(text, "logprobs") {
		t.Fatalf("body = %s, want logprobs deleted", text)
	}
	if !strings.Contains(text, `"temperature":0.1`) {
		t.Fatalf("body = %s, want temperature overridden", text)
	}
}
