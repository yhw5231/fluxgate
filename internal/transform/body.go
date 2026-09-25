package transform

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/textproto"
	"strings"

	"github.com/yhw5231/fluxgate/internal/domain"
)

// Body shapes the gateway can read a model name out of. Every OpenAI-compatible
// endpoint names its model in the request body, but not every one names it the
// same way: a chat completion, an embedding and an image generation send a JSON
// object, while an image edit uploads its source image as a multipart form and
// carries the model as one field beside it.
const (
	// ShapeJSON is a JSON object body.
	ShapeJSON = "json"
	// ShapeMultipart is a multipart/form-data body. It is re-encoded when the
	// model is rewritten, because a part's length is not known before the form is
	// rebuilt; the file parts travel unchanged.
	ShapeMultipart = "multipart"
)

// contentJSON is the media type a rebuilt JSON body is described by.
const contentJSON = "application/json"

// Body is a request body together with the shape it was sent in, which is what
// decides how its model name is read and how the body a channel receives is
// produced from it.
type Body struct {
	Raw         []byte
	ContentType string
	Shape       string
}

// NewBody classifies a request body by the media type it arrived under, which is
// what decides how its model name is read and how the body a channel receives is
// produced from it.
//
// Only one shape is told apart from the rest. A multipart form carries its fields
// and its uploaded files as parts, and rebuilding it is the only way to replace
// the model in it; everything else is read as a JSON object, which is the shape
// every proxy endpoint has always assumed and the only shape a caller that named
// no media type can mean. A body that is neither is reported when its model is
// read, rather than here, so the answer the client gets is the same one a
// malformed JSON body has always produced.
func NewBody(contentType string, raw []byte) Body {
	body := Body{Raw: raw, ContentType: strings.TrimSpace(contentType)}
	mediaType := body.ContentType
	if parsed, _, err := mime.ParseMediaType(body.ContentType); err == nil {
		mediaType = parsed
	}
	if strings.EqualFold(mediaType, "multipart/form-data") {
		body.Shape = ShapeMultipart
		return body
	}
	body.Shape = ShapeJSON
	return body
}

// Model reports the model name the body names, or an empty string when it names
// none. A form the gateway cannot read is an error, because a request that cannot
// be attributed to a model cannot be routed.
func (b Body) Model() (string, error) {
	switch b.Shape {
	case ShapeJSON:
		model, err := jsonModel(b.Raw)
		if err != nil {
			return "", err
		}
		return model, nil
	case ShapeMultipart:
		return b.formModel()
	default:
		return "", errors.New("the request body shape is not known")
	}
}

// Rewrite produces the body a channel receives: the model name the route maps it
// to is written in, and the configured JSON mutations are applied. The content
// type travels with the body because a rebuilt form carries a new boundary.
func (b Body) Rewrite(mappedModel string, rules domain.TransformRules) (Body, error) {
	// A request that carries no body names no model, which is what fetching a
	// video job by its identifier looks like. There is nothing to rewrite, and
	// reading it as an empty JSON document would only fail.
	if len(b.Raw) == 0 {
		return b, nil
	}
	switch b.Shape {
	case ShapeJSON:
		rewritten, err := ApplyJSON(b.Raw, mappedModel, rules)
		if err != nil {
			return Body{}, err
		}
		// The body is re-encoded, so the media type that travels with it describes
		// what is actually sent rather than what the client called it. A client that
		// sent a JSON body under another name has had it read as JSON either way, and
		// the channel is told JSON because that is what it is about to receive.
		return Body{Raw: rewritten, ContentType: contentJSON, Shape: b.Shape}, nil
	case ShapeMultipart:
		rewritten, contentType, err := rewriteForm(b.Raw, b.ContentType, mappedModel)
		if err != nil {
			return Body{}, err
		}
		// The configured body mutations address JSON paths, so they have no
		// meaning in a form whose fields are named by their parts. They are left
		// out rather than applied to a document the caller cannot have meant.
		return Body{Raw: rewritten, ContentType: contentType, Shape: b.Shape}, nil
	default:
		return Body{}, errors.New("the request body shape is not known")
	}
}

