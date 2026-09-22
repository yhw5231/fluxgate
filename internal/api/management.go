package api

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/yhw5231/fluxgate/internal/policy"
	"github.com/yhw5231/fluxgate/internal/store"
)

// Management writes to the configuration database.
//
// The console is a static page, so every add or edit it offers is an HTTP
// request the server authenticates and validates on its own. This file holds the
// endpoint family that does it:
//
//	GET    /management/configuration
//	POST   /management/configuration/{resource}
//	PUT    /management/configuration/{resource}/{id}
//	DELETE /management/configuration/{resource}/{id}
//	POST   /management/configuration/keys/{id}/rotate
//
// One family rather than a handler per table, because the tables differ only in
// their columns: the field list, the validation, and the references that have to
// survive a write all come from the store's resource description, which is also
// what the console renders its forms from.
//
// Every accepted write reloads the configuration and hands the result to the
// routing engine, so an added channel serves traffic immediately instead of
// after a restart, and the response carries the refreshed inventory so the
// console repaints without a second round trip.

// ConfigurationStore is the configuration write surface the console drives. It
// is satisfied by the SQLite store; tests substitute an in-memory double.
type ConfigurationStore interface {
	ListResource(ctx context.Context, name string) ([]map[string]any, error)
	CreateResource(ctx context.Context, name string, values map[string]any) (map[string]any, error)
	UpdateResource(ctx context.Context, name string, id int64, values map[string]any) (map[string]any, error)
	DeleteResource(ctx context.Context, name string, id int64) (map[string]int64, error)
	// PutSettings stores runtime policy overrides, deleting a key whose value is
	// empty so the value it overrode applies again.
	PutSettings(ctx context.Context, values map[string]string) error
	// UpstreamKey reads the first key of a stored upstream, for the model probe
	// that has to present a credential the console is never shown.
	UpstreamKey(ctx context.Context, id int64) (string, error)
	// DownstreamKey reads the stored value of one client key, for the console
	// action that shows an operator a credential of their own back.
	DownstreamKey(ctx context.Context, id int64) (string, error)
	LoadConfiguration(ctx context.Context) (store.Configuration, error)
}

// ConfigurationApplier installs a freshly loaded snapshot in the running routing
// engine. It is a function so the gateway process can rebuild the selector and
// proxy resolver it already owns without the API package knowing how they are
// assembled.
type ConfigurationApplier func(store.Configuration)

const (
	// maxConfigurationBodyBytes bounds a management write. The largest row the
	// console edits is a model mapping of a few kilobytes, so this is generous
	// while still keeping a hostile request from being buffered whole.
	maxConfigurationBodyBytes = 1 << 20

	// secretMask replaces a stored credential in a management response.
	secretMask = store.SecretMask

	// downstreamKeyBytes is the entropy of a generated client credential.
	downstreamKeyBytes = 32
	// downstreamKeyPrefix marks a generated key as one of this gateway's, so an
	// operator can tell it apart from an upstream vendor key.
	downstreamKeyPrefix = "sk-"
)

// handleConfiguration returns everything the console can display and edit: the
// rows of every managed resource with secrets masked, the field list of each
// resource, and the models the gateway currently routes.
func (s *Server) handleConfiguration(w http.ResponseWriter, r *http.Request) {
	if !s.authenticateConsole(w, r) {
		return
	}
	// A gateway started without a write store has no configuration view to read
	// either, which is the condition every write reports as well.
	if s.ConfigStore == nil {
		writeError(w, http.StatusServiceUnavailable, "configuration_not_supported",
			"this gateway was started without configuration management")
		return
	}
	inventory, err := s.configurationInventory(r.Context())
	if err != nil {
		s.writeConfigurationFailure(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, inventory)
}

// handleConfigurationCreate adds one row to a resource.
func (s *Server) handleConfigurationCreate(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeConfigurationWrite(w, r) {
		return
	}
	resource, ok := configurationResource(w, r)
	if !ok {
		return
	}
	values, ok := decodeConfigurationBody(w, r)
	if !ok {
		return
	}

	// A client credential is generated when the request does not carry one, so
	// the console can offer "add a key" as a single click and then show the value
	// it created.
	minted := ""
	if resource == "keys" {
		if text, _ := values["key"].(string); strings.TrimSpace(text) == "" {
			generated, err := newDownstreamKey()
			if err != nil {
				s.writeConfigurationFailure(w, r, err)
				return
			}
			values["key"] = generated
			minted = generated
		}
	}

	row, err := s.ConfigStore.CreateResource(r.Context(), resource, values)
	if err != nil {
		s.writeConfigurationFailure(w, r, err)
		return
	}
	s.logConfigurationChange(r, "console_configuration_created", resource, rowID(row))
	body := map[string]any{"row": maskRow(row, resource)}
	if minted != "" {
		body["generated"] = map[string]any{"key": minted}
	}
	s.respondWithWrite(w, r, http.StatusCreated, body)
}

// handleConfigurationUpdate applies the fields a request carries to one row.
func (s *Server) handleConfigurationUpdate(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeConfigurationWrite(w, r) {
		return
	}
	resource, ok := configurationResource(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	values, ok := decodeConfigurationBody(w, r)
	if !ok {
		return
	}
	stripEchoedSecrets(resource, values)

	row, err := s.ConfigStore.UpdateResource(r.Context(), resource, id, values)
	if err != nil {
		s.writeConfigurationFailure(w, r, err)
		return
	}
	s.logConfigurationChange(r, "console_configuration_updated", resource, id)
	s.respondWithWrite(w, r, http.StatusOK, map[string]any{"row": maskRow(row, resource)})
}

// handleConfigurationDelete removes one row, refusing while other configuration
// still points at it.
func (s *Server) handleConfigurationDelete(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeConfigurationWrite(w, r) {
		return
	}
	resource, ok := configurationResource(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r)
	if !ok {
		return
	}

	cascaded, err := s.ConfigStore.DeleteResource(r.Context(), resource, id)
	if err != nil {
		s.writeConfigurationFailure(w, r, err)
		return
	}
	s.logConfigurationChange(r, "console_configuration_deleted", resource, id)
	body := map[string]any{"deleted": true}
	if len(cascaded) > 0 {
		body["cascaded"] = cascaded
	}
	s.respondWithWrite(w, r, http.StatusOK, body)
}

// handleConfigurationKeyRotate mints a new value for an existing client
// credential. It is separate from an update because the value has to come from
// the gateway's random source rather than from the request, and because the
// replaced value has to be unrecoverable afterwards.
func (s *Server) handleConfigurationKeyRotate(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeConfigurationWrite(w, r) {
		return
	}
	id, ok := pathID(w, r)
	if !ok {
		return
	}

	generated, err := newDownstreamKey()
	if err != nil {
		s.writeConfigurationFailure(w, r, err)
		return
	}
	row, err := s.ConfigStore.UpdateResource(r.Context(), "keys", id, map[string]any{"key": generated})
	if err != nil {
		s.writeConfigurationFailure(w, r, err)
		return
	}
	s.logConfigurationChange(r, "console_configuration_key_rotated", "keys", id)
	s.respondWithWrite(w, r, http.StatusOK, map[string]any{
		"row":       maskRow(row, "keys"),
		"generated": map[string]any{"key": generated},
	})
}

// handleConfigurationKeyReveal answers with the stored value of one client key.
//
//	POST /management/configuration/keys/{id}/reveal
//
// A client key is the credential an operator hands out to callers, and the
// listing only ever shows its mask. Losing one used to mean rotating it and
// re-deploying every caller, so the value can be read back on demand. It is a
// deliberate action rather than an unmasked listing on purpose: a listing is
// fetched on every refresh, kept in the browser, and pasted into tickets, while
// this answers one request, for one key, and is recorded in the audit log.
//
// Upstream keys are not readable this way. Those belong to the upstream rather
// than to the operator, and the gateway is the only party that needs them.
func (s *Server) handleConfigurationKeyReveal(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeConfigurationWrite(w, r) {
		return
	}
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	key, err := s.ConfigStore.DownstreamKey(r.Context(), id)
	if err != nil {
		s.writeConfigurationFailure(w, r, err)
		return
	}
	if strings.TrimSpace(key) == "" {
		// A key row without a value cannot authenticate anyone, so there is
		// nothing to show; saying so beats answering with an empty string.
		writeError(w, http.StatusNotFound, "configuration_not_found", "this client key has no stored value")
		return
	}
	// The audit event names the row and the client, never the value: the log is
	// read by more people than the console is.
	s.logConfigurationChange(r, "console_key_revealed", "keys", id)
	writeJSON(w, http.StatusOK, map[string]any{"key": key})
}

// authorizeConfigurationWrite applies the checks every write shares: a console
// credential, a configured write store, and a same-origin request. It writes the
// response itself and reports whether the caller may continue.
func (s *Server) authorizeConfigurationWrite(w http.ResponseWriter, r *http.Request) bool {
	if !s.authenticateConsole(w, r) {
		return false
	}
	if s.ConfigStore == nil {
		writeError(w, http.StatusServiceUnavailable, "configuration_not_supported",
			"this gateway was started without configuration management")
		return false
	}
	// The console authenticates with a cookie, so a state-changing request is
	// only accepted from the gateway's own origin. A browser sends Origin on
	// cross-site requests; automation that carries the management token sends
	// none, which is allowed because its credential is not attached by a browser.
	if !sameOriginRequest(r) {
		writeError(w, http.StatusForbidden, "cross_origin_rejected",
			"a configuration write must come from the console's own origin")
		return false
	}
	return true
}

// configurationResource resolves the resource named in the path.
func configurationResource(w http.ResponseWriter, r *http.Request) (string, bool) {
	resource := r.PathValue("resource")
	if _, ok := store.FindResource(resource); !ok {
		writeError(w, http.StatusNotFound, "unknown_resource", "unknown configuration resource: "+resource)
		return "", false
	}
	return resource, true
}

// respondWithWrite answers an accepted write with the refreshed inventory, so
// the console repaints from one round trip. A minted secret is revealed exactly
// once here: it is never part of a read, because the stored value is masked in
// every listing.
//
// The reload happens before the response, so what the console is shown is what
// the gateway is now routing with. A reload that fails after the row was stored
// is reported as such rather than as a failed write, because the change is on
// disk and only the running process is behind.
func (s *Server) respondWithWrite(w http.ResponseWriter, r *http.Request, status int, body map[string]any) {
	if err := s.reloadConfiguration(r.Context()); err != nil {
		if s.Logger != nil {
			s.Logger.Error("console_configuration_reload_failed", "error", err.Error())
		}
		writeError(w, http.StatusInternalServerError, "configuration_reload_failed",
			"the change was stored, but the gateway could not reload its configuration; restart it to apply the change")
		return
	}
	inventory, err := s.configurationInventory(r.Context())
	if err != nil {
		// The write is on disk; only the view of it could not be built. Saying so
		// keeps the operator from repeating a change that already happened.
		if s.Logger != nil {
			s.Logger.Error("console_configuration_read_failed", "error", err.Error())
		}
		writeError(w, http.StatusInternalServerError, "configuration_read_failed",
			"the change was stored, but the gateway could not read the configuration back")
		return
	}
	body["configuration"] = inventory
	writeJSON(w, status, body)
}

// configurationInventory assembles the management view of the configuration.
func (s *Server) configurationInventory(ctx context.Context) (map[string]any, error) {
	rows := make(map[string][]map[string]any)
	fields := make(map[string][]map[string]any)
	for _, resource := range store.Resources() {
		listed, err := s.ConfigStore.ListResource(ctx, resource.Name)
		if err != nil {
			return nil, err
		}
		rows[resource.Name] = maskRows(resource, listed)
		fields[resource.Name] = describeFields(resource)
	}

	configuration := s.currentConfiguration()
	loadedAt := ""
	if !configuration.LoadedAt.IsZero() {
		loadedAt = configuration.LoadedAt.UTC().Format(time.RFC3339Nano)
	}
	models := s.currentModels()
	if models == nil {
		models = []string{}
	}
	// The runtime policy travels with the configuration because it is stored in
	// the same database and read back the same way: after a write the console
	// holds the values the gateway is now using, not the ones it asked for.
	applied := s.currentPolicy()
	return map[string]any{
		"generated_at":  time.Now().UTC().Format(time.RFC3339Nano),
		"loaded_at":     loadedAt,
		"models":        models,
		"resources":     rows,
		"fields":        fields,
		"policy":        applied.Values(),
		"policy_fields": policy.Fields(),
	}, nil
}

// describeFields renders the writable fields of a resource for the console, so
// the form it builds and the validation the server applies come from one place.
func describeFields(resource store.Resource) []map[string]any {
	described := make([]map[string]any, 0, len(resource.Columns))
	for _, column := range resource.Columns {
		field := map[string]any{
			"name":      column.Name,
			"kind":      string(column.Kind),
			"required":  column.Required,
			"secret":    column.Secret,
			"clearable": column.Clearable,
			"synthetic": column.Synthetic,
		}
		if column.MaxLength > 0 {
			field["max_length"] = column.MaxLength
		}
		if len(column.Choices) > 0 {
			field["choices"] = column.Choices
		}
		if column.Kind == store.KindBool {
			field["default"] = column.Default
		}
		// A create that omits the field stores this value, so the form shows it
		// as the preselected choice rather than leaving the field open.
		if column.DefaultValue != nil {
			field["default_value"] = column.DefaultValue
		}
		described = append(described, field)
	}
	return described
}

// maskRows masks the secrets of a listing.
func maskRows(resource store.Resource, rows []map[string]any) []map[string]any {
	masked := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		masked = append(masked, maskRow(row, resource.Name))
	}
	return masked
}

// maskRow replaces every secret value with its mask so a credential is never
// handed to the browser, and a response can be pasted into a bug report without
// leaking one. An empty secret stays empty, which is what tells the console the
// difference between "set" and "not set".
func maskRow(row map[string]any, resourceName string) map[string]any {
	masked := make(map[string]any, len(row))
	for name, value := range row {
		masked[name] = value
	}
	resource, ok := store.FindResource(resourceName)
	if !ok {
		return masked
	}
	for _, column := range resource.Columns {
		if !column.Secret {
			continue
		}
		if text, ok := masked[column.Name].(string); ok {
			masked[column.Name] = maskSecret(text)
		}
	}
	return masked
}

// maskSecret renders a stored credential so the console can show that one is
// set, and which one it is, without receiving the value itself.
func maskSecret(value string) string {
	return store.MaskSecret(value)
}

// stripEchoedSecrets drops a secret field that still carries the mask. An
// untouched console input submits an empty string, which the store already reads
// as "keep the stored value"; this covers a client that sent the row back
// exactly as it was read.
func stripEchoedSecrets(resource string, values map[string]any) {
	definition, ok := store.FindResource(resource)
	if !ok {
		return
	}
	for _, column := range definition.Columns {
		if !column.Secret {
			continue
		}
		text, ok := values[column.Name].(string)
		if ok && strings.Contains(text, secretMask) {
			delete(values, column.Name)
		}
	}
}

// decodeConfigurationBody reads a write body. An absent body is an empty set of
// fields rather than an error, so a create reports which required field is
// missing instead of reporting malformed JSON.
func decodeConfigurationBody(w http.ResponseWriter, r *http.Request) (map[string]any, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxConfigurationBodyBytes)
	values := map[string]any{}
	if err := json.NewDecoder(r.Body).Decode(&values); err != nil {
		if errors.Is(err, io.EOF) {
			return values, true
		}
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) {
			writeError(w, http.StatusRequestEntityTooLarge, "request_too_large",
				"the request body exceeds the configuration limit")
			return nil, false
		}
		writeError(w, http.StatusBadRequest, "invalid_json", "request body must be a JSON object")
		return nil, false
	}
	return values, true
}