// jsonModel reads the model field of a JSON object body.
func jsonModel(raw []byte) (string, error) {
	var envelope struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return "", fmt.Errorf("decode request JSON: %w", err)
	}
	return strings.TrimSpace(envelope.Model), nil
}

// formModel reads the model field of a multipart form. A form that cannot be
// read as one at all is an error; a form that simply carries no model field is a
// body with no model, which the endpoint answers as a missing model.
func (b Body) formModel() (string, error) {
	reader, err := formReader(b.Raw, b.ContentType)
	if err != nil {
		return "", err
	}
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			return "", nil
		}
		if err != nil {
			return "", fmt.Errorf("read multipart form: %w", err)
		}
		if !strings.EqualFold(part.FormName(), modelField) {
			continue
		}
		value, err := io.ReadAll(io.LimitReader(part, maxFormFieldBytes))
		if err != nil {
			return "", fmt.Errorf("read multipart model field: %w", err)
		}
		return strings.TrimSpace(string(value)), nil
	}
}

// modelField is the form field an OpenAI-compatible upload names its model in,
// the same name the JSON endpoints use for the same information.
const modelField = "model"

// maxFormFieldBytes bounds how much of one non-file form field is kept. A model
// name is short; a field that is not one should not be buffered whole merely
// because it claimed to be.
const maxFormFieldBytes = int64(1 << 16)

// rewriteForm rebuilds a multipart form with the model field set to the mapped
// name, returning the form and the content type describing it. Part order,
// headers, and payloads are preserved; only the model field's value changes. The
// field is added when the form does not carry one, so the channel is asked for
// the model the route resolved to rather than for whatever its own default is.
func rewriteForm(raw []byte, contentType, mappedModel string) ([]byte, string, error) {
	reader, err := formReader(raw, contentType)
	if err != nil {
		return nil, "", err
	}
	var buffer bytes.Buffer
	writer := multipart.NewWriter(&buffer)
	written := false
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, "", fmt.Errorf("read multipart form: %w", err)
		}
		header := clonePartHeader(part.Header)
		openPart, err := writer.CreatePart(header)
		if err != nil {
			return nil, "", fmt.Errorf("rebuild multipart form: %w", err)
		}
		if strings.EqualFold(part.FormName(), modelField) {
			written = true
			if _, err := openPart.Write([]byte(mappedModel)); err != nil {
				return nil, "", fmt.Errorf("write multipart model field: %w", err)
			}
			// The part's own bytes are dropped rather than copied: they are the
			// model name the field held, which is the one thing being replaced.
			continue
		}
		if _, err := io.Copy(openPart, part); err != nil {
			return nil, "", fmt.Errorf("copy multipart part: %w", err)
		}
	}
	if !written && strings.TrimSpace(mappedModel) != "" {
		if err := writer.WriteField(modelField, mappedModel); err != nil {
			return nil, "", fmt.Errorf("write multipart model field: %w", err)
		}
	}
	if err := writer.Close(); err != nil {
		return nil, "", fmt.Errorf("close multipart form: %w", err)
	}
	return buffer.Bytes(), writer.FormDataContentType(), nil
}

// formReader starts reading a multipart form, which needs the boundary its
// content type carries: the same media type with a different boundary describes a
// different body.
func formReader(raw []byte, contentType string) (*multipart.Reader, error) {
	_, parameters, err := mime.ParseMediaType(contentType)
	if err != nil {
		return nil, fmt.Errorf("parse multipart content type: %w", err)
	}
	boundary := strings.TrimSpace(parameters["boundary"])
	if boundary == "" {
		return nil, errors.New("multipart form has no boundary")
	}
	return multipart.NewReader(bytes.NewReader(raw), boundary), nil
}

// clonePartHeader copies a part's headers, keeping the Content-Disposition the
// reader already parsed. A rebuilt part is written with the headers its original
// carried, so a filename, a file's own content type, and anything else the client
// attached beside them survive the rewrite.
func clonePartHeader(source textproto.MIMEHeader) textproto.MIMEHeader {
	header := make(textproto.MIMEHeader, len(source))
	for name, values := range source {
		header[name] = append([]string(nil), values...)
	}
	return header
}