// pathID reads the row id from the request path.
func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(strings.TrimSpace(r.PathValue("id")), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_configuration", "the row id in the path must be a positive number")
		return 0, false
	}
	return id, true
}

// writeConfigurationFailure maps a rejected write to a response. A validation
// fault carries the field, a stable reason and its parameters, which is what
// lets the console point at the input and explain it in its own language.
func (s *Server) writeConfigurationFailure(w http.ResponseWriter, r *http.Request, err error) {
	var invalid store.ValidationError
	switch {
	case errors.As(err, &invalid):
		details := map[string]any{}
		if invalid.Field != "" {
			details["field"] = invalid.Field
		}
		if invalid.Reason != "" {
			details["reason"] = invalid.Reason
			params := invalid.Params
			if params == nil {
				params = map[string]any{}
			}
			details["params"] = params
		}
		writeConfigurationError(w, http.StatusBadRequest, "invalid_configuration", invalid.Message, details)
	case errors.Is(err, store.ErrUnknownResource):
		writeConfigurationError(w, http.StatusNotFound, "unknown_resource", err.Error(), nil)
	case errors.Is(err, store.ErrResourceNotFound):
		writeConfigurationError(w, http.StatusNotFound, "configuration_not_found", err.Error(), nil)
	case errors.Is(err, store.ErrResourceReferenced):
		// The counts drive the console's own wording, so they travel as data.
		var referenced store.ReferencedError
		details := map[string]any{}
		if errors.As(err, &referenced) {
			details["reason"] = "referenced"
			details["params"] = map[string]any{
				"resource": referenced.Resource,
				"count":    referenced.Count,
			}
		}
		writeConfigurationError(w, http.StatusConflict, "configuration_referenced", err.Error(), details)
	case errors.Is(err, store.ErrDuplicateValue):
		writeConfigurationError(w, http.StatusConflict, "configuration_conflict", err.Error(), nil)
	default:
		// Anything else is a database failure whose text belongs in the log
		// rather than in a response.
		if s.Logger != nil {
			s.Logger.Error("console_configuration_write_failed",
				"path", r.URL.Path,
				"error", err.Error(),
				"client_ip", clientIP(r))
		}
		writeError(w, http.StatusInternalServerError, "configuration_write_failed",
			"the gateway could not store the change")
	}
}

// writeConfigurationError writes the error envelope every client already reads,
// with the extra keys a field-level rejection carries.
func writeConfigurationError(w http.ResponseWriter, status int, code, message string, details map[string]any) {
	body := map[string]any{
		"code":    code,
		"message": message,
		"type":    "gateway_error",
	}
	for key, value := range details {
		body[key] = value
	}
	writeJSON(w, status, map[string]any{"error": body})
}

// sameOriginRequest reports whether a state-changing request came from the
// gateway's own origin. An absent Origin header is allowed: a browser always
// sends one on a cross-origin request, while a script presenting the management
// token has no origin at all and its credential cannot be attached by a page.
func sameOriginRequest(r *http.Request) bool {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return true
	}
	parsed, err := url.Parse(origin)
	if err != nil {
		return false
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return false
	}
	// Only the host is compared. TLS is often terminated by a proxy, so the
	// scheme the browser used and the one the gateway sees legitimately differ.
	return strings.EqualFold(parsed.Host, r.Host)
}

// reloadConfiguration re-reads the configuration database and installs the
// result in the running gateway, so a write takes effect for the next proxied
// request instead of at the next restart.
func (s *Server) reloadConfiguration(ctx context.Context) error {
	if s.ConfigStore == nil {
		return nil
	}
	configuration, err := s.ConfigStore.LoadConfiguration(ctx)
	if err != nil {
		return err
	}
	s.SetConfiguration(configuration)
	if s.Applier != nil {
		s.Applier(configuration)
	}
	return nil
}

// newDownstreamKey mints a client credential. The value is returned to the
// caller once and never stored in readable form anywhere else.
func newDownstreamKey() (string, error) {
	raw := make([]byte, downstreamKeyBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return downstreamKeyPrefix + base64.RawURLEncoding.EncodeToString(raw), nil
}

// logConfigurationChange records an accepted write. The values are deliberately
// absent: a write carries credentials, and the event only needs to say what was
// touched and by whom.
func (s *Server) logConfigurationChange(r *http.Request, event, resource string, id int64) {
	if s.Logger == nil {
		return
	}
	s.Logger.Info(event,
		"resource", resource,
		"id", id,
		"client_ip", clientIP(r))
}

func rowID(row map[string]any) int64 {
	id, _ := row["id"].(int64)
	return id
}
